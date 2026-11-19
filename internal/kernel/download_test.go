package kernel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 直连 GitHub 在国内常常 0 字节卡死，必须先试镜像，否则每次升级都白等一轮超时。
func TestDownloadCandidatesPrefersMirror(t *testing.T) {
	raw := "https://github.com/MetaCubeX/mihomo/releases/download/v1.19.34/x.zip"
	got := downloadCandidates(raw)
	if len(got) != 2 {
		t.Fatalf("应当返回 2 个候选，实际 %d", len(got))
	}
	if !strings.HasPrefix(got[0], "https://gh-proxy.com/") {
		t.Fatalf("第一个候选应当是镜像，实际 %s", got[0])
	}
	if !strings.HasSuffix(got[0], raw) {
		t.Fatalf("镜像地址应当保留原始 URL，实际 %s", got[0])
	}
	if got[1] != raw {
		t.Fatalf("第二个候选应当是原始地址，实际 %s", got[1])
	}
}

// 镜像地址不能再被套一层镜像，否则会变成 /gh-proxy.com/https://gh-proxy.com/...
func TestDownloadCandidatesNoDoubleMirror(t *testing.T) {
	raw := "https://gh-proxy.com/https://github.com/a/b.zip"
	got := downloadCandidates(raw)
	if len(got) != 1 || got[0] != raw {
		t.Fatalf("已是镜像地址时不应再套一层，实际 %v", got)
	}
}

// 下载过程中必须回报进度，否则用户面对几分钟的等待只能干瞪眼。
func TestFetchCoreZipReportsProgress(t *testing.T) {
	payload := strings.Repeat("A", 512<<10) // 512 KB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	var calls int
	var lastDone, lastTotal int64
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := fetchCoreZip(ctx, srv.URL, func(done, total int64) {
		calls++
		lastDone, lastTotal = done, total
	})
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if len(data) != len(payload) {
		t.Fatalf("内容长度不符：期望 %d，实际 %d", len(payload), len(data))
	}
	if calls == 0 {
		t.Fatal("必须报告下载进度")
	}
	if lastDone != int64(len(payload)) {
		t.Fatalf("最后一次进度应当是完整长度，实际 %d", lastDone)
	}
	if lastTotal != int64(len(payload)) {
		t.Fatalf("总长度应当来自 Content-Length，实际 %d", lastTotal)
	}
}

// 没有 Content-Length 时也要能正常工作（total 传 0，前端按已下载字节显示）。
func TestFetchCoreZipWithoutContentLength(t *testing.T) {
	payload := strings.Repeat("B", 64<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 先 Flush 强制走 chunked，否则 httptest 会自动补上 Content-Length
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	var sawZeroTotal bool
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := fetchCoreZip(ctx, srv.URL, func(done, total int64) {
		if total == 0 {
			sawZeroTotal = true
		}
	})
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if len(data) != len(payload) {
		t.Fatalf("内容不完整：%d", len(data))
	}
	if !sawZeroTotal {
		t.Fatal("未知总长度时 total 应当为 0")
	}
}
