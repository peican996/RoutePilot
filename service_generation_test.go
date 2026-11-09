package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"routepilot/internal/kernel"
)

// 旧代际被取消后，晚到的任务不能再被判定为当前实例。
func TestGenerationInvalidation(t *testing.T) {
	svc := NewCoreService()
	gen1, ctx1 := svc.beginKernelGeneration()
	if !svc.generationCurrent(gen1, ctx1) {
		t.Fatal("新代际应有效")
	}
	svc.invalidateKernelGeneration()
	if svc.generationCurrent(gen1, ctx1) {
		t.Fatal("invalidate 后旧代际必须失效")
	}
	gen2, ctx2 := svc.beginKernelGeneration()
	if gen2 == gen1 || !svc.generationCurrent(gen2, ctx2) {
		t.Fatalf("应生成新的有效代际：old=%d new=%d", gen1, gen2)
	}
	svc.invalidateKernelGeneration()
}

// 旧 afterStart 延迟返回时不能修改新实例状态。
func TestStaleAfterTaskCannotMutateNewGeneration(t *testing.T) {
	svc := NewCoreService()
	gen1, ctx1 := svc.beginKernelGeneration()
	release := make(chan struct{})
	var mutated int32

	svc.spawnAfterTask(gen1, ctx1, func(context.Context) {
		<-release
		if svc.generationCurrent(gen1, ctx1) {
			atomic.AddInt32(&mutated, 1)
		}
	})
	// 模拟新内核启动。
	svc.invalidateKernelGeneration()
	_, _ = svc.beginKernelGeneration()
	close(release)
	svc.waitAfterTasks(false)

	if atomic.LoadInt32(&mutated) != 0 {
		t.Fatal("旧任务在代际失效后仍修改了状态")
	}
}

// 旧 afterStart 在 /version 成功后延迟返回，此时新内核已启动；
// 它不能再读取端口、修改 mixedPort 或系统代理。
func TestStaleAfterStartCannotReadNewInstance(t *testing.T) {
	var configs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			_, _ = w.Write([]byte(`{"version":"v1"}`))
		case "/configs":
			atomic.AddInt32(&configs, 1)
			_, _ = w.Write([]byte(`{"mixed-port":19999,"mode":"rule"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	svc := NewCoreService()
	svc.api = kernel.NewClient(srv.Listener.Addr().String(), "")
	svc.mixedPort = 17890

	ready := make(chan struct{})
	release := make(chan struct{})
	svc.afterVersionHook = func() {
		close(ready)
		<-release
	}

	gen1, ctx1 := svc.beginKernelGeneration()
	svc.afterStart(gen1, ctx1)
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("afterStart 未到达 /version 成功点")
	}

	// 模拟新内核启动，旧代际立即失效。
	gen2, ctx2 := svc.beginKernelGeneration()
	close(release)
	svc.waitAfterTasks(false)

	if got := atomic.LoadInt32(&configs); got != 0 {
		t.Fatalf("旧任务在失效后仍读取了新实例配置：%d 次", got)
	}
	svc.mu.RLock()
	port := svc.mixedPort
	svc.mu.RUnlock()
	if port != 17890 {
		t.Fatalf("旧任务修改了端口：%d", port)
	}

	svc.invalidateKernelGeneration()
	if svc.generationCurrent(gen2, ctx2) {
		t.Fatal("清理后代际应失效")
	}
}

// Shutdown 必须等待已进入执行阶段的任务结束，并拒绝新任务。
func TestShutdownWaitsForAfterTasksAndClosesGate(t *testing.T) {
	oldDir := dataDirOverride
	dataDirOverride = t.TempDir()
	t.Cleanup(func() { dataDirOverride = oldDir })

	svc := NewCoreService()
	gen, ctx := svc.beginKernelGeneration()
	started := make(chan struct{})
	finished := make(chan struct{})

	svc.spawnAfterTask(gen, ctx, func(c context.Context) {
		close(started)
		<-c.Done()
		close(finished)
	})
	<-started

	shutdownDone := make(chan struct{})
	go func() {
		svc.Shutdown()
		close(shutdownDone)
	}()

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown 未取消 afterStart 任务")
	}
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown 未等待 afterStart 任务结束")
	}

	ran := make(chan struct{}, 1)
	svc.spawnAfterTask(999, context.Background(), func(context.Context) { ran <- struct{}{} })
	select {
	case <-ran:
		t.Fatal("Shutdown 后仍接受了新的 afterStart 任务")
	default:
	}
}
