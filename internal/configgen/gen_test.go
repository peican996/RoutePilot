package configgen

import "testing"

// 这是一个真实踩过的回归：Build 曾经直接改写传入的 Base，
// 导致每次重启内核都往 rules 里多插一遍 RULE-SET，规则无限叠加。
func TestBuildDoesNotMutateBase(t *testing.T) {
	base := map[string]any{
		"rules": []any{"MATCH,PROXY"},
		"proxy-groups": []any{
			map[string]any{
				"name":    "节点选择",
				"type":    "select",
				"proxies": []any{"a", "b"},
			},
		},
	}
	opts := Options{ProcessRules: true, Base: base}

	first := Build(opts)
	second := Build(opts)

	r1 := first["rules"].([]any)
	r2 := second["rules"].([]any)

	if len(r1) != len(r2) {
		t.Fatalf("两次 Build 规则数不一致：%d vs %d（说明 Base 被污染）", len(r1), len(r2))
	}
	// 2 条 RULE-SET + 原有 1 条 MATCH
	if len(r1) != 3 {
		t.Fatalf("预期 3 条规则，实际 %d 条：%v", len(r1), r1)
	}

	// 原始 Base 必须保持原样
	br, _ := base["rules"].([]any)
	if len(br) != 1 {
		t.Fatalf("Base 的 rules 被改写：%v", br)
	}
}

func TestMainProxyGroupPicksLargestSelector(t *testing.T) {
	cfg := map[string]any{
		"proxy-groups": []any{
			map[string]any{"name": "GLOBAL", "type": "select", "proxies": []any{"a", "b", "c"}},
			map[string]any{"name": "小分组", "type": "select", "proxies": []any{"a"}},
			map[string]any{"name": "自动选择", "type": "url-test", "proxies": []any{"a", "b", "c", "d"}},
			map[string]any{"name": "节点选择", "type": "select", "proxies": []any{"a", "b", "c", "d", "e"}},
		},
	}
	if got := MainProxyGroup(cfg); got != "节点选择" {
		t.Fatalf("应选中成员最多的 Selector「节点选择」，实际得到 %q", got)
	}
}

func TestMainProxyGroupSkipsUrlTest(t *testing.T) {
	cfg := map[string]any{
		"proxy-groups": []any{
			map[string]any{"name": "自动选择", "type": "url-test", "proxies": []any{"a", "b", "c", "d"}},
		},
	}
	if got := MainProxyGroup(cfg); got != "" {
		t.Fatalf("url-test 组不该被选中，实际得到 %q", got)
	}
}

// 订阅自带的 rule-providers 不能被整体覆盖：订阅规则里的 RULE-SET
// 会引用它们，丢掉之后内核会因找不到 provider 拒绝启动。
func TestBuildMergesRuleProviders(t *testing.T) {
	base := map[string]any{
		"rules": []any{"MATCH,PROXY"},
		"proxy-groups": []any{
			map[string]any{
				"name":    "节点选择",
				"type":    "select",
				"proxies": []any{"a", "b"},
			},
		},
		"rule-providers": map[string]any{
			"airport-set": map[string]any{
				"type": "http", "behavior": "domain", "url": "https://example.com/set.yaml",
			},
		},
	}

	cfg := Build(Options{ProcessRules: true, Base: base})

	providers, ok := cfg["rule-providers"].(map[string]any)
	if !ok {
		t.Fatalf("rule-providers 类型异常：%T", cfg["rule-providers"])
	}
	if _, ok := providers["airport-set"]; !ok {
		t.Fatalf("订阅自带的 provider 被丢弃了：%v", providers)
	}
	for _, want := range []string{"pp-direct", "pp-proxy"} {
		if _, ok := providers[want]; !ok {
			t.Fatalf("缺少进程分流 provider %s：%v", want, providers)
		}
	}
}
