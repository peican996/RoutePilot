package kernel

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// watchdogR1Config 返回适合确定性测试的策略：窗口足够长、
// StableAfter 足够长，避免退避/稳定窗口把测试结果搅乱。
func watchdogR1Config(max int) WatchdogConfig {
	return WatchdogConfig{
		Window:      time.Hour,
		MaxRestarts: max,
		BaseDelay:   5 * time.Millisecond,
		MaxDelay:    20 * time.Millisecond,
		StableAfter: time.Hour,
	}
}

func watchdogCrashes(w *Watchdog) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.crashes)
}

func watchdogHasTimer(w *Watchdog) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.timer != nil
}

// Restart 回调阻塞期间收到新的退出事件：只记账一次，本轮结束后只安排一个重启。
func TestWatchdogR1ReportExitDuringRestart(t *testing.T) {
	calls := make(chan struct{}, 8)
	started := make(chan struct{})
	release := make(chan struct{})
	var first int32

	w := NewWatchdog(WatchdogOptions{
		Config: watchdogR1Config(5),
		Restart: func() error {
			calls <- struct{}{}
			if atomic.CompareAndSwapInt32(&first, 0, 1) {
				close(started)
				<-release
			}
			return nil
		},
	})
	defer w.Stop()

	w.ReportExit(ExitInfo{Reason: "first", At: time.Now()})
	<-started

	// 重启回调仍在执行，新内核又退出（连续两次）。
	w.ReportExit(ExitInfo{Reason: "second", At: time.Now()})
	w.ReportExit(ExitInfo{Reason: "third", At: time.Now()})

	if got := watchdogCrashes(w); got != 3 {
		t.Fatalf("重启期间的退出事件都必须记账，实际 %d 次", got)
	}
	close(release)

	// 第一次 + 合并后的一次，共两次；不能因为连续两条退出排三次重启。
	waitCalls(t, calls, 2)
	select {
	case <-calls:
		t.Fatal("重启期间的事件被重复安排")
	case <-time.After(80 * time.Millisecond):
	}
}

// Restart 返回失败且回调内已触发 OnExit：同一次故障只能计一次。
func TestWatchdogR1RestartFailureAndOnExitDeduplicated(t *testing.T) {
	var calls int32
	exhausted := make(chan string, 1)

	var w *Watchdog
	w = NewWatchdog(WatchdogOptions{
		Config: watchdogR1Config(2),
		Restart: func() error {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				// 模拟新进程刚启动就退出：Manager.OnExit 同步回调。
				w.ReportExit(ExitInfo{Reason: "new process crashed", At: time.Now()})
			}
			return errors.New("restart failed")
		},
		OnExhausted: func(_ int, msg string) { exhausted <- msg },
	})
	defer w.Stop()

	w.ReportExit(ExitInfo{Reason: "boom", At: time.Now()})
	select {
	case <-exhausted:
	case <-time.After(2 * time.Second):
		t.Fatal("持续失败后应熔断")
	}
	// 初始崩溃 1 次 + 重启内 OnExit 1 次 + 第二次重启失败 1 次 = 3。
	// 若未去重会多记一次导致计数为 4。
	if got := watchdogCrashes(w); got != 3 {
		t.Fatalf("同一次故障被重复计数，实际 %d 次", got)
	}
	if !w.Exhausted() {
		t.Fatal("应已熔断")
	}
}

// Stop 与正在执行的重启并发：重启返回后不得再安排下一次，且没有待执行定时器。
func TestWatchdogR1StopDuringRestart(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls int32

	w := NewWatchdog(WatchdogOptions{
		Config: watchdogR1Config(5),
		Restart: func() error {
			atomic.AddInt32(&calls, 1)
			close(started)
			<-release
			return nil
		},
	})

	w.ReportExit(ExitInfo{Reason: "boom", At: time.Now()})
	<-started

	// 重启进行中用户退出。
	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop 被进行中的重启阻塞")
	}
	close(release)

	time.Sleep(80 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("Stop 后仍执行了重启：%d 次", got)
	}
	if watchdogHasTimer(w) {
		t.Fatal("Stop 后不应残留待执行的重启定时器")
	}
}

// Stop 之后即便定时器已经到点，也不允许启动内核。
func TestWatchdogR1StopBeforeTimerFires(t *testing.T) {
	var calls int32
	w := NewWatchdog(WatchdogOptions{
		Config: WatchdogConfig{
			Window:      time.Hour,
			MaxRestarts: 5,
			BaseDelay:   50 * time.Millisecond,
			MaxDelay:    50 * time.Millisecond,
			StableAfter: time.Hour,
		},
		Restart: func() error {
			atomic.AddInt32(&calls, 1)
			return nil
		},
	})

	w.ReportExit(ExitInfo{Reason: "boom", At: time.Now()})
	w.Stop()

	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("Stop 后仍启动了 %d 次内核", got)
	}
	if watchdogHasTimer(w) {
		t.Fatal("Stop 后仍有待执行定时器")
	}
}

// 两个连续崩溃事件与定时器并发：始终只保留一个定时器、只触发一次重启。
func TestWatchdogR1BurstKeepsSingleTimer(t *testing.T) {
	var calls int32
	fired := make(chan struct{}, 8)
	w := NewWatchdog(WatchdogOptions{
		Config: WatchdogConfig{
			Window:      time.Hour,
			MaxRestarts: 5,
			BaseDelay:   80 * time.Millisecond,
			MaxDelay:    time.Second,
			StableAfter: time.Hour,
		},
		Restart: func() error {
			atomic.AddInt32(&calls, 1)
			fired <- struct{}{}
			return nil
		},
	})
	defer w.Stop()

	w.ReportExit(ExitInfo{Reason: "a", At: time.Now()})
	timer1 := watchdogHasTimer(w)
	w.ReportExit(ExitInfo{Reason: "b", At: time.Now()})
	timer2 := watchdogHasTimer(w)
	if !timer1 || !timer2 {
		t.Fatal("崩溃后应存在待执行的重启定时器")
	}

	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("定时器未触发重启")
	}
	time.Sleep(40 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("连续崩溃叠加出 %d 次重启", got)
	}
}

// 重启成功但新内核立即崩溃：必须继续恢复，不能吞掉事件。
func TestWatchdogR1NewProcessCrashAfterSuccessfulRestart(t *testing.T) {
	var calls int32
	second := make(chan struct{}, 1)
	var w *Watchdog
	w = NewWatchdog(WatchdogOptions{
		Config: watchdogR1Config(5),
		Restart: func() error {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				// 第一次重启"成功"后新进程立刻崩溃。
				w.ReportExit(ExitInfo{Reason: "crashed immediately", At: time.Now()})
				second <- struct{}{}
			}
			return nil
		},
	})
	defer w.Stop()

	w.ReportExit(ExitInfo{Reason: "boom", At: time.Now()})
	<-second

	// 新进程的崩溃应安排下一次重启。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&calls) >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("新进程崩溃事件被吞掉，重启次数 %d", atomic.LoadInt32(&calls))
}

// 暂停（用户停止内核）与定时器并发：暂停后不再重启。
func TestWatchdogR1PauseCancelsTimerAndIgnoresExit(t *testing.T) {
	var calls int32
	w := NewWatchdog(WatchdogOptions{
		Config: WatchdogConfig{
			Window:      time.Hour,
			MaxRestarts: 5,
			BaseDelay:   40 * time.Millisecond,
			MaxDelay:    time.Second,
			StableAfter: time.Hour,
		},
		Restart: func() error {
			atomic.AddInt32(&calls, 1)
			return nil
		},
	})
	defer w.Stop()

	w.ReportExit(ExitInfo{Reason: "boom", At: time.Now()})
	w.Pause()
	w.ReportExit(ExitInfo{Reason: "boom again", At: time.Now()})
	time.Sleep(90 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("Pause 后仍重启了 %d 次", got)
	}
	if watchdogHasTimer(w) {
		t.Fatal("Pause 后不应残留定时器")
	}

	// Resume 后必须能重新恢复。
	w.Resume()
	w.ReportExit(ExitInfo{Reason: "boom", At: time.Now()})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&calls) == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Resume 后未恢复自动重启")
}

func waitCalls(t *testing.T, calls <-chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-calls:
		case <-time.After(2 * time.Second):
			t.Fatalf("等待第 %d 次重启超时", i+1)
		}
	}
}
