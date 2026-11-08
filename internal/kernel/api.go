package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client 是 mihomo 外部控制 API（external-controller）的轻量封装。
type Client struct {
	Base   string
	Secret string
	hc     *http.Client
}

func NewClient(addr, secret string) *Client {
	return &Client{
		Base:   "http://" + addr,
		Secret: secret,
		hc:     &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rdr)
	if err != nil {
		return err
	}
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接内核 API 失败: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("内核 API 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("解析内核响应失败: %w", err)
		}
	}
	return nil
}

// ---------- 版本 ----------

type VersionInfo struct {
	Version string `json:"version"`
	Meta    bool   `json:"meta"`
	Premium bool   `json:"premium"`
}

func (c *Client) Version(ctx context.Context) (*VersionInfo, error) {
	var v VersionInfo
	if err := c.do(ctx, http.MethodGet, "/version", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ---------- 配置 ----------

type Configs struct {
	Port        int    `json:"port"`
	SocksPort   int    `json:"socks-port"`
	MixedPort   int    `json:"mixed-port"`
	Mode        string `json:"mode"`
	LogLevel    string `json:"log-level"`
	AllowLan    bool   `json:"allow-lan"`
	IPv6        bool   `json:"ipv6"`
}

func (c *Client) Configs(ctx context.Context) (*Configs, error) {
	var v Configs
	if err := c.do(ctx, http.MethodGet, "/configs", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// SetMode 在 rule / global / direct 之间切换，不需要重启内核。
func (c *Client) SetMode(ctx context.Context, mode string) error {
	return c.do(ctx, http.MethodPatch, "/configs", map[string]any{"mode": mode}, nil)
}

// ---------- 代理与节点 ----------

type Proxy struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Now     string   `json:"now"`
	All     []string `json:"all"`
	History []struct {
		Time  string `json:"time"`
		Delay int    `json:"delay"`
	} `json:"history"`
	UDP       bool `json:"udp"`
	Alive     bool `json:"alive"`
}

type ProxiesResp struct {
	Proxies map[string]Proxy `json:"proxies"`
}

func (c *Client) Proxies(ctx context.Context) (map[string]Proxy, error) {
	var v ProxiesResp
	if err := c.do(ctx, http.MethodGet, "/proxies", nil, &v); err != nil {
		return nil, err
	}
	return v.Proxies, nil
}

// Select 把某个策略组切到指定节点。
func (c *Client) Select(ctx context.Context, group, proxy string) error {
	path := "/proxies/" + url.PathEscape(group)
	return c.do(ctx, http.MethodPut, path, map[string]string{"name": proxy}, nil)
}

// Delay 对单个节点发起延迟测试，返回毫秒。失败返回 -1。
func (c *Client) Delay(ctx context.Context, name, testURL string, timeoutMs int) (int, error) {
	if testURL == "" {
		testURL = "https://www.gstatic.com/generate_204"
	}
	path := fmt.Sprintf("/proxies/%s/delay?timeout=%d&url=%s",
		url.PathEscape(name), timeoutMs, url.QueryEscape(testURL))
	var v struct {
		Delay int `json:"delay"`
	}
	err := c.do(ctx, http.MethodGet, path, nil, &v)
	if err != nil {
		return -1, err
	}
	return v.Delay, nil
}

// ---------- 连接 ----------

type ConnMeta struct {
	Network         string `json:"network"`
	Type            string `json:"type"`
	SourceIP        string `json:"sourceIP"`
	DestinationIP   string `json:"destinationIP"`
	SourcePort      string `json:"sourcePort"`
	DestinationPort string `json:"destinationPort"`
	Host            string `json:"host"`
	DNSMode         string `json:"dnsMode"`
	ProcessPath     string `json:"processPath"`
	Process         string `json:"process"`
	SpecialRules    string `json:"specialRules"`
}

type Conn struct {
	ID          string   `json:"id"`
	Metadata    ConnMeta `json:"metadata"`
	Upload      int64    `json:"upload"`
	Download    int64    `json:"download"`
	Start       string   `json:"start"`
	Chains      []string `json:"chains"`
	Rule        string   `json:"rule"`
	RulePayload string   `json:"rulePayload"`
}

type ConnectionsResp struct {
	DownloadTotal int64  `json:"downloadTotal"`
	UploadTotal   int64  `json:"uploadTotal"`
	Connections   []Conn `json:"connections"`
}

func (c *Client) Connections(ctx context.Context) (*ConnectionsResp, error) {
	var v ConnectionsResp
	if err := c.do(ctx, http.MethodGet, "/connections", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Rule 是一条分流规则。
type Rule struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
	Proxy   string `json:"proxy"`
	Size    int    `json:"size"`
}

// Rules 返回当前生效的全部分流规则。
func (c *Client) Rules(ctx context.Context) ([]Rule, error) {
	var resp struct {
		Rules []Rule `json:"rules"`
	}
	if err := c.do(ctx, http.MethodGet, "/rules", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Rules, nil
}

func (c *Client) CloseConnection(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/connections/"+url.PathEscape(id), nil, nil)
}

func (c *Client) CloseAllConnections(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/connections", nil, nil)
}

// ---------- 规则提供者 ----------

// UpdateRuleProvider 触发规则集重新拉取。这条路径是"改规则不断网"的关键：
// 它只刷新 provider，不重载整个配置，已建立的连接不受影响。
func (c *Client) UpdateRuleProvider(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPut, "/providers/rules/"+url.PathEscape(name), nil, nil)
}
