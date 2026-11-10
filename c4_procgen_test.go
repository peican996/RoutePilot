package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errTestInjected = errors.New("injected failure")

func procGenTestService(t *testing.T) *CoreService {
	t.Helper()
	old := getDataDirOverride()
	setDataDirOverride(t.TempDir())
	t.Cleanup(func() { setDataDirOverride(old) })
	return NewCoreService()
}

func sampleProcRules() map[string]string {
	return map[string]string{
		"name:chrome.exe":  "direct",
		"name:firefox.exe": "proxy",
	}
}

// 发布后必须能解析出同一代三个文件，且 options() 使用 procgen 路径。
func TestProcGenPublishAndResolve(t *testing.T) {
	svc := procGenTestService(t)
	rules := sampleProcRules()
	if err := svc.publishProcRules(rules); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	paths, got, ok := svc.resolvePublishedProcRules()
	if !ok {
		t.Fatal("应能解析出已发布版本")
	}
	if len(got) != len(rules) || got["name:chrome.exe"] != "direct" {
		t.Fatalf("规则内容不符: %+v", got)
	}
	for _, p := range []string{paths.Proxy, paths.Direct, paths.Rules} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("发布文件缺失 %s: %v", p, err)
		}
	}

	opt := svc.options()
	if !strings.HasPrefix(opt.ProcessProxyPath, "./procgen/") ||
		!strings.HasPrefix(opt.ProcessDirectPath, "./procgen/") {
		t.Fatalf("options 未使用 procgen 路径: %q %q", opt.ProcessProxyPath, opt.ProcessDirectPath)
	}
}

// 写完文件但 manifest 发布失败：current.json 不变，旧版本继续可用。
func TestProcGenManifestFailureKeepsPrevious(t *testing.T) {
	svc := procGenTestService(t)
	v1 := map[string]string{"name:chrome.exe": "direct"}
	if err := svc.publishProcRules(v1); err != nil {
		t.Fatal(err)
	}

	origHook := procPublishHook
	t.Cleanup(func() { procPublishHook = origHook })
	procPublishHook = func(stage string) error {
		if stage == "before-manifest" {
			return errTestInjected
		}
		return nil
	}

	if err := svc.publishProcRules(map[string]string{"name:chrome.exe": "proxy"}); err == nil {
		t.Fatal("manifest 发布失败时 publish 必须报错")
	}
	procPublishHook = origHook

	_, got, ok := svc.resolvePublishedProcRules()
	if !ok {
		t.Fatal("旧版本应继续可用")
	}
	if got["name:chrome.exe"] != "direct" {
		t.Fatalf("解析到未提交的新版本: %+v", got)
	}
}

// current.json 损坏：回退上一代 manifest。
func TestProcGenCorruptManifestFallsBackPrev(t *testing.T) {
	svc := procGenTestService(t)
	if err := svc.publishProcRules(map[string]string{"name:a.exe": "direct"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.publishProcRules(map[string]string{"name:a.exe": "proxy"}); err != nil {
		t.Fatal(err)
	}

	cur := filepath.Join(procGenDir(), procGenManifestName)
	if err := os.WriteFile(cur, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, got, ok := svc.resolvePublishedProcRules()
	if !ok {
		t.Fatal("应回退到上一代")
	}
	if got["name:a.exe"] != "direct" {
		t.Fatalf("回退版本内容不符: %+v", got)
	}

	// 再启动时 loadProcRules 也不能被损坏 manifest 影响。
	svc2 := procGenTestServiceAt(t, dataDir())
	svc2.loadProcRules()
	svc2.mu.RLock()
	rules := svc2.procRules
	svc2.mu.RUnlock()
	if rules["name:a.exe"] != "direct" {
		t.Fatalf("启动加载未回退: %+v", rules)
	}
}

// 某一代文件被截断：哈希校验失败，回退上一代。
func TestProcGenTruncatedFileFallsBack(t *testing.T) {
	svc := procGenTestService(t)
	if err := svc.publishProcRules(map[string]string{"name:a.exe": "direct"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.publishProcRules(map[string]string{"name:a.exe": "proxy"}); err != nil {
		t.Fatal(err)
	}

	cur := filepath.Join(procGenDir(), procGenManifestName)
	mb, err := os.ReadFile(cur)
	if err != nil {
		t.Fatal(err)
	}
	var m procGenManifest
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	// 模拟写一半崩溃：把最新一代的 proxy 文件截断。
	if err := os.WriteFile(filepath.Join(procGenDir(), m.Proxy), []byte("PROCESS-NAME,a.exe"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, got, ok := svc.resolvePublishedProcRules()
	if !ok {
		t.Fatal("应回退到上一代")
	}
	if got["name:a.exe"] != "direct" {
		t.Fatalf("截断后仍使用了不完整版本: %+v", got)
	}
}

// 没有 generation 时回退旧版平铺文件，升级前数据不能丢。
func TestLoadProcRulesFallsBackToLegacy(t *testing.T) {
	svc := procGenTestService(t)
	legacy := filepath.Join(dataDir(), procRulesJSON)
	if err := os.WriteFile(legacy, []byte(`{"name:chrome.exe":"direct"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	svc.loadProcRules()

	svc.mu.RLock()
	rules := svc.procRules
	proxyPath := svc.procProxyPath
	svc.mu.RUnlock()
	if rules["name:chrome.exe"] != "direct" {
		t.Fatalf("旧版规则未加载: %+v", rules)
	}
	if proxyPath != procProxyYAML {
		t.Fatalf("旧版回退路径不符: %s", proxyPath)
	}
}

// 多次发布后只保留当前与上一代，避免无限增长。
func TestProcGenCleanupKeepsTwoGenerations(t *testing.T) {
	svc := procGenTestService(t)
	for i := 0; i < 4; i++ {
		if err := svc.publishProcRules(map[string]string{"name:a.exe": "direct"}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(procGenDir())
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	names = sortGenFiles(names)
	// 2 代 × 3 个文件 + current + prev = 8。
	if len(names) > 8 {
		t.Fatalf("旧 generation 未清理: %v", names)
	}
}

// manifest 被手工改成路径穿越：必须拒绝，不能读取 procgen 目录外的文件。
func TestProcGenRejectsPathTraversalManifest(t *testing.T) {
	svc := procGenTestService(t)
	if err := svc.publishProcRules(map[string]string{"name:a.exe": "direct"}); err != nil {
		t.Fatal(err)
	}
	cur := filepath.Join(procGenDir(), procGenManifestName)
	mb, err := os.ReadFile(cur)
	if err != nil {
		t.Fatal(err)
	}
	var m procGenManifest
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	bad := m
	bad.Proxy = "../outside.yaml"
	badBytes, _ := json.Marshal(bad)
	if err := os.WriteFile(cur, badBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// 同时移除 prev，确保回退路径也不可用，验证的是"拒绝"而不是"回退"。
	_ = os.Remove(cur + ".prev")

	if _, _, ok := svc.resolvePublishedProcRules(); ok {
		t.Fatal("路径穿越的 manifest 必须被拒绝")
	}
}

func procGenTestServiceAt(t *testing.T, dir string) *CoreService {
	t.Helper()
	old := getDataDirOverride()
	setDataDirOverride(dir)
	t.Cleanup(func() { setDataDirOverride(old) })
	return NewCoreService()
}
