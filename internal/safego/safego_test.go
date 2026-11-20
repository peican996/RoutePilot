package safego

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// 一次性后台任务 panic 时不能让进程退出，并且必须上报。
func TestGoRecoversPanicAndReports(t *testing.T) {
	done := make(chan struct{})
	var gotName string
	var gotErr any
	var gotStack []byte

	Go("订阅更新", func() {
		defer close(done)
		panic("nil map")
	}, func(name string, err any, stack []byte) {
		gotName, gotErr, gotStack = name, err, stack
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("任务没有结束")
	}
	// report 在 protect 里同步调用，done 可能先关；留一点余量
	time.Sleep(50 * time.Millisecond)
	if gotName != "订阅更新" {
		t.Fatalf("上报的任务名不对: %q", gotName)
	}
	if gotErr != "nil map" {
		t.Fatalf("上报的 panic 值不对: %v", gotErr)
	}
	if len(gotStack) == 0 {
		t.Fatal("必须带栈信息，否则没法定位")
	}
}

// Run 是同步的：panic 被吃掉后调用方继续往下走，不中断外层循环。
func TestRunDoesNotPropagatePanic(t *testing.T) {
	var reported int
	continued := false

	Run("流量统计", func() {
		panic("index out of range")
	}, func(string, any, []byte) { reported++ })

	continued = true
	if reported != 1 {
		t.Fatalf("应当上报一次，实际 %d", reported)
	}
	if !continued {
		t.Fatal("panic 不应中断调用方")
	}
}

// 周期性循环里用 Run：某一轮 panic 后，后面的轮次照常执行。
func TestRunKeepsLoopAliveAcrossPanics(t *testing.T) {
	var ran, reported int
	for i := 0; i < 5; i++ {
		n := i
		Run("健康检查", func() {
			ran++
			if n == 2 {
				panic("偶发")
			}
		}, func(string, any, []byte) { reported++ })
	}
	if ran != 5 {
		t.Fatalf("5 轮都必须执行到，实际 %d", ran)
	}
	if reported != 1 {
		t.Fatalf("只有第 3 轮该上报，实际 %d", reported)
	}
}

// 没传 reporter 也不能因为空指针把兜底自己搞崩。
func TestRunWithoutReporterStillRecovers(t *testing.T) {
	Run("无常驻上报", func() { panic("boom") }, nil)
}

// reporter 自己 panic 时不能再把进程带走（否则兜底形同虚设）。
func TestReporterPanicIsContained(t *testing.T) {
	done := make(chan struct{})
	Go("上报器有 bug", func() {
		defer close(done)
		panic("原始错误")
	}, func(string, any, []byte) { panic("上报器也炸了") })

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("上报器 panic 后任务未能结束")
	}
}

// 正常任务不应该触发上报，也不能被额外包装影响返回值语义。
func TestNoReportWhenNoPanic(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	reported := false
	Go("正常任务", func() { defer wg.Done() }, func(string, any, []byte) { reported = true })
	wg.Wait()
	time.Sleep(50 * time.Millisecond)
	if reported {
		t.Fatal("没有 panic 时不应上报")
	}
}

func TestDescribe(t *testing.T) {
	got := Describe("订阅更新", "boom")
	if !strings.Contains(got, "订阅更新") || !strings.Contains(got, "boom") {
		t.Fatalf("描述不完整: %s", got)
	}
}
