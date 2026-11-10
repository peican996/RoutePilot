package procid

import (
	"errors"
	"testing"
	"time"
)

// PID 复用：快照尚未过期时同名进程换成了新实例（创建时间不同）。
// 采纳路径前必须实时校验并强制刷新，绝不能把连接挂到旧路径上。
func TestLookupNamePIDReuseForcesRefresh(t *testing.T) {
	src := &mutableSource{}
	tr, _ := newTestTracker(src)

	current := map[uint32]time.Time{100: time.Unix(1000, 0)}
	tr.SetVerifyFunc(func(info ProcInfo) bool {
		c, ok := current[info.PID]
		return ok && c.Equal(info.Created)
	})

	src.set(ProcInfo{PID: 100, Name: "app.exe", Path: `C:\old\app.exe`, Created: time.Unix(1000, 0)})
	if res := tr.LookupName("app.exe"); res.State != LookupUnique || res.Info.Path != `C:\old\app.exe` {
		t.Fatalf("初始解析应命中旧路径: %+v", res)
	}

	// 旧实例退出，同名新实例复用了同一个 PID，但创建时间不同。
	current[100] = time.Unix(2000, 0)
	src.set(ProcInfo{PID: 100, Name: "app.exe", Path: `C:\new\app.exe`, Created: time.Unix(2000, 0)})

	res := tr.LookupName("app.exe")
	if res.State != LookupUnique || res.Info.Path != `C:\new\app.exe` {
		t.Fatalf("PID 复用时必须刷新到新实例路径: %+v", res)
	}
}

// 实例校验始终失败：必须显式降级并记录，不能静默采用陈旧路径。
func TestLookupNameStaleInstanceDowngradesWithLog(t *testing.T) {
	src := &mutableSource{}
	tr, logs := newTestTracker(src)
	tr.SetVerifyFunc(func(ProcInfo) bool { return false })

	src.set(ProcInfo{PID: 100, Name: "app.exe", Path: `C:\a\app.exe`, Created: time.Unix(1000, 0)})
	res := tr.LookupName("app.exe")
	if res.State != LookupAmbiguous {
		t.Fatalf("实例校验失败时必须降级为名称匹配: %+v", res)
	}
	if !hasLog(*logs, "降级为名称匹配") {
		t.Fatalf("降级必须留痕，日志: %v", *logs)
	}
}

// 同名不同路径：两个实例必须判为歧义，不能任选一个生成 PROCESS-PATH 规则。
func TestLookupNameSameNameDifferentPathIsAmbiguous(t *testing.T) {
	src := &mutableSource{}
	tr, _ := newTestTracker(src)
	src.set(
		ProcInfo{PID: 100, Name: "chrome.exe", Path: `C:\A\chrome.exe`, Created: time.Unix(1000, 0)},
		ProcInfo{PID: 200, Name: "chrome.exe", Path: `C:\B\chrome.exe`, Created: time.Unix(1000, 0)},
	)
	res := tr.LookupName("chrome.exe")
	if res.State != LookupAmbiguous || res.Candidates != 2 {
		t.Fatalf("同名不同路径必须判为歧义: %+v", res)
	}
}

// 快照刷新失败：必须记录并沿用上一份快照，不能静默。
func TestRefreshFailureIsLogged(t *testing.T) {
	src := &mutableSource{}
	tr, logs := newTestTracker(src)
	src.set(ProcInfo{PID: 100, Name: "app.exe", Path: `C:\a\app.exe`, Created: time.Unix(1000, 0)})
	if res := tr.LookupName("app.exe"); res.State != LookupUnique {
		t.Fatalf("预热快照失败: %+v", res)
	}

	src.mu.Lock()
	src.err = errors.New("access denied")
	src.mu.Unlock()

	if err := tr.Refresh(); err == nil {
		t.Fatal("刷新失败必须返回错误")
	}
	if !hasLog(*logs, "进程快照刷新失败") {
		t.Fatalf("刷新失败必须留痕，日志: %v", *logs)
	}
}
