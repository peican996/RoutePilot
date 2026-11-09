//go:build windows

package sysproxy

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const regPath = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// WinHTTP 的代理设置是机器级状态，放在 HKLM 下。
const winHTTPKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings\Connections`

const (
	optionRefresh         = 37 // INTERNET_OPTION_REFRESH
	optionSettingsChanged = 39 // INTERNET_OPTION_SETTINGS_CHANGED
)

var procInternetSetOptionW = windows.NewLazySystemDLL("wininet.dll").NewProc("InternetSetOptionW")

// notify 通知系统代理设置已变更。少了这一步，浏览器会继续用旧的代理配置，
// 表现为"开关拨了但没反应"。
func notify() {
	_, _, _ = procInternetSetOptionW.Call(0, uintptr(optionSettingsChanged), 0, 0)
	_, _, _ = procInternetSetOptionW.Call(0, uintptr(optionRefresh), 0, 0)
}

// Enable 把系统代理指向 addr，形如 127.0.0.1:7890。
func Enable(addr string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表失败: %w", err)
	}
	defer k.Close()

	if err := k.SetStringValue("ProxyServer", addr); err != nil {
		return fmt.Errorf("写入 ProxyServer 失败: %w", err)
	}
	// 本地地址一律绕过，否则访问 127.0.0.1 的服务（含内核自己的 API）会被二次代理。
	bypass := "<local>;localhost;127.*;10.*;172.16.*;172.17.*;172.18.*;192.168.*;*.local"
	if err := k.SetStringValue("ProxyOverride", bypass); err != nil {
		return fmt.Errorf("写入 ProxyOverride 失败: %w", err)
	}
	if err := k.SetDWordValue("ProxyEnable", 1); err != nil {
		return fmt.Errorf("开启 ProxyEnable 失败: %w", err)
	}
	notify()
	return nil
}

// Disable 关闭系统代理，但保留 ProxyServer 值，方便下次秒开。
func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表失败: %w", err)
	}
	defer k.Close()

	if err := k.SetDWordValue("ProxyEnable", 0); err != nil {
		return fmt.Errorf("关闭 ProxyEnable 失败: %w", err)
	}
	notify()
	return nil
}

// SetServer 单独恢复 ProxyServer。关闭代理时 Disable 只改 ProxyEnable，
// 不会动地址；要把注册表完整还原到接管前，必须显式写回/删除该值。
func SetServer(addr string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表失败: %w", err)
	}
	defer k.Close()

	if addr == "" {
		if err := k.DeleteValue("ProxyServer"); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return fmt.Errorf("删除 ProxyServer 失败: %w", err)
		}
	} else if err := k.SetStringValue("ProxyServer", addr); err != nil {
		return fmt.Errorf("写入 ProxyServer 失败: %w", err)
	}
	notify()
	return nil
}

// Current 返回当前系统代理地址以及是否启用。
func Current() (string, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()

	enabled := false
	if v, _, err := k.GetIntegerValue("ProxyEnable"); err == nil {
		enabled = v == 1
	}
	addr, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		addr = ""
	}
	return addr, enabled
}

// Override 读取当前的代理绕过列表。接管前存一份，还原时写回，
// 否则用户自定义的绕过规则会被我们的固定串永久覆盖。
func Override() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("ProxyOverride")
	if err != nil {
		return ""
	}
	return v
}

// SetOverride 写回代理绕过列表。
func SetOverride(s string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表失败: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue("ProxyOverride", s); err != nil {
		return fmt.Errorf("写入 ProxyOverride 失败: %w", err)
	}
	notify()
	return nil
}

// AutoConfigURL 读取当前的 PAC 地址。
// Windows 里 PAC 的优先级高于手动 ProxyServer，接管时必须一起处理，
// 否则用户明明看到"系统代理已开启"，浏览器却还在按 PAC 走。
func AutoConfigURL() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("AutoConfigURL")
	if err != nil {
		return ""
	}
	return v
}

// SetAutoConfigURL 写回 PAC 地址。
func SetAutoConfigURL(s string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表失败: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue("AutoConfigURL", s); err != nil {
		return fmt.Errorf("写入 AutoConfigURL 失败: %w", err)
	}
	notify()
	return nil
}

// ClearAutoConfigURL 移除 PAC 地址。原本不存在时也视为成功。
func ClearAutoConfigURL() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, regPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表失败: %w", err)
	}
	defer k.Close()
	_ = k.DeleteValue("AutoConfigURL")
	notify()
	return nil
}

// WinHTTPBypass 是 WinHTTP 的本地绕过列表。
const WinHTTPBypass = "<local>;localhost;127.*;10.*;172.16.*;172.17.*;172.18.*;192.168.*;*.local"

// WinHTTPRaw 读取系统当前的 WinHTTP 代理二进制设置。
// 返回值 nil 表示从未设置过（即直连），还原时删除该值即可。
func WinHTTPRaw() ([]byte, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, winHTTPKey, registry.QUERY_VALUE)
	if err != nil {
		return nil, fmt.Errorf("读取 WinHTTP 设置失败: %w", err)
	}
	defer k.Close()

	b, _, err := k.GetBinaryValue("WinHttpSettings")
	if err != nil {
		// 值不存在等价于直连，不算错误。
		return nil, nil
	}
	return b, nil
}

// RestoreWinHTTP 还原 WinHTTP 代理设置。data 为 nil 时删除设置（直连）。
// 写 HKLM 需要管理员权限。
func RestoreWinHTTP(data []byte) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, winHTTPKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开 WinHTTP 注册表失败: %w", err)
	}
	defer k.Close()

	if len(data) == 0 {
		// 值本来就不存在等价于直连成功；其它删除失败必须上报，
		// 否则调用方会误以为已经恢复，实际服务流量仍指向死端口。
		if err := k.DeleteValue("WinHttpSettings"); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return fmt.Errorf("删除 WinHTTP 设置失败: %w", err)
		}
		return nil
	}
	if err := k.SetBinaryValue("WinHttpSettings", data); err != nil {
		return fmt.Errorf("还原 WinHTTP 设置失败: %w", err)
	}
	return nil
}

// EnableWinHTTP 把 WinHTTP 代理指向 addr。需要管理员权限；
// 这里走 netsh 而不是自己拼二进制注册表值，减少格式出错的概率。
func EnableWinHTTP(addr string) error {
	cmd := exec.Command("netsh", "winhttp", "set", "proxy", addr, "bypass-list="+WinHTTPBypass)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("设置 WinHTTP 代理失败: %v（%s）", err, strings.TrimSpace(string(out)))
	}
	return nil
}
