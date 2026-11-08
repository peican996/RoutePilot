package kernel

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"testing"
)

// 官方 release 包里的 exe 叫 mihomo-windows-amd64-compatible.exe，
// 不是固定的 mihomo.exe；写死文件名会让内核升级永远失败。
func TestExtractMihomoFromReleaseZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("mihomo-windows-amd64-compatible.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("fake-binary")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := extractMihomo(buf.Bytes())
	if err != nil {
		t.Fatalf("解包失败: %v", err)
	}
	if string(got) != "fake-binary" {
		t.Fatalf("解出的内容不符: %q", got)
	}
}

func TestExtractMihomoRejectsZipWithoutExe(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if _, err := zw.Create("readme.txt"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := extractMihomo(buf.Bytes()); err == nil {
		t.Fatal("没有 exe 的压缩包应当报错")
	}
}

func TestPickWindowsAssetPrefersCompatible(t *testing.T) {
	rel := &release{}
	err := json.Unmarshal([]byte(`{
		"assets": [
			{"name": "mihomo-windows-amd64-v2-go124-v1.19.33.zip", "browser_download_url": "https://example.com/v2.zip"},
			{"name": "mihomo-windows-amd64-compatible-v1.19.33.zip", "browser_download_url": "https://example.com/compatible.zip"},
			{"name": "mihomo-windows-amd64-v1.19.33.zip", "browser_download_url": "https://example.com/std.zip"}
		]
	}`), rel)
	if err != nil {
		t.Fatal(err)
	}

	got := pickWindowsAsset(rel)
	if got.name != "mihomo-windows-amd64-compatible-v1.19.33.zip" {
		t.Fatalf("应优先 compatible 变体，实际 %q", got.name)
	}
	if got.url != "https://example.com/compatible.zip" {
		t.Fatalf("下载地址不符: %q", got.url)
	}
}

// 没有 compatible 时也不能选 v2/v3 变体，否则老 CPU 升级后起不来。
func TestPickWindowsAssetSkipsV2V3(t *testing.T) {
	rel := &release{}
	err := json.Unmarshal([]byte(`{
		"assets": [
			{"name": "mihomo-windows-amd64-v2-go124-v1.19.33.zip", "browser_download_url": "https://example.com/v2.zip"},
			{"name": "mihomo-windows-amd64-v1.19.33.zip", "browser_download_url": "https://example.com/std.zip"}
		]
	}`), rel)
	if err != nil {
		t.Fatal(err)
	}

	got := pickWindowsAsset(rel)
	if got.name != "mihomo-windows-amd64-v1.19.33.zip" {
		t.Fatalf("应选标准 amd64 包，实际 %q", got.name)
	}
}
