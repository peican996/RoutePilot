package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 进程规则的三个文件（规则表 JSON + 两个 mihomo classical provider）必须
// 作为一个整体发布：分别原子写只能保证"单个文件不截断"，不能保证
// "三个文件属于同一版本"。中途崩溃会留下新旧混合，内核可能读到
// 一套自相矛盾的进程规则。
//
// 最小可靠方案：generation 目录 + manifest。
//
//	procgen/proxy-<gen>.yaml
//	procgen/direct-<gen>.yaml
//	procgen/rules-<gen>.json
//	procgen/current.json      ← 最后原子写入，指向本代全部文件与哈希
//	procgen/current.json.prev ← 上一代 manifest，用于 current 损坏时回退
//
// 启动时只认"manifest + 哈希全部校验通过"的那一代；否则回退上一代，
// 再否则回退旧版平铺文件（process-rules.json 等），保证升级/降级都可用。
const (
	procGenDirName      = "procgen"
	procGenManifestName = "current.json"
	procGenVersion      = 1
)

// procGenPaths 是当前已发布版本三个文件的绝对路径。
type procGenPaths struct {
	Gen    int64
	Proxy  string
	Direct string
	Rules  string
}

// procGenManifest 是 procgen 版本的提交点。
type procGenManifest struct {
	Version   int               `json:"version"`
	Gen       int64             `json:"gen"`
	Proxy     string            `json:"proxy"`
	Direct    string            `json:"direct"`
	Rules     string            `json:"rules"`
	Hashes    map[string]string `json:"hashes"`
	CreatedAt int64             `json:"createdAt"`
}

// procPublishHook 仅用于测试注入"写完文件但尚未发布 manifest"等故障点。
var procPublishHook = func(stage string) error { return nil }

func procGenDir() string {
	return filepath.Join(dataDir(), procGenDirName)
}

// publishProcRules 把一份规则快照写成新 generation，校验后原子发布 manifest。
// 任一步失败都不会改变 current.json，旧版本继续可用。
func (s *CoreService) publishProcRules(rules map[string]string) error {
	s.procGenMu.Lock()
	defer s.procGenMu.Unlock()

	proxyYAML, directYAML := renderProcRulesPayload(rules)
	rulesJSON, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err
	}

	gen := time.Now().UnixNano()
	dir := procGenDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建进程规则目录失败: %w", err)
	}

	m := procGenManifest{
		Version:   procGenVersion,
		Gen:       gen,
		Proxy:     fmt.Sprintf("proxy-%d.yaml", gen),
		Direct:    fmt.Sprintf("direct-%d.yaml", gen),
		Rules:     fmt.Sprintf("rules-%d.json", gen),
		Hashes:    map[string]string{},
		CreatedAt: time.Now().UnixMilli(),
	}
	payloads := map[string][]byte{
		m.Proxy:  []byte(proxyYAML),
		m.Direct: []byte(directYAML),
		m.Rules:  rulesJSON,
	}

	cleanup := func() {
		for name := range payloads {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}

	for name, data := range payloads {
		path := filepath.Join(dir, name)
		if err := writeFileAtomic(path, data, 0o644); err != nil {
			cleanup()
			return fmt.Errorf("写入进程规则 %s 失败: %w", name, err)
		}
		sum := sha256.Sum256(data)
		m.Hashes[name] = hex.EncodeToString(sum[:])
	}

	// 模拟"文件写完、manifest 未发布"的中断：调用方会看到错误，
	// 但 current.json 仍指向上一个完整版本。
	if err := procPublishHook("before-manifest"); err != nil {
		cleanup()
		return err
	}

	// manifest 最后写，它就是整个版本的提交点。
	if err := s.writeProcGenManifestLocked(m); err != nil {
		cleanup()
		return fmt.Errorf("发布进程规则版本失败: %w", err)
	}

	paths := &procGenPaths{
		Gen:    gen,
		Proxy:  filepath.Join(dir, m.Proxy),
		Direct: filepath.Join(dir, m.Direct),
		Rules:  filepath.Join(dir, m.Rules),
	}
	s.setProcGenPaths(paths)
	// 旧版平铺文件只用于降级兼容，写失败不影响当前版本。
	s.writeLegacyProcSnapshot(rules)
	s.cleanupProcGensLocked(m)
	return nil
}

// writeLegacyProcSnapshot 尽力维护旧版平铺文件，方便回退到老版本程序。
func (s *CoreService) writeLegacyProcSnapshot(rules map[string]string) {
	mb, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(filepath.Join(dataDir(), procRulesJSON), mb, 0o644); err != nil {
		s.dbg("写入旧版进程规则失败: %v", err)
		return
	}
	if err := s.writeProcRuleSnapshotFiles(rules); err != nil {
		s.dbg("写入旧版进程规则 provider 失败: %v", err)
	}
}

// writeProcGenManifestLocked 先保存上一代 manifest，再原子替换 current。
func (s *CoreService) writeProcGenManifestLocked(m procGenManifest) error {
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	dir := procGenDir()
	cur := filepath.Join(dir, procGenManifestName)
	tmp := cur + ".next"
	prev := cur + ".prev"
	if err := writeFileAtomic(tmp, mb, 0o644); err != nil {
		return err
	}
	if _, statErr := os.Stat(cur); statErr == nil {
		_ = os.Remove(prev)
		if err := copyFile(cur, prev); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("备份上一版 manifest 失败: %w", err)
		}
	}
	if err := os.Rename(tmp, cur); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// resolvePublishedProcRules 返回当前可用的版本与规则内容。
// 优先 current，哈希校验失败时回退 prev；都不行返回 ok=false，
// 由调用方回退旧版平铺文件。
func (s *CoreService) resolvePublishedProcRules() (*procGenPaths, map[string]string, bool) {
	s.procGenMu.Lock()
	defer s.procGenMu.Unlock()

	dir := procGenDir()
	cur := filepath.Join(dir, procGenManifestName)
	for _, manifestPath := range []string{cur, cur + ".prev"} {
		m, ok := readProcGenManifest(manifestPath)
		if !ok {
			continue
		}
		if err := verifyProcGen(dir, m); err != nil {
			s.dbg("进程规则版本校验失败（%s）: %v", filepath.Base(manifestPath), err)
			continue
		}
		rules, err := readProcGenRules(filepath.Join(dir, m.Rules))
		if err != nil {
			s.dbg("读取进程规则版本失败: %v", err)
			continue
		}
		return &procGenPaths{
			Gen:    m.Gen,
			Proxy:  filepath.Join(dir, m.Proxy),
			Direct: filepath.Join(dir, m.Direct),
			Rules:  filepath.Join(dir, m.Rules),
		}, rules, true
	}
	return nil, nil, false
}

func readProcGenManifest(path string) (procGenManifest, bool) {
	var m procGenManifest
	state, err := readConfigJSON(path, &m)
	if state != configOK || err != nil {
		return m, false
	}
	// 未来版本写下的 generation 不能按当前结构解释。
	if m.Version > procGenVersion || m.Gen == 0 ||
		m.Proxy == "" || m.Direct == "" || m.Rules == "" {
		return m, false
	}
	// manifest 是本地文件，但仍要拒绝路径穿越，避免被手工改坏后读到目录外。
	for _, name := range []string{m.Proxy, m.Direct, m.Rules} {
		if filepath.Base(name) != name || strings.ContainsAny(name, `/\`) || name == "." {
			return m, false
		}
	}
	return m, true
}

func verifyProcGen(dir string, m procGenManifest) error {
	for _, name := range []string{m.Proxy, m.Direct, m.Rules} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("缺少文件 %s: %w", name, err)
		}
		want := m.Hashes[name]
		if want == "" {
			return fmt.Errorf("manifest 缺少 %s 的哈希", name)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != want {
			return fmt.Errorf("文件 %s 哈希不符", name)
		}
	}
	return nil
}

func readProcGenRules(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// setProcGenPaths 缓存当前发布路径，供 options() 生成内核配置使用。
func (s *CoreService) setProcGenPaths(p *procGenPaths) {
	s.mu.Lock()
	if p == nil {
		s.procProxyPath = procProxyYAML
		s.procDirectPath = procDirectYAML
	} else {
		// mihomo 以数据目录为工作目录；配置里统一用正斜杠，跨平台稳定。
		s.procProxyPath = "./" + procGenDirName + "/" + filepath.Base(p.Proxy)
		s.procDirectPath = "./" + procGenDirName + "/" + filepath.Base(p.Direct)
	}
	s.mu.Unlock()
}

// cleanupProcGensLocked 删除不再被 current / prev 引用的旧 generation 文件。
func (s *CoreService) cleanupProcGensLocked(current procGenManifest) {
	dir := procGenDir()
	keep := map[string]bool{
		current.Proxy: true, current.Direct: true, current.Rules: true,
		procGenManifestName:           true,
		procGenManifestName + ".prev": true,
	}
	if prev, ok := readProcGenManifest(filepath.Join(dir, procGenManifestName+".prev")); ok {
		keep[prev.Proxy] = true
		keep[prev.Direct] = true
		keep[prev.Rules] = true
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || keep[e.Name()] {
			continue
		}
		if !strings.HasPrefix(e.Name(), "proxy-") &&
			!strings.HasPrefix(e.Name(), "direct-") &&
			!strings.HasPrefix(e.Name(), "rules-") {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, b, 0o600)
}

// legacyProcFiles 返回旧版平铺文件路径，用于降级兼容与回退。
func legacyProcFiles() (rules, proxy, direct string) {
	dir := dataDir()
	return filepath.Join(dir, procRulesJSON), filepath.Join(dir, procProxyYAML), filepath.Join(dir, procDirectYAML)
}

// sortGenFiles 仅供测试/诊断输出使用。
func sortGenFiles(names []string) []string {
	sort.Strings(names)
	return names
}
