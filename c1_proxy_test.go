package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRegistry 是系统代理注册表的可控替身，用来真实执行事务代码并注入失败。
type fakeRegistry struct {
	server   string
	enabled  bool
	override string
	pac      string

	failEnable      bool
	failDisable     bool
	failSetEnabled  bool
	failSetServer   bool
	failSetOverride bool
	failSetPAC      bool
	failClearPAC    bool

	// readbackServer 非空时 Current 在**已有写入发生**后返回该值，
	// 用于模拟"写入成功但注册表未生效"。还原事务的归属判定会在
	// 写入之前先读一次注册表，把"写入后才生效的错值"提前到写入前
	// 返回，会被误判成外部已接管。
	readbackServer string
	// written 表示已经发生过注册表写入。
	written bool
	// enableHook 在 Enable 真正写值之前调用，供并发测试插入确定性 barrier。
	enableHook func()
	// disableHook 在 Disable 成功之后调用，供测试等待事务真正落地。
	disableHook func()
	// setEnabledHook 在 SetEnabled 成功之后调用。系统代理还原走字段级写入，
	// 不再调用 Disable，需要还原落地的测试用这个钩子。
	setEnabledHook func()

	enableCalls     int
	disableCalls    int
	setEnabledCalls int
	winHTTPCalls    int
}

func (f *fakeRegistry) ops() systemProxyOps {
	return systemProxyOps{
		Current: func() (string, bool) {
			if f.readbackServer != "" && f.written {
				return f.readbackServer, f.enabled
			}
			return f.server, f.enabled
		},
		Enable: func(addr string) error {
			f.enableCalls++
			if f.enableHook != nil {
				f.enableHook()
			}
			if f.failEnable {
				return errors.New("injected enable failure")
			}
			f.server = addr
			f.enabled = true
			f.written = true
			// 真实 sysproxy.Enable 会写入固定的本地绕过列表，这里同步模拟。
			f.override = "ours"
			return nil
		},
		Disable: func() error {
			f.disableCalls++
			if f.failDisable {
				return errors.New("injected disable failure")
			}
			f.enabled = false
			f.written = true
			if f.disableHook != nil {
				f.disableHook()
			}
			return nil
		},
		SetEnabled: func(enabled bool) error {
			f.setEnabledCalls++
			if f.failSetEnabled {
				return errors.New("injected set-enabled failure")
			}
			f.enabled = enabled
			f.written = true
			if f.setEnabledHook != nil {
				f.setEnabledHook()
			}
			return nil
		},
		SetServer: func(addr string) error {
			if f.failSetServer {
				return errors.New("injected set-server failure")
			}
			f.server = addr
			return nil
		},
		Override: func() string { return f.override },
		SetOverride: func(s string) error {
			if f.failSetOverride {
				return errors.New("injected override failure")
			}
			f.override = s
			return nil
		},
		AutoConfigURL: func() string { return f.pac },
		SetAutoConfigURL: func(s string) error {
			if f.failSetPAC {
				return errors.New("injected PAC failure")
			}
			f.pac = s
			return nil
		},
		ClearAutoConfigURL: func() error {
			if f.failClearPAC {
				return errors.New("injected clear PAC failure")
			}
			f.pac = ""
			return nil
		},
		WinHTTPRaw:     func() ([]byte, error) { return nil, nil },
		RestoreWinHTTP: func([]byte) error { return nil },
		EnableWinHTTP: func(string) error {
			f.winHTTPCalls++
			return nil
		},
	}
}

func newC1Service(t *testing.T, fake *fakeRegistry) *CoreService {
	t.Helper()
	old := getDataDirOverride()
	setDataDirOverride(t.TempDir())
	t.Cleanup(func() { setDataDirOverride(old) })

	svc := NewCoreService()
	svc.sys = fake.ops()
	svc.elevated = func() bool { return true }
	svc.kernelRunningFn = func() bool { return true }
	svc.mixedPort = 17890
	return svc
}

func writeState(t *testing.T, svc *CoreService, bk proxyBackup) {
	t.Helper()
	if err := svc.persistProxyState(bk); err != nil {
		t.Fatalf("写入代理状态失败: %v", err)
	}
}

func readState(t *testing.T, svc *CoreService) (proxyBackup, bool) {
	t.Helper()
	bk, err := readProxyState()
	if err != nil {
		return proxyBackup{}, false
	}
	return bk, bk.Saved
}

func TestT01CrashRecoveryRestoresOriginal(t *testing.T) {
	fake := &fakeRegistry{
		server:   "127.0.0.1:17890",
		enabled:  true,
		override: "ours",
	}
	svc := newC1Service(t, fake)
	svc.restoreProxy = true

	// 模拟上次恢复过程中崩溃：备份还在，Pending=true。
	orig := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", AutoConfigURL: "http://old/pac",
		Enabled: false, Saved: true, Pending: true,
	}
	writeState(t, svc, orig)

	svc.recoverPendingProxyRestore()

	if fake.server != orig.Server || fake.enabled != orig.Enabled {
		t.Fatalf("恢复后注册表不符: server=%s enabled=%v", fake.server, fake.enabled)
	}
	if fake.override != orig.Override {
		t.Fatalf("Override 未恢复: %q", fake.override)
	}
	if fake.pac != orig.AutoConfigURL {
		t.Fatalf("PAC 未恢复: %q", fake.pac)
	}
	if _, ok := readState(t, svc); ok {
		t.Fatal("恢复成功后不应保留 proxy.json")
	}
	if svc.restoreProxy {
		t.Fatal("恢复成功后应清除旧的 restoreProxy 意图")
	}
	if svc.bootErr != "" {
		t.Fatalf("恢复成功不应有 bootErr: %s", svc.bootErr)
	}
}

// 正常接管后进程崩溃：下次启动应把代理校正到当前内核端口，退出时再还原。
func TestT01CrashRecoveryReconcilesActiveTakeover(t *testing.T) {
	fake := &fakeRegistry{
		server:   "127.0.0.1:17890",
		enabled:  true,
		override: "ours",
	}
	svc := newC1Service(t, fake)
	svc.mixedPort = 17891
	orig := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", Enabled: true, Saved: true,
	}
	writeState(t, svc, orig)

	svc.reconcileProxy()

	if !strings.EqualFold(fake.server, "127.0.0.1:17891") {
		t.Fatalf("启动后应校正到新端口，实际 %s", fake.server)
	}
	if !svc.proxyOn {
		t.Fatal("仍由本程序接管时应标记 proxyOn")
	}
	st, ok := readState(t, svc)
	if !ok || !strings.EqualFold(st.Addr, "127.0.0.1:17891") {
		t.Fatalf("接管记录应更新到新端口: %+v", st)
	}
}

func TestT02RestoreFailureKeepsBackupAndRetries(t *testing.T) {
	fake := &fakeRegistry{
		server:          "127.0.0.1:17890",
		enabled:         true,
		override:        "ours",
		failSetOverride: true,
	}
	svc := newC1Service(t, fake)
	cp := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", AutoConfigURL: "http://old/pac",
		Enabled: true, Saved: true,
	}
	svc.backup = cp
	writeState(t, svc, cp)

	err := svc.restoreSystemProxy()
	if err == nil {
		t.Fatal("Override 写入失败时恢复必须报错")
	}
	if !strings.Contains(err.Error(), "恢复系统代理失败") {
		t.Fatalf("错误信息不明确: %v", err)
	}
	st, ok := readState(t, svc)
	if !ok {
		t.Fatal("恢复失败后必须保留 proxy.json")
	}
	if !st.Pending {
		t.Fatal("恢复失败后必须标记 Pending")
	}
	if !svc.backup.Saved || !svc.backup.Pending {
		t.Fatal("内存备份也必须保留并标记 Pending")
	}

	// 故障解除后再次恢复：必须成功并删除备份。
	fake.failSetOverride = false
	if err := svc.restoreSystemProxy(); err != nil {
		t.Fatalf("重试恢复失败: %v", err)
	}
	if _, ok := readState(t, svc); ok {
		t.Fatal("重试成功后应删除 proxy.json")
	}
	if fake.server != cp.Server || fake.override != cp.Override || fake.pac != cp.AutoConfigURL {
		t.Fatalf("重试后注册表未完全还原: %+v", fake)
	}
	if svc.proxyOn {
		t.Fatal("恢复完成后 proxyOn 应为 false")
	}
}

func TestT03TUNEnableFailureRollsBackSystemProxy(t *testing.T) {
	fake := &fakeRegistry{
		server:   "127.0.0.1:17890",
		enabled:  true,
		override: "ours",
	}
	svc := newC1Service(t, fake)
	svc.proxyOn = true
	cp := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", AutoConfigURL: "http://old/pac",
		Enabled: true, Saved: true,
	}
	svc.backup = cp
	writeState(t, svc, cp)

	err := svc.setTUN(true, func() error { return errors.New("injected restart failure") })
	if err == nil {
		t.Fatal("内核重启失败时 SetTUN 必须返回错误")
	}
	if svc.tunOn {
		t.Fatal("失败回滚后 tunOn 应为 false")
	}
	if !svc.proxyOn {
		t.Fatal("失败回滚后应重新接管系统代理")
	}
	if !strings.EqualFold(fake.server, cp.Addr) || !fake.enabled {
		t.Fatalf("系统代理未恢复到本程序地址: %s enabled=%v", fake.server, fake.enabled)
	}
	if fake.override != "ours" {
		t.Fatalf("回滚后 ProxyOverride 应重新是接管值，实际 %q", fake.override)
	}
	if fake.pac != "" {
		t.Fatalf("回滚后 PAC 应重新被清除，实际 %q", fake.pac)
	}
	st, ok := readState(t, svc)
	if !ok || !st.Saved {
		t.Fatal("回滚后必须重新生成接管备份")
	}
	if !strings.EqualFold(st.Server, cp.Server) {
		t.Fatalf("备份里应是用户原地址，实际 %q", st.Server)
	}
}

func TestT04TUNDisableFailureKeepsUIInSync(t *testing.T) {
	fake := &fakeRegistry{
		server:   "127.0.0.1:8080",
		enabled:  false,
		override: "old-bypass",
	}
	svc := newC1Service(t, fake)
	svc.tunOn = true
	svc.tunPrevProxyOn = true
	svc.proxyOn = false
	running := true
	svc.kernelRunningFn = func() bool { return running }

	err := svc.setTUN(false, func() error {
		running = false // 旧内核已停止，新内核没起来
		return errors.New("injected restart failure")
	})
	if err == nil {
		t.Fatal("重启失败时必须返回错误")
	}
	if svc.tunOn {
		t.Fatal("内核已停止时 TUN 实际不存在，tunOn 必须为 false")
	}
	if !svc.tunPrevProxyOn {
		t.Fatal("恢复意图应保留，供下次启动继续处理")
	}
}

func TestT16ExternalProxyChangeIsNotOverwritten(t *testing.T) {
	fake := &fakeRegistry{
		server:   "127.0.0.1:8888",
		enabled:  true,
		override: "external",
	}
	svc := newC1Service(t, fake)
	svc.mixedPort = 17891
	st := proxyBackup{
		Addr: "127.0.0.1:17890", Server: "127.0.0.1:8080",
		Override: "old-bypass", Enabled: true, Saved: true,
	}
	writeState(t, svc, st)

	svc.reconcileProxy()

	if !strings.EqualFold(fake.server, "127.0.0.1:8888") || fake.enableCalls != 0 {
		t.Fatalf("外部代理不应被覆盖: server=%s enableCalls=%d", fake.server, fake.enableCalls)
	}
	if svc.proxyOn {
		t.Fatal("外部接管后不应再声称代理由本程序控制")
	}
	if _, ok := readState(t, svc); ok {
		t.Fatal("放弃接管时应删除旧备份，避免退出时覆盖外部设置")
	}
	// 退出时不应对注册表做任何写操作。
	svc.Shutdown()
	if !strings.EqualFold(fake.server, "127.0.0.1:8888") {
		t.Fatalf("Shutdown 不应覆盖外部代理，实际 %s", fake.server)
	}
}

func TestT10ProxyStateAtomicWriteFailureKeepsOldFile(t *testing.T) {
	svc := newC1Service(t, &fakeRegistry{})
	path := svc.proxyStatePath()
	old := []byte(`{"saved":true,"server":"127.0.0.1:8080"}`)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}

	orig := writeFileAtomic
	writeFileAtomic = func(string, []byte, os.FileMode) error {
		return errors.New("injected disk failure")
	}
	t.Cleanup(func() { writeFileAtomic = orig })

	err := svc.persistProxyState(proxyBackup{Saved: true, Server: "127.0.0.1:9999"})
	if err == nil {
		t.Fatal("写入失败必须返回错误")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(old) {
		t.Fatalf("写失败后旧文件被破坏: %q", got)
	}
	// 临时文件必须清理干净。
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "."+filepath.Base(path)+".tmp-") {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}
}
