package subscription

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// maxSubscriptionBytes 限制单次订阅响应体大小。正常订阅远小于此值，
	// 超大响应既可能是异常服务端，也可能是把客户端内存打满的攻击。
	maxSubscriptionBytes int64 = 8 << 20
	// maxRedirects 是允许跟随的重定向次数。
	maxRedirects = 5
)

var (
	// ErrBlockedAddress 表示订阅地址或重定向目标指向回环/内网/链路本地等
	// 不允许的特殊地址。用 errors.Is 判断，便于测试与上层展示。
	ErrBlockedAddress   = errors.New("订阅地址指向受限网络地址")
	ErrTooManyRedirects = errors.New("订阅重定向次数过多")
	ErrResponseTooLarge = errors.New("订阅响应体超过大小限制")
)

// fetchDeps 把网络依赖抽出来，测试可以注入解析结果与底层拨号，
// 生产环境使用真实 DNS 与 net.Dialer。
type fetchDeps struct {
	resolve func(ctx context.Context, host string) ([]net.IP, error)
	dialTCP func(ctx context.Context, network, addr string) (net.Conn, error)
	timeout time.Duration
}

func defaultFetchDeps() fetchDeps {
	return fetchDeps{
		resolve: func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			out := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				out = append(out, a.IP)
			}
			return out, nil
		},
		dialTCP: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		timeout: 45 * time.Second,
	}
}

// blockedCIDRs 是 net.IP 自带判断覆盖不到、但同样不应被订阅访问的网段。
// 包括 CGNAT、文档/基准网段、保留段，以及 IPv6 转换/文档/丢弃网段。
var blockedCIDRs = mustParseCIDRs(
	"0.0.0.0/8",
	"100.64.0.0/10",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"240.0.0.0/4",
	"255.255.255.255/32",
	"64:ff9b::/96",
	"100::/64",
	"2001:2::/48",
	"2001:db8::/32",
)

func mustParseCIDRs(list ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(list))
	for _, s := range list {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic("invalid CIDR in blockedCIDRs: " + s)
		}
		out = append(out, n)
	}
	return out
}

// validateSubscriptionURL 校验订阅 URL 的协议与主机字面量。
//
// 域名是否解析到内网由实际拨号阶段校验；这里先拦住明显的
// localhost / 回环 IP / 非法协议 / 数字形式主机，能更早给出清晰错误。
func validateSubscriptionURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("订阅地址无法解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("只允许 http/https 订阅地址: %q", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("订阅地址缺少主机名")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, fmt.Errorf("%w: localhost", ErrBlockedAddress)
	}
	if strings.Contains(host, "%") {
		// IPv6 zone（如 fe80::1%25eth0）不应出现在公网订阅里。
		return nil, fmt.Errorf("%w: 带 zone 的 IPv6 地址", ErrBlockedAddress)
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := checkPublicIP(ip); err != nil {
			return nil, err
		}
	} else if isNumericHost(host) {
		// 纯数字/十六进制形式可能被系统解析成 IP（如 2130706433），
		// 交给 DNS 之前直接拒绝，避免绕过字符串层面的限制。
		return nil, fmt.Errorf("%w: 数字形式主机名 %s", ErrBlockedAddress, host)
	}
	return u, nil
}

// checkPublicIP 判断一个 IP 是否允许作为订阅目标。
func checkPublicIP(ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("%w: 空 IP", ErrBlockedAddress)
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if !ip.IsGlobalUnicast() ||
		ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, ip)
	}
	for _, n := range blockedCIDRs {
		if n.Contains(ip) {
			return fmt.Errorf("%w: %s", ErrBlockedAddress, ip)
		}
	}
	return nil
}

// resolvePublicIPs 解析主机并校验全部结果。任何一个结果落在受限网段
// 都整体拒绝：混合解析（公网 + 内网）不能只挑公网 IP 用，否则 DNS
// 重绑定仍有机会把连接引到内网。
func resolvePublicIPs(ctx context.Context, host string, deps fetchDeps) ([]net.IP, error) {
	host = strings.TrimSuffix(host, ".")
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		if err := checkPublicIP(ip); err != nil {
			return nil, err
		}
		return []net.IP{ip}, nil
	}
	if isNumericHost(host) {
		return nil, fmt.Errorf("%w: 数字形式主机名 %s", ErrBlockedAddress, host)
	}
	ips, err := deps.resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("解析订阅域名 %s 失败: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("订阅域名 %s 没有解析结果", host)
	}
	for _, ip := range ips {
		if err := checkPublicIP(ip); err != nil {
			return nil, err
		}
	}
	return ips, nil
}

// guardedDialContext 返回一个先解析并校验、再按校验结果拨号的 DialContext。
// 关键点：连接目标使用刚刚校验过的 IP，而不是把域名交给系统再解析一次，
// 从根上消除 DNS Rebinding 的时间窗。
func guardedDialContext(deps fetchDeps) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if deps.resolve == nil {
		deps.resolve = defaultFetchDeps().resolve
	}
	if deps.dialTCP == nil {
		deps.dialTCP = defaultFetchDeps().dialTCP
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("订阅连接地址无效: %w", err)
		}
		ips, err := resolvePublicIPs(ctx, host, deps)
		if err != nil {
			return nil, err
		}

		var errs []error
		for _, ip := range ips {
			conn, dialErr := deps.dialTCP(ctx, "tcp", net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			errs = append(errs, fmt.Errorf("%s: %w", ip, dialErr))
		}
		return nil, fmt.Errorf("连接订阅地址失败: %w", errors.Join(errs...))
	}
}

// newGuardedClient 构造带 SSRF 防护的 HTTP 客户端。
//
// insecure 只能由用户为单个订阅显式开启；这里不会自动降级 TLS。
func newGuardedClient(insecure bool, deps fetchDeps) *http.Client {
	if deps.timeout <= 0 {
		deps.timeout = defaultFetchDeps().timeout
	}
	tr := &http.Transport{
		Proxy:                 nil, // 订阅请求不走系统代理，避免被本机代理配置劫持或循环
		DialContext:           guardedDialContext(deps),
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Timeout:   deps.timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("%w（超过 %d 次）", ErrTooManyRedirects, maxRedirects)
			}
			// 每一次重定向都重新做完整 URL 校验；域名/IP 还会在拨号时
			// 重新解析并校验，双重防护。
			if _, err := validateSubscriptionURL(req.URL.String()); err != nil {
				return fmt.Errorf("重定向目标被拒绝: %w", err)
			}
			return nil
		},
	}
}

func isNumericHost(host string) bool {
	host = strings.ToLower(host)
	if host == "" {
		return false
	}
	// 0x 开头的十六进制形式，以及纯数字/点分十进制形式，
	// 都可能被系统当成 IP 解析，必须在进入 DNS 前拒绝。
	if strings.HasPrefix(host, "0x") {
		return true
	}
	for _, r := range host {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
