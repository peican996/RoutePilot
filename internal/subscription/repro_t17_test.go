//go:build repro

package subscription

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// T17：订阅地址可以 302 到本机/内网地址。当前实现没有限制重定向目标，
// 预期安全行为是拒绝；测试在修复前应当失败（复现 SSRF 面）。
func TestReproT17RedirectToLoopback(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml")
		_, _ = w.Write([]byte("proxies:\n  - name: LocalLeak\n    type: ss\n    server: 127.0.0.1\n    port: 8388\n    cipher: aes-128-gcm\n    password: x\nproxy-groups:\n  - name: PROXY\n    type: select\n    proxies: [LocalLeak]\nrules:\n  - MATCH,PROXY\n"))
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	res, err := Fetch(context.Background(), redirect.URL, false)
	if err == nil {
		t.Fatalf("订阅被重定向到本机地址并成功解析（SSRF 复现）: nodes=%d", len(res.Nodes))
	}
}
