package subscription

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// clashUA 决定服务端返回什么格式。V2board 系的面板靠 UA 判断，
// 不带这个头会拿到 base64 节点串而不是完整配置。
const clashUA = "clash-verge/v1.7.7"

type NodeInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type Result struct {
	Config    map[string]any `json:"-"`
	Nodes     []NodeInfo     `json:"nodes"`
	Groups    []string       `json:"groups"`
	Rules     int            `json:"rules"`
	FetchedAt int64          `json:"fetchedAt"`
	Insecure  bool           `json:"insecure"`
	Raw       []byte         `json:"-"`

	// 以下来自响应头，是 V2board 系面板的通行做法。
	// 没有这些头也不影响使用，只是界面显示不到流量信息。
	Name      string `json:"name"`      // content-disposition 里的文件名
	Upload    int64  `json:"upload"`    // 已上传字节
	Download  int64  `json:"download"`  // 已下载字节
	Total     int64  `json:"total"`     // 套餐总量，0 表示未知
	Expire    int64  `json:"expire"`    // 到期 unix 秒，0 表示长期有效
	WebPage   string `json:"webPage"`   // 面板地址
	UpdateHrs int    `json:"updateHrs"` // 建议的自动更新间隔（小时）
}

func do(ctx context.Context, u string, insecure bool) ([]byte, http.Header, error) {
	tr := &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure},
		ResponseHeaderTimeout: 20 * time.Second,
	}
	c := &http.Client{Timeout: 45 * time.Second, Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", clashUA)
	req.Header.Set("Accept", "*/*")
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("订阅返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	if len(body) == 0 {
		return nil, nil, fmt.Errorf("订阅返回空内容")
	}
	return body, resp.Header.Clone(), nil
}

// download 拉取订阅内容。
//
// allowInsecure 由调用方显式传入：只有用户为该订阅主动勾选"允许跳过证书校验"
// 时才为 true。不再"正常失败就自动忽略证书重试"——订阅里带着节点凭据，
// 静默降级等于把凭据交给任何能中间人的链路。
func download(ctx context.Context, u string, allowInsecure bool) ([]byte, http.Header, bool, error) {
	body, hdr, err := do(ctx, u, allowInsecure)
	if err == nil {
		return body, hdr, allowInsecure, nil
	}
	if allowInsecure {
		return nil, nil, true, fmt.Errorf("拉取失败（已跳过证书校验）: %w", err)
	}
	if isTLSError(err) {
		return nil, nil, false, fmt.Errorf(
			"HTTPS 证书校验失败，已中止：%w。若这是你自建的面板（自签证书），"+
				"可在订阅卡片上选择「忽略证书校验」后重试", err)
	}
	return nil, nil, false, fmt.Errorf("拉取失败: %w", err)
}

// isTLSError 判断是否证书/握手类错误，用来给出可操作的提示。
func isTLSError(err error) bool {
	if err == nil {
		return false
	}
	var cve *tls.CertificateVerificationError
	if errors.As(err, &cve) {
		return true
	}
	var unknownAuth x509.UnknownAuthorityError
	if errors.As(err, &unknownAuth) {
		return true
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "x509") || strings.Contains(msg, "certificate") ||
		strings.Contains(msg, "tls:")
}

// parseUserInfo 解析 subscription-userinfo 头：
// upload=123; download=456; total=789; expire=1700000000
func parseUserInfo(h string) (up, down, total, expire int64) {
	if h == "" {
		return
	}
	for _, part := range strings.Split(h, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		val := strings.TrimSpace(kv[1])
		if val == "" {
			continue
		}
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "upload":
			up = n
		case "download":
			down = n
		case "total":
			total = n
		case "expire":
			expire = n
		}
	}
	return
}

// parseFilename 从 content-disposition 里取文件名。
// 常见两种写法：filename=xxx 和 filename*=UTF-8”%E9%AD%94%E6%88%92.net
func parseFilename(cd string) string {
	if cd == "" {
		return ""
	}
	lower := strings.ToLower(cd)
	if i := strings.Index(lower, "filename*=utf-8''"); i >= 0 {
		s := cd[i+len("filename*=utf-8''"):]
		if j := strings.IndexAny(s, ";"); j >= 0 {
			s = s[:j]
		}
		if v, err := url.PathUnescape(strings.TrimSpace(s)); err == nil {
			return v
		}
	}
	if i := strings.Index(lower, "filename="); i >= 0 {
		s := cd[i+len("filename="):]
		if j := strings.IndexAny(s, ";"); j >= 0 {
			s = s[:j]
		}
		return strings.Trim(strings.TrimSpace(s), `"`)
	}
	return ""
}

// Fetch 拉取订阅并解析成 Clash 配置结构。
// allowInsecure 由用户针对该订阅显式开启，默认 false。
func Fetch(ctx context.Context, subURL string, allowInsecure bool) (*Result, error) {
	subURL = strings.TrimSpace(subURL)
	if subURL == "" {
		return nil, fmt.Errorf("订阅地址为空")
	}
	if !strings.HasPrefix(subURL, "http://") && !strings.HasPrefix(subURL, "https://") {
		return nil, fmt.Errorf("订阅地址必须以 http:// 或 https:// 开头")
	}

	body, hdr, insecure, err := download(ctx, subURL, allowInsecure)
	if err != nil {
		return nil, err
	}

	var cfg map[string]any
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("订阅内容不是合法的 YAML: %w", err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("订阅内容为空")
	}

	rawProxies, hasProxies := cfg["proxies"]
	if !hasProxies {
		return nil, fmt.Errorf("订阅里没有 proxies 字段，可能是 base64 节点格式，当前只支持 Clash 格式订阅")
	}

	res := &Result{
		Config:    cfg,
		FetchedAt: time.Now().UnixMilli(),
		Insecure:  insecure,
		Raw:       body,
	}

	// 机场元信息
	res.Name = parseFilename(hdr.Get("content-disposition"))
	res.Upload, res.Download, res.Total, res.Expire = parseUserInfo(hdr.Get("subscription-userinfo"))
	res.WebPage = hdr.Get("profile-web-page-url")
	if v := hdr.Get("profile-update-interval"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			res.UpdateHrs = n
		}
	}

	if arr, ok := rawProxies.([]any); ok {
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, _ := m["name"].(string)
			typ, _ := m["type"].(string)
			if name == "" {
				continue
			}
			res.Nodes = append(res.Nodes, NodeInfo{Name: name, Type: typ})
		}
	}

	if arr, ok := cfg["proxy-groups"].([]any); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				if n, ok := m["name"].(string); ok {
					res.Groups = append(res.Groups, n)
				}
			}
		}
	}

	if arr, ok := cfg["rules"].([]any); ok {
		res.Rules = len(arr)
	}

	if len(res.Nodes) == 0 {
		return nil, fmt.Errorf("订阅解析完成，但没有拿到任何节点")
	}
	return res, nil
}
