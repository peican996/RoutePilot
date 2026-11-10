package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 准备一个"系统代理正由本程序接管"的服务。
func tunService(t *testing.T, fake *fakeRegistry) *CoreService {
	t.Helper()
	svc := newC1Service(t, fake)
	cp := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", Enabled: true, Saved: true,
	}
	svc.proxyOn = true
	svc.backup = cp
	writeState(t, svc, cp)
	return svc
}

// TUN 开启事务进行到"内核重启"阶段时，并发的 SetSystemProxy(true) 必须被拒绝，
// 否则会出现 TUN 与系统代理同时接管的双接管状态。
func TestTUNToggleBlocksConcurrentProxyEnable(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:17890", enabled: true, override: "ours"}
	svc := tunService(t, fake)

	restartEntered := make(chan struct{})
	releaseRestart := make(chan struct{})
	var once sync.Once
	restart := func() error {
		once.Do(func() { close(restartEntered) })
		<-releaseRestart
		return nil
	}

	tunDone := make(chan error, 1)
	go func() { tunDone <- svc.setTUN(true, restart) }()

	select {
	case <-restartEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("TUN 事务未进入内核重启阶段")
	}

	// 此时注册表已关闭、tunOn 尚未提交，但 tunChanging=true。
	beforeEnable := fake.enableCalls
	if err := svc.SetSystemProxy(true); err == nil {
		t.Fatal("TUN 切换期间必须拒绝开启系统代理")
	}
	if fake.enableCalls != beforeEnable {
		t.Fatal("被拒绝的请求不能写注册表")
	}

	close(releaseRestart)
	if err := <-tunDone; err != nil {
		t.Fatalf("TUN 开启失败: %v", err)
	}

	svc.mu.RLock()
	tunOn, proxyOn, changing := svc.tunOn, svc.proxyOn, svc.tunChanging
	svc.mu.RUnlock()
	if !tunOn || proxyOn || changing {
		t.Fatalf("提交状态不符: tunOn=%v proxyOn=%v changing=%v", tunOn, proxyOn, changing)
	}
	if fake.enabled {
		t.Fatal("TUN 已开启时系统代理必须保持关闭")
	}
	if err := svc.SetSystemProxy(true); err == nil {
		t.Fatal("TUN 已开启后仍应拒绝开启系统代理")
	}
}

// TUN 开启失败：必须回滚到"切换前"的系统代理接管状态。
func TestTUNEnableFailureRestoresProxyTakeover(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:17890", enabled: true, override: "ours"}
	svc := tunService(t, fake)

	err := svc.setTUN(true, func() error { return errors.New("injected restart failure") })
	if err == nil {
		t.Fatal("内核重启失败时 SetTUN 必须报错")
	}

	svc.mu.RLock()
	tunOn, proxyOn, bk, changing := svc.tunOn, svc.proxyOn, svc.backup, svc.tunChanging
	svc.mu.RUnlock()
	if tunOn {
		t.Fatal("失败后不能宣称 TUN 已开启")
	}
	if changing {
		t.Fatal("失败后必须清除切换中标记")
	}
	if !proxyOn || !bk.Saved {
		t.Fatalf("失败后应重新接管系统代理: proxyOn=%v backupSaved=%v", proxyOn, bk.Saved)
	}
	if !fake.enabled || fake.server != "127.0.0.1:17890" {
		t.Fatalf("系统代理未恢复到本程序地址: enabled=%v server=%s", fake.enabled, fake.server)
	}
}

// 切 TUN 前恢复系统代理失败：必须保留 Pending 备份，交给下次启动重试。
func TestTUNEnableRestoreFailureKeepsPendingBackup(t *testing.T) {
	fake := &fakeRegistry{
		server:          "127.0.0.1:17890",
		enabled:         true,
		override:        "ours",
		failSetOverride: true,
	}
	svc := tunService(t, fake)

	restartCalled := int32(0)
	err := svc.setTUN(true, func() error {
		atomic.AddInt32(&restartCalled, 1)
		return nil
	})
	if !errors.Is(err, errTUNRestoreFailed) {
		t.Fatalf("期望 errTUNRestoreFailed，实际 %v", err)
	}
	if atomic.LoadInt32(&restartCalled) != 0 {
		t.Fatal("恢复系统代理失败后不能继续重启内核")
	}

	svc.mu.RLock()
	tunOn, proxyOn, bk, changing := svc.tunOn, svc.proxyOn, svc.backup, svc.tunChanging
	svc.mu.RUnlock()
	if tunOn || proxyOn || changing {
		t.Fatalf("失败后状态不符: tunOn=%v proxyOn=%v changing=%v", tunOn, proxyOn, changing)
	}
	if !bk.Saved || !bk.Pending {
		t.Fatalf("必须保留 Pending 备份: %+v", bk)
	}
	st, ok := readState(t, svc)
	if !ok || !st.Pending {
		t.Fatalf("磁盘备份必须保留并标记 Pending: %+v", st)
	}
}

// TUN 关闭失败：不能错误宣告"系统代理已恢复"。
func TestTUNDisableFailureDoesNotRestoreProxy(t *testing.T) {
	fake := &fakeRegistry{server: "", enabled: false}
	svc := newC1Service(t, fake)
	svc.tunOn = true
	svc.tunPrevProxyOn = true
	svc.proxyOn = false

	var alive int32 = 1
	svc.kernelRunningFn = func() bool { return atomic.LoadInt32(&alive) == 1 }
	restart := func() error {
		atomic.StoreInt32(&alive, 0)
		return errors.New("injected restart failure")
	}
	beforeEnable := fake.enableCalls

	err := svc.setTUN(false, restart)
	if err == nil {
		t.Fatal("关闭 TUN 失败时必须返回错误")
	}
	svc.mu.RLock()
	tunOn, proxyOn, prev, restore := svc.tunOn, svc.proxyOn, svc.tunPrevProxyOn, svc.restoreProxy
	svc.mu.RUnlock()
	if tunOn {
		t.Fatal("内核已停时不能宣称 TUN 仍开启")
	}
	if proxyOn {
		t.Fatal("关闭 TUN 失败时不能宣称系统代理已恢复")
	}
	if !prev {
		t.Fatal("恢复意图必须保留，供下次启动继续处理")
	}
	if !restore {
		t.Fatal("失败后必须把恢复意图写入启动恢复流程")
	}
	if fake.enableCalls != beforeEnable {
		t.Fatal("失败路径不应写注册表开启代理")
	}
}

// TUN 关闭成功：按记录恢复系统代理接管。
func TestTUNDisableSuccessRestoresProxy(t *testing.T) {
	fake := &fakeRegistry{server: "", enabled: false}
	svc := newC1Service(t, fake)
	svc.tunOn = true
	svc.tunPrevProxyOn = true
	svc.proxyOn = false

	if err := svc.setTUN(false, func() error { return nil }); err != nil {
		t.Fatalf("关闭 TUN 失败: %v", err)
	}
	svc.mu.RLock()
	tunOn, proxyOn, prev := svc.tunOn, svc.proxyOn, svc.tunPrevProxyOn
	svc.mu.RUnlock()
	if tunOn || !proxyOn || prev {
		t.Fatalf("关闭后状态不符: tunOn=%v proxyOn=%v prev=%v", tunOn, proxyOn, prev)
	}
	if !fake.enabled || fake.server != "127.0.0.1:17890" {
		t.Fatalf("系统代理未恢复接管: enabled=%v server=%s", fake.enabled, fake.server)
	}
}

// 内核未运行时关闭 TUN：不能当场接管代理，但恢复意图必须留给下次启动。
func TestTUNDisableWithoutKernelPersistsRestoreIntent(t *testing.T) {
	fake := &fakeRegistry{server: "", enabled: false}
	svc := newC1Service(t, fake)
	svc.tunOn = true
	svc.tunPrevProxyOn = true
	svc.kernelRunningFn = func() bool { return false }

	if err := svc.setTUN(false, func() error {
		t.Fatal("内核未运行时不应当调用重启")
		return nil
	}); err != nil {
		t.Fatalf("关闭 TUN 失败: %v", err)
	}
	svc.mu.RLock()
	tunOn, restore := svc.tunOn, svc.restoreProxy
	svc.mu.RUnlock()
	if tunOn {
		t.Fatal("TUN 应已关闭")
	}
	if !restore {
		t.Fatal("内核未运行时必须把代理恢复意图写入启动恢复流程")
	}
	if fake.enableCalls != 0 {
		t.Fatal("内核未运行不应写注册表")
	}
}

// reconcileProxy 在 TUN 切换期间必须跳过，不能把系统代理重新打开。
func TestReconcileSkippedDuringTUNToggle(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:17890", enabled: true, override: "ours"}
	svc := tunService(t, fake)

	svc.mu.Lock()
	svc.tunChanging = true
	svc.tunTarget = true
	svc.mu.Unlock()
	beforeEnable := fake.enableCalls

	svc.reconcileProxy()

	if fake.enableCalls != beforeEnable {
		t.Fatal("TUN 切换期间 reconcileProxy 不得写注册表")
	}
	if !fake.enabled || fake.server != "127.0.0.1:17890" {
		t.Fatal("TUN 切换期间 reconcileProxy 改变了注册表")
	}
}
