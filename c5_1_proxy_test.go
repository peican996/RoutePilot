package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------- C5.1：代理恢复边界 ----------
//
// 全部使用 fakeRegistry；不触碰真实注册表。时序验证依靠注入钩子与磁盘状态构造。

func c51TryReadAppState(t *testing.T) (appState, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dataDir(), appStateFile))
	if err != nil {
		return appState{}, false
	}
	var st appState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("解析 app-state 失败: %v", err)
	}
	return st, true
}

// 构造"TUN 已提交为关闭、恢复尚未完成"的服务状态。
// 真实的关 TUN 流程会先把 tunOn 置 false 再执行恢复，因此这里也按该阶段建模，
// 否则启动补偿 retryTunProxyRestore 会因为 tunOn=true 而跳过。
func c51PendingService(t *testing.T, fake *fakeRegistry, prev, after proxyBackup) *CoreService {
	t.Helper()
	svc := c5Service(t, fake, t.TempDir())
	svc.mu.Lock()
	svc.tunOn = false
	svc.tunPrevRegistry = prev
	svc.tunAfterDisable = after
	svc.tunProxyPending = false
	svc.tunRestoreTxn = nil
	svc.mu.Unlock()
	svc.saveAppState()
	return svc
}

// 首次恢复失败后，用户修改了某个字段；再次启动必须保留用户新值。
// 覆盖 ProxyEnable / ProxyServer / ProxyOverride / AutoConfigURL 四个字段。
func TestC51PendingRetryPreservesUserNewProxy(t *testing.T) {
	type tc struct {
		name   string
		prev   proxyBackup
		after  proxyBackup
		mutate func(*fakeRegistry)
		want   func(*fakeRegistry) error
	}
	cases := []tc{
		{
			name: "ProxyEnable",
			// 用户原本关着代理（Enabled=false），TUN 期间把它打开了。
			prev:   proxyBackup{Enabled: false, Server: "127.0.0.1:7897", Override: "user-bypass", Saved: true},
			after:  proxyBackup{Enabled: false, Server: "127.0.0.1:7897", Override: "user-bypass", Saved: true},
			mutate: func(f *fakeRegistry) { f.enabled = true },
			want: func(f *fakeRegistry) error {
				if !f.enabled {
					return errBool("用户开启的代理被覆盖")
				}
				return nil
			},
		},
		{
			name:   "ProxyServer",
			prev:   proxyBackup{Enabled: true, Server: "127.0.0.1:7897", Override: "user-bypass", Saved: true},
			after:  proxyBackup{Enabled: false, Server: "127.0.0.1:7897", Override: "user-bypass", Saved: true},
			mutate: func(f *fakeRegistry) { f.server = "127.0.0.1:1080" },
			want: func(f *fakeRegistry) error {
				if f.server != "127.0.0.1:1080" {
					return errBool("用户新设置的 Server 被覆盖: " + f.server)
				}
				return nil
			},
		},
		{
			name:   "ProxyOverride",
			prev:   proxyBackup{Enabled: true, Server: "127.0.0.1:7897", Override: "user-bypass", Saved: true},
			after:  proxyBackup{Enabled: false, Server: "127.0.0.1:7897", Override: "user-bypass", Saved: true},
			mutate: func(f *fakeRegistry) { f.override = "user-new-bypass" },
			want: func(f *fakeRegistry) error {
				if f.override != "user-new-bypass" {
					return errBool("用户新设置的 Override 被覆盖: " + f.override)
				}
				return nil
			},
		},
		{
			name:   "AutoConfigURL",
			prev:   proxyBackup{Enabled: true, Server: "127.0.0.1:7897", Override: "user-bypass", Saved: true},
			after:  proxyBackup{Enabled: false, Server: "127.0.0.1:7897", Override: "user-bypass", Saved: true},
			mutate: func(f *fakeRegistry) { f.pac = "http://user.local/new.pac" },
			want: func(f *fakeRegistry) error {
				if f.pac != "http://user.local/new.pac" {
					return errBool("用户新设置的 PAC 被覆盖: " + f.pac)
				}
				return nil
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeRegistry{
				server: c.after.Server, enabled: c.after.Enabled, override: c.after.Override, pac: c.after.AutoConfigURL,
				// 首次恢复在最后一步（SetEnabled）失败，留下 Pending。
				failSetEnabled: true,
			}
			svc := c51PendingService(t, fake, c.prev, c.after)

			// 首次恢复：必须失败且 Pending 落盘。
			err := svc.withProxyTx(0, nil, func() error {
				return svc.restoreTunExternalProxyLocked(c.prev, c.after)
			})
			if err == nil {
				t.Fatal("注入写入失败后恢复必须报错")
			}
			st, ok := c51TryReadAppState(t)
			if !ok || !st.TunProxyPending || st.TunRestoreTxn == nil || st.TunPrevRegistry == nil {
				t.Fatalf("Pending 与字段级记录必须落盘: %+v", st)
			}

			// 用户修改字段。
			c.mutate(fake)

			// 模拟重启：新实例从磁盘恢复。
			svc2 := c5Service(t, fake, dataDir())
			svc2.loadAppState()
			fake.failSetEnabled = false
			if err := svc2.retryTunProxyRestore(); err != nil {
				t.Fatalf("检测到用户修改后应安全放弃而不是报错: %v", err)
			}
			if err := c.want(fake); err != nil {
				t.Fatal(err)
			}
			st2, ok := c51TryReadAppState(t)
			if !ok {
				t.Fatal("app-state 不应消失")
			}
			if st2.TunPrevRegistry != nil || st2.TunRestoreTxn != nil || st2.TunProxyPending {
				t.Fatalf("放弃还原后必须清理快照与恢复记录: %+v", st2)
			}
			// 未被用户修改的字段也不能被写回旧值。
			if c.name != "ProxyServer" && fake.server != c.after.Server {
				t.Fatalf("未修改字段被写回旧值: %s", fake.server)
			}
		})
	}
}

type errBool string

func (e errBool) Error() string { return string(e) }

// 关闭 TUN 后重新接管：新备份必须完整保留用户原始四字段，
// 用户之后关闭代理时能还原到最初配置。
func TestC51ReTakeoverPreservesOriginalFourFields(t *testing.T) {
	orig := proxyBackup{
		Enabled: true, Server: "127.0.0.1:7897",
		Override: "user-bypass", AutoConfigURL: "http://pac.local/proxy.pac", Saved: true,
	}
	after := proxyBackup{
		Enabled: false, Server: "127.0.0.1:7897",
		Override: "user-bypass", AutoConfigURL: "http://pac.local/proxy.pac", Saved: true,
	}
	fake := &fakeRegistry{server: "127.0.0.1:7897", enabled: false, override: "user-bypass", pac: "http://pac.local/proxy.pac"}

	svc := c5Service(t, fake, t.TempDir())
	svc.mu.Lock()
	svc.tunOn = true
	svc.tunPrevProxyOn = true
	svc.tunPrevRegistry = orig
	svc.tunAfterDisable = after
	svc.mu.Unlock()
	svc.saveAppState()

	if err := svc.setTUN(false, func() error { return nil }); err != nil {
		t.Fatalf("关闭 TUN 失败: %v", err)
	}
	// 已重新接管：注册表指向本程序端口。
	if !fake.enabled || fake.server != "127.0.0.1:17890" {
		t.Fatalf("应重新接管系统代理: %+v", fake)
	}
	// 新备份必须与用户原始状态一致，不能记录 Enabled=false。
	bk, ok := readState(t, svc)
	if !ok {
		t.Fatal("重新接管后必须持久化备份")
	}
	if bk.Enabled != orig.Enabled || bk.Server != orig.Server ||
		bk.Override != orig.Override || bk.AutoConfigURL != orig.AutoConfigURL {
		t.Fatalf("重新接管的备份必须保留用户原始四字段: %+v", bk)
	}

	// 用户关闭 RoutePilot 系统代理：必须还原到最初配置。
	if err := svc.SetSystemProxy(false); err != nil {
		t.Fatalf("关闭系统代理失败: %v", err)
	}
	if !fake.enabled || fake.server != "127.0.0.1:7897" ||
		fake.override != "user-bypass" || fake.pac != "http://pac.local/proxy.pac" {
		t.Fatalf("未还原到最初用户配置: %+v", fake)
	}
	if _, ok := readState(t, svc); ok {
		t.Fatal("还原成功后备份必须删除")
	}
}

// afterDisable 落盘失败：必须回滚注册表并中止开启 TUN。
func TestC51AfterDisablePersistFailureRollsBack(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:7897", enabled: true, override: "user-bypass"}
	svc := c5Service(t, fake, t.TempDir())

	origWriter := appStateWriter
	var calls int32
	t.Cleanup(func() { appStateWriter = origWriter })
	appStateWriter = func(path string, data []byte, perm os.FileMode) error {
		if atomic.AddInt32(&calls, 1) == 2 { // 第 2 次是 afterDisable
			return os.ErrPermission
		}
		return origWriter(path, data, perm)
	}

	if err := svc.setTUN(true, func() error { return nil }); err == nil {
		t.Fatal("afterDisable 落盘失败必须中止开启 TUN")
	}
	if !fake.enabled || fake.server != "127.0.0.1:7897" || fake.override != "user-bypass" {
		t.Fatalf("失败后必须把注册表回滚到原始状态: %+v", fake)
	}
	if svc.TUNState() {
		t.Fatal("中止后不得声称 TUN 已开启")
	}
	st, ok := c51TryReadAppState(t)
	if ok && (st.TunPrevRegistry != nil || st.TunProxyPending) {
		t.Fatalf("回滚成功后不应留下恢复依据: %+v", st)
	}
}

// afterDisable 保存失败且回滚也失败：保留快照并标记 Pending；
// 启动时因缺少 afterDisable 判定依据，绝不能覆盖当前注册表。
func TestC51AfterDisablePersistAndRollbackFailureKeepsPending(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:7897", enabled: true, override: "user-bypass"}
	svc := c5Service(t, fake, t.TempDir())

	origWriter := appStateWriter
	var calls int32
	t.Cleanup(func() { appStateWriter = origWriter })
	appStateWriter = func(path string, data []byte, perm os.FileMode) error {
		if atomic.AddInt32(&calls, 1) == 2 {
			return os.ErrPermission
		}
		return origWriter(path, data, perm)
	}
	fake.failSetOverride = true // 让回滚也失败

	if err := svc.setTUN(true, func() error { return nil }); err == nil {
		t.Fatal("必须返回错误")
	}
	st, ok := c51TryReadAppState(t)
	if !ok || st.TunPrevRegistry == nil || !st.TunProxyPending {
		t.Fatalf("回滚失败必须保留快照并标记 Pending: %+v", st)
	}

	// 用户在重启前设置了新代理。
	fake.failSetOverride = false
	fake.server = "127.0.0.1:1080"
	fake.enabled = true
	fake.override = "new-bypass"

	svc2 := c5Service(t, fake, dataDir())
	svc2.loadAppState()
	err := svc2.retryTunProxyRestore()
	if err == nil || !strings.Contains(err.Error(), "判定依据") {
		t.Fatalf("缺少判定依据时必须报错而不是覆盖: %v", err)
	}
	if fake.server != "127.0.0.1:1080" || fake.override != "new-bypass" || !fake.enabled {
		t.Fatalf("绝不能覆盖用户的新代理: %+v", fake)
	}
}

// 崩溃发生在 afterDisable 落盘之前：启动恢复不能覆盖当前代理。
func TestC51MissingAfterDisableNeverOverwrites(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:1080", enabled: true, override: "new-bypass"}
	dir := t.TempDir()
	svc := c5Service(t, fake, dir)
	prev := proxyBackup{Enabled: true, Server: "127.0.0.1:7897", Override: "user-bypass", Saved: true}
	// 模拟崩溃现场：有原始快照，没有 afterDisable，也没有 Pending。
	c5WriteAppState(t, appState{TUN: false, TunPrevRegistry: &prev})

	svc.loadAppState()
	err := svc.retryTunProxyRestore()
	if err == nil || !strings.Contains(err.Error(), "判定依据") {
		t.Fatalf("缺少 afterDisable 时必须报错: %v", err)
	}
	if fake.server != "127.0.0.1:1080" || fake.override != "new-bypass" || !fake.enabled {
		t.Fatalf("绝不能覆盖当前代理: %+v", fake)
	}
	st, ok := c51TryReadAppState(t)
	if !ok {
		t.Fatal("app-state 不应消失")
	}
	if st.TunPrevRegistry != nil || st.TunProxyPending {
		t.Fatalf("无法判定时应清理快照避免反复覆盖风险: %+v", st)
	}
}

// 内核未运行时开启 TUN：直接拒绝，且不改动任何状态。
func TestC51TunRefusedWhenKernelNotRunning(t *testing.T) {
	fake := &fakeRegistry{server: "127.0.0.1:7897", enabled: true, override: "user-bypass"}
	dir := t.TempDir()
	svc := c5Service(t, fake, dir)
	svc.kernelRunningFn = func() bool { return false }

	err := svc.setTUN(true, func() error { t.Fatal("内核未运行时不得重启内核"); return nil })
	if err == nil || !strings.Contains(err.Error(), "内核未运行") {
		t.Fatalf("必须拒绝开启 TUN: %v", err)
	}
	if !fake.enabled || fake.server != "127.0.0.1:7897" || fake.override != "user-bypass" {
		t.Fatalf("不得修改系统代理: %+v", fake)
	}
	if fake.disableCalls != 0 || fake.setEnabledCalls != 0 {
		t.Fatal("不得写入注册表")
	}
	if svc.TUNState() {
		t.Fatal("不得提交 tunOn=true")
	}
	st, ok := c51TryReadAppState(t)
	if ok && (st.TunPrevRegistry != nil || st.TunAfterDisable != nil || st.TunProxyPending || st.TUN) {
		t.Fatalf("不得留下恢复意图: %+v", st)
	}
}
