//go:build repro

package kernel

import (
	"os"
	"sync"
	"testing"
	"time"
)

// TestMain 让同一个测试二进制在子进程模式下变成一个"假内核"：
// 忽略 -d/-f 参数，只保持运行，供生命周期并发测试启动和杀掉。
func TestMain(m *testing.M) {
	if os.Getenv("NEXUSLINK_FAKE_KERNEL") == "1" {
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// reproManager 用独立临时目录。不用 t.TempDir()：被强杀的子进程
// 可能仍短暂持有 kernel.log 句柄，导致清理阶段误报失败。
func reproManager(t *testing.T) *Manager {
	t.Helper()
	dir, err := os.MkdirTemp("", "nx-repro-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	m := NewManager(dir)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m.exePath = exe
	t.Cleanup(func() { _ = m.Stop() })
	return m
}

func withFakeKernel(t *testing.T) {
	t.Helper()
	os.Setenv("NEXUSLINK_FAKE_KERNEL", "1")
	t.Cleanup(func() { os.Unsetenv("NEXUSLINK_FAKE_KERNEL") })
}

// T05：并发 Start。预期最多一个内核进程。
func TestReproT05ConcurrentStart(t *testing.T) {
	withFakeKernel(t)
	m := reproManager(t)

	var mu sync.Mutex
	success := 0
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Start("dummy.yaml"); err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if success > 1 {
		t.Fatalf("FAIL: 并发 Start 成功 %d 次，预期最多 1 次（会启动多个内核）", success)
	}
	if !m.IsRunning() {
		t.Fatal("并发 Start 后应有一个内核在运行")
	}
}

// T06：Start 与 Stop 并发。预期不产生孤儿进程，最终状态一致。
func TestReproT06ConcurrentStartStop(t *testing.T) {
	withFakeKernel(t)
	m := reproManager(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = m.Start("dummy.yaml") }()
	go func() { defer wg.Done(); _ = m.Stop() }()
	wg.Wait()

	// 无论最终是运行还是停止，都不允许 Stop 之后仍残留 cmd 句柄。
	if err := m.Stop(); err != nil {
		t.Fatalf("收尾 Stop 失败: %v", err)
	}
	if m.IsRunning() {
		t.Fatal("FAIL: Stop 返回后仍认为内核在运行")
	}
}

// T07：并发 Restart。预期生命周期串行化，全部成功。
func TestReproT07ConcurrentRestart(t *testing.T) {
	withFakeKernel(t)
	m := reproManager(t)
	if err := m.Start("dummy.yaml"); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
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
		t.Fatalf("FAIL: 并发 Restart 出现 %d 个错误（生命周期未串行化）: %v", len(errs), errs[0])
	}
	if !m.IsRunning() {
		t.Fatal("并发 Restart 后内核应仍在运行")
	}
}

// T08：崩溃回调次数。只验证 Manager 是否把"主动停止"误报为崩溃，
// 上限策略属于 CoreService 看门狗，另见报告中的静态结论。
func TestReproT08ActiveStopNotReportedAsCrash(t *testing.T) {
	withFakeKernel(t)
	m := reproManager(t)
	crashes := 0
	m.OnExit = func(string) { crashes++ }

	if err := m.Start("dummy.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if crashes != 0 {
		t.Fatalf("FAIL: 主动 Stop 被误报为崩溃 %d 次", crashes)
	}
}

// T09：Job Object 分配失败。验证失败路径不会 panic、也不会误报成功。
func TestReproT09JobObjectFailureIsSilent(t *testing.T) {
	// 不存在的 PID：AssignProcessToJobObject 必然失败。
	// 当前实现返回 void 且完全忽略返回值，调用方无法感知未托管。
	attachToJob(0x7FFFFFF0)
	t.Log("attachToJob 对不存在的 PID 静默返回，调用方无法区分已托管/未托管（T09 复现）")
}

// T18：内核替换过程被中断。目标被占用/无法删除时，旧内核已删除、
// 新文件未就位，二进制处于缺失状态。
func TestReproT18ReplaceLeavesNoKernel(t *testing.T) {
	dir, err := os.MkdirTemp("", "nx-t18-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	binDir := dir + string(os.PathSeparator) + "bin"
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := binDir + string(os.PathSeparator) + "mihomo.exe"
	// 目标位置被一个非空目录占住：os.Remove 与 os.Rename 都会失败。
	if err := os.MkdirAll(target+string(os.PathSeparator)+"blocker", 0o755); err != nil {
		t.Fatal(err)
	}

	tmp := target + ".new"
	if err := os.WriteFile(tmp, []byte("new-kernel"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(target)
	renameErr := os.Rename(tmp, target)
	if renameErr == nil {
		t.Skip("当前环境允许替换目录，无法复现")
	}
	if fi, statErr := os.Stat(target); statErr == nil && !fi.IsDir() {
		t.Fatal("预期目标内核文件缺失")
	}
	t.Logf("FAIL: 替换失败后没有可用的内核文件（%v），残留 %s.new", renameErr, target)
}
