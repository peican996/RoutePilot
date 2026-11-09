//go:build repro

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"routepilot/internal/subscription"
)

// T10（订阅/进程规则）：文件被截断时加载静默返回空，随后一次保存
// 会用空数据覆盖损坏文件，原内容不可恢复。
func TestReproT10SubsTruncatedIsOverwritten(t *testing.T) {
	old := dataDirOverride
	dataDirOverride = t.TempDir()
	t.Cleanup(func() { dataDirOverride = old })

	path := filepath.Join(dataDir(), subsFile)
	original := `{"items":[{"url":"https://example.com/sub?token=SECRET","name":"机场"}],"active":0}`
	// 模拟写到一半崩溃：截断成前 20 字节。
	if err := os.WriteFile(path, []byte(original[:20]), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := NewCoreService()
	svc.loadSubs() // 解析失败 -> 静默返回，内存里没有订阅
	if len(svc.subs) != 0 {
		t.Fatalf("预期解析失败后内存为空，实际 %d 条", len(svc.subs))
	}

	svc.saveSubs() // 任意一次后续保存（如切换/刷新）都会覆盖
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "SECRET") {
		t.Fatal("预期原订阅已被覆盖")
	}
	t.Logf("FAIL: 损坏的 subs.json 被空数据覆盖，原订阅不可恢复（现内容=%s）", strings.TrimSpace(string(got)))
}

// T11：订阅写入与切换并发。subscription.yaml 是共用文件，
// 两个不同订阅同时写入会交错，产生无法解析的混合内容。
func TestReproT11ConcurrentSubscriptionWrite(t *testing.T) {
	old := dataDirOverride
	dataDirOverride = t.TempDir()
	t.Cleanup(func() { dataDirOverride = old })

	svc := NewCoreService()

	// 2MB 级内容：超过单次 WriteFile 的原子性范围，更容易交错。
	pad := strings.Repeat("a", 1<<21)
	a := &subscription.Result{
		Config: map[string]any{"proxies": []any{map[string]any{"name": "A"}}},
		Raw:    []byte("#A\n" + pad),
	}
	b := &subscription.Result{
		Config: map[string]any{"proxies": []any{map[string]any{"name": "B"}}},
		Raw:    []byte("#B\n" + pad),
	}

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = svc.writeSubscriptionFiles("https://a.example/sub", a) }()
		go func() { defer wg.Done(); _ = svc.writeSubscriptionFiles("https://b.example/sub", b) }()
	}
	wg.Wait()

	// 两个订阅各自有独立文件，本身不应互相覆盖；
	// 真正共享的是 subscription.yaml（由 applySubscription 写入）。
	for i := 0; i < 12; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = os.WriteFile(filepath.Join(dataDir(), subConfigFile), a.Raw, 0o600) }()
		go func() { defer wg.Done(); _ = os.WriteFile(filepath.Join(dataDir(), subConfigFile), b.Raw, 0o600) }()
	}
	wg.Wait()

	got, err := os.ReadFile(filepath.Join(dataDir(), subConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("共享订阅文件为空")
	}
	// 合法结果只可能是纯 A 或纯 B；混合内容说明发生了交错写入。
	if !strings.HasPrefix(string(got), "#A\n") && !strings.HasPrefix(string(got), "#B\n") {
		t.Fatalf("FAIL: 共享订阅文件头部损坏（%q）", string(got[:min(16, len(got))]))
	}
	body := got[len("#A\n"):]
	if len(body) != len(pad) {
		t.Fatalf("FAIL: 共享订阅文件长度异常：%d，期望 %d", len(body), len(pad))
	}
	for i, c := range body {
		if c != 'a' {
			t.Fatalf("FAIL: 共享订阅文件在偏移 %d 处被并发写坏", i)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
