//go:build realkernel

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"routepilot/internal/kernel"
)

func c4FreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// 用真实 mihomo 验证 generation 目录里的 rule-provider 能被正确加载。
// 执行条件：仓库内存在 assets/mihomo.exe；显式开启 realkernel 标签。
//
//	go test -tags realkernel -run TestRealKernelLoadsProcGenProvider -v .
func TestRealKernelLoadsProcGenProvider(t *testing.T) {
	exePath := filepath.Join("assets", "mihomo.exe")
	if _, err := os.Stat(exePath); err != nil {
		t.Skipf("未找到内核二进制: %v", err)
	}
	exeData, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatal(err)
	}

	old := dataDirOverride
	dataDirOverride = t.TempDir()
	t.Cleanup(func() { dataDirOverride = old })

	svc := NewCoreService()
	rules := map[string]string{"name:curl.exe": "direct"}
	if err := svc.publishProcRules(rules); err != nil {
		t.Fatalf("发布规则失败: %v", err)
	}
	opt := svc.options()
	if !strings.HasPrefix(opt.ProcessDirectPath, "./procgen/") {
		t.Fatalf("provider 路径不是 procgen: %s", opt.ProcessDirectPath)
	}

	mixedPort := c4FreePort(t)
	apiPort := c4FreePort(t)
	conf := fmt.Sprintf(`mixed-port: %d
external-controller: 127.0.0.1:%d
secret: test-secret
mode: rule
log-level: warning
rule-providers:
  pp-direct:
    type: file
    behavior: classical
    path: %s
rules:
  - RULE-SET,pp-direct,DIRECT
  - MATCH,DIRECT
`, mixedPort, apiPort, opt.ProcessDirectPath)
	confPath := filepath.Join(dataDir(), "config.yaml")
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := kernel.NewManager(dataDir())
	if _, err := mgr.EnsureBinary(exeData); err != nil {
		t.Fatalf("释放内核失败: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	addr := fmt.Sprintf("127.0.0.1:%d", apiPort)
	mgr.SetStartupValidation(addr, func(ctx context.Context) error {
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
		return nil
	}, 20*time.Second)

	if err := mgr.Start(confPath); err != nil {
		t.Fatalf("内核未能加载 procgen provider: %v", err)
	}

	// /providers/rules 应能看到 pp-direct，说明文件被真正读取。
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/providers/rules", nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var payload struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("providers 响应无法解析: %v", err)
	}
	if _, ok := payload.Providers["pp-direct"]; !ok {
		t.Fatalf("内核没有加载 pp-direct provider: %s", body)
	}
}
