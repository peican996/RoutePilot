//go:build windows

package procid

import (
	"os"
	"testing"
)

// 真实 Windows 快照必须能拿到自身进程的路径与创建时间，
// 并能从 exe 读取文件身份。PID 复用判断依赖这些字段。
func TestWindowsSourceFindsSelf(t *testing.T) {
	infos, err := listWindowsProcesses()
	if err != nil {
		t.Fatalf("枚举进程失败: %v", err)
	}
	self := uint32(os.Getpid())
	var found *ProcInfo
	for i := range infos {
		if infos[i].PID == self {
			found = &infos[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("进程快照中没有当前进程 %d", self)
	}
	if found.Path == "" {
		t.Fatalf("当前进程应能获取完整路径: %+v", *found)
	}
	if found.Created.IsZero() {
		t.Fatalf("当前进程应能获取创建时间: %+v", *found)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if id := FileIdentity(exe); id == "" {
		t.Fatalf("应能读取 exe 文件身份: %s", exe)
	}
}
