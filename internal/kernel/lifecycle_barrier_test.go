package kernel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// 用 hook 把 Start 卡在「进程已创建、尚未就绪」的阶段，确定性验证
// Stop 不会误判为已停止，也不会与 Start 交叉产生孤儿进程。
func TestStopDuringStartingWaitsForTransaction(t *testing.T) {
	m := newFakeManager(t, "sleep")
	started := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	m.hook = func(stage string) {
		if stage == "process-started" {
			once.Do(func() { close(started) })
			<-gate
		}
	}

	startDone := make(chan error, 1)
	go func() { startDone <- m.Start("dummy.yaml") }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("Start 未进入 process-started 阶段")
	}
	pid := m.Status().PID
	if pid <= 0 {
		t.Fatal("进程已创建但 Status 没有 PID")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- m.Stop() }()
	select {
	case <-stopDone:
		t.Fatal("Start 仍在执行时 Stop 不应返回")
	case <-time.After(120 * time.Millisecond):
	}

	close(gate)
	if err := <-startDone; err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if m.IsRunning() {
		t.Fatal("Stop 返回后内核仍在运行")
	}
	if processAlive(pid) {
		t.Fatalf("Stop 返回后进程 %d 仍存活", pid)
	}
}

// Start 执行期间调用 Close：Close 必须等待 Start 事务结束，然后停止进程。
func TestCloseDuringStartingLeavesNoProcess(t *testing.T) {
	m := newFakeManager(t, "sleep")
	started := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	m.hook = func(stage string) {
		if stage == "process-started" {
			once.Do(func() { close(started) })
			<-gate
		}
	}

	startDone := make(chan error, 1)
	go func() { startDone <- m.Start("dummy.yaml") }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("Start 未进入 process-started 阶段")
	}
	pid := m.Status().PID

	closeDone := make(chan error, 1)
	go func() { closeDone <- m.Close() }()
	select {
	case <-closeDone:
		t.Fatal("Start 仍在执行时 Close 不应返回")
	case <-time.After(120 * time.Millisecond):
	}

	close(gate)
	if err := <-startDone; err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if processAlive(pid) {
		t.Fatalf("Close 返回后进程 %d 仍存活", pid)
	}
	if err := m.Start("dummy.yaml"); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Close 后 Start 应返回 ErrManagerClosed，实际 %v", err)
	}
}

// Start 正在等待 API 就绪时 Stop 介入：Stop 必须等待启动事务超时清理，
// 不能把“Starting”误判为已停止。
func TestStopWhileWaitingAPIReady(t *testing.T) {
	m := newFakeManager(t, "sleep")
	addr := freeAddr(t)
	m.SetStartupValidation(addr, func(context.Context) error {
		return errProbe
	}, 300*time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(2)
	var startErr, stopErr error
	go func() {
		defer wg.Done()
		startErr = m.Start("dummy.yaml")
	}()
	go func() {
		defer wg.Done()
		stopErr = m.Stop()
	}()
	wg.Wait()

	if !errors.Is(startErr, ErrStartTimeout) {
		t.Fatalf("Start 应因 API 超时失败，实际 %v", startErr)
	}
	if stopErr != nil {
		t.Fatalf("Stop 失败: %v", stopErr)
	}
	if m.IsRunning() {
		t.Fatal("最终不应有内核在运行")
	}
	if st := m.Status(); st.PID != 0 {
		t.Fatalf("启动失败后 PID 应清空，实际 %d", st.PID)
	}
}

// Restart 必须等旧进程真正退出后才创建新进程。
func TestRestartWaitsForOldProcessExit(t *testing.T) {
	m := newFakeManager(t, "sleep")
	if err := m.Start("dummy.yaml"); err != nil {
		t.Fatal(err)
	}
	oldPID := m.Status().PID
	if oldPID <= 0 {
		t.Fatal("旧进程 PID 无效")
	}
	if err := m.Restart("dummy.yaml"); err != nil {
		t.Fatal(err)
	}
	newPID := m.Status().PID
	if newPID <= 0 || newPID == oldPID {
		t.Fatalf("Restart 后 PID 应变化：old=%d new=%d", oldPID, newPID)
	}
	if processAlive(oldPID) {
		t.Fatalf("Restart 返回后旧进程 %d 仍存活", oldPID)
	}
}

// 成功启动的进程必须明确记录已加入 Job Object。
func TestJobAttachedForStartedProcess(t *testing.T) {
	m := newFakeManager(t, "sleep")
	if err := m.Start("dummy.yaml"); err != nil {
		t.Fatal(err)
	}
	if !m.JobAttached() {
		t.Fatal("启动成功的进程应已加入 Job Object")
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
}
