package kernel

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 进程在"探测成功"与"提交 Running"之间退出：
// Start 必须返回 ErrProcessExited，且最终状态不能停留在 Running。
//
// 测试用 before-running hook 精确卡住提交点，等子进程真正退出
// （r.exited 关闭）后再放行，不依赖 sleep 或延长超时。
func TestStartRejectsExitBeforeRunningCommit(t *testing.T) {
	m := newFakeManager(t, "crash")

	hookEntered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	m.hook = func(stage string) {
		if stage == "before-running" {
			once.Do(func() { close(hookEntered) })
			<-release
		}
	}

	startDone := make(chan error, 1)
	go func() { startDone <- m.Start("dummy.yaml") }()

	select {
	case <-hookEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("Start 未到达 before-running 阶段")
	}

	m.mu.Lock()
	r := m.run
	m.mu.Unlock()
	if r == nil {
		t.Fatal("before-running 阶段应存在 processRun")
	}

	// 确定性子进程退出：fake kernel 的 "crash" 模式在 100ms 后 os.Exit(3)。
	select {
	case <-r.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("子进程未在预期时间内退出")
	}

	close(release)
	select {
	case err := <-startDone:
		if !errors.Is(err, ErrProcessExited) {
			t.Fatalf("进程已退出时 Start 必须返回 ErrProcessExited，实际 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start 未返回")
	}

	if st := m.Status().State; st == StateRunning {
		t.Fatal("进程已退出却把状态提交为 Running")
	}
	if m.IsRunning() {
		t.Fatal("IsRunning 不应为 true")
	}
}

// 同一场景下再次 Start：不应被上一次已退出的 run 卡住（不能报"已在运行"）。
func TestStartAfterExitBeforeRunningCanRetry(t *testing.T) {
	m := newFakeManager(t, "crash")

	hookEntered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	m.hook = func(stage string) {
		if stage == "before-running" {
			once.Do(func() { close(hookEntered) })
			<-release
		}
	}

	startDone := make(chan error, 1)
	go func() { startDone <- m.Start("dummy.yaml") }()
	select {
	case <-hookEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("Start 未到达 before-running 阶段")
	}
	m.mu.Lock()
	r := m.run
	m.mu.Unlock()
	if r == nil {
		t.Fatal("before-running 阶段应存在 processRun")
	}
	select {
	case <-r.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("子进程未在预期时间内退出")
	}
	close(release)
	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start 未返回")
	}

	// 已退出的 run 必须已被摘掉，否则下一次启动会误报 ErrAlreadyRunning。
	m.mu.Lock()
	run := m.run
	state := m.state
	m.mu.Unlock()
	if run != nil {
		t.Fatal("进程退出后 run 未清空")
	}
	if state == StateRunning {
		t.Fatal("进程退出后状态仍为 Running")
	}

	// 第二次启动会再次遇到同一个立即退出的假内核，但错误不能是
	// "已在运行"——那说明旧 run 没有被正确清理。
	m.hook = nil
	err := m.Start("dummy.yaml")
	if errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("旧 run 未清理，第二次启动被误判为已在运行: %v", err)
	}
	if err == nil {
		// 极少数情况下假内核在提交前尚未退出，Start 可能成功；
		// 只要不是 ErrAlreadyRunning 就说明清理路径正确。
		_ = m.Stop()
	}
}
