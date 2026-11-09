//go:build realkernel

package kernel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealKernelDistinguishesSameNameProcessPaths 用真实 mihomo 验证 C2 的核心
// 验收点：两个目录下同名 EXE 可以通过 PROCESS-PATH 走不同出口，且路径匹配
// 大小写不敏感。
//
// 做法：把系统 curl.exe 复制成两个目录下的 same.exe，按完整路径配置
// A=DIRECT、B=REJECT，分别通过代理发起请求。两者进程名完全相同，只有路径不同。
//
//	go test -tags realkernel -run TestRealKernelDistinguishesSameNameProcessPaths -v ./internal/kernel
func TestRealKernelDistinguishesSameNameProcessPaths(t *testing.T) {
	curlSrc := filepath.Join(os.Getenv("SystemRoot"), "System32", "curl.exe")
	if _, err := os.Stat(curlSrc); err != nil {
		t.Skipf("系统没有 curl.exe，跳过: %v", err)
	}
	kernelExe, err := filepath.Abs(filepath.Join("..", "..", "assets", "mihomo.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kernelExe); err != nil {
		t.Skipf("未找到内核二进制: %v", err)
	}

	dir := t.TempDir()
	dirA := filepath.Join(dir, "A")
	dirB := filepath.Join(dir, "B")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exeA := filepath.Join(dirA, "same.exe")
	exeB := filepath.Join(dirB, "same.exe")
	data, err := os.ReadFile(curlSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{exeA, exeB} {
		if err := os.WriteFile(p, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	apiPort := mustFreePort(t)
	mixedPort := mustFreePort(t)
	// A 的规则故意用全大写路径：验证 Windows 路径大小写不敏感。
	conf := fmt.Sprintf(`mixed-port: %d
external-controller: 127.0.0.1:%d
secret: test-secret
find-process-mode: always
mode: rule
log-level: warning
rules:
  - PROCESS-PATH,%s,DIRECT
  - PROCESS-PATH,%s,REJECT
  - MATCH,REJECT
`, mixedPort, apiPort, strings.ToUpper(exeA), exeB)
	confPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(dir)
	m.exePath = kernelExe
	addr := fmt.Sprintf("127.0.0.1:%d", apiPort)
	m.SetStartupValidation(addr, func(ctx context.Context) error {
		_, err := versionOfAddr(ctx, addr)
		return err
	}, 10*time.Second)
	if err := m.Start(confPath); err != nil {
		t.Fatalf("启动真实内核失败: %v", err)
	}
	defer func() { _ = m.Close() }()

	proxy := fmt.Sprintf("http://127.0.0.1:%d", mixedPort)
	codeA, errA := curlThroughProxy(t, exeA, proxy, srv.URL)
	codeB, errB := curlThroughProxy(t, exeB, proxy, srv.URL)

	if errA != nil || codeA != "200" {
		t.Fatalf("路径 A 应直连成功: code=%s err=%v", codeA, errA)
	}
	if errB == nil && codeB == "200" {
		t.Fatalf("路径 B 应被 REJECT，但请求成功了（同名程序未区分）")
	}
	t.Logf("同名进程已按路径区分: A=%s (direct), B=%s (%v)", codeA, codeB, errB)
}

func curlThroughProxy(t *testing.T, exe, proxy, target string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe,
		"--noproxy", "",
		"--fail",
		"-sS",
		"-o", os.DevNull,
		"-w", "%{http_code}",
		"-x", proxy,
		target,
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func versionOfAddr(ctx context.Context, addr string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/version", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer test-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return "ok", nil
}
