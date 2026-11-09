// Package procid 统一进程分流规则的匹配身份。
//
// 规则不再只依赖 EXE basename：优先使用规范化后的完整路径，
// 只有确实拿不到路径时才降级为「仅名称」匹配，并保留可诊断的降级原因。
//
// 规范化策略（Windows）：
//   - 环境变量：展开 $VAR / ${VAR} / %VAR%（仅展开已存在的变量）
//   - 相对路径：转换为绝对路径
//   - 分隔符与冗余段：filepath.Clean（统一为反斜杠）
//   - 长路径前缀：去掉 \\?\ 与 \\?\UNC\，与 mihomo 上报格式对齐
//   - 大小写：匹配身份统一 ToLower，大小写不同的同一路径视为同一进程
//   - 符号链接/目录联接：不解析。mihomo 上报的是启动时使用的路径，
//     解析到目标路径反而可能与上报值不一致，导致规则匹配不上
//   - 8.3 短路径：不展开，按上报字符串处理
//
// 规则格式是逗号分隔的，因此包含逗号、换行、制表符或 NUL 的路径无法
// 安全写入 mihomo 规则；这类进程会明确降级为名称匹配并在日志中说明。
package procid

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind 是匹配身份的类型。
type Kind string

const (
	KindPath Kind = "path"
	KindName Kind = "name"
)

const (
	PathPrefix = "path:"
	NamePrefix = "name:"
)

var pctEnvRe = regexp.MustCompile(`%([^%]+)%`)

// Identity 是一个可持久化的进程匹配身份。
//
// Key 是规则表与 UI 使用的稳定键：
//   - path:<规范化完整路径>（保留原始大小写，便于界面展示）
//   - name:<小写 EXE 名>
//
// 路径身份在比较时必须忽略大小写，使用 Equal 而不是直接比较 Key。
type Identity struct {
	Kind Kind
	Key  string
	// Path 仅 KindPath 时有效，保留规范化后的原始大小写。
	Path string
	// Name 是 EXE basename（原始大小写）；KindName 时用于 PROCESS-NAME。
	Name string
	// Degraded 表示拿不到可用路径，只能按名称匹配。
	Degraded bool
	// DegradeReason 说明降级原因，供日志与界面提示。
	DegradeReason string
}

func (i Identity) Valid() bool { return i.Key != "" }

// Equal 比较两个身份是否指向同一个进程匹配目标。
func (i Identity) Equal(other Identity) bool {
	if i.Kind != other.Kind {
		return false
	}
	if i.Kind == KindPath {
		return strings.EqualFold(i.Key, other.Key)
	}
	return strings.EqualFold(i.Key, other.Key)
}

// RulePayload 返回 rule-provider payload 里的匹配部分：
// PROCESS-PATH,<path> 或 PROCESS-NAME,<name>。
func (i Identity) RulePayload() (string, error) {
	switch i.Kind {
	case KindPath:
		if err := validatePathPayload(i.Path); err != nil {
			return "", err
		}
		return "PROCESS-PATH," + i.Path, nil
	case KindName:
		if err := validateNamePayload(i.Name); err != nil {
			return "", err
		}
		return "PROCESS-NAME," + i.Name, nil
	default:
		return "", fmt.Errorf("未知的进程身份类型: %s", i.Kind)
	}
}

// Rule 返回完整分流规则（带出口）。
func (i Identity) Rule(target string) (string, error) {
	payload, err := i.RulePayload()
	if err != nil {
		return "", err
	}
	return payload + "," + target, nil
}

// FromProcess 根据 mihomo 上报的进程名与完整路径构造身份。
//
// 路径可用时返回 KindPath；路径为空、非法或包含会破坏规则语法的字符时，
// 降级为 KindName，并在 Degraded/DegradeReason 中说明。
func FromProcess(name, fullPath string) Identity {
	if strings.TrimSpace(fullPath) != "" {
		if p, err := NormalizePath(fullPath); err == nil {
			return Identity{
				Kind: KindPath,
				// 键统一小写，保证连接、规则表与前端永远能对上；
				// Path 保留原始大小写，用于展示与写入规则 payload。
				Key:  PathPrefix + strings.ToLower(p),
				Path: p,
				Name: filepath.Base(p),
			}
		} else if n, nameErr := NormalizeName(name); nameErr == nil {
			return Identity{
				Kind:          KindName,
				Key:           NamePrefix + strings.ToLower(n),
				Name:          n,
				Degraded:      true,
				DegradeReason: "完整路径不可用于规则匹配: " + err.Error(),
			}
		}
	}

	if n, err := NormalizeName(name); err == nil {
		return Identity{
			Kind:          KindName,
			Key:           NamePrefix + strings.ToLower(n),
			Name:          n,
			Degraded:      true,
			DegradeReason: "无法获取进程完整路径",
		}
	}
	return Identity{}
}

// ParseKey 解析持久化/前端传入的键。兼容旧版仅名称的键。
func ParseKey(raw string) (Identity, bool) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return Identity{}, false
	}
	lower := strings.ToLower(key)

	switch {
	case strings.HasPrefix(lower, PathPrefix):
		p, err := NormalizePath(key[len(PathPrefix):])
		if err != nil {
			return Identity{}, false
		}
		return Identity{Kind: KindPath, Key: PathPrefix + strings.ToLower(p), Path: p, Name: filepath.Base(p)}, true
	case strings.HasPrefix(lower, NamePrefix):
		n, err := NormalizeName(key[len(NamePrefix):])
		if err != nil {
			return Identity{}, false
		}
		return Identity{Kind: KindName, Key: NamePrefix + strings.ToLower(n), Name: n}, true
	default:
		// 旧版数据：直接以 EXE 名作为键。
		n, err := NormalizeName(key)
		if err != nil {
			return Identity{}, false
		}
		return Identity{
			Kind:          KindName,
			Key:           NamePrefix + strings.ToLower(n),
			Name:          n,
			Degraded:      true,
			DegradeReason: "旧版仅名称规则",
		}, true
	}
}

// NormalizePath 把任意来源的路径规范化为 mihomo 规则可用的绝对路径。
func NormalizePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", fmt.Errorf("路径为空")
	}
	p = expandEnv(p)
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", fmt.Errorf("相对路径无法转换为绝对路径: %w", err)
		}
		p = abs
	}
	p = filepath.Clean(p)
	// 去掉 Win32 长路径前缀，与 mihomo 上报的普通路径对齐。
	if len(p) >= 8 && strings.EqualFold(p[:8], `\\?\UNC\`) {
		p = `\\` + p[8:]
	} else if strings.HasPrefix(p, `\\?\`) {
		p = p[4:]
	}
	if err := validatePathPayload(p); err != nil {
		return "", err
	}
	return p, nil
}

// NormalizeName 校验并返回 EXE basename。
func NormalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("进程名为空")
	}
	// 兼容调用方误传完整路径。
	if strings.ContainsAny(name, `/\`) {
		name = filepath.Base(name)
	}
	if err := validateNamePayload(name); err != nil {
		return "", err
	}
	return name, nil
}

func validateNamePayload(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("进程名为空")
	}
	if strings.ContainsAny(name, ",\n\r\t\x00") {
		return fmt.Errorf("进程名包含逗号或控制字符，无法写入规则")
	}
	if strings.ContainsAny(name, `/\:`) {
		return fmt.Errorf("进程名包含路径分隔符或冒号")
	}
	return nil
}

func validatePathPayload(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("路径为空")
	}
	if strings.ContainsAny(p, ",\n\r\t\x00") {
		return fmt.Errorf("路径包含逗号或控制字符，无法安全写入规则")
	}
	return nil
}

// expandEnv 展开 $VAR、${VAR} 与 %VAR%。未定义的变量保持原样。
func expandEnv(p string) string {
	p = os.ExpandEnv(p)
	return pctEnvRe.ReplaceAllStringFunc(p, func(m string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(m, "%"), "%")
		if name == "" {
			return m
		}
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m
	})
}
