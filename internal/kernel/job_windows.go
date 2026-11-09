//go:build windows

package kernel

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
)

// jobObject 拥有一个「句柄关闭即杀死其中进程」的 Job。
//
// 所有权：每个 Manager 持有自己的 Job，创建后一直复用，只在 Manager.Close
// 时关闭。创建、配置、分配进程的每一步错误都会返回给调用方，不再静默降级。
//
// 兼容性：Windows 8 及以上支持嵌套 Job。如果当前进程已经在一个禁止嵌套的
// Job 中（主要是 Windows 7），AssignProcessToJobObject 会返回
// ERROR_ACCESS_DENIED，此时调用方会中止启动并给出可诊断错误。
type jobObject struct {
	mu      sync.Mutex
	handle  windows.Handle
	initErr error
	closed  bool
}

func newJobObject() *jobObject {
	return &jobObject{}
}

// ensureLocked 惰性创建并配置 Job。调用方必须持有 j.mu。
func (j *jobObject) ensureLocked() error {
	if j.closed {
		return fmt.Errorf("Job Object 已关闭")
	}
	if j.handle != 0 {
		return j.initErr
	}

	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		j.initErr = fmt.Errorf("CreateJobObject 失败: %w", err)
		return j.initErr
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	if _, err := windows.SetInformationJobObject(
		h,
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(h)
		j.initErr = fmt.Errorf("SetInformationJobObject 失败: %w", err)
		return j.initErr
	}

	j.handle = h
	return nil
}

// assign 把指定 PID 加入 Job。任何一步失败都会返回错误。
func (j *jobObject) assign(pid int) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if pid <= 0 {
		return fmt.Errorf("无效的进程 PID: %d", pid)
	}
	if err := j.ensureLocked(); err != nil {
		return err
	}

	hProc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
		false,
		uint32(pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess(%d) 失败: %w", pid, err)
	}
	defer windows.CloseHandle(hProc)

	if err := windows.AssignProcessToJobObject(j.handle, hProc); err != nil {
		return fmt.Errorf("AssignProcessToJobObject(%d) 失败: %w", pid, err)
	}
	return nil
}

// close 关闭 Job 句柄，并因此终止仍挂在其中的进程。
func (j *jobObject) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.closed {
		return nil
	}
	j.closed = true
	if j.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(j.handle)
	j.handle = 0
	if err != nil {
		return fmt.Errorf("关闭 Job Object 失败: %w", err)
	}
	return nil
}
