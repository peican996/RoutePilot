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
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GitHub 上 mihomo 的发布仓库。
const repoAPI = "https://api.github.com/repos/MetaCubeX/mihomo/releases/latest"

// coreCustomMark 是"内核已被手动升级"的标记文件名。
const coreCustomMark = ".core_custom"

// upgradeRecordFile 记录最近一次内核升级/回滚结果，便于事后排查。
const upgradeRecordFile = "core-upgrade.json"

// maxCoreDownload 限制内核压缩包下载大小（官方包约 15-30MB）。
const maxCoreDownload int64 = 200 << 20

type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
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
	Size      int64  `json:"size"`      // 资源字节数，0 表示未知
	Digest    string `json:"digest"`    // "sha256:<hex>"，为空表示该 release 没提供
}

// UpgradeResult 是一次成功替换的结果。
type UpgradeResult struct {
	Hash       string
	BackupPath string
}

// UpgradeRecord 是最近一次升级/回滚的持久化记录。
type UpgradeRecord struct {
	BeforeVersion  string `json:"beforeVersion"`
	TargetVersion  string `json:"targetVersion"`
	Hash           string `json:"hash"`
	BackupPath     string `json:"backupPath"`
	ReplacedAt     string `json:"replacedAt"`
	RolledBack     bool   `json:"rolledBack"`
	RollbackReason string `json:"rollbackReason,omitempty"`
	RolledBackAt   string `json:"rolledBackAt,omitempty"`
}

// fetchLatest 查询最新 release。timeout 控制整体超时，国内访问 GitHub 可能很慢。
func fetchLatest(timeout time.Duration) (*release, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, repoAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "RoutePilot")

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
	size   int64
}

func pickWindowsAsset(rel *release) winAsset {
	// 内置内核用的是 compatible（GOAMD64=v1）变体，升级也沿用同一变体；
	// 否则老 CPU 用户升级后会直接起不来，和内置版本的选择自相矛盾。
	for _, a := range rel.Assets {
		n := strings.ToLower(a.Name)
		if strings.Contains(n, "windows") && strings.Contains(n, "amd64") &&
			strings.HasSuffix(n, ".zip") && strings.Contains(n, "compatible") {
			return winAsset{a.Name, a.BrowserDownloadURL, a.Digest, a.Size}
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
		return winAsset{a.Name, a.BrowserDownloadURL, a.Digest, a.Size}
	}
	// 最后兜底：任何 Windows zip
	for _, a := range rel.Assets {
		n := strings.ToLower(a.Name)
		if strings.Contains(n, "windows") && strings.HasSuffix(n, ".zip") {
			return winAsset{a.Name, a.BrowserDownloadURL, a.Digest, a.Size}
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
	// 读不到当前版本时不能断定"有新版本"；版本比较按 SemVer 2.0.0 的
	// 优先级规则判断大小，不能用"字符串不相等"——本地比 latest 新
	// （如装了预发布版）时会永远提示可升级，点下去实际是降级。
	hasUpdate := false
	if cur != "" && lat != "" {
		if cmp, ok := compareSemver(lat, cur); ok {
			hasUpdate = cmp > 0
		}
		// 无法解析时保守处理：不提示升级，避免把新版本误判为旧版本后降级。
	}
	return &LatestInfo{
		Current:   currentVersion,
		Latest:    rel.TagName,
		HasUpdate: hasUpdate,
		URL:       asset.url,
		AssetName: asset.name,
		Size:      asset.size,
		Digest:    asset.digest,
	}, nil
}

// semver 是解析后的版本号：core 为核心数字段，pre 为预发布标识符。
type semver struct {
	core []int
	pre  []string
}

// parseSemver 严格解析 SemVer 2.0.0 版本号（允许省略前导 v/V）。
//
// 校验规则（全部拒绝即返回 ok=false）：
//   - 核心版本必须是三段数字 major.minor.patch，不允许前导零、不允许缺段；
//   - 数字必须落在 int64 范围内；
//   - 预发布标识符以 . 分隔，不得为空，只允许 [0-9A-Za-z-]；
//     纯数字标识符不允许前导零；
//   - build metadata（+xxx）只允许出现一次，标识符不得为空且只允许 [0-9A-Za-z-]；
//     build metadata 不参与优先级比较，但格式非法仍判为无效版本。
func parseSemver(v string) (semver, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	if v == "" {
		return semver{}, false
	}

	if i := strings.IndexByte(v, '+'); i >= 0 {
		build := v[i+1:]
		v = v[:i]
		if build == "" || strings.ContainsRune(build, '+') {
			return semver{}, false
		}
		for _, id := range strings.Split(build, ".") {
			if !isSemverIdent(id, true) {
				return semver{}, false
			}
		}
	}

	var pre []string
	if i := strings.IndexByte(v, '-'); i >= 0 {
		preStr := v[i+1:]
		v = v[:i]
		if preStr == "" {
			return semver{}, false
		}
		pre = strings.Split(preStr, ".")
		for _, id := range pre {
			if !isSemverIdent(id, false) {
				return semver{}, false
			}
			// 纯数字标识符还要确认没有前导零且不溢出。
			if isAllDigits(id) {
				if len(id) > 1 && id[0] == '0' {
					return semver{}, false
				}
				if _, err := strconv.ParseUint(id, 10, 63); err != nil {
					return semver{}, false
				}
			}
		}
	}

	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	core := make([]int, 3)
	for i, p := range parts {
		if !isAllDigits(p) {
			return semver{}, false
		}
		if len(p) > 1 && p[0] == '0' {
			return semver{}, false
		}
		n, err := strconv.ParseUint(p, 10, 63)
		if err != nil {
			return semver{}, false
		}
		core[i] = int(n)
	}
	return semver{core: core, pre: pre}, true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isSemverIdent 校验单个标识符：非空且字符集为 [0-9A-Za-z-]。
// allowLeadingZero=false 时，纯数字标识符不允许前导零（SemVer 规范要求）。
func isSemverIdent(s string, allowLeadingZero bool) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-' {
			continue
		}
		return false
	}
	if !allowLeadingZero && len(s) > 1 && s[0] == '0' && isAllDigits(s) {
		return false
	}
	return true
}

func numericIdent(s string) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, 63)
	if err != nil {
		return 0, false
	}
	return n, true
}

// compareSemver 按 SemVer 2.0.0 的优先级规则比较 a 与 b，返回 -1/0/1。
// 规则要点：核心段按数字比较；有预发布标识符的版本低于同核心的正式版；
// 预发布标识符中数字按数值比较、数字低于字母、字母按 ASCII 比较；
// 前缀全部相同时字段更少者优先级更低。build metadata 不参与比较。
func compareSemver(a, b string) (int, bool) {
	pa, okA := parseSemver(a)
	pb, okB := parseSemver(b)
	if !okA || !okB {
		return 0, false
	}

	n := len(pa.core)
	if len(pb.core) > n {
		n = len(pb.core)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa.core) {
			x = pa.core[i]
		}
		if i < len(pb.core) {
			y = pb.core[i]
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}

	// 正式版优先于预发布版。
	switch {
	case len(pa.pre) == 0 && len(pb.pre) == 0:
		return 0, true
	case len(pa.pre) == 0:
		return 1, true
	case len(pb.pre) == 0:
		return -1, true
	}

	m := len(pa.pre)
	if len(pb.pre) < m {
		m = len(pb.pre)
	}
	for i := 0; i < m; i++ {
		ai, aNum := numericIdent(pa.pre[i])
		bi, bNum := numericIdent(pb.pre[i])
		switch {
		case aNum && bNum:
			if ai != bi {
				if ai < bi {
					return -1, true
				}
				return 1, true
			}
		case aNum:
			// 数字标识符优先级低于字母标识符。
			return -1, true
		case bNum:
			return 1, true
		default:
			if pa.pre[i] != pb.pre[i] {
				if pa.pre[i] < pb.pre[i] {
					return -1, true
				}
				return 1, true
			}
		}
	}
	if len(pa.pre) != len(pb.pre) {
		if len(pa.pre) < len(pb.pre) {
			return -1, true
		}
		return 1, true
	}
	return 0, true
}

// versionNewer 判断 latest 是否严格大于 cur。
func versionNewer(cur, latest string) bool {
	cmp, ok := compareSemver(latest, cur)
	return ok && cmp > 0
}

// IsCustomCore 磁盘上的内核是否被手动升级过。
func IsCustomCore(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, "bin", coreCustomMark))
	return err == nil
}

// markCustom 写入"内核已手动升级"标记。
func markCustom(dataDir string) error {
	p := filepath.Join(dataDir, "bin", coreCustomMark)
	return atomicWriteBytes(p, []byte(time.Now().Format(time.RFC3339)), 0o644)
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

// DownloadAndReplace 下载 zip、校验并原子替换本地 mihomo.exe。
//
// 安全/一致性约束：
//   - 官方未提供 sha256 摘要时拒绝升级（不能只靠 HTTPS 判断二进制可信）
//   - 下载大小必须与 release 声明一致，且不超过上限
//   - 先写临时文件并 Sync，替换失败时旧内核保持原样
//   - 替换前保留旧内核备份，新版本启动失败可回滚
func (m *Manager) DownloadAndReplace(rawURL, wantDigest string, wantSize int64, timeout time.Duration) (UpgradeResult, error) {
	if strings.TrimSpace(wantDigest) == "" {
		return UpgradeResult{}, fmt.Errorf("官方 release 未提供 sha256 摘要，已拒绝自动升级")
	}
	if err := validateDownloadURL(rawURL); err != nil {
		return UpgradeResult{}, err
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
		return UpgradeResult{}, fmt.Errorf("下载内核失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return UpgradeResult{}, fmt.Errorf("下载内核失败: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxCoreDownload {
		return UpgradeResult{}, fmt.Errorf("内核包过大：声明 %d 字节", resp.ContentLength)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCoreDownload+1))
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("读取下载内容失败: %w", err)
	}
	if int64(len(data)) > maxCoreDownload {
		return UpgradeResult{}, fmt.Errorf("内核包超过 %d 字节上限", maxCoreDownload)
	}
	if wantSize > 0 && int64(len(data)) != wantSize {
		return UpgradeResult{}, fmt.Errorf("内核包大小不符：期望 %d 字节，实际 %d 字节", wantSize, len(data))
	}

	// 摘要校验针对下载到的 zip 原文，必须在解包前做
	sum := sha256.Sum256(data)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if !strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(wantDigest)) {
		return UpgradeResult{}, fmt.Errorf("内核包校验失败：摘要不匹配（期望 %s，实际 %s）", wantDigest, got)
	}

	exe, err := extractMihomo(data)
	if err != nil {
		return UpgradeResult{}, err
	}
	if err := validatePE(exe); err != nil {
		return UpgradeResult{}, fmt.Errorf("下载的内核不是有效的 Windows 可执行文件: %w", err)
	}

	return m.installBinary(exe)
}

// installBinary 把已校验的内核写入 bin 目录。
// 这是升级事务的核心：备份旧版 → 临时文件 → 原子替换 → 写升级标记。
func (m *Manager) installBinary(exe []byte) (UpgradeResult, error) {
	binDir := filepath.Join(m.dataDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return UpgradeResult{}, err
	}
	target := filepath.Join(binDir, "mihomo.exe")

	// 1. 保留当前可用内核。替换失败时它就是用户唯一能用的版本。
	backupPath := ""
	if _, err := os.Stat(target); err == nil {
		backupPath = target + ".bak"
		if err := copyFileAtomic(target, backupPath); err != nil {
			return UpgradeResult{}, fmt.Errorf("备份旧内核失败（未做任何替换）: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return UpgradeResult{}, fmt.Errorf("检查旧内核失败: %w", err)
	}

	// 2. 新内核先落到同目录临时文件并同步到磁盘。
	tmp, err := os.CreateTemp(binDir, ".mihomo-download-*")
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("创建临时内核文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	replaced := false
	defer func() {
		_ = tmp.Close()
		if !replaced {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o755); err != nil {
		return UpgradeResult{}, err
	}
	if _, err := tmp.Write(exe); err != nil {
		return UpgradeResult{}, fmt.Errorf("写入新内核失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return UpgradeResult{}, fmt.Errorf("同步新内核失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return UpgradeResult{}, fmt.Errorf("关闭临时内核失败: %w", err)
	}

	// 3. 原子替换；失败时旧文件不会被删除。
	if err := replaceFileWithRetry(tmpPath, target); err != nil {
		return UpgradeResult{}, fmt.Errorf("替换内核失败（旧内核仍可用）: %w", err)
	}
	replaced = true

	// 4. 标记失败会让下次启动回退到内置内核，因此按升级失败处理并回滚。
	if err := markCustom(m.dataDir); err != nil {
		if backupPath != "" {
			if rbErr := restoreFromBackup(backupPath, target); rbErr != nil {
				return UpgradeResult{}, fmt.Errorf("写入升级标记失败: %v；回滚旧内核也失败: %w", err, rbErr)
			}
			return UpgradeResult{}, fmt.Errorf("写入升级标记失败，已回滚旧内核: %w", err)
		}
		return UpgradeResult{}, fmt.Errorf("写入升级标记失败: %w", err)
	}

	m.mu.Lock()
	m.exePath = target
	m.lastBackup = backupPath
	m.mu.Unlock()

	sum := sha256.Sum256(exe)
	return UpgradeResult{
		Hash:       hex.EncodeToString(sum[:])[:12],
		BackupPath: backupPath,
	}, nil
}

// RollbackBinary 用最近一次升级前的备份恢复内核。
// 备份在校验通过前不会被删除；调用方负责在新内核启动失败后调用本方法。
func (m *Manager) RollbackBinary() error {
	m.mu.Lock()
	backup := m.lastBackup
	m.mu.Unlock()
	if backup == "" {
		return fmt.Errorf("没有可用的旧内核备份")
	}
	target := filepath.Join(m.dataDir, "bin", "mihomo.exe")
	if err := restoreFromBackup(backup, target); err != nil {
		return err
	}
	m.mu.Lock()
	m.exePath = target
	m.mu.Unlock()
	return nil
}

// LastBackupPath 返回当前升级事务的备份路径（可能为空）。
func (m *Manager) LastBackupPath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastBackup
}

// SaveUpgradeRecord 持久化最近一次升级/回滚结果。
func (m *Manager) SaveUpgradeRecord(rec UpgradeRecord) error {
	mb, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteBytes(filepath.Join(m.dataDir, upgradeRecordFile), mb, 0o644)
}

// LastUpgradeRecord 读取最近一次升级记录。文件不存在返回零值。
func (m *Manager) LastUpgradeRecord() (UpgradeRecord, error) {
	b, err := os.ReadFile(filepath.Join(m.dataDir, upgradeRecordFile))
	if err != nil {
		if os.IsNotExist(err) {
			return UpgradeRecord{}, nil
		}
		return UpgradeRecord{}, err
	}
	var rec UpgradeRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return UpgradeRecord{}, err
	}
	return rec, nil
}

func restoreFromBackup(backup, target string) error {
	data, err := os.ReadFile(backup)
	if err != nil {
		return fmt.Errorf("读取旧内核备份失败（备份已保留）: %w", err)
	}
	if err := validatePE(data); err != nil {
		return fmt.Errorf("旧内核备份无效: %w", err)
	}
	if err := atomicWriteBytes(target, data, 0o755); err != nil {
		return fmt.Errorf("恢复旧内核失败（备份已保留）: %w", err)
	}
	return nil
}

// validatePE 做最小可执行文件校验：MZ 头 + PE 签名。
// 摘要校验已保证内容来自官方 release，这里额外挡住解包错文件的情况。
func validatePE(exe []byte) error {
	if len(exe) < 0x40 || exe[0] != 'M' || exe[1] != 'Z' {
		return fmt.Errorf("缺少 MZ 头")
	}
	off := int(binary.LittleEndian.Uint32(exe[0x3c:0x40]))
	if off <= 0 || off+4 > len(exe) {
		return fmt.Errorf("PE 头偏移无效")
	}
	if !bytes.Equal(exe[off:off+4], []byte{'P', 'E', 0, 0}) {
		return fmt.Errorf("缺少 PE 签名")
	}
	return nil
}

// replaceFileWithRetry 用 Rename 原子替换目标。
// Windows 上内核刚退出时文件句柄可能短暂未释放，做几次短重试。
func replaceFileWithRetry(src, dst string) error {
	var err error
	for i := 0; i < 4; i++ {
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		time.Sleep(time.Duration(150*(i+1)) * time.Millisecond)
	}
	return err
}

// copyFileAtomic 把 src 复制到同目录临时文件后再原子替换 dst。
func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	done := false
	defer func() {
		_ = tmp.Close()
		if !done {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	done = true
	return nil
}

// atomicWriteBytes 用于标记文件与升级记录，避免写一半崩溃留下坏文件。
func atomicWriteBytes(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	done := false
	defer func() {
		_ = f.Close()
		if !done {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	done = true
	return nil
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
	cleanupUpgradeTemps(binDir)
	target := filepath.Join(binDir, "mihomo.exe")

	// 手动升级过就用磁盘上的版本（先校验文件仍然可用）
	if IsCustomCore(m.dataDir) {
		if data, err := os.ReadFile(target); err == nil {
			if validatePE(data) == nil {
				m.mu.Lock()
				m.exePath = target
				m.mu.Unlock()
				return target, nil
			}
			m.Logs.Add("已升级的内核文件无效，回退到内置版本")
			ClearCustomMark(m.dataDir)
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("读取已升级内核失败: %w", err)
		} else {
			// 文件丢了就退回落到内置版本
			ClearCustomMark(m.dataDir)
		}
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
		tmp, err := os.CreateTemp(binDir, ".mihomo-embed-*")
		if err != nil {
			return "", fmt.Errorf("创建临时内核文件失败: %w", err)
		}
		tmpPath := tmp.Name()
		done := false
		defer func() {
			_ = tmp.Close()
			if !done {
				_ = os.Remove(tmpPath)
			}
		}()
		if err := tmp.Chmod(0o755); err != nil {
			return "", err
		}
		if _, err := tmp.Write(embedded); err != nil {
			return "", fmt.Errorf("写入内核失败: %w", err)
		}
		if err := tmp.Sync(); err != nil {
			return "", fmt.Errorf("同步内核失败: %w", err)
		}
		if err := tmp.Close(); err != nil {
			return "", fmt.Errorf("关闭临时内核失败: %w", err)
		}
		// 不再先删除旧文件：Rename 在 Windows 上会替换目标，
		// 失败时旧内核保持可用，不会出现"两边都没有"的空窗。
		if err := replaceFileWithRetry(tmpPath, target); err != nil {
			return "", fmt.Errorf("替换内核失败（旧内核仍可用）: %w", err)
		}
		done = true
	}
	m.mu.Lock()
	m.exePath = target
	m.mu.Unlock()
	return target, nil
}

// cleanupUpgradeTemps 清理上次升级中断留下的临时文件。
// 正式内核与 .bak 备份不会被动。
func cleanupUpgradeTemps(binDir string) {
	// 除了下载/解压的临时包，copyFileAtomic（.bak 备份）、
	// atomicWriteBytes（内核本体替换、.core_custom 标记）也会在 binDir
	// 留下 .tmp-* 孤儿，进程崩溃时来不及走 defer 清理，必须一并纳入。
	for _, pattern := range []string{
		".mihomo-download-*", ".mihomo-embed-*",
		".mihomo.exe.bak.tmp-*", ".mihomo.exe.tmp-*", "..core_custom.tmp-*",
	} {
		matches, _ := filepath.Glob(filepath.Join(binDir, pattern))
		for _, p := range matches {
			_ = os.Remove(p)
		}
	}
	for _, name := range []string{"mihomo.exe.tmp", "mihomo.exe.new"} {
		_ = os.Remove(filepath.Join(binDir, name))
	}
}
