package main

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"routepilot/internal/kernel"
)

// r1Service 在指定数据目录上构造可注入故障的 CoreService。
// 重启恢复测试需要在同一目录上先后创建两个实例。
func r1Service(t *testing.T, fake *fakeRegistry, dir string) *CoreService {
	t.Helper()
	old := getDataDirOverride()
	setDataDirOverride(dir)
	t.Cleanup(func() { setDataDirOverride(old) })

	svc := NewCoreService()
	svc.sys = fake.ops()
	svc.elevated = func() bool { return true }
	svc.kernelRunningFn = func() bool { return true }
	svc.mixedPort = 17890
	return svc
}

func r1NewService(t *testing.T, fake *fakeRegistry) *CoreService {
	return r1Service(t, fake, t.TempDir())
}

// ---------- P1：恢复事务完整性 ----------

// 还原过程中任何一步注册表写入失败，都必须保留磁盘与内存备份，并标记 Pending。
func TestR1RestoreWriteFailuresKeepBackup(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		pac     string
		apply   func(*fakeRegistry)
	}{
		{"SetServer", false, "", func(f *fakeRegistry) { f.failSetServer = true }},
		{"SetOverride", true, "", func(f *fakeRegistry) { f.failSetOverride = true }},
		{"SetAutoConfigURL", true, "http://old/pac", func(f *fakeRegistry) { f.failSetPAC = true }},
		{"ClearAutoConfigURL", true, "", func(f *fakeRegistry) { f.failClearPAC = true }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRegistry{server: "127.0.0.1:17890", enabled: true, override: "ours"}
			tc.apply(fake)
			svc := r1NewService(t, fake)

			cp := proxyBackup{
				Addr:          "127.0.0.1:17890",
				Server:        "127.0.0.1:8080",
				Override:      "old-bypass",
				AutoConfigURL: tc.pac,
				Enabled:       tc.enabled,
				Saved:         true,
			}
			svc.backup = cp
			writeState(t, svc, cp)

			if err := svc.restoreSystemProxy(); err == nil {
				t.Fatal("注册表写入失败时恢复必须报错")
			}
			st, ok := readState(t, svc)
			if !ok || !st.Saved || !st.Pending {
				t.Fatalf("失败后磁盘备份必须保留并标记 Pending: %+v", st)
			}
			svc.mu.RLock()
			mem := svc.backup
			svc.mu.RUnlock()
			if !mem.Saved || !mem.Pending {
				t.Fatalf("失败后内存备份不能清空: %+v", mem)
			}
		})
	}
}

// 写入成功但回读不一致：不能当作恢复完成，备份必须保留。
func TestR1ReadbackMismatchKeepsBackup(t *testing.T) {
	fake := &fakeRegistry{
		server:   "127.0.0.1:17890",
		enabled:  true,
		override: "ours",
		// 写入成功但注册表读出来是别的地址，模拟设置未生效。
		readbackServer: "172.16.0.9:1",
	}
	svc := r1NewService(t, fake)
	cp := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", Enabled: true, Saved: true,
	}
	svc.backup = cp
	writeState(t, svc, cp)

	err := svc.restoreSystemProxy()
	if err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("回读不一致必须返回校验失败，实际 %v", err)
	}
	if _, ok := readState(t, svc); !ok {
		t.Fatal("回读校验失败后必须保留备份")
	}
	svc.mu.RLock()
	mem := svc.backup
	svc.mu.RUnlock()
	if !mem.Saved {
		t.Fatal("回读校验失败后内存备份不能清空")
	}
}

// 恢复与校验都成功、仅删除备份失败：必须返回明确错误且保留内存备份。
func TestR1RemoveFailureKeepsMemoryBackup(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:17890", enabled: true, override: "ours"}
	svc := r1NewService(t, fake)
	cp := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", Enabled: true, Saved: true,
	}
	svc.backup = cp
	writeState(t, svc, cp)

	orig := removeProxyState
	removeProxyState = func(string) error { return errors.New("injected remove failure") }
	t.Cleanup(func() { removeProxyState = orig })

	err := svc.restoreSystemProxy()
	if err == nil || !strings.Contains(err.Error(), "删除系统代理备份失败") {
		t.Fatalf("应返回删除失败，实际 %v", err)
	}
	if _, ok := readState(t, svc); !ok {
		t.Fatal("删除失败时磁盘备份必须保留")
	}
	svc.mu.RLock()
	mem := svc.backup
	svc.mu.RUnlock()
	if !mem.Saved {
		t.Fatal("删除失败时不能清空内存备份，否则失去恢复依据")
	}
	if fake.server != "127.0.0.1:8080" || fake.override != "old-bypass" {
		t.Fatalf("注册表应已还原: %+v", fake)
	}
}

// 恢复失败后重新启动：必须在启动阶段优先完成还原，并清除旧的恢复意图。
func TestR1BootRetriesPendingRestore(t *testing.T) {
	fake := &fakeRegistry{
		server:          "127.0.0.1:17890",
		enabled:         true,
		override:        "ours",
		failSetOverride: true,
	}
	dir := t.TempDir()
	svc := r1Service(t, fake, dir)
	cp := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", AutoConfigURL: "http://old/pac",
		Enabled: true, Saved: true,
	}
	svc.backup = cp
	writeState(t, svc, cp)

	if err := svc.restoreSystemProxy(); err == nil {
		t.Fatal("故障未解除时恢复必须失败")
	}

	// 模拟进程重启：注册表故障消失，新实例读同一数据目录。
	fake.failSetOverride = false
	svc2 := r1Service(t, fake, dir)
	svc2.restoreProxy = true
	svc2.recoverPendingProxyRestore()

	if _, ok := readState(t, svc2); ok {
		t.Fatal("启动阶段重试成功后应删除备份")
	}
	if fake.server != "127.0.0.1:8080" || fake.override != "old-bypass" || fake.pac != "http://old/pac" {
		t.Fatalf("启动重试未完整还原: %+v", fake)
	}
	svc2.mu.RLock()
	restore := svc2.restoreProxy
	svc2.mu.RUnlock()
	if restore {
		t.Fatal("恢复成功后必须清除旧恢复意图，避免再次自动接管")
	}
}

// 恢复失败后用户又设置了新的代理：再次恢复必须放弃，不得覆盖用户配置。
func TestR1ExternalChangeAfterFailedRestoreIsNotOverwritten(t *testing.T) {
	fake := &fakeRegistry{
		server:          "127.0.0.1:17890",
		enabled:         true,
		override:        "ours",
		failSetOverride: true,
	}
	svc := r1NewService(t, fake)
	cp := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", Enabled: true, Saved: true,
	}
	svc.backup = cp
	writeState(t, svc, cp)

	if err := svc.restoreSystemProxy(); err == nil {
		t.Fatal("第一次恢复应失败")
	}

	// 用户/外部软件在失败后接管了系统代理。
	fake.failSetOverride = false
	fake.server = "10.0.0.5:7890"
	fake.enabled = true
	fake.override = "external-bypass"
	beforeEnable := fake.enableCalls

	if err := svc.restoreSystemProxy(); err != nil {
		t.Fatalf("外部接管后应放弃还原而不是报错: %v", err)
	}
	if fake.server != "10.0.0.5:7890" || fake.override != "external-bypass" {
		t.Fatalf("用户新设置被覆盖: %+v", fake)
	}
	if fake.enableCalls != beforeEnable {
		t.Fatal("放弃还原时不应再写注册表")
	}
	if _, ok := readState(t, svc); ok {
		t.Fatal("放弃接管后应撤销本地还原点")
	}
	svc.mu.RLock()
	on := svc.proxyOn
	svc.mu.RUnlock()
	if on {
		t.Fatal("放弃接管后不能声称系统代理仍由本程序控制")
	}
}

// 重复恢复必须幂等：第二次调用不产生任何注册表写入。
func TestR1RestoreIsIdempotent(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:17890", enabled: true, override: "ours"}
	svc := r1NewService(t, fake)
	cp := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", Enabled: true, Saved: true,
	}
	svc.backup = cp
	writeState(t, svc, cp)

	if err := svc.restoreSystemProxy(); err != nil {
		t.Fatalf("首次恢复失败: %v", err)
	}
	enables := fake.enableCalls
	overrides := fake.override

	if err := svc.restoreSystemProxy(); err != nil {
		t.Fatalf("重复恢复必须幂等: %v", err)
	}
	if fake.enableCalls != enables {
		t.Fatal("重复恢复不应再次写注册表")
	}
	if fake.override != overrides {
		t.Fatal("重复恢复不应改变绕过列表")
	}
}

// ---------- P1：代理事务串行化 ----------

// StopKernel 必须等待正在进行的代理启用事务完成，并在返回前把代理还原。
func TestR1StopKernelWaitsForProxyTx(t *testing.T) {
	fake := &fakeRegistry{}
	svc := r1NewService(t, fake)
	svc.mgr = kernel.NewManager(t.TempDir())

	enableStarted := make(chan struct{})
	releaseEnable := make(chan struct{})
	var once sync.Once
	fake.enableHook = func() {
		once.Do(func() {
			close(enableStarted)
			<-releaseEnable
		})
	}

	userDone := make(chan error, 1)
	go func() { userDone <- svc.SetSystemProxy(true) }()
	<-enableStarted

	stopDone := make(chan error, 1)
	go func() { stopDone <- svc.StopKernel() }()

	// 代理事务还握着 proxyMu，StopKernel 不可能在此刻返回。
	select {
	case <-stopDone:
		t.Fatal("StopKernel 在代理事务完成前返回")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseEnable)

	if err := <-userDone; err != nil {
		t.Fatalf("开启系统代理失败: %v", err)
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("停止内核失败: %v", err)
	}
	if fake.enabled {
		t.Fatal("StopKernel 返回成功后系统代理必须已关闭")
	}
	svc.mu.RLock()
	on := svc.proxyOn
	svc.mu.RUnlock()
	if on {
		t.Fatal("StopKernel 返回成功后 proxyOn 必须为 false")
	}
}

// Shutdown 与用户开启代理并发：串行执行，最终代理必须被还原。
func TestR1ShutdownWaitsForProxyTx(t *testing.T) {
	fake := &fakeRegistry{}
	svc := r1NewService(t, fake)
	svc.mgr = kernel.NewManager(t.TempDir())

	enableStarted := make(chan struct{})
	releaseEnable := make(chan struct{})
	var once sync.Once
	fake.enableHook = func() {
		once.Do(func() {
			close(enableStarted)
			<-releaseEnable
		})
	}

	userDone := make(chan error, 1)
	go func() { userDone <- svc.SetSystemProxy(true) }()
	<-enableStarted

	shutdownDone := make(chan struct{})
	go func() {
		svc.Shutdown()
		close(shutdownDone)
	}()
	close(releaseEnable)

	if err := <-userDone; err != nil {
		t.Fatalf("开启系统代理失败: %v", err)
	}
	select {
	case <-shutdownDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown 未能在代理事务结束后完成")
	}
	if fake.enabled {
		t.Fatal("Shutdown 后系统代理必须已还原")
	}
	if err := svc.SetSystemProxy(true); err == nil {
		t.Fatal("Shutdown 后必须拒绝再次开启系统代理")
	}
	if fake.enabled {
		t.Fatal("Shutdown 后的拒绝不能写注册表")
	}
}

// SetSystemProxy(false) 与 RestartKernel 并发：不能死锁或交错。
func TestR1SetProxyFalseAndRestartConcurrent(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:17890", enabled: true, override: "ours"}
	svc := r1NewService(t, fake)
	svc.mgr = kernel.NewManager(t.TempDir())
	svc.backup = proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", Enabled: true, Saved: true,
	}
	writeState(t, svc, svc.backup)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = svc.SetSystemProxy(false)
	}()
	go func() {
		defer wg.Done()
		_ = svc.RestartKernel()
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SetSystemProxy(false) 与 RestartKernel 并发时死锁")
	}
	if fake.server != "127.0.0.1:8080" {
		t.Fatalf("并发后仍应完成还原，实际 %s", fake.server)
	}
}

// reconcileProxy 与用户手动关闭代理并发：串行执行，最终还原到原始配置。
func TestR1ReconcileAndUserToggleSerialized(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:17890", enabled: true, override: "ours"}
	svc := r1NewService(t, fake)
	svc.mixedPort = 17891
	st := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", Enabled: true, Saved: true,
	}
	writeState(t, svc, st)

	enableStarted := make(chan struct{})
	releaseEnable := make(chan struct{})
	var once sync.Once
	fake.enableHook = func() {
		once.Do(func() {
			close(enableStarted)
			<-releaseEnable
		})
	}

	reconcileDone := make(chan struct{})
	go func() {
		svc.reconcileProxy()
		close(reconcileDone)
	}()
	<-enableStarted

	userDone := make(chan error, 1)
	go func() { userDone <- svc.SetSystemProxy(false) }()
	close(releaseEnable)

	select {
	case <-reconcileDone:
	case <-time.After(3 * time.Second):
		t.Fatal("reconcileProxy 未完成")
	}
	if err := <-userDone; err != nil {
		t.Fatalf("关闭系统代理失败: %v", err)
	}
	if fake.server != "127.0.0.1:8080" || fake.override != "old-bypass" {
		t.Fatalf("并发后未还原到原始配置: %+v", fake)
	}
}

// Watchdog 熔断恢复与用户开启代理并发：串行执行，熔断恢复必须最终生效。
func TestR1WatchdogExhaustedRestoreSerializedWithUser(t *testing.T) {
	fake := &fakeRegistry{}
	svc := r1NewService(t, fake)
	svc.mgr = kernel.NewManager(t.TempDir())
	svc.startKernelWatchdog()
	wd := svc.watchdog
	t.Cleanup(wd.Stop)

	enableStarted := make(chan struct{})
	releaseEnable := make(chan struct{})
	var enableOnce sync.Once
	fake.enableHook = func() {
		enableOnce.Do(func() {
			close(enableStarted)
			<-releaseEnable
		})
	}
	restored := make(chan struct{})
	var restoreOnce sync.Once
	fake.setEnabledHook = func() { restoreOnce.Do(func() { close(restored) }) }

	userDone := make(chan error, 1)
	go func() { userDone <- svc.SetSystemProxy(true) }()
	<-enableStarted

	// 连续退出达到上限，触发熔断恢复（与用户操作并发）。
	for i := 0; i < 6; i++ {
		wd.ReportExit(kernel.ExitInfo{Reason: "boom", At: time.Now()})
	}
	close(releaseEnable)

	if err := <-userDone; err != nil {
		t.Fatalf("开启系统代理失败: %v", err)
	}
	select {
	case <-restored:
	case <-time.After(3 * time.Second):
		t.Fatal("熔断恢复未在用户操作结束后执行")
	}
	if fake.enabled {
		t.Fatal("熔断恢复后系统代理必须关闭")
	}
}

// 代际失效后，启动恢复任务绝不能再打开系统代理。
func TestR1StaleGenerationCannotOpenProxy(t *testing.T) {
	fake := &fakeRegistry{}
	svc := r1NewService(t, fake)

	gen, ctx := svc.beginKernelGeneration()
	svc.invalidateKernelGeneration()

	err := svc.setSystemProxyForGeneration(gen, ctx)
	if !errors.Is(err, errProxyStale) {
		t.Fatalf("旧代际必须被拒绝，实际 %v", err)
	}
	if fake.enableCalls != 0 {
		t.Fatal("旧代际任务写入了注册表")
	}
	if _, statErr := os.Stat(svc.proxyStatePath()); !os.IsNotExist(statErr) {
		t.Fatal("旧代际任务写入了代理备份")
	}
}

// 事务在等待 proxyMu 期间代际失效：拿到锁后必须重新校验并放弃。
func TestR1GenerationInvalidatedWhileWaitingForProxyLock(t *testing.T) {
	fake := &fakeRegistry{}
	svc := r1NewService(t, fake)

	gen, ctx := svc.beginKernelGeneration()
	svc.proxyMu.Lock()

	done := make(chan error, 1)
	go func() { done <- svc.setSystemProxyForGeneration(gen, ctx) }()

	// 先失效代际再释放锁，保证事务拿到锁时已是旧代际。
	svc.invalidateKernelGeneration()
	svc.proxyMu.Unlock()

	select {
	case err := <-done:
		if !errors.Is(err, errProxyStale) {
			t.Fatalf("期望 errProxyStale，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("代理事务未返回")
	}
	if fake.enableCalls != 0 {
		t.Fatal("失效代际不应写注册表")
	}
}
