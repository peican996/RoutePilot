//go:build windows

package autostart

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey  = `Software\Microsoft\Windows\CurrentVersion\Run`
	appName = "NexusLink"
	// 旧版本的启动项名。产品改名后要顺手清掉，否则注册表里
	// 会留一条指向新路径但叫 ProxDeck 的僵尸启动项。
	legacyAppName = "ProxDeck"
)

// Enabled 检查是否已经在开机启动项里。
func Enabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	if v, _, err := k.GetStringValue(appName); err == nil && v != "" {
		return true
	}
	// 旧名字的启动项也算"开着"，否则升级后用户会看到开关被莫名关掉
	v, _, err := k.GetStringValue(legacyAppName)
	return err == nil && v != ""
}

// Set 增删注册表启动项。
// 写 HKCU 而不是 HKLM：只对当前用户生效，且不需要管理员权限——
// 否则每次开机都要弹一次 UAC，等于没有自启。
func Set(enable bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	if !enable {
		// 不存在时 DeleteValue 会报错，忽略即可。
		_ = k.DeleteValue(appName)
		_ = k.DeleteValue(legacyAppName)
		return nil
	}

	// 清理旧名字的启动项，避免两个同名程序同时自启
	_ = k.DeleteValue(legacyAppName)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// 路径可能含空格，必须加引号，否则 Windows 会截断。
	return k.SetStringValue(appName, `"`+exe+`"`)
}
