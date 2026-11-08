//go:build windows

package sysproxy

import (
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const regPath = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

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
