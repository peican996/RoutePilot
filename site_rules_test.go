package main

import (
	"strings"
	"testing"
)

func TestNormalizeSiteDomain(t *testing.T) {
	valid := map[string]string{
		"Example.COM":       "example.com",
		" www.google.com ":  "www.google.com",
		".example.com.":     "example.com",
		"a-b.example.co.uk": "a-b.example.co.uk",
	}
	for in, want := range valid {
		got, err := normalizeSiteDomain(in)
		if err != nil {
			t.Fatalf("%q 应合法: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q -> %q，期望 %q", in, got, want)
		}
	}

	invalid := []string{
		"", "   ", "example", "http://example.com", "example.com/path",
		"a,b.com", "a b.com", "-bad.com", "bad-.com", "127.0.0.1",
		"例子.中国", "a..b.com",
	}
	for _, in := range invalid {
		if _, err := normalizeSiteDomain(in); err == nil {
			t.Fatalf("%q 应被拒绝", in)
		}
	}
}

func TestBuildSiteRulePlanDirectAndExplicit(t *testing.T) {
	plan := buildSiteRulePlan([]SiteRule{
		{Domain: "example.com", Policy: "direct"},
		{Domain: "google.com", Policy: "target:NodeA"},
		{Domain: "corp.com", Policy: "proxy"},
		{Domain: "stalk.google.com", Type: "exact", Policy: "direct"},
	}, map[string]bool{"NodeA": true}, nil, "节点选择")

	if len(plan.Groups) != 0 || len(plan.Selections) != 0 {
		t.Fatalf("显式目标不应生成隐藏组: %+v", plan)
	}
	want := []string{
		"DOMAIN,stalk.google.com,DIRECT",
		"DOMAIN-SUFFIX,example.com,DIRECT",
		"DOMAIN-SUFFIX,google.com,NodeA",
		"DOMAIN-SUFFIX,corp.com,节点选择",
	}
	if len(plan.Rules) != len(want) {
		t.Fatalf("规则数量不符: %v", plan.Rules)
	}
	for i := range want {
		if plan.Rules[i] != want[i] {
			t.Fatalf("第 %d 条规则应为 %q，实际 %q", i, want[i], plan.Rules[i])
		}
	}
}

func TestBuildSiteRulePlanProviderTarget(t *testing.T) {
	plan := buildSiteRulePlan([]SiteRule{
		{Domain: "google.com", Policy: "target:ProviderNode"},
	}, nil, []string{"routepilot"}, "")

	if len(plan.Groups) != 1 || len(plan.Selections) != 1 {
		t.Fatalf("provider 目标应生成隐藏组与选中动作: %+v", plan)
	}
	group, _ := plan.Groups[0]["name"].(string)
	if !strings.HasPrefix(group, "nx-pp-") {
		t.Fatalf("隐藏组名应带前缀: %q", group)
	}
	if !strings.HasSuffix(plan.Rules[0], ","+group) {
		t.Fatalf("规则应指向隐藏组: %q", plan.Rules[0])
	}
	if !plan.Selections[0].ProviderBacked || plan.Selections[0].Target != "ProviderNode" {
		t.Fatalf("选中的 provider 信息不符: %+v", plan.Selections[0])
	}
}

func TestMergeTargetPlansDedup(t *testing.T) {
	proc := processTargetPlan{
		Groups: []map[string]any{{"name": "nx-pp-x"}},
		Selections: []processTargetSelection{
			{Group: "nx-pp-x", Target: "NodeX", ProviderBacked: true},
		},
	}
	site := processTargetPlan{
		Groups: []map[string]any{{"name": "nx-pp-x"}},
		Selections: []processTargetSelection{
			{Group: "nx-pp-x", Target: "NodeX", ProviderBacked: true},
		},
	}
	got := mergeTargetPlans(proc, site)
	if len(got.Groups) != 1 || len(got.Selections) != 1 {
		t.Fatalf("同名隐藏组/选中动作应去重: %+v", got)
	}
}
