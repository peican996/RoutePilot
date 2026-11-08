//go:build windows

package elevate

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32           = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteW = shell32.NewProc("ShellExecuteW")
)

const swShowNormal = 1

// IsElevated 判断当前进程是否以管理员身份运行。
// TUN 要创建虚拟网卡和改路由表，没有管理员权限时 mihomo 会直接失败。
func IsElevated() bool {
	var sid *windows.SID
	err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY,
		2,
		windows.SECURITY_BUILTIN_DOMAIN_RID,
		windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0,
		&sid,
	)
	if err != nil {
		return false
	}
	defer windows.FreeSid(sid)

	token := windows.GetCurrentProcessToken()
	member, err := token.IsMember(sid)
	if err != nil {
		return false
	}
	return member
}

// RelaunchAsAdmin 以管理员身份重新拉起自己。
// 调用方应在成功后立刻退出，否则会出现两个实例。
func RelaunchAsAdmin() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取程序路径失败: %w", err)
	}
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}

	ret, _, _ := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		0,
		0,
		swShowNormal,
	)
	// ShellExecuteW 返回值 <= 32 表示失败，其中 5 是用户点了"否"。
	if ret <= 32 {
		if ret == 5 {
			return fmt.Errorf("已取消管理员授权")
		}
		return fmt.Errorf("提权失败 (代码 %d)", ret)
	}
	return nil
}
