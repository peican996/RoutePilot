//go:build repro

package main

import (
	"strings"
	"testing"

	"routepilot/internal/configgen"
)

// T12（已修复）：两个不同目录下的同名 EXE 现在按完整路径匹配，
// 不再共享同一条 PROCESS-NAME 规则。
func TestReproT12SameNameExeDistinguishedByPath(t *testing.T) {
	plan := buildProcessTargetPlan(
		map[string]string{
			`path:C:\Apps\A\chrome.exe`: "target:NodeA",
			`path:C:\Apps\B\chrome.exe`: "target:NodeA",
		},
		map[string]bool{"NodeA": true},
		nil,
	)
	if len(plan.Rules) != 2 {
		t.Fatalf("同名不同目录应生成两条规则，实际 %v", plan.Rules)
	}
	for _, rule := range plan.Rules {
		if !strings.HasPrefix(rule, "PROCESS-PATH,") {
			t.Fatalf("应使用路径规则区分同名程序：%q", rule)
		}
	}

	cfg := configgen.Build(configgen.Options{
		ProcessRules:       true,
		ProcessCustomRules: plan.Rules,
		ProcessGroups:      plan.Groups,
	})
	rules, _ := cfg["rules"].([]any)
	pathRules := 0
	for _, r := range rules {
		if s, ok := r.(string); ok && strings.HasPrefix(s, "PROCESS-PATH") {
			pathRules++
		}
	}
	if pathRules != 2 {
		t.Fatalf("生成的配置应包含两条 PROCESS-PATH 规则，实际 %d", pathRules)
	}
}
