//go:build repro

package main

import (
	"strings"
	"testing"

	"routepilot/internal/configgen"
)

// T12：两个不同目录下的同名 EXE。当前规则只能按 EXE 名匹配，
// 且进程名里不允许出现路径分隔符，因此无法区分同名的两个程序。
func TestReproT12SameNameExeCannotBeDistinguished(t *testing.T) {
	// 1. 路径形式的进程名被拒绝，说明没有"按路径匹配"的能力。
	if got := sanitizeProcName(`C:\Program Files\App\chrome.exe`); got != "" {
		t.Fatalf("预期路径形式被拒绝，实际 %q", got)
	}

	// 2. 规则只按 basename 生成，两个目录的同名程序共享同一条规则。
	plan := buildProcessTargetPlan(
		map[string]string{"chrome.exe": "target:NodeA"},
		map[string]bool{"NodeA": true},
		nil,
	)
	if len(plan.Rules) != 1 {
		t.Fatalf("预期只有一条按名称的规则，实际 %v", plan.Rules)
	}
	if !strings.HasPrefix(plan.Rules[0], "PROCESS-NAME,chrome.exe,") {
		t.Fatalf("规则应只含 EXE 名：%q", plan.Rules[0])
	}

	// 3. 生成的配置里不存在 ANY 路径匹配条件（PROCESS-PATH）。
	cfg := configgen.Build(configgen.Options{
		ProcessRules:       true,
		ProcessCustomRules: plan.Rules,
		ProcessGroups:      plan.Groups,
	})
	rules, _ := cfg["rules"].([]any)
	for _, r := range rules {
		if s, ok := r.(string); ok && strings.HasPrefix(s, "PROCESS-PATH") {
			t.Fatalf("不应出现路径规则：%s", s)
		}
	}
	t.Log("FAIL: 同名不同路径的 exe 会命中同一条规则（T12 复现）")
}
