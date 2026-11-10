package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------- C5：TUN 外部代理快照与恢复 ----------
//
// 全部使用 fakeRegistry，不触碰开发机真实注册表。
// 并发/时序验证一律用注入钩子或直接构造磁盘状态，不使用 sleep。

func c5Service(t *testing.T, fake *fakeRegistry, dir string) *CoreService {
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

func c5ReadAppState(t *testing.T) appState {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dataDir(), appStateFile))
	if err != nil {
		t.Fatalf("读取 app-state 失败: %v", err)
	}
	var st appState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("解析 app-state 失败: %v", err)
	}
	return st
}

func c5WriteAppState(t *testing.T, st appState) {
	t.Helper()
	// 防呆：必须先由 c5Service 把 dataDirOverride 指向测试临时目录，
	// 否则会写到开发机真实的 %APPDATA%\RoutePilot。
	if getDataDirOverride() == "" {
		t.Fatal("c5WriteAppState 必须在 c5Service 之后调用，避免污染真实配置目录")
	}
	if st.Version == 0 {
		st.Version = currentConfigVersion
	}
	if st.Mode == "" {
		st.Mode = "rule"
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatalf("序列化 app-state 失败: %v", err)
	}
	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir(), appStateFile), b, 0o600); err != nil {
		t.Fatalf("写入 app-state 失败: %v", err)
	}
}

// 开 TUN 之前，用户原始系统代理快照必须在"关闭系统代理"这一动作之前落盘。
// 否则进程在关闭代理与内核重启完成之间被强制终止时，恢复依据永久丢失。
func TestC5TunSnapshotPersistedBeforeDisable(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:7897", enabled: true, override: "user-bypass"}
	svc := c5Service(t, fake, t.TempDir())

	var atDisable *appState
	fake.disableHook = func() {
		st := c5ReadAppState(t)
		atDisable = &st
	}

	if err := svc.setTUN(true, func() error { return nil }); err != nil {
		t.Fatalf("开启 TUN 失败: %v", err)
	}
	if atDisable == nil || atDisable.TunPrevRegistry == nil {
		t.Fatal("关闭系统代理时原始快照必须已经落盘")
	}
	got := atDisable.TunPrevRegistry
	if got.Server != "127.0.0.1:7897" || !got.Enabled || got.Override != "user-bypass" {
		t.Fatalf("快照内容不完整: %+v", got)
	}
	if fake.enabled {
		t.Fatal("开启 TUN 后系统代理必须被关闭")
	}
	st := c5ReadAppState(t)
	if st.TunPrevRegistry == nil || st.TunAfterDisable == nil {
		t.Fatalf("TUN 生效后必须持久化前后两份快照: %+v", st)
	}
	if st.TunAfterDisable.Enabled {
		t.Fatal("TUN 生效后的快照应记录系统代理已关闭")
	}
}

// 开 TUN 过程中被强制终止（注册表已关闭、tunOn 尚未提交）：
// 下次启动必须识别出快照并恢复用户原始代理。
func TestC5TunCrashBeforeCommitRestoresOnBoot(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:7897", enabled: false, override: "user-bypass"}
	dir := t.TempDir()
	svc := c5Service(t, fake, dir)
	prev := proxyBackup{Server: "127.0.0.1:7897", Override: "user-bypass", Enabled: true, Saved: true}
	after := proxyBackup{Server: "127.0.0.1:7897", Override: "user-bypass", Enabled: false, Saved: true}
	// 模拟崩溃现场：TUN 未提交（false），快照在盘上，尚未标记 Pending。
	c5WriteAppState(t, appState{TUN: false, TunPrevRegistry: &prev, TunAfterDisable: &after})

	svc.loadAppState()
	if err := svc.retryTunProxyRestore(); err != nil {
		t.Fatalf("启动恢复失败: %v", err)
	}
	if !fake.enabled || fake.server != "127.0.0.1:7897" || fake.override != "user-bypass" {
		t.Fatalf("启动时应恢复用户原始代理: %+v", fake)
	}
	st := c5ReadAppState(t)
	if st.TunPrevRegistry != nil || st.TunProxyPending {
		t.Fatalf("恢复成功后必须删除快照: %+v", st)
	}
}

// TUN 已生效时进程被强制终止：启动恢复 TUN 后，绝不能把系统代理重新打开，
// 快照必须保留给用户之后关闭 TUN 时使用。
func TestC5TunOnBootKeepsSnapshot(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:7897", enabled: false, override: "user-bypass"}
	dir := t.TempDir()
	svc := c5Service(t, fake, dir)
	prev := proxyBackup{Server: "127.0.0.1:7897", Override: "user-bypass", Enabled: true, Saved: true}
	after := proxyBackup{Server: "127.0.0.1:7897", Override: "user-bypass", Enabled: false, Saved: true}
	c5WriteAppState(t, appState{TUN: true, TunPrevRegistry: &prev, TunAfterDisable: &after})

	svc.loadAppState()
	if !svc.TUNState() {
		t.Fatal("管理员启动应恢复上次的 TUN 开启状态")
	}
	if err := svc.retryTunProxyRestore(); err != nil {
		t.Fatalf("TUN 生效时不应报错: %v", err)
	}
	if fake.enabled {
		t.Fatal("TUN 仍生效时绝不能打开系统代理（双代理）")
	}
	st := c5ReadAppState(t)
	if st.TunPrevRegistry == nil {
		t.Fatal("TUN 仍生效时必须保留快照")
	}
}

// 关 TUN 时恢复原始代理失败：必须报错、保留快照并置 Pending；
// 故障消失后重试要能完成恢复。全程不允许 sleep。
func TestC5TunOffRestoreFailureKeepsPendingThenRetries(t *testing.T) {
	fake := &fakeRegistry{
		server: "127.0.0.1:7897", enabled: false, override: "user-bypass",
		failSetOverride: true,
	}
	svc := c5Service(t, fake, t.TempDir())
	prev := proxyBackup{Server: "127.0.0.1:7897", Override: "user-bypass", Enabled: true, Saved: true}
	after := proxyBackup{Server: "127.0.0.1:7897", Override: "user-bypass", Enabled: false, Saved: true}
	svc.mu.Lock()
	svc.tunOn = true
	svc.tunPrevRegistry = prev
	svc.tunAfterDisable = after
	svc.mu.Unlock()
	svc.saveAppState()

	if err := svc.setTUN(false, func() error { return nil }); err == nil {
		t.Fatal("恢复系统代理失败时必须返回错误")
	}
	st := c5ReadAppState(t)
	if !st.TunProxyPending || st.TunPrevRegistry == nil {
		t.Fatalf("失败后必须保留 Pending 与快照: %+v", st)
	}

	fake.failSetOverride = false
	if err := svc.retryTunProxyRestore(); err != nil {
		t.Fatalf("重试恢复失败: %v", err)
	}
	if !fake.enabled || fake.override != "user-bypass" || fake.server != "127.0.0.1:7897" {
		t.Fatalf("重试后应还原原始代理: %+v", fake)
	}
	st = c5ReadAppState(t)
	if st.TunProxyPending || st.TunPrevRegistry != nil {
		t.Fatalf("重试成功后应清理快照: %+v", st)
	}
}

// PAC-only 配置（ProxyEnable=1、ProxyServer 为空、AutoConfigURL 有值）
// 必须能完整恢复，不能因为 Server 为空就跳过。
func TestC5TunRestorePACOnly(t *testing.T) {
	fake := &fakeRegistry{
		server: "", enabled: false, override: "",
		pac: "http://pac.local/proxy.pac",
	}
	svc := c5Service(t, fake, t.TempDir())
	prev := proxyBackup{Enabled: true, Server: "", Override: "", AutoConfigURL: "http://pac.local/proxy.pac", Saved: true}
	after := proxyBackup{Enabled: false, Server: "", Override: "", AutoConfigURL: "http://pac.local/proxy.pac", Saved: true}
	svc.mu.Lock()
	svc.tunOn = true
	svc.tunPrevRegistry = prev
	svc.tunAfterDisable = after
	svc.mu.Unlock()
	svc.saveAppState()

	if err := svc.setTUN(false, func() error { return nil }); err != nil {
		t.Fatalf("PAC-only 恢复失败: %v", err)
	}
	if !fake.enabled {
		t.Fatal("PAC-only 配置的启用状态必须恢复")
	}
	if fake.pac != "http://pac.local/proxy.pac" {
		t.Fatalf("PAC 地址未恢复: %q", fake.pac)
	}
	if fake.server != "" {
		t.Fatalf("PAC-only 不应写入 ProxyServer: %q", fake.server)
	}
	st := c5ReadAppState(t)
	if st.TunPrevRegistry != nil {
		t.Fatal("恢复成功后应删除快照")
	}
}

// TUN 运行期间用户/外部软件改了系统代理：关 TUN 时绝不覆盖新设置。
func TestC5TunExternalChangeNotOverwritten(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:7897", enabled: false, override: "user-bypass"}
	svc := c5Service(t, fake, t.TempDir())
	prev := proxyBackup{Server: "127.0.0.1:7897", Override: "user-bypass", Enabled: true, Saved: true}
	after := proxyBackup{Server: "127.0.0.1:7897", Override: "user-bypass", Enabled: false, Saved: true}
	svc.mu.Lock()
	svc.tunOn = true
	svc.tunPrevRegistry = prev
	svc.tunAfterDisable = after
	svc.mu.Unlock()
	svc.saveAppState()

	// 用户在 TUN 期间启用了另一个代理。
	fake.server = "127.0.0.1:1080"
	fake.enabled = true
	fake.override = "other-tool"

	if err := svc.setTUN(false, func() error { return nil }); err != nil {
		t.Fatalf("外部已接管时不应报错: %v", err)
	}
	if fake.server != "127.0.0.1:1080" || !fake.enabled || fake.override != "other-tool" {
		t.Fatalf("绝不能覆盖用户在 TUN 期间的新代理设置: %+v", fake)
	}
	st := c5ReadAppState(t)
	if st.TunPrevRegistry != nil || st.TunProxyPending {
		t.Fatalf("放弃还原后应清理快照: %+v", st)
	}
}

// 注册表恢复成功，但删除快照（app-state 落盘）失败：
// 必须如实报错、保留快照，不能谎报成功；故障消失后重试应幂等收敛。
func TestC5TunSnapshotDeleteFailureReported(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:7897", enabled: false, override: "user-bypass"}
	svc := c5Service(t, fake, t.TempDir())

	origWriter := appStateWriter
	var calls int32
	t.Cleanup(func() { appStateWriter = origWriter })
	// 前两次写入（提交 tunOn=false、标记 Pending）成功，删除快照那次失败。
	appStateWriter = func(path string, data []byte, perm os.FileMode) error {
		if atomic.AddInt32(&calls, 1) >= 3 {
			return os.ErrPermission
		}
		return origWriter(path, data, perm)
	}

	prev := proxyBackup{Server: "127.0.0.1:7897", Override: "user-bypass", Enabled: true, Saved: true}
	after := proxyBackup{Server: "127.0.0.1:7897", Override: "user-bypass", Enabled: false, Saved: true}
	svc.mu.Lock()
	svc.tunOn = true
	svc.tunPrevRegistry = prev
	svc.tunAfterDisable = after
	svc.mu.Unlock()

	err := svc.setTUN(false, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "删除 TUN 系统代理快照失败") {
		t.Fatalf("删除快照失败必须如实上报，实际 %v", err)
	}
	if !fake.enabled || fake.server != "127.0.0.1:7897" || fake.override != "user-bypass" {
		t.Fatalf("注册表应已恢复: %+v", fake)
	}

	// 故障消失后重试：注册表已是目标状态，必须幂等收敛并清掉快照。
	appStateWriter = origWriter
	if err := svc.retryTunProxyRestore(); err != nil {
		t.Fatalf("重试收敛失败: %v", err)
	}
	if !fake.enabled || fake.override != "user-bypass" {
		t.Fatalf("重试不应破坏已恢复的配置: %+v", fake)
	}
	st := c5ReadAppState(t)
	if st.TunPrevRegistry != nil || st.TunProxyPending {
		t.Fatalf("重试后应清理快照: %+v", st)
	}
}

// 开启 TUN 时若本程序此前正在接管系统代理：快照必须取"用户原始配置"
// （存在于接管备份中），而不是注册表里我们自己的旧端口。否则关 TUN 后
// 会把用户代理还原成一个早已失效的本程序地址。
func TestC5TunSnapshotUsesOriginalFromTakeoverBackup(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:17890", enabled: true, override: "ours"}
	svc := c5Service(t, fake, t.TempDir())
	bk := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:7897",
		Override: "user-bypass", Enabled: true, Saved: true,
	}
	writeState(t, svc, bk)
	svc.mu.Lock()
	svc.backup = bk
	svc.proxyOn = true
	svc.mu.Unlock()

	if err := svc.setTUN(true, func() error { return nil }); err != nil {
		t.Fatalf("开启 TUN 失败: %v", err)
	}
	st := c5ReadAppState(t)
	if st.TunPrevRegistry == nil {
		t.Fatal("必须持久化原始代理快照")
	}
	if st.TunPrevRegistry.Server != "127.0.0.1:7897" {
		t.Fatalf("快照必须是用户原始代理而不是本程序旧端口: %+v", st.TunPrevRegistry)
	}
	if fake.enabled || fake.server != "127.0.0.1:7897" || fake.override != "user-bypass" {
		t.Fatalf("开 TUN 前应还原用户原始代理再关闭: %+v", fake)
	}

	// 原本由本程序接管：关 TUN 后应重新接管，且接管备份里仍记录用户原值。
	if err := svc.setTUN(false, func() error { return nil }); err != nil {
		t.Fatalf("关闭 TUN 失败: %v", err)
	}
	if !fake.enabled || fake.server != "127.0.0.1:17890" {
		t.Fatalf("关 TUN 后应按原意图重新接管系统代理: %+v", fake)
	}
	state, ok := readState(t, svc)
	if !ok || state.Server != "127.0.0.1:7897" {
		t.Fatalf("重新接管的备份必须记录用户原始地址: %+v", state)
	}
}

// 开 TUN 前置快照落盘失败：必须中止，绝不能关闭用户的系统代理。
func TestC5TunAbortsWhenSnapshotCannotPersist(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:7897", enabled: true, override: "user-bypass"}
	svc := c5Service(t, fake, t.TempDir())

	origWriter := appStateWriter
	t.Cleanup(func() { appStateWriter = origWriter })
	appStateWriter = func(string, []byte, os.FileMode) error { return os.ErrPermission }

	err := svc.setTUN(true, func() error { return nil })
	if err == nil {
		t.Fatal("快照落盘失败时必须中止开启 TUN")
	}
	if !fake.enabled || fake.server != "127.0.0.1:7897" || fake.override != "user-bypass" {
		t.Fatalf("中止时不得改动系统代理: %+v", fake)
	}
	if fake.disableCalls != 0 {
		t.Fatal("快照未落盘时不允许调用 Disable")
	}
	if svc.TUNState() {
		t.Fatal("中止后不得声称 TUN 已开启")
	}
}
