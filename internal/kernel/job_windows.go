//go:build windows

package kernel

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
)

type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobExtendedLimit struct {
	BasicLimitInformation jobBasicLimit
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

var (
	kernel32Job                  = windows.NewLazySystemDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32Job.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32Job.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32Job.NewProc("AssignProcessToJobObject")
	jobHandle                    uintptr
)

// attachToJob 把内核进程挂到一个"随父进程退出而终止"的 Job Object 上。
//
// 正常关窗口会走 Shutdown 主动停内核，但被任务管理器强杀或崩溃时不会。
// 没有这层保护就会留下仍占着端口的孤儿内核，下次启动只能被迫换端口。
// 失败时静默降级：只是少了保护，不影响功能。
func attachToJob(pid int) {
	if jobHandle == 0 {
		h, _, _ := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			return
		}
		jobHandle = h
		info := jobExtendedLimit{}
		info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
		procSetInformationJobObject.Call(
			jobHandle,
			uintptr(jobObjectExtendedLimitInformation),
			uintptr(unsafe.Pointer(&info)),
			unsafe.Sizeof(info),
		)
	}
	hProc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(hProc)
	procAssignProcessToJobObject.Call(jobHandle, uintptr(hProc))
}
