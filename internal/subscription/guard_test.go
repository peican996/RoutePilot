package subscription

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func localPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("监听地址类型异常: %T", srv.Listener.Addr())
	}
	return addr.Port
}

func testDeps(t *testing.T, srv *httptest.Server, ips map[string][]net.IP) fetchDeps {
	t.Helper()
	var dialed int32
	return fetchDeps{
		resolve: func(_ context.Context, host string) ([]net.IP, error) {
			if v, ok := ips[strings.ToLower(host)]; ok {
				return v, nil
			}
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
		dialTCP: func(ctx context.Context, network, _ string) (net.Conn, error) {
			atomic.AddInt32(&dialed, 1)
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
		timeout: 5 * time.Second,
	}
}

func TestSSRFRejectsBlockedLiterals(t *testing.T) {
	cases := []string{
		"http://127.0.0.1:8080/sub",
		"http://127.1.2.3:8080/",
		"http://localhost:8080/",
		"http://localhost.:8080/",
		"http://[::1]:8080/",
		"http://10.0.0.5/",
		"http://172.16.1.1/",
		"http://192.168.1.1/",
		"http://169.254.10.10/",
		"http://100.64.0.1/",
		"http://0.0.0.0/",
		"http://2130706433/",
		"http://0x7f000001/",
		"http://[fe80::1]/",
		"http://[2001:db8::1]/",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			if _, err := validateSubscriptionURL(u); !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("应拒绝受限地址 %s，实际错误 %v", u, err)
			}
		})
	}
}

func TestSSRFRejectsNonHTTPScheme(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/a", "gopher://example.com/"} {
		if _, err := validateSubscriptionURL(u); err == nil {
			t.Fatalf("应拒绝非 http/https 协议: %s", u)
		}
	}
}

func TestSSRFDialRejectsPrivateDNS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	port := localPort(t, srv)
	deps := testDeps(t, srv, map[string][]net.IP{
		"public.test": {net.ParseIP("10.1.2.3")},
	})
	_, _, err := doWithDeps(context.Background(),
		fmt.Sprintf("http://public.test:%d/sub", port), false, deps)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("DNS 解析到私有 IP 应被拒绝，实际 %v", err)
	}
}

func TestSSRFRejectsMixedDNS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	port := localPort(t, srv)
	deps := testDeps(t, srv, map[string][]net.IP{
		"public.test": {
			net.ParseIP("93.184.216.34"),
			net.ParseIP("192.168.1.10"),
		},
	})
	_, _, err := doWithDeps(context.Background(),
		fmt.Sprintf("http://public.test:%d/sub", port), false, deps)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("混合解析（公网+内网）应整体拒绝，实际 %v", err)
	}
}

func TestSSRFAllowsPublicAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("public-ok"))
	}))
	defer srv.Close()

	port := localPort(t, srv)
	deps := testDeps(t, srv, nil)
	body, _, err := doWithDeps(context.Background(),
		fmt.Sprintf("http://public.test:%d/sub", port), false, deps)
	if err != nil {
		t.Fatalf("公网地址应放行: %v", err)
	}
	if string(body) != "public-ok" {
		t.Fatalf("响应不符: %q", body)
	}
}

func TestSSRFBlocksRedirectToLoopback(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, fmt.Sprintf("http://127.0.0.1:%d/final", localPort(t, srv)), http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("should-not-reach"))
	}))
	defer srv.Close()

	port := localPort(t, srv)
	deps := testDeps(t, srv, nil)
	_, _, err := doWithDeps(context.Background(),
		fmt.Sprintf("http://public.test:%d/start", port), false, deps)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("重定向到 localhost 应被拒绝，实际 %v", err)
	}
}

func TestSSRFBlocksRedirectToPrivateIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.5/secret", http.StatusFound)
	}))
	defer srv.Close()

	port := localPort(t, srv)
	deps := testDeps(t, srv, nil)
	_, _, err := doWithDeps(context.Background(),
		fmt.Sprintf("http://public.test:%d/start", port), false, deps)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("重定向到私有 IP 应被拒绝，实际 %v", err)
	}
}

func TestSSRFBlocksRedirectToPrivateDNS(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, fmt.Sprintf("http://evil.test:%d/final", localPort(t, srv)), http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("bad"))
	}))
	defer srv.Close()

	port := localPort(t, srv)
	deps := testDeps(t, srv, map[string][]net.IP{
		"evil.test": {net.ParseIP("192.168.0.9")},
	})
	_, _, err := doWithDeps(context.Background(),
		fmt.Sprintf("http://public.test:%d/start", port), false, deps)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("重定向到解析为内网的域名应被拒绝，实际 %v", err)
	}
}

func TestSSRFRedirectLimit(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, _ = fmt.Sscanf(r.URL.Path, "/%d", &n)
		http.Redirect(w, r, fmt.Sprintf("http://public.test:%d/%d", localPort(t, srv), n+1), http.StatusFound)
	}))
	defer srv.Close()

	port := localPort(t, srv)
	deps := testDeps(t, srv, nil)
	_, _, err := doWithDeps(context.Background(),
		fmt.Sprintf("http://public.test:%d/0", port), false, deps)
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("超过重定向次数应报错，实际 %v", err)
	}
}

func TestSSRFLargeResponseRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(make([]byte, maxSubscriptionBytes+1))
	}))
	defer srv.Close()

	port := localPort(t, srv)
	deps := testDeps(t, srv, nil)
	_, _, err := doWithDeps(context.Background(),
		fmt.Sprintf("http://public.test:%d/big", port), false, deps)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("超大响应应被拒绝，实际 %v", err)
	}
}

func TestSSRFTLSInsecureExplicitOnly(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tls-ok"))
	}))
	defer srv.Close()

	port := localPort(t, srv)
	deps := testDeps(t, srv, nil)
	u := fmt.Sprintf("https://public.test:%d/sub", port)

	if _, _, err := doWithDeps(context.Background(), u, false, deps); err == nil {
		t.Fatal("默认必须校验证书，自签证书应失败")
	}
	body, _, err := doWithDeps(context.Background(), u, true, deps)
	if err != nil {
		t.Fatalf("用户显式允许跳过证书校验时应成功: %v", err)
	}
	if string(body) != "tls-ok" {
		t.Fatalf("响应不符: %q", body)
	}
}

func TestSSRFTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	}))
	defer srv.Close()

	port := localPort(t, srv)
	deps := testDeps(t, srv, nil)
	deps.timeout = 100 * time.Millisecond
	_, _, err := doWithDeps(context.Background(),
		fmt.Sprintf("http://public.test:%d/slow", port), false, deps)
	if err == nil {
		t.Fatal("超过客户端超时应报错")
	}
}

func TestCheckPublicIPRanges(t *testing.T) {
	blocked := []string{
		"0.0.0.0", "127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1",
		"169.254.0.1", "100.64.0.1", "224.0.0.1", "255.255.255.255",
		"::1", "fc00::1", "fe80::1", "ff02::1", "2001:db8::1",
	}
	for _, s := range blocked {
		if err := checkPublicIP(net.ParseIP(s)); err == nil {
			t.Fatalf("应判为受限地址: %s", s)
		}
	}
	allowed := []string{"8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if err := checkPublicIP(net.ParseIP(s)); err != nil {
			t.Fatalf("应判为公网地址: %s（%v）", s, err)
		}
	}
}
