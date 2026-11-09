package procid

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type mutableSource struct {
	mu    sync.Mutex
	infos []ProcInfo
	err   error
}

func (s *mutableSource) List() ([]ProcInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProcInfo, len(s.infos))
	copy(out, s.infos)
	return out, s.err
}

func (s *mutableSource) set(infos ...ProcInfo) {
	s.mu.Lock()
	s.infos = infos
	s.mu.Unlock()
}

func newTestTracker(src Source) (*Tracker, *[]string) {
	var mu sync.Mutex
	var logs []string
	tr := NewTracker(src, time.Hour)
	tr.SetLogger(func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	})
	return tr, &logs
}

func hasLog(logs []string, substr string) bool {
	for _, l := range logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func TestTrackerResolveUniqueAndAmbiguous(t *testing.T) {
	src := &mutableSource{}
	tr, _ := newTestTracker(src)

	src.set(ProcInfo{PID: 100, Name: "chrome.exe", Path: `C:\A\chrome.exe`, Created: time.Unix(1000, 0)})
	res := tr.LookupName("chrome.exe")
	if res.State != LookupUnique || res.Info.Path != `C:\A\chrome.exe` {
		t.Fatalf("唯一同名进程应解析出路径: %+v", res)
	}

	src.set(
		ProcInfo{PID: 100, Name: "chrome.exe", Path: `C:\A\chrome.exe`, Created: time.Unix(1000, 0)},
		ProcInfo{PID: 101, Name: "chrome.exe", Path: `D:\B\chrome.exe`, Created: time.Unix(1001, 0)},
	)
	tr.Refresh()
	res = tr.LookupName("chrome.exe")
	if res.State != LookupAmbiguous || res.Candidates != 2 {
		t.Fatalf("同名多实例必须判为歧义: %+v", res)
	}

	res = tr.LookupName("notepad.exe")
	if res.State != LookupMissing {
		t.Fatalf("不存在的进程应为 missing: %+v", res)
	}
}

func TestTrackerPIDReuseUpdatesCache(t *testing.T) {
	src := &mutableSource{}
	src.set(ProcInfo{
		PID: 100, Name: "app.exe", Path: `C:\A\app.exe`,
		Created: time.Unix(1000, 0), FileID: "VOL1:0001",
	})
	tr, logs := newTestTracker(src)
	if res := tr.LookupName("app.exe"); res.State != LookupUnique {
		t.Fatalf("首次解析失败: %+v", res)
	}

	// 同一个 PID 被新进程复用：创建时间、路径、文件身份都变了。
	src.set(ProcInfo{
		PID: 100, Name: "app.exe", Path: `C:\B\app.exe`,
		Created: time.Unix(2000, 0), FileID: "VOL1:0002",
	})
	if err := tr.Refresh(); err != nil {
		t.Fatal(err)
	}

	info, ok := tr.LookupPID(100)
	if !ok {
		t.Fatal("PID 复用后仍应能查到新进程")
	}
	if info.Path != `C:\B\app.exe` || !info.Created.Equal(time.Unix(2000, 0)) {
		t.Fatalf("不得沿用旧 PID 的缓存: %+v", info)
	}
	if !hasLog(*logs, "PID 复用") {
		t.Fatalf("PID 复用应写日志: %v", *logs)
	}
}

func TestTrackerFileIdentityChangeLogged(t *testing.T) {
	src := &mutableSource{}
	src.set(ProcInfo{
		PID: 7, Name: "app.exe", Path: `C:\App\app.exe`,
		Created: time.Unix(100, 0), FileID: "VOL:AAAA",
	})
	tr, logs := newTestTracker(src)
	_, _ = tr.LookupPID(7)

	// 程序升级：同一路径、同一 PID 复用，但创建时间与文件索引变化。
	src.set(ProcInfo{
		PID: 7, Name: "app.exe", Path: `C:\App\app.exe`,
		Created: time.Unix(200, 0), FileID: "VOL:BBBB",
	})
	if err := tr.Refresh(); err != nil {
		t.Fatal(err)
	}
	info, _ := tr.LookupPID(7)
	if info.FileID != "VOL:BBBB" {
		t.Fatalf("升级后应返回新的文件身份: %+v", info)
	}
	if !hasLog(*logs, "文件身份变化") {
		t.Fatalf("文件身份变化应写日志: %v", *logs)
	}
}

func TestTrackerCleansExitedPIDs(t *testing.T) {
	src := &mutableSource{}
	src.set(ProcInfo{PID: 10, Name: "a.exe", Path: `C:\a.exe`, Created: time.Unix(1, 0)})
	tr, logs := newTestTracker(src)
	_, _ = tr.LookupPID(10)

	src.set()
	if err := tr.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.LookupPID(10); ok {
		t.Fatal("进程退出后缓存必须清理")
	}
	if !hasLog(*logs, "清理退出的进程缓存") {
		t.Fatalf("应记录缓存清理日志: %v", *logs)
	}
}

func TestTrackerFileIDLazyLookup(t *testing.T) {
	src := &mutableSource{}
	src.set(ProcInfo{PID: 11, Name: "b.exe", Path: `C:\b.exe`, Created: time.Unix(5, 0)})
	tr := NewTracker(src, time.Hour)
	calls := 0
	tr.SetFileIDFunc(func(path string) string {
		calls++
		return "VOL:LAZY"
	})

	res := tr.LookupName("b.exe")
	if res.State != LookupUnique || res.Info.FileID != "VOL:LAZY" {
		t.Fatalf("唯一命中时应补齐文件身份: %+v", res)
	}
	_ = tr.LookupName("b.exe")
	if calls != 1 {
		t.Fatalf("文件身份应缓存，实际读取 %d 次", calls)
	}
}

func TestTrackerTTLRefreshes(t *testing.T) {
	src := &mutableSource{}
	src.set(ProcInfo{PID: 20, Name: "c.exe", Path: `C:\old\c.exe`, Created: time.Unix(1, 0)})
	tr := NewTracker(src, 10*time.Millisecond)

	if res := tr.LookupName("c.exe"); res.Info.Path != `C:\old\c.exe` {
		t.Fatalf("首次解析不符: %+v", res)
	}
	src.set(ProcInfo{PID: 20, Name: "c.exe", Path: `C:\new\c.exe`, Created: time.Unix(2, 0)})
	time.Sleep(30 * time.Millisecond)
	if res := tr.LookupName("c.exe"); res.Info.Path != `C:\new\c.exe` {
		t.Fatalf("TTL 过期后应刷新快照: %+v", res)
	}
}
