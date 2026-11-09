package configgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Options 是生成内核配置所需的全部输入。
type Options struct {
	MixedPort int    // HTTP/SOCKS 混合入站端口
	APIPort   int    // 外部控制 API 端口
	DNSListen int    // 内置 DNS 监听端口
	Secret    string // API 密钥，防止本机其他程序乱调
	Mode      string // rule / global / direct

	// Tun 开启后内核创建虚拟网卡，在 IP 层接管全部流量。
	// 这是代理"所有程序"（含不读系统代理设置的软件）的唯一办法。
	Tun bool

	// ProcessRules 开启按程序分流的 rule-provider。
	// 规则内容由上层动态写文件，改完通过 API 热刷新，不重载整个配置。
	ProcessRules bool

	// ProcessCustomRules 是"进程 → 指定节点/策略组"的直接规则，例如
	// PROCESS-NAME,chrome.exe,nx-pp-xxxx。改这部分需要重载配置，
	// 但不需要重启内核进程。
	ProcessCustomRules []string

	// ProcessGroups 是这些自定义规则指向的隐藏 select 组。
	// 组名统一带 nx-pp- 前缀，界面上不作为普通策略组展示。
	ProcessGroups []map[string]any

	// Base 是订阅解析出来的原始配置。为空时退化为一份最小可用的直连配置。
	Base map[string]any
}

func (o *Options) applyDefaults() {
	if o.MixedPort == 0 {
		o.MixedPort = 7890
	}
	if o.APIPort == 0 {
		o.APIPort = 9090
	}
	if o.DNSListen == 0 {
		o.DNSListen = 1053
	}
	if o.Mode == "" {
		o.Mode = "rule"
	}
}

// ProcessGroupPrefix 是进程自定义出口的隐藏策略组前缀。
// 这类组是内部实现，不应出现在普通策略组列表里。
const ProcessGroupPrefix = "nx-pp-"

// defaultGroups 在没有订阅时提供最小可用的策略组，
// 避免内核因为找不到引用的 proxy group 而拒绝启动。
func defaultGroups() []map[string]any {
	return []map[string]any{
		{
			"name":    "PROXY",
			"type":    "select",
			"proxies": []string{"DIRECT"},
		},
	}
}

// defaultRulesFor 生成以指定策略组兜底的默认规则。
// base64 订阅的策略组不叫 PROXY，兜底规则必须指向真实存在的组，
// 否则内核会因引用不存在的策略组拒绝启动。
func defaultRulesFor(group string) []string {
	if group == "" {
		group = "PROXY"
	}
	return []string{
		"DOMAIN-SUFFIX,local,DIRECT",
		"IP-CIDR,127.0.0.0/8,DIRECT,no-resolve",
		"IP-CIDR,10.0.0.0/8,DIRECT,no-resolve",
		"IP-CIDR,172.16.0.0/12,DIRECT,no-resolve",
		"IP-CIDR,192.168.0.0/16,DIRECT,no-resolve",
		"GEOIP,LAN,DIRECT,no-resolve",
		"GEOIP,CN,DIRECT",
		"MATCH," + group,
	}
}

func defaultDNS(port int) map[string]any {
	return map[string]any{
		"enable":         true,
		"ipv6":           false,
		"listen":         fmt.Sprintf("127.0.0.1:%d", port),
		"enhanced-mode":  "fake-ip",
		"fake-ip-range":  "198.18.0.1/16",
		"fake-ip-filter": []string{"*.lan", "*.local", "localhost.ptlogin2.qq.com"},
		"default-nameserver": []string{
			"223.5.5.5",
			"119.29.29.29",
		},
		"nameserver": []string{
			"https://dns.alidns.com/dns-query",
			"https://doh.pub/dns-query",
		},
		"fallback": []string{
			"https://1.1.1.1/dns-query",
			"https://dns.google/dns-query",
		},
	}
}

// deepCopy 递归复制 map / slice。
// Build 会往配置里插入端口、规则等字段，如果不拷贝就会直接改到调用方持有的
// 订阅配置上——每重启一次内核就多插一遍 RULE-SET，规则越叠越多。
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = deepCopy(val)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = deepCopy(val)
		}
		return s
	default:
		return v
	}
}

// strippedSubscriptionKeys 是订阅里即使出现也必须移除的字段。
//
// 这些字段要么会额外开放入站端口（listeners / tunnels / port / socks-port /
// redir-port / tproxy-port），要么会把控制面板或认证逻辑塞进内核
// （external-ui / authentication），要么可能引入脚本执行。
// 本应用只负责一个监听在 127.0.0.1 的混合端口，其余一律不应由订阅决定。
var strippedSubscriptionKeys = []string{
	"listeners",
	"tunnels",
	"authentication",
	"port",
	"socks-port",
	"redir-port",
	"tproxy-port",
	"external-ui",
	"external-ui-url",
	"external-ui-name",
	"script",
}

func sanitizeSubscriptionConfig(cfg map[string]any) {
	for _, k := range strippedSubscriptionKeys {
		delete(cfg, k)
	}
}

// Build 组装最终的配置对象。
//
// 有 Base（订阅配置）时以它为骨架，节点、代理组、规则全部保留机场原样，
// 只覆盖本应用必须掌控的几个字段——端口、控制 API、密钥、进程识别。
// 没有 Base 时生成一份最小可用的直连配置。
func Build(o Options) map[string]any {
	o.applyDefaults()

	var cfg map[string]any
	if o.Base != nil {
		cfg, _ = deepCopy(o.Base).(map[string]any)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}

	// 订阅内容不可全信：入站监听、隧道、外部 UI 等字段会以管理员权限
	// 在内核里生效，必须由本应用白名单式接管或直接剥掉。
	sanitizeSubscriptionConfig(cfg)

	// 以下字段无论订阅里怎么写都必须由本应用决定，否则端口会撞车、
	// 控制 API 会被订阅里的设置顶掉。
	cfg["mixed-port"] = o.MixedPort
	cfg["allow-lan"] = false
	cfg["bind-address"] = "127.0.0.1"
	cfg["mode"] = o.Mode
	cfg["log-level"] = "info"
	cfg["external-controller"] = fmt.Sprintf("127.0.0.1:%d", o.APIPort)
	cfg["secret"] = o.Secret

	// 整个项目的地基：打开它，/connections 返回的每条连接才会带 processPath，
	// 按程序分流才有可能实现。
	cfg["find-process-mode"] = "always"

	// 记住用户手动选的节点，重启内核后不会重置。
	profile, ok := cfg["profile"].(map[string]any)
	if !ok || profile == nil {
		profile = map[string]any{}
	}
	profile["store-selected"] = true
	profile["store-fake-ip"] = true
	cfg["profile"] = profile

	if _, ok := cfg["proxies"]; !ok {
		cfg["proxies"] = []map[string]any{}
	}
	if _, ok := cfg["proxy-groups"]; !ok {
		cfg["proxy-groups"] = defaultGroups()
	}
	// 进程自定义出口的隐藏组追加在订阅组之后。
	if len(o.ProcessGroups) > 0 {
		groups, _ := cfg["proxy-groups"].([]any)
		if groups == nil {
			if typed, ok := cfg["proxy-groups"].([]map[string]any); ok {
				for _, g := range typed {
					groups = append(groups, g)
				}
			}
		}
		for _, g := range o.ProcessGroups {
			groups = append(groups, g)
		}
		cfg["proxy-groups"] = groups
	}

	// 规则统一成 []any，方便在后面插入进程分流规则。
	var rules []any
	switch v := cfg["rules"].(type) {
	case []any:
		rules = v
	case []string:
		for _, s := range v {
			rules = append(rules, s)
		}
	}
	if rules == nil {
		// 没有订阅规则时用默认规则，兜底组要跟随实际的主策略组：
		// base64 订阅的组叫「节点选择」，写死 MATCH,PROXY 内核会拒绝启动。
		for _, s := range defaultRulesFor(MainProxyGroup(cfg)) {
			rules = append(rules, s)
		}
	}

	// 按程序分流的规则由本应用动态维护，必须排在所有订阅规则之前，
	// 否则机场自己的 DOMAIN-SUFFIX 规则会先命中，用户的选择就失效了。
	//
	// 关键约束：mihomo 的 classical rule-provider 里**不能自带策略**，
	// payload 只写匹配条件，出口由外层 RULE-SET 的第二个参数决定。
	// 所以按策略拆成两个 provider，而不是把出口写进规则里。
	if o.ProcessRules {
		providers := map[string]any{
			"pp-direct": map[string]any{
				"type": "file", "behavior": "classical", "path": "./process-direct.yaml",
			},
		}
		prepend := []any{"RULE-SET,pp-direct,DIRECT"}

		if group := MainProxyGroup(cfg); group != "" {
			providers["pp-proxy"] = map[string]any{
				"type": "file", "behavior": "classical", "path": "./process-proxy.yaml",
			}
			prepend = append(prepend, fmt.Sprintf("RULE-SET,pp-proxy,%s", group))
		}

		// "进程 → 指定节点/组"的直接规则必须排在自己的 direct/proxy
		// 规则之后、订阅规则之前；每条自带策略，不需要额外 provider。
		for _, rule := range o.ProcessCustomRules {
			prepend = append(prepend, rule)
		}

		// 合并而不是覆盖。订阅规则里经常有 RULE-SET 引用它自己的 rule-provider，
		// 整体替换会让这些引用指向不存在的 provider，内核直接拒绝启动。
		merged := map[string]any{}
		if exist, ok := cfg["rule-providers"].(map[string]any); ok {
			for k, v := range exist {
				merged[k] = v
			}
		}
		for k, v := range providers {
			merged[k] = v
		}
		cfg["rule-providers"] = merged
		rules = append(prepend, rules...)
	}
	cfg["rules"] = rules

	// DNS 保留订阅自己的策略（fake-ip 过滤表通常很全），只把监听端口换成我们挑的，
	// 避免和系统里其他代理软件撞端口。
	if dns, ok := cfg["dns"].(map[string]any); ok {
		dns["enable"] = true
		dns["listen"] = fmt.Sprintf("127.0.0.1:%d", o.DNSListen)
		cfg["dns"] = dns
	} else {
		cfg["dns"] = defaultDNS(o.DNSListen)
	}

	// TUN 段由本应用独占控制，订阅里就算有也一律覆盖。
	if o.Tun {
		cfg["tun"] = map[string]any{
			"enable": true,
			// mixed = system + gvisor 混合。Windows 上纯 system 栈兼容性差，
			// 纯 gvisor 吞吐略低，mixed 是折中最优。
			"stack": "mixed",
			// 把发往 53 端口的 DNS 查询全部劫持进内核，配合 fake-ip 使用。
			"dns-hijack": []string{"any:53"},
			// 自动添加/撤销路由表，把默认流量引到虚拟网卡。
			"auto-route":            true,
			"auto-detect-interface": true,
			"mtu":                   9000,
		}
	} else {
		delete(cfg, "tun")
	}

	return cfg
}

var builtinGroups = map[string]bool{
	"GLOBAL": true, "DIRECT": true, "REJECT": true, "REJECT-DROP": true,
	"COMPATIBLE": true, "PASS": true, "PASS-RULE": true, "PROXY": true,
}

// MainProxyGroup 从配置里找出用户真正在用的策略组。
// 机场的组名五花八门（"节点选择" / "Proxy" / "手动切换"），唯一稳定的特征是
// 「成员最多的 Selector」——url-test 那类是自动选路，往里面塞节点不生效。
func MainProxyGroup(cfg map[string]any) string {
	if cfg == nil {
		return ""
	}
	groups, _ := cfg["proxy-groups"].([]any)
	best, bestCount := "", 0
	for _, g := range groups {
		m, ok := g.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		if typ != "select" && typ != "Selector" {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" || builtinGroups[name] || strings.HasPrefix(name, ProcessGroupPrefix) {
			continue
		}
		proxies, _ := m["proxies"].([]any)
		members := len(proxies)
		// base64 订阅走 proxy-provider，组里只有 use 没有 proxies；
		// 不把 use 算进成员数，这类订阅会被误判成"没有可用策略组"。
		if use, ok := m["use"].([]any); ok {
			members += len(use)
		}
		if members > bestCount {
			bestCount = members
			best = name
		}
	}
	return best
}

// Write 把配置写到 dataDir/config.yaml 并返回路径。
func Write(dataDir string, o Options) (string, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", fmt.Errorf("创建数据目录失败: %w", err)
	}
	cfg := Build(o)
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("序列化配置失败: %w", err)
	}
	path := filepath.Join(dataDir, "config.yaml")
	// config.yaml 里有 API secret，权限收紧到仅本人可读。
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return "", fmt.Errorf("写入配置失败: %w", err)
	}
	return path, nil
}

// writeFileAtomic 先写同目录临时文件并 Sync，再原子替换。
// 写一半崩溃时磁盘上仍是上一份完整配置，内核不会读到截断的 YAML。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	done := false
	defer func() {
		_ = f.Close()
		if !done {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	done = true
	return nil
}
