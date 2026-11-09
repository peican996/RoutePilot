//go:build realkernel

package kernel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRealKernelSmartRoutingRulesStart 验证内置国内外分流规则能被真实 mihomo
// 接受：GEOSITE/GEOIP 数据来自仓库内置数据库，规则存在时内核必须正常就绪。
func TestRealKernelSmartRoutingRulesStart(t *testing.T) {
	kernelExe, err := filepath.Abs(filepath.Join("..", "..", "assets", "mihomo.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kernelExe); err != nil {
		t.Skipf("未找到内核二进制: %v", err)
	}
	geoDir := filepath.Join("..", "..", "assets", "geo")
	for _, name := range []string{"geosite.dat", "geoip.metadb"} {
		if _, err := os.Stat(filepath.Join(geoDir, name)); err != nil {
			t.Skipf("缺少 geo 数据 %s: %v", name, err)
		}
	}

	dir := t.TempDir()
	for _, name := range []string{"geosite.dat", "geoip.metadb"} {
		data, err := os.ReadFile(filepath.Join(geoDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	apiPort := mustFreePort(t)
	mixedPort := mustFreePort(t)
	conf := fmt.Sprintf(`mixed-port: %d
external-controller: 127.0.0.1:%d
secret: test-secret
find-process-mode: always
mode: rule
log-level: warning
proxies: []
proxy-groups:
  - name: PROXY
    type: select
    proxies: [DIRECT]
rules:
  - GEOSITE,cn,DIRECT
  - GEOIP,CN,DIRECT
  - MATCH,PROXY
`, mixedPort, apiPort)
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
		t.Fatalf("带 GEOSITE/GEOIP 规则的内核启动失败: %v", err)
	}
	defer func() { _ = m.Close() }()
	if !m.IsRunning() {
		t.Fatal("内核应处于运行状态")
	}
}
