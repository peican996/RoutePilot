//go:build windows

package sysproxy

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// withSandbox 把 regPath 指到 HKCU 下的临时键，并在用例结束后删掉。
//
// 这一层隔离是必须的：这些函数直接读写用户真实的系统代理，
// 测试若跑在真实路径上，一次失败就会把用户的网络设置改成沙箱值。
func withSandbox(t *testing.T) {
	t.Helper()
	root := fmt.Sprintf(`Software\RoutePilotTest_%d`, time.Now().UnixNano())
	k, _, err := registry.CreateKey(registry.CURRENT_USER, root, registry.ALL_ACCESS)
	if err != nil {
		t.Fatalf("创建沙箱键失败: %v", err)
	}
	_ = k.Close()

	old := regPath
	regPath = root
	t.Cleanup(func() {
		regPath = old
		// 键里只有值没有子键，DeleteKey 能连同值一起删掉
		_ = registry.DeleteKey(registry.CURRENT_USER, root)
	})
}

// 开启系统代理必须同时写入地址、绕过列表和开关，缺一个浏览器就用不上。
func TestEnableWritesAddrBypassAndSwitch(t *testing.T) {
	withSandbox(t)

	if err := Enable("127.0.0.1:7890"); err != nil {
		t.Fatalf("Enable 失败: %v", err)
	}

	addr, enabled := Current()
	if addr != "127.0.0.1:7890" {
		t.Fatalf("代理地址不对: %q", addr)
	}
	if !enabled {
		t.Fatal("ProxyEnable 应当为 1")
	}
	if got := Override(); got != ProxyBypass {
		t.Fatalf("绕过列表没写对:\n期望 %s\n实际 %s", ProxyBypass, got)
	}
}

// 关闭时只关开关，地址要留着，否则下次开启还得重新填。
func TestDisableKeepsServer(t *testing.T) {
	withSandbox(t)

	if err := Enable("127.0.0.1:7890"); err != nil {
		t.Fatalf("Enable 失败: %v", err)
	}
	if err := Disable(); err != nil {
		t.Fatalf("Disable 失败: %v", err)
	}

	addr, enabled := Current()
	if enabled {
		t.Fatal("关闭后 ProxyEnable 应当为 0")
	}
	if addr != "127.0.0.1:7890" {
		t.Fatalf("关闭不应清掉地址，实际 %q", addr)
	}
}

// SetEnabled 只碰开关，不能顺带覆盖地址——字段级恢复依赖这个契约。
func TestSetEnabledLeavesServerUntouched(t *testing.T) {
	withSandbox(t)

	if err := SetServer("10.0.0.1:8080"); err != nil {
		t.Fatalf("SetServer 失败: %v", err)
	}
	if err := SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled 失败: %v", err)
	}
	if addr, enabled := Current(); addr != "10.0.0.1:8080" || !enabled {
		t.Fatalf("开启开关不应改动地址: addr=%q enabled=%v", addr, enabled)
	}

	if err := SetEnabled(false); err != nil {
		t.Fatalf("SetEnabled(false) 失败: %v", err)
	}
	if addr, enabled := Current(); addr != "10.0.0.1:8080" || enabled {
		t.Fatalf("关闭开关不应改动地址: addr=%q enabled=%v", addr, enabled)
	}
}

// 传空串表示"原本没有代理"，必须把值删掉而不是写成空字符串，
// 否则还原后注册表里会留一个启用状态指向空的怪配置。
func TestSetServerEmptyDeletesValue(t *testing.T) {
	withSandbox(t)

	if err := SetServer("127.0.0.1:7890"); err != nil {
		t.Fatalf("SetServer 失败: %v", err)
	}
	if err := SetServer(""); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if addr, _ := Current(); addr != "" {
		t.Fatalf("清空后不应还有地址，实际 %q", addr)
	}

	// 再清一次：值已不存在，仍要视为成功（幂等）
	if err := SetServer(""); err != nil {
		t.Fatalf("重复清空应当幂等: %v", err)
	}
}

// 用户自定义的绕过列表要能原样读回，这是接管前备份的基础。
func TestOverrideRoundtrip(t *testing.T) {
	withSandbox(t)

	const custom = "*.corp.example;192.168.*"
	if err := SetOverride(custom); err != nil {
		t.Fatalf("SetOverride 失败: %v", err)
	}
	if got := Override(); got != custom {
		t.Fatalf("读回的绕过列表不对: %q", got)
	}
}

// PAC 地址写入 / 清除 / 重复清除。
func TestAutoConfigURLRoundtrip(t *testing.T) {
	withSandbox(t)

	if got := AutoConfigURL(); got != "" {
		t.Fatalf("初始应当为空，实际 %q", got)
	}
	if err := SetAutoConfigURL("http://127.0.0.1:9090/pac"); err != nil {
		t.Fatalf("写入 PAC 失败: %v", err)
	}
	if got := AutoConfigURL(); got != "http://127.0.0.1:9090/pac" {
		t.Fatalf("PAC 读回不对: %q", got)
	}
	if err := ClearAutoConfigURL(); err != nil {
		t.Fatalf("清除 PAC 失败: %v", err)
	}
	if got := AutoConfigURL(); got != "" {
		t.Fatalf("清除后应为空，实际 %q", got)
	}
	if err := ClearAutoConfigURL(); err != nil {
		t.Fatalf("重复清除应当幂等: %v", err)
	}
}

// 172.16.0.0/12 的 16 个 /16 一个都不能漏：
// 漏掉哪段，那段内网/Docker/WSL 流量就会被送进代理隧道。
func TestProxyBypassCoversAllPrivateRanges(t *testing.T) {
	for i := 16; i <= 31; i++ {
		seg := fmt.Sprintf("172.%d.*", i)
		if !strings.Contains(ProxyBypass, seg) {
			t.Fatalf("绕过列表缺少 %s", seg)
		}
	}
	for _, must := range []string{"<local>", "localhost", "127.*", "10.*", "192.168.*"} {
		if !strings.Contains(ProxyBypass, must) {
			t.Fatalf("绕过列表缺少 %s", must)
		}
	}
}

// WinHTTP 与 WinINET 必须用同一份绕过列表——两边各写一遍时曾经漂移过。
func TestWinHTTPBypassMatchesSystemProxyBypass(t *testing.T) {
	if WinHTTPBypass != ProxyBypass {
		t.Fatal("WinHTTP 与系统代理的绕过列表必须一致")
	}
}
