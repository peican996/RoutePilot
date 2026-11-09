package kernel

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackoffDelay(t *testing.T) {
	cfg := WatchdogConfig{BaseDelay: 2 * time.Second, MaxDelay: 30 * time.Second}
	want := []time.Duration{
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}
	for i, w := range want {
		if got := backoffDelay(cfg, i+1); got != w {
			t.Fatalf("第 %d 次退避应为 %s，实际 %s", i+1, w, got)
		}
	}
}

// 同一故障在退避窗口内连续上报多次退出事件，只能触发一次重启。
func TestWatchdogSingleRestartForBurst(t *testing.T) {
	var restarts int32
	w := NewWatchdog(WatchdogOptions{
		Config: WatchdogConfig{
			Window:      time.Hour,
			MaxRestarts: 5,
			BaseDelay:   80 * time.Millisecond,
			MaxDelay:    time.Second,
			StableAfter: time.Hour,
		},
		Restart: func() error {
			atomic.AddInt32(&restarts, 1)
			return nil
		},
	})
	defer w.Stop()

	for i := 0; i < 5; i++ {
		w.ReportExit(ExitInfo{Reason: "boom", At: time.Now()})
	}
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt32(&restarts); got != 1 {
		t.Fatalf("重复退出事件触发了 %d 次重启，预期 1 次", got)
	}
}

// 用户主动退出（Stop 之后）不得再触发重启。
func TestWatchdogStopsOnUserExit(t *testing.T) {
	var restarts int32
	w := NewWatchdog(WatchdogOptions{
		Config: WatchdogConfig{
			Window:      time.Hour,
			MaxRestarts: 5,
			BaseDelay:   10 * time.Millisecond,
			MaxDelay:    20 * time.Millisecond,
			StableAfter: time.Hour,
		},
		Restart: func() error {
			atomic.AddInt32(&restarts, 1)
			return nil
		},
	})
	w.Stop()
	w.ReportExit(ExitInfo{Reason: "boom", At: time.Now()})
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&restarts); got != 0 {
		t.Fatalf("用户退出后仍触发了 %d 次重启", got)
	}
}

// 连续崩溃达到上限后停止自动重启，并只通知一次。
func TestWatchdogExhaustsAfterMaxRestarts(t *testing.T) {
	restarted := make(chan struct{}, 10)
	exhausted := make(chan string, 1)
	var calls int32

	w := NewWatchdog(WatchdogOptions{
		Config: WatchdogConfig{
			Window:      time.Hour,
			MaxRestarts: 2,
			BaseDelay:   20 * time.Millisecond,
			MaxDelay:    50 * time.Millisecond,
			StableAfter: time.Hour,
		},
		Restart: func() error {
			atomic.AddInt32(&calls, 1)
			restarted <- struct{}{}
			return nil
		},
		OnExhausted: func(_ int, msg string) { exhausted <- msg },
	})
	defer w.Stop()

	for crash := 1; crash <= 3; crash++ {
		w.ReportExit(ExitInfo{Reason: "boom", At: time.Now()})
		if crash <= 2 {
			select {
			case <-restarted:
			case <-time.After(time.Second):
				t.Fatalf("第 %d 次崩溃后未触发重启", crash)
			}
		}
	}

	select {
	case <-exhausted:
	case <-time.After(time.Second):
		t.Fatal("超过上限后未通知停止自动重启")
	}
	if !w.Exhausted() {
		t.Fatal("应处于耗尽状态")
	}
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("超过上限后仍继续重启：调用 %d 次", got)
	}
}

// 重启本身失败也要计入上限，不能无限重试。
func TestWatchdogRestartFailureCountsTowardLimit(t *testing.T) {
	exhausted := make(chan string, 1)
	var calls int32

	w := NewWatchdog(WatchdogOptions{
		Config: WatchdogConfig{
			Window:      time.Hour,
			MaxRestarts: 2,
			BaseDelay:   10 * time.Millisecond,
			MaxDelay:    20 * time.Millisecond,
			StableAfter: time.Hour,
		},
		Restart: func() error {
			atomic.AddInt32(&calls, 1)
			return errors.New("still broken")
		},
		OnExhausted: func(_ int, msg string) { exhausted <- msg },
	})
	defer w.Stop()

	w.ReportExit(ExitInfo{Reason: "boom", At: time.Now()})
	select {
	case <-exhausted:
	case <-time.After(time.Second):
		t.Fatal("重启持续失败后未停止重试")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("重启失败次数与上限不符：实际调用 %d 次，预期 2 次", got)
	}
}

// 长稳运行后的崩溃重新计数：不是启动成功就立刻清零，而是运行足够久才清零。
func TestWatchdogStableRunResetsCounter(t *testing.T) {
	w := NewWatchdog(WatchdogOptions{
		Config: WatchdogConfig{
			Window:      time.Hour,
			MaxRestarts: 5,
			BaseDelay:   20 * time.Millisecond,
			MaxDelay:    time.Second,
			StableAfter: time.Minute,
		},
		Restart: func() error { return nil },
	})
	defer w.Stop()

	w.ReportExit(ExitInfo{Reason: "first", Uptime: 0, At: time.Now()})
	time.Sleep(80 * time.Millisecond)

	w.ReportExit(ExitInfo{Reason: "after stable run", Uptime: 2 * time.Minute, At: time.Now()})
	w.mu.Lock()
	n := len(w.crashes)
	w.mu.Unlock()
	if n != 1 {
		t.Fatalf("长稳运行后应重新计数为 1，实际 %d", n)
	}
}

// 窗口外的旧失败不再计入连续次数。
func TestWatchdogPrunesExpiredFailures(t *testing.T) {
	w := NewWatchdog(WatchdogOptions{
		Config: WatchdogConfig{
			Window:      50 * time.Millisecond,
			MaxRestarts: 5,
			BaseDelay:   time.Second,
			MaxDelay:    time.Second,
			StableAfter: time.Hour,
		},
		Restart: func() error { return nil },
	})
	defer w.Stop()

	w.ReportExit(ExitInfo{Reason: "old", At: time.Now()})
	time.Sleep(120 * time.Millisecond)
	w.ReportExit(ExitInfo{Reason: "new", At: time.Now()})

	w.mu.Lock()
	n := len(w.crashes)
	w.mu.Unlock()
	if n != 1 {
		t.Fatalf("窗口外失败应被清理，实际保留 %d 次", n)
	}
}

// 用户手动停止内核（Pause）后：待执行的重启被取消，后续退出事件也不再恢复。
func TestWatchdogPauseCancelsRecovery(t *testing.T) {
	var restarts int32
	w := NewWatchdog(WatchdogOptions{
		Config: WatchdogConfig{
			Window:      time.Hour,
			MaxRestarts: 5,
			BaseDelay:   50 * time.Millisecond,
			MaxDelay:    time.Second,
			StableAfter: time.Hour,
		},
		Restart: func() error {
			atomic.AddInt32(&restarts, 1)
			return nil
		},
	})
	defer w.Stop()

	w.ReportExit(ExitInfo{Reason: "boom", At: time.Now()})
	w.Pause()
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt32(&restarts); got != 0 {
		t.Fatalf("Pause 后仍执行了 %d 次重启", got)
	}

	// 暂停期间的新退出事件也必须被忽略。
	w.ReportExit(ExitInfo{Reason: "boom again", At: time.Now()})
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&restarts); got != 0 {
		t.Fatalf("暂停期间仍执行了 %d 次重启", got)
	}
}

// 手动启动（Resume）后恢复自动恢复，并重新计数。
func TestWatchdogResumeReenables(t *testing.T) {
	restarted := make(chan struct{}, 1)
	w := NewWatchdog(WatchdogOptions{
		Config: WatchdogConfig{
			Window:      time.Hour,
			MaxRestarts: 5,
			BaseDelay:   20 * time.Millisecond,
			MaxDelay:    time.Second,
			StableAfter: time.Hour,
		},
		Restart: func() error {
			restarted <- struct{}{}
			return nil
		},
	})
	defer w.Stop()

	w.Pause()
	w.Resume()
	w.ReportExit(ExitInfo{Reason: "boom", At: time.Now()})
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("Resume 后未恢复自动重启")
	}
}
