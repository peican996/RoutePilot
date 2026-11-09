package main

import (
	"strings"
	"testing"

	"routepilot/internal/procid"
)

// 同名不同目录的 EXE 必须生成两条独立的 PROCESS-PATH 规则。
func TestProcessTargetPlanPathRules(t *testing.T) {
	plan := buildProcessTargetPlan(
		map[string]string{
			`path:C:\Apps\A\chrome.exe`:     "target:NodeA",
			`path:D:\Portable\B\chrome.exe`: "target:NodeA",
		},
		map[string]bool{"NodeA": true},
		nil,
	)
	if len(plan.Groups) != 1 || len(plan.Rules) != 2 {
		t.Fatalf("同名不同目录应复用隐藏组但生成两条规则：groups=%d rules=%d",
			len(plan.Groups), len(plan.Rules))
	}
	for _, rule := range plan.Rules {
		if !strings.HasPrefix(rule, "PROCESS-PATH,") {
			t.Fatalf("路径身份必须生成 PROCESS-PATH：%q", rule)
		}
	}
	if plan.Rules[0] == plan.Rules[1] {
		t.Fatalf("两条规则不应相同：%v", plan.Rules)
	}
	if !strings.Contains(strings.ToLower(plan.Rules[0]), strings.ToLower(`C:\Apps\A\chrome.exe`)) ||
		!strings.Contains(strings.ToLower(plan.Rules[1]), strings.ToLower(`D:\Portable\B\chrome.exe`)) {
		t.Fatalf("规则未保留完整路径：%v", plan.Rules)
	}
}

// 旧数据或无法获取路径时退化为 PROCESS-NAME，行为保持兼容。
func TestProcessTargetPlanNameFallback(t *testing.T) {
	plan := buildProcessTargetPlan(
		map[string]string{"chrome.exe": "target:NodeA"},
		map[string]bool{"NodeA": true},
		nil,
	)
	if len(plan.Rules) != 1 || plan.Rules[0] != `PROCESS-NAME,chrome.exe,`+plan.Selections[0].Group {
		t.Fatalf("名称降级规则不符：%v", plan.Rules)
	}
}

// provider 规则文件同样要区分路径与名称。
func TestRenderProcRulesPayloadIdentity(t *testing.T) {
	proxyYAML, directYAML := renderProcRulesPayload(map[string]string{
		`path:C:\Apps\A\app.exe`: "proxy",
		"name:curl.exe":          "direct",
	})
	if !strings.Contains(strings.ToLower(proxyYAML), strings.ToLower(`  - PROCESS-PATH,C:\Apps\A\app.exe`)) {
		t.Fatalf("代理规则未生成路径 payload：\n%s", proxyYAML)
	}
	if !strings.Contains(directYAML, "  - PROCESS-NAME,curl.exe") {
		t.Fatalf("直连规则未生成名称 payload：\n%s", directYAML)
	}
}

// 旧版 process-rules.json 中的纯 EXE 名应迁移为 name: 身份键。
func TestNormalizeProcRulesMigration(t *testing.T) {
	out, changed := normalizeProcRules(map[string]string{
		"chrome.exe":               "proxy",
		"PATH:C:\\Apps\\app.exe":   "direct",
		`path:C:\Apps\A,B\bad.exe`: "direct",
		"name:CURL.EXE":            "direct",
	})
	if !changed {
		t.Fatal("迁移应标记为已变更")
	}
	if len(out) != 3 {
		t.Fatalf("非法路径应被丢弃，实际 %v", out)
	}
	if out["name:chrome.exe"] != "proxy" {
		t.Fatalf("旧名称键未迁移：%v", out)
	}
	if out[`path:c:\apps\app.exe`] != "direct" {
		t.Fatalf("路径键未规范化：%v", out)
	}
	if out["name:curl.exe"] != "direct" {
		t.Fatalf("名称键未统一大小写：%v", out)
	}
}

// 仅大小写不同的路径是同一身份，迁移时只保留一条。
func TestNormalizeProcRulesCaseDedup(t *testing.T) {
	out, changed := normalizeProcRules(map[string]string{
		`path:C:\Apps\app.exe`: "proxy",
		`path:c:\apps\APP.EXE`: "direct",
	})
	if !changed || len(out) != 1 {
		t.Fatalf("大小写重复应合并为一条：changed=%v out=%v", changed, out)
	}
	if out[`path:c:\apps\app.exe`] != "proxy" {
		t.Fatalf("排序后应保留第一条：%v", out)
	}
}

// 路径规则只影响同一路径；名称规则影响所有同名进程。
func TestIdentityMatches(t *testing.T) {
	targetPath := procid.FromProcess("chrome.exe", `C:\Apps\A\chrome.exe`)
	connPathSame := procid.FromProcess("CHROME.EXE", `c:\apps\a\CHROME.EXE`)
	connPathOther := procid.FromProcess("chrome.exe", `D:\Other\chrome.exe`)
	connNameOnly := procid.FromProcess("chrome.exe", "")

	if !identityMatches(targetPath, connPathSame) {
		t.Fatal("大小写不同的同一路径应命中")
	}
	if identityMatches(targetPath, connPathOther) {
		t.Fatal("不同目录的同名程序不应命中路径规则")
	}
	if identityMatches(targetPath, connNameOnly) {
		t.Fatal("路径规则不应命中无法确定路径的连接")
	}

	targetName := procid.FromProcess("chrome.exe", "")
	if !identityMatches(targetName, connPathOther) {
		t.Fatal("名称规则应命中所有同名进程")
	}
	if !identityMatches(targetName, connNameOnly) {
		t.Fatal("名称规则应命中同名降级连接")
	}
}
