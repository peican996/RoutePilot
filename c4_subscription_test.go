package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"routepilot/internal/subscription"
)

const testSubYAML = "proxies:\n" +
	"  - name: n1\n    type: ss\n    server: 1.2.3.4\n    port: 443\n" +
	"    cipher: aes-128-gcm\n    password: x\n"

// setupTwoSubs 构造两个已保存订阅（A 为激活），用于验证事务串行化。
func setupTwoSubs(t *testing.T) (*CoreService, string, string) {
	t.Helper()
	old := dataDirOverride
	dataDirOverride = t.TempDir()
	t.Cleanup(func() { dataDirOverride = old })

	svc := NewCoreService()
	urlA := "https://a.example/sub?token=AAA"
	urlB := "https://b.example/sub?token=BBB"
	for _, u := range []string{urlA, urlB} {
		res, err := subscription.Parse([]byte(testSubYAML), u, false)
		if err != nil {
			t.Fatalf("构造订阅失败: %v", err)
		}
		if err := svc.writeSubscriptionFiles(u, res); err != nil {
			t.Fatalf("写订阅文件失败: %v", err)
		}
	}
	svc.subs = []subMeta{{URL: urlA}, {URL: urlB}}
	svc.activeIdx = 0
	svc.subURL = urlA
	return svc, urlA, urlB
}

// 刷新 A 的下载尚未返回时用户切到 B：刷新提交必须放弃，不能把 B 覆盖回去。
func TestRefreshDoesNotOverrideConcurrentSwitch(t *testing.T) {
	svc, _, urlB := setupTwoSubs(t)

	fetchStarted := make(chan struct{})
	releaseFetch := make(chan struct{})
	var once sync.Once
	origFetch := fetchSubscription
	t.Cleanup(func() { fetchSubscription = origFetch })
	fetchSubscription = func(_ context.Context, u string, _ bool) (*subscription.Result, error) {
		once.Do(func() { close(fetchStarted) })
		<-releaseFetch
		return subscription.Parse([]byte(testSubYAML), u, false)
	}

	refreshDone := make(chan error, 1)
	go func() { refreshDone <- svc.RefreshSubscription() }()

	select {
	case <-fetchStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("刷新未进入网络阶段")
	}

	// 下载期间切换订阅：此时 subsMu 未被占用。
	if err := svc.SwitchSubscription(1); err != nil {
		t.Fatalf("切换订阅失败: %v", err)
	}
	if got := svc.currentSubURL(); got != urlB {
		t.Fatalf("切换后激活订阅应为 B，实际 %s", got)
	}

	close(releaseFetch)
	select {
	case err := <-refreshDone:
		if !errors.Is(err, errSubscriptionChanged) {
			t.Fatalf("期望 errSubscriptionChanged，实际 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("刷新未返回")
	}

	if got := svc.currentSubURL(); got != urlB {
		t.Fatalf("刷新结果覆盖了用户切换: %s", got)
	}
	if svc.subs[svc.activeIdx].URL != urlB {
		t.Fatal("激活下标与 subURL 不一致")
	}
}

// 并发刷新/切换/删除不能死锁，且最终状态内部一致。
func TestSubscriptionOpsConcurrentConsistency(t *testing.T) {
	svc, urlA, urlB := setupTwoSubs(t)

	resA, _ := subscription.Parse([]byte(testSubYAML), urlA, false)
	resB, _ := subscription.Parse([]byte(testSubYAML), urlB, false)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		url := urlA
		res := resA
		if i%2 == 1 {
			url, res = urlB, resB
		}
		go func() {
			defer wg.Done()
			_ = svc.applySubscription(url, res)
		}()
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		idx := i % 2
		go func() {
			defer wg.Done()
			_ = svc.SwitchSubscription(idx)
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("并发订阅操作死锁")
	}

	svc.mu.RLock()
	active := svc.subURL
	idx := svc.activeIdx
	count := len(svc.subs)
	config := svc.subConfig
	svc.mu.RUnlock()
	if count != 2 {
		t.Fatalf("并发提交产生了重复/丢失条目: %d", count)
	}
	if active != urlA && active != urlB {
		t.Fatalf("激活订阅异常: %s", active)
	}
	if config == nil {
		t.Fatal("激活订阅的配置不应为空")
	}
	if idx < 0 || idx >= count {
		t.Fatalf("激活下标越界: idx=%d count=%d", idx, count)
	}
	svc.mu.RLock()
	activeOK := svc.subs[idx].URL == active
	svc.mu.RUnlock()
	if !activeOK {
		t.Fatalf("激活下标与激活地址不一致: idx=%d active=%s", idx, active)
	}
}

// 删除非激活订阅不改变当前激活配置。
func TestRemoveInactiveKeepsActive(t *testing.T) {
	svc, urlA, _ := setupTwoSubs(t)
	svc.mu.Lock()
	svc.activeIdx = 0
	svc.subURL = urlA
	svc.mu.Unlock()

	if err := svc.RemoveSubscription(1); err != nil {
		t.Fatalf("删除非激活订阅失败: %v", err)
	}
	if got := svc.currentSubURL(); got != urlA {
		t.Fatalf("删除非激活订阅后激活项被改变: %s", got)
	}
	svc.mu.RLock()
	n := len(svc.subs)
	svc.mu.RUnlock()
	if n != 1 {
		t.Fatalf("剩余订阅数应为 1，实际 %d", n)
	}
}
