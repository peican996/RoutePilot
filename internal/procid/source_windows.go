//go:build windows

package procid

import (
	"fmt"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WindowsSource 返回基于 Toolhelp32 快照的进程来源。
func WindowsSource() Source { return SourceFunc(listWindowsProcesses) }

// FileIdentity 返回 exe 的卷序列号 + 文件索引（0 表示读取失败）。
// 它只用于诊断/升级识别，不参与规则键：规则必须能跨重启稳定匹配。
func FileIdentity(path string) string {
	if path == "" {
		return ""
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	h, err := windows.CreateFile(
		p,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return ""
	}
	return fmt.Sprintf("%08X:%08X%08X", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow)
}

func listWindowsProcesses() ([]ProcInfo, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("创建进程快照失败: %w", err)
	}
	defer windows.CloseHandle(snap)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	out := make([]ProcInfo, 0, 256)
	err = windows.Process32First(snap, &entry)
	for err == nil {
		pid := entry.ProcessID
		// PID 0 是 Idle，4 是 System；它们没有可用的用户态可执行路径。
		if pid != 0 && pid != 4 {
			name := windows.UTF16ToString(entry.ExeFile[:])
			out = append(out, queryProcess(pid, name))
		}
		err = windows.Process32Next(snap, &entry)
	}
	if err != nil && err != windows.ERROR_NO_MORE_FILES {
		return nil, fmt.Errorf("遍历进程快照失败: %w", err)
	}
	return out, nil
}

func queryProcess(pid uint32, fallbackName string) ProcInfo {
	info := ProcInfo{PID: pid, Name: fallbackName}

	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		// 权限不足的系统进程：保留名称，路径留空，由上层降级。
		return info
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err == nil {
		info.Path = windows.UTF16ToString(buf[:size])
		if base := filepath.Base(info.Path); base != "" {
			info.Name = base
		}
	}

	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err == nil {
		info.Created = time.Unix(0, created.Nanoseconds())
	}
	return info
}
