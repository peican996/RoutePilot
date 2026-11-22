package main

import "testing"

// 启动恢复决策矩阵。核心是这一格：上次开着 TUN 但这次没管理员权限——
// 必须回落到系统代理，否则 TUN 用户重启电脑后处于"零接管"，浏览器打不开。
func TestStartupRestorePlan(t *testing.T) {
	cases := []struct {
		name         string
		restoreTUN   bool
		restoreProxy bool
		admin        bool
		wantTUN      bool
		wantProxy    bool
		wantWarn     bool
	}{
		{
			name:       "上次TUN_有管理员_恢复TUN",
			restoreTUN: true, admin: true,
			wantTUN: true, wantProxy: false, wantWarn: false,
		},
		{
			name:       "上次TUN_无管理员_回落系统代理并告警",
			restoreTUN: true, admin: false,
			wantTUN: false, wantProxy: true, wantWarn: true,
		},
		{
			name:         "上次系统代理_恢复系统代理",
			restoreProxy: true, admin: false,
			wantTUN: false, wantProxy: true, wantWarn: false,
		},
		{
			name:         "上次系统代理_有管理员也不动TUN",
			restoreProxy: true, admin: true,
			wantTUN: false, wantProxy: true, wantWarn: false,
		},
		{
			name:    "上次什么都没开_不要擅自接管",
			admin:   true,
			wantTUN: false, wantProxy: false, wantWarn: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tun, proxy, warn := startupRestorePlan(c.restoreTUN, c.restoreProxy, c.admin)
			if tun != c.wantTUN || proxy != c.wantProxy || warn != c.wantWarn {
				t.Fatalf("期望 (tun=%v proxy=%v warn=%v)，实际 (tun=%v proxy=%v warn=%v)",
					c.wantTUN, c.wantProxy, c.wantWarn, tun, proxy, warn)
			}
		})
	}
}

// 用户没开过任何接管时，重启后不能被程序擅自开代理——
// 那会让"我只是打开看看"变成"我的网络被改了"。
func TestStartupRestorePlanDoesNotTakeOverByDefault(t *testing.T) {
	if tun, proxy, _ := startupRestorePlan(false, false, true); tun || proxy {
		t.Fatalf("没有任何恢复意图时不应接管: tun=%v proxy=%v", tun, proxy)
	}
}
