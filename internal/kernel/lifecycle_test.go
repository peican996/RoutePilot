package kernel

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// 测试二进制在以下环境变量下伪装成内核进程。
// init 早于 flag 解析执行，因此不会因为 Manager 传入的 -d/-f 参数报错。
func init() {
	switch os.Getenv("ROUTEPILOT_FAKE_KERNEL") {
	case "":
		return
	case "sleep":
	case "exit":
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	case "crash":
		time.Sleep(100 * time.Millisecond)
		os.Exit(3)
	default:
		return
	}
	if p := os.Getenv("ROUTEPILOT_FAKE_PIDFILE"); p != "" {
		_ = os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o644)
	}
	time.Sleep(10 * time.Minute)
	os.Exit(0)
}

func newFakeManager(t *testing.T, mode string) *Manager {
	t.Helper()
	dir, err := os.MkdirTemp("", "routepilot-kernel-*")
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir)
	m.exePath = exe
	t.Setenv("ROUTEPILOT_FAKE_KERNEL", mode)
	t.Cleanup(func() {
		_ = m.Close()
		_ = os.RemoveAll(dir)
	})
	return m
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitState(t *testing.T, m *Manager, want State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m.Status().State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待状态 %s 超时，当前 %s（%s）", want, m.Status().State, m.Status().Error)
}

// waitPID 等待状态快照里的 PID 就绪。
// Starting 状态与 PID 赋值之间存在极短窗口，直接取样会读到 0
// 并与随后才写入的 PID 比较，造成偶发失败。
func waitPID(t *testing.T, m *Manager, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pid := m.Status().PID; pid > 0 {
			return pid
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 PID 就绪超时，当前状态 %s（%s）", m.Status().State, m.Status().Error)
	return 0
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}

// 100 个 goroutine 同时 Start：只能有一个成功，也只能产生一个受管进程。
func TestConcurrentStartOnlyOneInstance(t *testing.T) {
	m := newFakeManager(t, "sleep")

	const goroutines = 100
	var success int32
	var wg sync.WaitGroup
	var mu sync.Mutex
	var unexpected []error
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := m.Start("dummy.yaml")
			if err == nil {
				atomic.AddInt32(&success, 1)
				return
			}
			if !errors.Is(err, ErrAlreadyRunning) {
				mu.Lock()
				unexpected = append(unexpected, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if success != 1 {
		t.Fatalf("并发 Start 成功 %d 次，预期恰好 1 次", success)
	}
	if len(unexpected) > 0 {
		t.Fatalf("出现非预期错误: %v", unexpected[0])
	}
	if !m.IsRunning() {
		t.Fatalf("并发 Start 后应处于 Running，实际 %s（%s）", m.Status().State, m.Status().Error)
	}
	pid := m.Status().PID
	if pid <= 0 {
		t.Fatal("Running 状态必须带有效 PID")
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if processAlive(pid) {
		t.Fatalf("Stop 返回后进程 %d 仍存活（孤儿进程）", pid)
	}
}

// Starting 期间再次 Start：不得创建第二个进程，返回明确的「已在运行」。
func TestStartWhileStartingRejected(t *testing.T) {
	m := newFakeManager(t, "sleep")
	m.SetStartupValidation("", func(ctx context.Context) error {
		time.Sleep(150 * time.Millisecond)
		return nil
	}, 2*time.Second)

	first := make(chan error, 1)
	go func() { first <- m.Start("dummy.yaml") }()
	waitState(t, m, StateStarting, time.Second)
	pid := waitPID(t, m, time.Second)
	if m.IsRunning() {
		t.Fatal("Starting 状态不应被判定为 Running")
	}

	err := m.Start("dummy.yaml")
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Starting 期间再次 Start 应返回 ErrAlreadyRunning，实际 %v", err)
	}
	if got := m.Status().PID; got != pid {
		t.Fatalf("不应创建新进程：PID %d → %d", pid, got)
	}
	if err := <-first; err != nil {
		t.Fatalf("首次 Start 失败: %v", err)
	}
	if !m.IsRunning() {
		t.Fatal("首次 Start 完成后应处于 Running")
	}
}

// Start 与 Stop 并发：无论谁先获得生命周期锁，最终状态与真实进程一致。
func TestStartStopConcurrentConsistency(t *testing.T) {
	m := newFakeManager(t, "sleep")

	for i := 0; i < 20; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = m.Start("dummy.yaml") }()
		go func() { defer wg.Done(); _ = m.Stop() }()
		wg.Wait()

		st := m.Status()
		if m.IsRunning() != (st.PID > 0) {
			t.Fatalf("第 %d 轮状态不一致：running=%v pid=%d state=%s",
				i, m.IsRunning(), st.PID, st.State)
		}
		if st.State == StateStopping || st.State == StateStarting {
			t.Fatalf("第 %d 轮残留中间态 %s", i, st.State)
		}
		if err := m.Stop(); err != nil {
			t.Fatalf("第 %d 轮收尾 Stop 失败: %v", i, err)
		}
	}
}

// Restart 与 Stop 并发：Restart 是完整事务，不允许出现半停半起。
func TestRestartStopConcurrent(t *testing.T) {
	m := newFakeManager(t, "sleep")
	if err := m.Start("dummy.yaml"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = m.Restart("dummy.yaml") }()
	go func() { defer wg.Done(); _ = m.Stop() }()
	wg.Wait()

	st := m.Status()
	if m.IsRunning() != (st.PID > 0) {
		t.Fatalf("Restart/Stop 并发后状态不一致：running=%v pid=%d state=%s",
			m.IsRunning(), st.PID, st.State)
	}
	if st.State == StateStopping || st.State == StateStarting {
		t.Fatalf("不应残留中间态：%s", st.State)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("收尾 Stop 失败: %v", err)
	}
}

// 多个 Restart 并发：生命周期串行化，最终只应有一个内核在运行。
func TestConcurrentRestartAllSucceed(t *testing.T) {
	m := newFakeManager(t, "sleep")
	if err := m.Start("dummy.yaml"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Restart("dummy.yaml"); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("并发 Restart 出现错误: %v", errs[0])
	}
	if !m.IsRunning() {
		t.Fatal("并发 Restart 后应处于 Running")
	}
}

// 主动 Stop 不得触发崩溃回调。
func TestIntentionalStopNotReported(t *testing.T) {
	m := newFakeManager(t, "sleep")
	var crashes int32
	m.OnExit = func(ExitInfo) { atomic.AddInt32(&crashes, 1) }

	if err := m.Start("dummy.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt32(&crashes); got != 0 {
		t.Fatalf("主动 Stop 被误报为崩溃 %d 次", got)
	}
	if m.Status().State != StateStopped {
		t.Fatalf("Stop 后状态应为 stopped，实际 %s", m.Status().State)
	}
}

// 进程以 0 正常退出：清理状态，但不触发自动恢复回调。
func TestNormalExitNotReported(t *testing.T) {
	m := newFakeManager(t, "exit")
	var crashes int32
	m.OnExit = func(ExitInfo) { atomic.AddInt32(&crashes, 1) }

	if err := m.Start("dummy.yaml"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, StateStopped, 3*time.Second)
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&crashes); got != 0 {
		t.Fatalf("正常退出被误报为崩溃 %d 次", got)
	}
}

// 异常退出：上报退出码与运行时长，状态进入 failed。
func TestUnexpectedExitReportsInfo(t *testing.T) {
	m := newFakeManager(t, "crash")
	infoCh := make(chan ExitInfo, 1)
	m.OnExit = func(info ExitInfo) { infoCh <- info }

	start := time.Now()
	if err := m.Start("dummy.yaml"); err != nil {
		t.Fatal(err)
	}
	select {
	case info := <-infoCh:
		if info.ExitCode != 3 {
			t.Fatalf("退出码应为 3，实际 %d（%s）", info.ExitCode, info.Reason)
		}
		if info.Uptime < 0 || time.Since(start) < time.Duration(0) {
			t.Fatalf("运行时长异常: %s", info.Uptime)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("异常退出未触发 OnExit")
	}
	waitState(t, m, StateFailed, 2*time.Second)
	if m.IsRunning() {
		t.Fatal("异常退出后不应仍为 Running")
	}
}

// 崩溃回调执行期间手动 Restart：回调不得持锁阻塞生命周期操作。
func TestExitCallbackConcurrentWithRestart(t *testing.T) {
	m := newFakeManager(t, "sleep")
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	m.OnExit = func(ExitInfo) {
		entered <- struct{}{}
		<-release
	}

	if err := m.Start("dummy.yaml"); err != nil {
		t.Fatal(err)
	}
	// 模拟外部强制结束，触发「非主动退出」回调。
	m.mu.Lock()
	proc := m.run.cmd.Process
	m.mu.Unlock()
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("异常退出未进入 OnExit")
	}

	// 回调仍阻塞时执行 Restart：如果回调持有 Manager 锁，这里会卡死。
	done := make(chan error, 1)
	go func() { done <- m.Restart("dummy.yaml") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("回调期间 Restart 失败: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnExit 回调阻塞了 Restart（疑似持锁回调）")
	}
	close(release)

	if !m.IsRunning() {
		t.Fatalf("Restart 后应处于 Running，实际 %s", m.Status().State)
	}
}

// 端口被占用：启动前就明确报错，且不产生进程。
func TestStartFailsWhenAPIPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	m := newFakeManager(t, "sleep")
	m.SetStartupValidation(ln.Addr().String(), func(context.Context) error {
		return errors.New("不应被调用")
	}, time.Second)

	start := time.Now()
	err = m.Start("dummy.yaml")
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("应返回 ErrPortInUse，实际 %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("端口冲突应快速失败，实际耗时 %s", time.Since(start))
	}
	st := m.Status()
	if st.State != StateFailed {
		t.Fatalf("启动失败后状态应为 failed，实际 %s", st.State)
	}
	if st.PID != 0 {
		t.Fatalf("端口预检失败不应产生进程，实际 PID %d", st.PID)
	}
}

// 进程活着但 API 一直不响应：启动超时，并清理进程。
func TestStartTimeoutCleansProcess(t *testing.T) {
	pidFile := t.TempDir() + string(os.PathSeparator) + "kernel.pid"
	t.Setenv("ROUTEPILOT_FAKE_PIDFILE", pidFile)

	m := newFakeManager(t, "sleep")
	addr := freeAddr(t)
	m.SetStartupValidation(addr, func(context.Context) error {
		return errors.New("connection refused")
	}, 600*time.Millisecond)

	err := m.Start("dummy.yaml")
	if !errors.Is(err, ErrStartTimeout) {
		t.Fatalf("应返回 ErrStartTimeout，实际 %v", err)
	}
	if m.Status().State != StateFailed {
		t.Fatalf("启动失败后状态应为 failed，实际 %s", m.Status().State)
	}
	pid := readPID(t, pidFile)
	if pid <= 0 {
		t.Fatal("未读到假内核 PID")
	}
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("启动失败后进程 %d 未被清理（孤儿进程）", pid)
	}
}

// 端口有监听但 HTTP 不健康：归类为 API 不可访问，而不是启动超时。
func TestAPIUnreachableClassified(t *testing.T) {
	addr := freeAddr(t)
	lnCh := make(chan net.Listener, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			lnCh <- ln
		}
	}()

	m := newFakeManager(t, "sleep")
	m.SetStartupValidation(addr, func(context.Context) error {
		return errors.New("http 500")
	}, 700*time.Millisecond)

	err := m.Start("dummy.yaml")
	if !errors.Is(err, ErrAPIUnreachable) {
		t.Fatalf("应返回 ErrAPIUnreachable，实际 %v", err)
	}
	select {
	case ln := <-lnCh:
		_ = ln.Close()
	case <-time.After(time.Second):
	}
}

// 进程在启动完成前退出：应立即归类为进程退出，而不是等满超时。
func TestProcessExitDuringStartup(t *testing.T) {
	m := newFakeManager(t, "exit")
	addr := freeAddr(t)
	m.SetStartupValidation(addr, func(context.Context) error {
		return errors.New("connection refused")
	}, 3*time.Second)

	start := time.Now()
	err := m.Start("dummy.yaml")
	if !errors.Is(err, ErrProcessExited) {
		t.Fatalf("应返回 ErrProcessExited，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("进程已退出却等待了 %s，未及时判定失败", elapsed)
	}
	if m.Status().State != StateFailed {
		t.Fatalf("状态应为 failed，实际 %s", m.Status().State)
	}
}

// Job Object 分配失败必须返回错误，不能静默当作已托管。
func TestJobObjectAssignInvalidPID(t *testing.T) {
	job := newJobObject()
	if err := job.assign(0); err == nil {
		t.Fatal("PID 0 应返回错误")
	}
	if err := job.assign(0x7FFFFFF0); err == nil {
		t.Fatal("不存在的 PID 应返回错误")
	}
}

// 关闭后的 Manager 不得再启动内核。
func TestStartAfterCloseRejected(t *testing.T) {
	m := newFakeManager(t, "sleep")
	if err := m.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := m.Start("dummy.yaml"); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Close 后 Start 应返回 ErrManagerClosed，实际 %v", err)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(string(data))
			if convErr == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0
}
