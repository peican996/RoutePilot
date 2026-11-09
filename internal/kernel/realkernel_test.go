//go:build realkernel

package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRealKernelLifecycle 用仓库内置的真实 mihomo 验证 C1 的启动/就绪/停止链路：
// 进程启动 → 控制 API 真正可访问 → Running → Stop 后进程与端口都释放。
//
// 需要显式开启：
//
//	go test -tags realkernel -run TestRealKernelLifecycle -v ./internal/kernel
func TestRealKernelLifecycle(t *testing.T) {
	exe, err := filepath.Abs(filepath.Join("..", "..", "assets", "mihomo.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Skipf("未找到内核二进制: %v", err)
	}

	apiPort := mustFreePort(t)
	mixedPort := mustFreePort(t)
	dir := t.TempDir()

	conf := fmt.Sprintf(
		"mixed-port: %d\nexternal-controller: 127.0.0.1:%d\nsecret: test-secret\nmode: rule\nlog-level: warning\n",
		mixedPort, apiPort,
	)
	confPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(dir)
	m.exePath = exe
	addr := fmt.Sprintf("127.0.0.1:%d", apiPort)
	m.SetStartupValidation(addr, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/version", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer test-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		var v struct {
			Version string `json:"version"`
		}
		return json.NewDecoder(resp.Body).Decode(&v)
	}, 10*time.Second)

	if err := m.Start(confPath); err != nil {
		t.Fatalf("真实内核启动失败: %v", err)
	}
	st := m.Status()
	if st.State != StateRunning || st.PID <= 0 {
		t.Fatalf("启动后状态异常: %+v", st)
	}
	t.Logf("真实内核已就绪: pid=%d version=%s", st.PID, versionOf(t, addr))
	pid := st.PID

	if err := m.Stop(); err != nil {
		t.Fatalf("停止真实内核失败: %v", err)
	}
	if processAlive(pid) {
		t.Fatalf("Stop 后进程 %d 仍存活", pid)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Stop 后 API 端口 %s 未释放: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("关闭 Manager 失败: %v", err)
	}
}

func mustFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func versionOf(t *testing.T, addr string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/version", nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	var v struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v.Version
}
