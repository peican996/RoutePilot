package subscription

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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
	Config map[string]any `json:"-"`
	// Format 是识别出来的订阅格式：clash / clash-base64 / base64。
	Format string `json:"format"`
	// ProviderContent 是 base64 订阅解码后的明文节点列表。
	// 落盘给内核 file provider 用：mihomo 的 base64 解码不接受首尾空白，
	// 直接喂原始 base64 很容易因换行失败，喂解码后的 URI 行最稳。
	ProviderContent []byte     `json:"-"`
	Providers       []string   `json:"providers"`
	Nodes           []NodeInfo `json:"nodes"`
	Groups          []string   `json:"groups"`
	Rules           int        `json:"rules"`
	FetchedAt       int64      `json:"fetchedAt"`
	Insecure        bool       `json:"insecure"`
	Raw             []byte     `json:"-"`

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

// RawFileName 返回订阅原文的稳定文件名。
// 服务层持久化订阅原文、base64 订阅的 file provider 都靠它定位文件，
// 两边必须用同一套命名规则，否则内核会读到一个不存在的文件。
func RawFileName(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return "sub-" + hex.EncodeToString(sum[:8]) + ".yaml"
}

// ProviderFileName 是 base64 订阅解码后给内核 file provider 读取的文件名。
func ProviderFileName(rawURL string) string {
	return strings.TrimSuffix(RawFileName(rawURL), ".yaml") + ".provider.yaml"
}

// Parse 解析订阅原文。支持 Clash YAML，也支持 base64 / 明文的节点 URI 列表。
//
// base64 订阅不自己实现各种协议的 URI 转换：生成一个 file 类型的
// proxy-provider 指向落盘的原文，由内核的 V2Ray 转换器解析。
// 这样 ss / vmess / vless / trojan / hysteria2 等格式都能直接复用内核能力。
func Parse(body []byte, subURL string, allowInsecure bool) (*Result, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, fmt.Errorf("订阅内容为空")
	}

	res := &Result{
		Insecure: allowInsecure,
		Raw:      body,
	}

	// 1. 直接是 Clash YAML
	if cfg, ok := parseClashConfig(body); ok {
		res.Config = cfg
		res.Format = "clash"
		collectClashMeta(res, cfg)
		if len(res.Nodes) == 0 {
			return nil, fmt.Errorf("订阅解析完成，但没有拿到任何节点")
		}
		return res, nil
	}

	// 2. base64 包着 Clash YAML（少数面板会这么返回）
	if decoded, ok := decodeBase64Body(body); ok {
		if cfg, ok := parseClashConfig(decoded); ok {
			res.Config = cfg
			res.Format = "clash-base64"
			collectClashMeta(res, cfg)
			if len(res.Nodes) == 0 {
				return nil, fmt.Errorf("订阅解析完成，但没有拿到任何节点")
			}
			return res, nil
		}
		// 3. base64 节点 URI 列表
		if uris := uriLines(decoded); len(uris) > 0 {
			res.Config = base64ProviderConfig(subURL)
			res.Format = "base64"
			res.ProviderContent = []byte(strings.Join(uris, "\n"))
			res.Nodes = nodesFromURIs(uris)
			res.Groups = []string{"节点选择"}
			res.Providers = []string{"routepilot"}
			return res, nil
		}
	}

	// 4. 明文节点 URI 列表（部分面板直接返回多行链接）
	if uris := uriLines(body); len(uris) > 0 {
		res.Config = base64ProviderConfig(subURL)
		res.Format = "base64"
		res.ProviderContent = []byte(strings.Join(uris, "\n"))
		res.Nodes = nodesFromURIs(uris)
		res.Groups = []string{"节点选择"}
		res.Providers = []string{"routepilot"}
		return res, nil
	}

	return nil, fmt.Errorf("无法识别订阅格式：需要 Clash YAML，或 base64/明文节点列表")
}

// Fetch 拉取订阅并解析。
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

	res, err := Parse(body, subURL, insecure)
	if err != nil {
		return nil, err
	}
	res.FetchedAt = time.Now().UnixMilli()

	// 机场元信息
	res.Name = parseFilename(hdr.Get("content-disposition"))
	res.Upload, res.Download, res.Total, res.Expire = parseUserInfo(hdr.Get("subscription-userinfo"))
	res.WebPage = hdr.Get("profile-web-page-url")
	if v := hdr.Get("profile-update-interval"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			res.UpdateHrs = n
		}
	}
	return res, nil
}

// parseClashConfig 判断并解析 Clash 配置。没有 proxies 字段就不算 Clash，
// 交给 base64 / URI 分支继续识别。
func parseClashConfig(body []byte) (map[string]any, bool) {
	var cfg map[string]any
	if err := yaml.Unmarshal(body, &cfg); err != nil || cfg == nil {
		return nil, false
	}
	rawProxies, ok := cfg["proxies"].([]any)
	if !ok || len(rawProxies) == 0 {
		return nil, false
	}
	return cfg, true
}

// collectClashMeta 统计 Clash 配置里的节点/策略组/规则。
func collectClashMeta(res *Result, cfg map[string]any) {
	if arr, ok := cfg["proxies"].([]any); ok {
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

	// 记录 proxy-providers 名称：进程指定到 provider 节点时，
	// 需要用它生成隐藏 select 组。
	if providers, ok := cfg["proxy-providers"].(map[string]any); ok {
		for name := range providers {
			res.Providers = append(res.Providers, name)
		}
		sort.Strings(res.Providers)
	}
}

// base64ProviderConfig 为节点 URI 列表生成最小可用配置。
// provider 用 file 类型读落盘的解码后 URI 列表（不是原始 base64，
// 避免 mihomo 的 base64 解码被首尾空白卡住），内核自己完成 URI → proxy 转换；
// 选择组显式引用 provider，进程分流与节点切换才有出口可用。
func base64ProviderConfig(subURL string) map[string]any {
	const providerName = "routepilot"
	return map[string]any{
		"proxy-providers": map[string]any{
			providerName: map[string]any{
				"type": "file",
				"path": "./" + ProviderFileName(subURL),
				// 让内核定期测速，provider 节点才有 History 供界面展示初始延迟。
				"health-check": map[string]any{
					"enable":   true,
					"url":      "https://www.gstatic.com/generate_204",
					"interval": 600,
				},
			},
		},
		"proxy-groups": []any{
			map[string]any{
				// 不能叫 PROXY / GLOBAL 这类内置名，否则会被当作内置组跳过。
				"name": "节点选择",
				"type": "select",
				"use":  []any{providerName},
			},
		},
	}
}

// decodeBase64Body 解 base64。订阅里常带换行和空格，先整体去掉。
func decodeBase64Body(body []byte) ([]byte, bool) {
	s := strings.TrimSpace(string(body))
	compact := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if data, err := enc.DecodeString(compact); err == nil && len(data) > 0 {
			return data, true
		}
	}
	return nil, false
}

// uriLines 从文本里挑出节点 URI 行。
func uriLines(body []byte) []string {
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, "://") {
			out = append(out, line)
		}
	}
	return out
}

// nodesFromURIs 构造节点元信息。名称优先取 URI 的 # 片段，
// 取不到就用序号；这只是给订阅卡片展示数量/名称用，真实节点由内核解析。
func nodesFromURIs(uris []string) []NodeInfo {
	out := make([]NodeInfo, 0, len(uris))
	for i, uri := range uris {
		name := fmt.Sprintf("节点 %d", i+1)
		if k := strings.LastIndex(uri, "#"); k >= 0 && k < len(uri)-1 {
			if v, err := url.PathUnescape(uri[k+1:]); err == nil && v != "" {
				name = v
			}
		}
		typ := ""
		if k := strings.Index(uri, "://"); k > 0 {
			typ = uri[:k]
		}
		out = append(out, NodeInfo{Name: name, Type: typ})
	}
	return out
}
