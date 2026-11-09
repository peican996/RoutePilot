package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// 用户手动关闭系统代理时，reconcileProxy 必须先恢复原始设置；
// 任一步失败都要保留备份，便于下次启动继续恢复。
func TestReconcileClosedProxyFailureKeepsBackup(t *testing.T) {
	fake := &fakeRegistry{
		enabled:         false,
		failSetOverride: true,
	}
	svc := newC1Service(t, fake)
	writeState(t, svc, proxyBackup{
		Addr:          "127.0.0.1:17890",
		Server:        "old:1",
		Override:      "old-override",
		AutoConfigURL: "http://old/pac",
		Enabled:       false,
		Saved:         true,
	})

	svc.reconcileProxy()
	if _, ok := readState(t, svc); !ok {
		t.Fatal("恢复失败时备份必须保留")
	}

	// 幂等重试：失败因素消失后再次执行，应完成恢复并删除备份。
	fake.failSetOverride = false
	svc.reconcileProxy()
	if _, ok := readState(t, svc); ok {
		t.Fatal("恢复成功并回读校验通过后应删除备份")
	}
	if fake.server != "old:1" || fake.override != "old-override" || fake.pac != "http://old/pac" {
		t.Fatalf("原始代理设置未完整恢复: %+v", fake)
	}
	if fake.enabled {
		t.Fatal("用户手动关闭的代理不应被重新打开")
	}
}

// 恢复成功后必须回读校验，再清理备份。
func TestReconcileClosedProxySuccessClearsBackup(t *testing.T) {
	fake := &fakeRegistry{enabled: false}
	svc := newC1Service(t, fake)
	writeState(t, svc, proxyBackup{
		Addr:     "127.0.0.1:17890",
		Server:   "old:2",
		Override: "bypass",
		Enabled:  false,
		Saved:    true,
	})

	svc.reconcileProxy()
	if _, ok := readState(t, svc); ok {
		t.Fatal("恢复成功后应清理备份")
	}
	svc.mu.RLock()
	proxyOn := svc.proxyOn
	svc.mu.RUnlock()
	if proxyOn {
		t.Fatal("用户关闭代理后 proxyOn 应为 false")
	}
}

// 恢复动作和回读校验都成功，但删除备份失败：必须返回错误并保留备份，
// 下次启动继续校验/恢复，不能误认为事务完成。
func TestRestoreSuccessRemoveFailureKeepsBackup(t *testing.T) {
	fake := &fakeRegistry{
		server:   "ours:17890",
		enabled:  true,
		override: "ours",
	}
	svc := newC1Service(t, fake)
	writeState(t, svc, proxyBackup{
		Addr:     "127.0.0.1:17890",
		Server:   "old:1",
		Override: "old",
		Enabled:  true,
		Saved:    true,
	})

	origRemove := removeProxyState
	removeProxyState = func(string) error { return errors.New("injected remove failure") }
	t.Cleanup(func() { removeProxyState = origRemove })

	err := svc.restoreSystemProxy()
	if err == nil || !strings.Contains(err.Error(), "删除系统代理备份失败") {
		t.Fatalf("应返回删除备份失败，实际 %v", err)
	}
	if _, ok := readState(t, svc); !ok {
		t.Fatal("删除失败时必须保留备份")
	}
	if fake.server != "old:1" || fake.override != "old" {
		t.Fatalf("恢复动作应已执行: %+v", fake)
	}

	// 删除故障消失后重试，应完成清理。
	removeProxyState = os.Remove
	if err := svc.restoreSystemProxy(); err != nil {
		t.Fatalf("重试恢复失败: %v", err)
	}
	if _, ok := readState(t, svc); ok {
		t.Fatal("重试成功后应删除备份")
	}
}
