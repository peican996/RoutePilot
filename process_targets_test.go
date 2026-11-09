package main

import (
	"strings"
	"testing"

	"routepilot/internal/configgen"
	"routepilot/internal/subscription"
)

func TestProcessTargetPlanExplicitTarget(t *testing.T) {
	plan := buildProcessTargetPlan(
		map[string]string{"chrome.exe": "target:NodeA"},
		map[string]bool{"NodeA": true},
		nil,
	)
	if len(plan.Groups) != 1 || len(plan.Rules) != 1 || len(plan.Selections) != 1 {
		t.Fatalf("计划数量不符：groups=%d rules=%d selections=%d",
			len(plan.Groups), len(plan.Rules), len(plan.Selections))
	}
	if !strings.HasPrefix(plan.Groups[0]["name"].(string), configgen.ProcessGroupPrefix) {
		t.Fatalf("隐藏组名应带前缀：%v", plan.Groups[0]["name"])
	}
	proxies, ok := plan.Groups[0]["proxies"].([]any)
	if !ok || len(proxies) != 1 || proxies[0] != "NodeA" {
		t.Fatalf("显式目标组应只包含目标节点：%v", plan.Groups[0])
	}
	if plan.Selections[0].ProviderBacked {
		t.Fatal("显式节点不应标记为 provider 支撑")
	}
	if plan.Rules[0] != `PROCESS-NAME,chrome.exe,`+plan.Selections[0].Group {
		t.Fatalf("规则格式不符：%q", plan.Rules[0])
	}
}

func TestProcessTargetPlanProviderTarget(t *testing.T) {
	plan := buildProcessTargetPlan(
		map[string]string{"curl.exe": "target:ProviderNode"},
		nil,
		[]string{"routepilot"},
	)
	if len(plan.Groups) != 1 {
		t.Fatalf("provider 目标应生成隐藏组：%v", plan.Groups)
	}
	g := plan.Groups[0]
	proxies, _ := g["proxies"].([]any)
	if len(proxies) != 1 || proxies[0] != "DIRECT" {
		t.Fatalf("provider 组应含 DIRECT 兜底：%v", g)
	}
	use, _ := g["use"].([]any)
	if len(use) != 1 || use[0] != "routepilot" {
		t.Fatalf("provider 组应引用 provider：%v", g)
	}
	if !plan.Selections[0].ProviderBacked {
		t.Fatal("provider 目标应标记为 provider 支撑")
	}
}

func TestProcessTargetPlanDedupAndStableName(t *testing.T) {
	plan := buildProcessTargetPlan(
		map[string]string{
			"chrome.exe": "target:NodeA",
			"curl.exe":   "target:NodeA",
		},
		map[string]bool{"NodeA": true},
		nil,
	)
	if len(plan.Groups) != 1 || len(plan.Rules) != 2 {
		t.Fatalf("同一目标应复用隐藏组：groups=%d rules=%d", len(plan.Groups), len(plan.Rules))
	}
	if processTargetGroupName("NodeA") != processTargetGroupName("NodeA") {
		t.Fatal("隐藏组名应稳定")
	}
	if processTargetGroupName("NodeA") == processTargetGroupName("NodeB") {
		t.Fatal("不同目标不应共用隐藏组名")
	}
}

func TestProcessTargetPlanInvalidTargetFallsBack(t *testing.T) {
	plan := buildProcessTargetPlan(
		map[string]string{"curl.exe": "target:Missing"},
		nil, // 没有 provider 可用
		nil,
	)
	if len(plan.Groups) != 0 || len(plan.Rules) != 0 {
		t.Fatalf("失效目标应被忽略：%v", plan)
	}
}

func TestProcessTargetPlanIgnoresBasicPolicies(t *testing.T) {
	plan := buildProcessTargetPlan(
		map[string]string{
			"a.exe": "direct",
			"b.exe": "proxy",
			"c.exe": "follow",
		},
		nil, nil,
	)
	if len(plan.Groups) != 0 || len(plan.Rules) != 0 {
		t.Fatalf("direct/proxy/follow 不应生成隐藏组：%v", plan)
	}
}

func TestSubscriptionTargetsBase64(t *testing.T) {
	// base64 订阅的节点由 provider 在解析阶段之外加载，
	// 直接写进 group 的 proxies 会让内核报 proxy not found。
	res := &subscription.Result{
		Format:    "base64",
		Nodes:     []subscription.NodeInfo{{Name: "NodeA"}},
		Groups:    []string{"节点选择"},
		Providers: []string{"routepilot"},
	}
	targets, providers := subscriptionTargets(res)
	if targets["NodeA"] {
		t.Fatal("base64 节点不应作为显式目标")
	}
	if !targets["节点选择"] {
		t.Fatal("base64 策略组应可作为目标")
	}
	if len(providers) != 1 || providers[0] != "routepilot" {
		t.Fatalf("provider 列表不符：%v", providers)
	}
}

func TestSubscriptionTargetsClash(t *testing.T) {
	res := &subscription.Result{
		Format: "clash",
		Nodes:  []subscription.NodeInfo{{Name: "NodeA"}},
		Groups: []string{"Proxy"},
	}
	targets, providers := subscriptionTargets(res)
	if !targets["NodeA"] || !targets["Proxy"] {
		t.Fatalf("Clash 订阅的节点与组都应可作为目标：%v", targets)
	}
	if len(providers) != 0 {
		t.Fatalf("没有 provider 时不应返回：%v", providers)
	}
}
