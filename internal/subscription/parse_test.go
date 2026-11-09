package subscription

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseClashConfig(t *testing.T) {
	body := []byte(`
proxies:
  - name: a
    type: ss
  - name: b
    type: trojan
proxy-groups:
  - name: PROXY
    type: select
proxy-providers:
  airport:
    type: http
    url: https://example.com/provider
rules:
  - MATCH,PROXY
`)
	res, err := Parse(body, "https://example.com/clash", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != "clash" {
		t.Fatalf("格式应为 clash，实际 %q", res.Format)
	}
	if len(res.Nodes) != 2 || len(res.Groups) != 1 || res.Rules != 1 {
		t.Fatalf("统计不符：nodes=%d groups=%d rules=%d", len(res.Nodes), len(res.Groups), res.Rules)
	}
	if len(res.Providers) != 1 || res.Providers[0] != "airport" {
		t.Fatalf("应记录 Clash provider：%v", res.Providers)
	}
	if res.Config == nil {
		t.Fatal("Clash 订阅应保留原始配置")
	}
}

func TestParseBase64NodeList(t *testing.T) {
	plain := "trojan://pass@example.com:443#Node1\nss://YWVzOnBhc3NAMS4yLjMuNDo0NDM=#Node2"
	body := []byte(base64.StdEncoding.EncodeToString([]byte(plain)))
	const subURL = "https://example.com/base64"

	res, err := Parse(body, subURL, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != "base64" {
		t.Fatalf("格式应为 base64，实际 %q", res.Format)
	}
	if len(res.Nodes) != 2 || len(res.Groups) != 1 {
		t.Fatalf("统计不符：nodes=%d groups=%d", len(res.Nodes), len(res.Groups))
	}
	if res.Nodes[0].Name != "Node1" || res.Nodes[0].Type != "trojan" {
		t.Fatalf("节点元信息不符：%+v", res.Nodes[0])
	}
	if len(res.Providers) != 1 || res.Providers[0] != "routepilot" {
		t.Fatalf("base64 订阅应记录 provider：%v", res.Providers)
	}
	if !strings.Contains(string(res.ProviderContent), "trojan://pass@example.com:443#Node1") {
		t.Fatalf("provider 内容应为解码后的 URI 列表：%q", res.ProviderContent)
	}

	providers, ok := res.Config["proxy-providers"].(map[string]any)
	if !ok {
		t.Fatal("base64 订阅应生成 proxy-providers")
	}
	p, ok := providers["routepilot"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 routepilot provider：%v", providers)
	}
	if p["type"] != "file" {
		t.Fatalf("provider 应为 file 类型，实际 %v", p["type"])
	}
	wantPath := "./" + ProviderFileName(subURL)
	if p["path"] != wantPath {
		t.Fatalf("provider 路径应为 %q，实际 %v", wantPath, p["path"])
	}

	groups, ok := res.Config["proxy-groups"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("proxy-groups 不符：%v", res.Config["proxy-groups"])
	}
	g, _ := groups[0].(map[string]any)
	use, _ := g["use"].([]any)
	if len(use) != 1 || use[0] != "routepilot" {
		t.Fatalf("策略组应引用 provider：%v", g)
	}
}

func TestParsePlainNodeList(t *testing.T) {
	body := []byte("trojan://pass@example.com:443#Node1\nvless://uuid@example.com:443#Node2\n")
	res, err := Parse(body, "https://example.com/plain", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != "base64" || len(res.Nodes) != 2 {
		t.Fatalf("明文节点列表解析失败：format=%q nodes=%d", res.Format, len(res.Nodes))
	}
}

func TestParseRejectsUnknownContent(t *testing.T) {
	if _, err := Parse([]byte("hello, this is not a subscription"), "https://example.com/x", false); err == nil {
		t.Fatal("无法识别的内容应当报错")
	}
}

func TestRawFileNameStable(t *testing.T) {
	a := RawFileName("https://example.com/sub")
	b := RawFileName("https://example.com/sub")
	if a != b || !strings.HasPrefix(a, "sub-") || !strings.HasSuffix(a, ".yaml") {
		t.Fatalf("文件名应符合稳定命名规则：%q / %q", a, b)
	}
	if a == RawFileName("https://example.com/other") {
		t.Fatal("不同订阅地址不应共用文件名")
	}
}
