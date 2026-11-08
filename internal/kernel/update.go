// 内核版本更新：从 GitHub 拉取 mihomo 最新 release，下载并替换本地内核。
//
// 为什么需要标记文件：EnsureBinary 每次启动都会拿 embed 的内核和磁盘上的
// 做哈希比对，不一致就覆盖——如果我们手动升级了内核，下次启动会被打回原形。
// 所以升级成功后写一个标记，EnsureBinary 看到标记就保留磁盘版本。

package kernel

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GitHub 上 mihomo 的发布仓库。
const repoAPI = "https://api.github.com/repos/MetaCubeX/mihomo/releases/latest"

// coreCustomMark 是"内核已被手动升级"的标记文件名。
const coreCustomMark = ".core_custom"

type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		// 新版 GitHub release API 会给资产摘要，形如 "sha256:<hex>"。
		// 用于下载后校验，防止被中间人替换成别的二进制。
		Digest string `json:"digest"`
	} `json:"assets"`
}

// LatestInfo 描述可用的内核版本。
type LatestInfo struct {
	Current   string `json:"current"`   // 当前内核版本（v 开头）
	Latest    string `json:"latest"`    // 最新版本
	HasUpdate bool   `json:"hasUpdate"` // 是否有新版
	URL       string `json:"url"`       // 下载地址
	AssetName string `json:"assetName"` // 资源文件名
	Digest    string `json:"digest"`    // "sha256:<hex>"，为空表示该 release 没提供
}

// fetchLatest 查询最新 release。timeout 控制整体超时，国内访问 GitHub 可能很慢。
func fetchLatest(timeout time.Duration) (*release, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, repoAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "NexusLink")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("访问 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub 返回 %d", resp.StatusCode)
	}

	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 release 信息失败: %w", err)
	}
	return &rel, nil
}

// pickWindowsAsset 从 release 资源里挑出 Windows amd64 的压缩包。
type winAsset struct {
	name   string
	url    string
	digest string
}

func pickWindowsAsset(rel *release) winAsset {
	// 内置内核用的是 compatible（GOAMD64=v1）变体，升级也沿用同一变体；
	// 否则老 CPU 用户升级后会直接起不来，和内置版本的选择自相矛盾。
	for _, a := range rel.Assets {
		n := strings.ToLower(a.Name)
		if strings.Contains(n, "windows") && strings.Contains(n, "amd64") &&
			strings.HasSuffix(n, ".zip") && strings.Contains(n, "compatible") {
			return winAsset{a.Name, a.BrowserDownloadURL, a.Digest}
		}
	}
	// 没有 compatible 就优先标准 amd64 包，跳过带 go1xx / v2 / v3 的变体，
	// 它们对 CPU 指令集有额外要求。
	for _, a := range rel.Assets {
		n := strings.ToLower(a.Name)
		if !strings.Contains(n, "windows") || !strings.Contains(n, "amd64") || !strings.HasSuffix(n, ".zip") {
			continue
		}
		if strings.Contains(n, "compatible") || strings.Contains(n, "go1") ||
			strings.Contains(n, "-v2-") || strings.Contains(n, "-v3-") {
			continue
		}
		return winAsset{a.Name, a.BrowserDownloadURL, a.Digest}
	}
	// 最后兜底：任何 Windows zip
	for _, a := range rel.Assets {
		n := strings.ToLower(a.Name)
		if strings.Contains(n, "windows") && strings.HasSuffix(n, ".zip") {
			return winAsset{a.Name, a.BrowserDownloadURL, a.Digest}
		}
	}
	return winAsset{}
}

// CheckUpdate 对比本地版本和最新 release。currentVersion 传 v 开头的版本号。
func CheckUpdate(currentVersion string, timeout time.Duration) (*LatestInfo, error) {
	rel, err := fetchLatest(timeout)
	if err != nil {
		return nil, err
	}
	asset := pickWindowsAsset(rel)
	if asset.url == "" {
		return nil, fmt.Errorf("该 release 没有 Windows 版本")
	}

	cur := strings.TrimPrefix(strings.TrimSpace(currentVersion), "v")
	lat := strings.TrimPrefix(rel.TagName, "v")
	// 读不到当前版本时不能断定"有新版本"，否则永远提示可升级
	hasUpdate := cur != "" && lat != "" && lat != cur
	return &LatestInfo{
		Current:   currentVersion,
		Latest:    rel.TagName,
		HasUpdate: hasUpdate,
		URL:       asset.url,
		AssetName: asset.name,
		Digest:    asset.digest,
	}, nil
}

// IsCustomCore 磁盘上的内核是否被手动升级过。
func IsCustomCore(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, "bin", coreCustomMark))
	return err == nil
}

// markCustom 写入"内核已手动升级"标记。
func markCustom(dataDir string) error {
	p := filepath.Join(dataDir, "bin", coreCustomMark)
	return os.WriteFile(p, []byte(time.Now().Format(time.RFC3339)), 0o644)
}

// ClearCustomMark 清除标记（回退到内置内核时用）。
func ClearCustomMark(dataDir string) {
	_ = os.Remove(filepath.Join(dataDir, "bin", coreCustomMark))
}

// 允许下载内核的域名。GitHub 的 release 资产最终会 302 到
// objects.githubusercontent.com / release-assets.githubusercontent.com。
var allowedDownloadHosts = map[string]bool{
	"github.com":                            true,
	"api.github.com":                        true,
	"objects.githubusercontent.com":         true,
	"release-assets.githubusercontent.com":  true,
	"github-releases.githubusercontent.com": true,
}

// validateDownloadURL 校验下载地址的协议与域名。
// 不校验的话，一个被篡改的 URL 就能让我们下载并执行任意二进制
// （TUN 场景下还是管理员权限），等于开放了远程代码执行。
func validateDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("下载地址无法解析: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("只允许 https 下载: %s", u.Scheme)
	}
	h := strings.ToLower(u.Hostname())
	if !allowedDownloadHosts[h] {
		return fmt.Errorf("下载地址域名不在白名单内: %s", h)
	}
	return nil
}

// DownloadAndReplace 下载 zip 并替换本地 mihomo.exe。
// wantDigest 形如 "sha256:<hex>"；非空时必须匹配，否则拒绝替换。
// 返回新内核的 sha256 前 12 位，方便前端显示。
func (m *Manager) DownloadAndReplace(rawURL, wantDigest string, timeout time.Duration) (string, error) {
	if err := validateDownloadURL(rawURL); err != nil {
		return "", err
	}

	client := &http.Client{
		Timeout: timeout,
		// 重定向后的地址也要过白名单，否则可以先用合法域名跳转到任意地址
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("重定向次数过多")
			}
			return validateDownloadURL(req.URL.String())
		},
	}

	resp, err := client.Get(rawURL)
	if err != nil {
		return "", fmt.Errorf("下载内核失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载内核失败: HTTP %d", resp.StatusCode)
	}

	// 限制读取大小，防止异常响应撑爆内存（内核包一般 15MB 上下）
	data, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return "", fmt.Errorf("读取下载内容失败: %w", err)
	}

	// 摘要校验针对下载到的 zip 原文，必须在解包前做
	if wantDigest != "" {
		sum := sha256.Sum256(data)
		got := "sha256:" + hex.EncodeToString(sum[:])
		if !strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(wantDigest)) {
			return "", fmt.Errorf("内核包校验失败：摘要不匹配（期望 %s，实际 %s）", wantDigest, got)
		}
	}

	exe, err := extractMihomo(data)
	if err != nil {
		return "", err
	}

	binDir := filepath.Join(m.dataDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	target := filepath.Join(binDir, "mihomo.exe")

	// 先写临时文件，再原子替换，避免写一半的文件被当成内核启动。
	tmp := target + ".new"
	if err := os.WriteFile(tmp, exe, 0o755); err != nil {
		return "", fmt.Errorf("写入新内核失败: %w", err)
	}
	_ = os.Remove(target)
	if err := os.Rename(tmp, target); err != nil {
		return "", fmt.Errorf("替换内核失败: %w", err)
	}

	if err := markCustom(m.dataDir); err != nil {
		return "", fmt.Errorf("写入升级标记失败: %w", err)
	}

	m.mu.Lock()
	m.exePath = target
	m.mu.Unlock()

	sum := sha256.Sum256(exe)
	return hex.EncodeToString(sum[:])[:12], nil
}

// extractMihomo 从 zip 里解出内核 exe。
//
// 不能写死 "mihomo.exe"：官方 release 包里的文件名是
// mihomo-windows-amd64-compatible.exe（标准版则是 mihomo-windows-amd64.exe），
// 只有按前缀 + 后缀匹配才能兼容这两种命名。
func extractMihomo(zipData []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("内核包不是合法的 zip: %w", err)
	}
	for _, f := range zr.File {
		name := strings.ToLower(filepath.Base(f.Name))
		if !strings.HasPrefix(name, "mihomo") || !strings.HasSuffix(name, ".exe") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, 200<<20))
	}
	return nil, fmt.Errorf("压缩包里没有 mihomo.exe")
}

// EnsureBinary 把内置内核释放到磁盘。
//
// 如果检测到"已手动升级"标记，说明磁盘上的内核比 embed 的新，
// 此时保留磁盘版本，不覆盖。
func (m *Manager) EnsureBinary(embedded []byte) (string, error) {
	binDir := filepath.Join(m.dataDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("创建内核目录失败: %w", err)
	}
	target := filepath.Join(binDir, "mihomo.exe")

	// 手动升级过就用磁盘上的版本
	if IsCustomCore(m.dataDir) {
		if _, err := os.Stat(target); err == nil {
			m.mu.Lock()
			m.exePath = target
			m.mu.Unlock()
			return target, nil
		}
		// 文件丢了就退回落到内置版本
		ClearCustomMark(m.dataDir)
	}

	needWrite := true
	if old, err := os.ReadFile(target); err == nil && len(old) == len(embedded) {
		oldSum := sha256.Sum256(old)
		newSum := sha256.Sum256(embedded)
		if hex.EncodeToString(oldSum[:]) == hex.EncodeToString(newSum[:]) {
			needWrite = false
		}
	}
	if needWrite {
		tmp := target + ".tmp"
		if err := os.WriteFile(tmp, embedded, 0o755); err != nil {
			return "", fmt.Errorf("写入内核失败: %w", err)
		}
		// 旧进程可能仍占用文件，先删再改名。
		_ = os.Remove(target)
		if err := os.Rename(tmp, target); err != nil {
			return "", fmt.Errorf("替换内核失败: %w", err)
		}
	}
	m.mu.Lock()
	m.exePath = target
	m.mu.Unlock()
	return target, nil
}
