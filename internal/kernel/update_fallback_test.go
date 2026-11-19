package kernel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withEndpoints 临时替换入口列表，用例结束后恢复。
func withEndpoints(t *testing.T, eps []releaseEndpoint) {
	t.Helper()
	old := repoAPIEndpoints
	repoAPIEndpoints = eps
	t.Cleanup(func() { repoAPIEndpoints = old })
}

// releaseJSON 生成一份最小可用的 release 响应。
// Assets 在 update.go 里是匿名结构，测试直接用 map 拼 JSON 更省事。
func releaseJSON(tag string) []byte {
	name := "mihomo-windows-amd64-compatible-" + tag + ".zip"
	b, _ := json.Marshal(map[string]any{
		"tag_name": tag,
		"assets": []map[string]any{{
			"name":                 name,
			"browser_download_url": "https://github.com/MetaCubeX/mihomo/releases/download/" + tag + "/" + name,
			"digest":               "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			"size":                 1024,
		}},
	})
	return b
}

// 官方入口被限流（403）时必须自动切到备用入口，而不是直接把 403 甩给用户。
func TestFetchLatestFallsBackWhenPrimaryRateLimited(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer primary.Close()

	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(releaseJSON("v9.9.9"))
	}))
	defer backup.Close()

	withEndpoints(t, []releaseEndpoint{
		{label: "GitHub", url: primary.URL},
		{label: "镜像", url: backup.URL},
	})

	rel, err := fetchLatest(5 * time.Second)
	if err != nil {
		t.Fatalf("备用入口可用时不应报错: %v", err)
	}
	if rel.TagName != "v9.9.9" {
		t.Fatalf("应当返回备用入口的数据，实际 tag=%q", rel.TagName)
	}
}

// 入口全挂时，错误信息要能让用户看懂发生了什么（含限流字样），不能只给一个空洞的失败。
func TestFetchLatestAllEndpointsFailReportsReason(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer primary.Close()

	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer backup.Close()

	withEndpoints(t, []releaseEndpoint{
		{label: "GitHub", url: primary.URL},
		{label: "镜像", url: backup.URL},
	})

	_, err := fetchLatest(5 * time.Second)
	if err == nil {
		t.Fatal("全部入口失败时应当报错")
	}
	msg := err.Error()
	for _, want := range []string{"GitHub", "镜像", "限流", "502"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息缺少 %q: %s", want, msg)
		}
	}
}

// 拿到备用入口的数据后，摘要与大小仍要如实透出——升级流程靠它做完整性校验。
func TestFetchLatestKeepsDigestFromFallback(t *testing.T) {
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(releaseJSON("v9.9.9"))
	}))
	defer backup.Close()

	withEndpoints(t, []releaseEndpoint{{label: "镜像", url: backup.URL}})

	info, err := CheckUpdate("v1.19.30", 5*time.Second)
	if err != nil {
		t.Fatalf("CheckUpdate 失败: %v", err)
	}
	if !info.HasUpdate {
		t.Fatal("v9.9.9 相对 v1.19.30 应当判定为有新版本")
	}
	if info.Digest == "" {
		t.Fatal("摘要不能丢，否则下载环节会拒绝升级")
	}
	if info.Size <= 0 {
		t.Fatal("大小不能丢")
	}
}
