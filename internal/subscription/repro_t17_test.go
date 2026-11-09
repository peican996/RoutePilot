//go:build repro

package subscription

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// T17（已修复）：订阅入口/重定向目标不允许指向本机或内网。
// 这里初始地址就是 httptest 的 127.0.0.1，必须被 SSRF 防线直接拒绝。
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
		t.Fatalf("订阅被重定向到本机地址并成功解析（SSRF）: nodes=%d", len(res.Nodes))
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("应以受限地址错误拒绝，实际 %v", err)
	}
}
