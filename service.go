package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"routepilot/internal/autostart"
	"routepilot/internal/configgen"
	"routepilot/internal/elevate"
	"routepilot/internal/geoip"
	"routepilot/internal/kernel"
	"routepilot/internal/subscription"
	"routepilot/internal/sysproxy"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// proxyBackup 记录接管系统代理之前的原始设置。
// 用户机器上很可能已经有别的代理软件在跑（比如另一个 Clash），
// 关掉自己的代理时必须把它原样还回去，而不是一律关闭。
//
// 这份记录要落盘：系统代理存在注册表里是持久的，但进程内存不是。
// 应用重启后必须能认出"注册表里那个地址是上次我自己设的"，
// 否则要么误占别人的设置，要么显示未开启而实际开着、还指向一个死端口。
type proxyBackup struct {
	Addr          string `json:"addr"`                    // 我们设进去的地址
	Server        string `json:"server"`                  // 接管前系统原本的地址
	Override      string `json:"override"`                // 接管前系统原本的绕过列表
	AutoConfigURL string `json:"autoConfigUrl,omitempty"` // 接管前系统原本的 PAC 地址
	Enabled       bool   `json:"enabled"`                 // 接管前系统原本是否启用
	Saved         bool   `json:"saved"`
	// Pending 表示恢复动作已经开始但尚未确认成功。下次启动必须优先重试，
	// 不能再把注册表里的旧地址当作"仍然由本程序接管"而去改写。
	Pending bool `json:"pending,omitempty"`
}

// systemProxyOps 把注册表操作抽成可替换的函数集合。
// 生产环境使用真实 sysproxy 实现；测试用它注入失败，验证事务与回滚。
type systemProxyOps struct {
	Current            func() (string, bool)
	Enable             func(string) error
	Disable            func() error
	SetServer          func(string) error
	Override           func() string
	SetOverride        func(string) error
	AutoConfigURL      func() string
	SetAutoConfigURL   func(string) error
	ClearAutoConfigURL func() error
	WinHTTPRaw         func() ([]byte, error)
	RestoreWinHTTP     func([]byte) error
	EnableWinHTTP      func(string) error
}

func defaultSystemProxyOps() systemProxyOps {
	return systemProxyOps{
		Current:            sysproxy.Current,
		Enable:             sysproxy.Enable,
		Disable:            sysproxy.Disable,
		SetServer:          sysproxy.SetServer,
		Override:           sysproxy.Override,
		SetOverride:        sysproxy.SetOverride,
		AutoConfigURL:      sysproxy.AutoConfigURL,
		SetAutoConfigURL:   sysproxy.SetAutoConfigURL,
		ClearAutoConfigURL: sysproxy.ClearAutoConfigURL,
		WinHTTPRaw:         sysproxy.WinHTTPRaw,
		RestoreWinHTTP:     sysproxy.RestoreWinHTTP,
		EnableWinHTTP:      sysproxy.EnableWinHTTP,
	}
}

const proxyStateFile = "proxy.json"

// dataDirOverride 仅用于测试，把数据目录指向临时目录。
var dataDirOverride string

// writeFileAtomic 允许测试注入写失败。生产实现见 atomicWriteFile。
var writeFileAtomic = atomicWriteFile

// processTargetPrefix 是进程规则里"指定节点/策略组"的值前缀。
// 例：target:NodeA。direct / proxy / follow 仍用原来的短值。
const processTargetPrefix = "target:"

// CoreService 是暴露给前端的唯一入口。所有方法都会生成 TypeScript 绑定。
type CoreService struct {
	app *application.App

	mgr *kernel.Manager
	api *kernel.Client

	// 系统代理注册表操作。默认走真实 sysproxy，测试可替换为故障注入版本。
	sys systemProxyOps
	// 管理员权限判断，测试可替换。
	elevated func() bool
	// 内核是否运行。测试可替换，避免为了测回滚而真的拉起内核。
	kernelRunningFn func() bool

	mu        sync.RWMutex
	mixedPort int
	apiPort   int
	dnsPort   int
	secret    string
	proxyOn   bool
	tunOn     bool
	mode      string
	quitting  bool
	backup    proxyBackup
	booted    bool
	bootErr   string
	// tunPrevProxyOn 记录"因为开 TUN 而被临时关掉的系统代理"。
	// 关 TUN 成功后据此恢复，避免丢失用户原本的接管状态。
	tunPrevProxyOn bool

	// WinHTTP 是机器级代理设置，只有管理员能改。备份与"待还原"状态
	// 落 app-state，避免这次以普通权限退出后把服务流量指向死端口。
	winHTTPPending bool
	winHTTPBackup  []byte

	// 启动时要自动恢复的运行状态（从 app-state.json 读来）。
	// TUN 需要管理员，做不到就保持关闭；系统代理随时能开。
	restoreTUN   bool
	restoreProxy bool

	// GeoIP 查询器，用来把连接目标的 IP 定位到国家/经纬度。
	geoDB *geoip.DB

	// 订阅相关。subConfig 是机场返回的原始 Clash 配置，
	// 作为生成最终配置的骨架。
	subURL    string
	subConfig map[string]any
	subInfo   SubInfo
	subRaw    []byte // 机场返回的原始内容，用来判断自动更新后配置有没有真变化
	// 当前订阅里可作为"进程指定出口"的目标：显式节点名、策略组名，
	// 以及 provider 名称（provider 节点要在内核里按 provider 选择）。
	subTargets   map[string]bool
	subProviders []string

	// 多订阅列表。subs[i] 是第 i 个订阅的元信息，activeIdx 指向正在用的那个。
	subs      []subMeta
	activeIdx int

	// 按程序分流：进程名 → "proxy" / "direct"。没有记录的进程走订阅规则。
	procRules map[string]string
	// 用户明确选择"不再提示"的进程。
	ignored map[string]bool

	trafficMu sync.Mutex
	lastUp    int64
	lastDown  int64
	lastTick  time.Time
	rateUp    int64
	rateDown  int64

	// 分钟级流量历史，落盘持久化。
	histMu  sync.Mutex
	history []TrafficSample

	// 节点延迟历史，记录每次测速。
	delayMu      sync.Mutex
	delayHistory map[string][]DelaySample

	// 内核版本缓存。Status 被前端高频轮询，/version 一分钟查一次就够。
	verMu     sync.Mutex
	verCache  string
	verCached time.Time

	// /connections 在同一轮轮询里会被 Traffic 和 Connections 各请求一次，
	// 这里做 500ms 短缓存，避免重复传输和序列化整张连接表。
	connMu      sync.Mutex
	connCache   *kernel.ConnectionsResp
	connCacheAt time.Time

	// 窗口位置记忆
	winX, winY, winW, winH int

	// 流量告警阈值（字节），0 = 关闭
	alertLimit int64
	// 已发过告警的日期（"2006-01-02"）。跨天自动失效，实现"当日一次"。
	alertSentDay string

	// 收藏节点名集合
	favorites map[string]bool

	// 地图上"本机"的坐标。默认东莞，可在设置里改；
	// 不在该地区的用户不改的话，地图连线和距离全是错的。
	homeLon, homeLat float64
	homeLabel        string
}

func NewCoreService() *CoreService {
	return &CoreService{
		mixedPort: defaultMixedPort,
		apiPort:   defaultAPIPort,
		sys:       defaultSystemProxyOps(),
		elevated:  elevate.IsElevated,
		homeLon:   113.75, // 东莞
		homeLat:   23.02,
		homeLabel: "本机",
	}
}

// admin 统一走可替换的管理员判断，测试里可以模拟提权。
func (s *CoreService) admin() bool {
	if s.elevated != nil {
		return s.elevated()
	}
	return elevate.IsElevated()
}

func (s *CoreService) kernelRunning() bool {
	if s.kernelRunningFn != nil {
		return s.kernelRunningFn()
	}
	return s.mgr != nil && s.mgr.IsRunning()
}

// 默认端口刻意避开 Clash 生态惯用的 7890/7891/7897 和 9090。
// 这台机器上同时装着 Clash Verge 和 mini-clash，用 7890 必然被抢，
// 内核就会静默漂移，系统代理跟着指错端口——这类问题排查成本极高。
// 换到冷门号段后端口基本稳定，漂移检测退化成纯防御。
const (
	defaultMixedPort = 17890
	defaultAPIPort   = 19090
	defaultDNSPort   = 11053
)

// ---------- 视图模型 ----------

type AppStatus struct {
	State     string `json:"state"`
	PID       int    `json:"pid"`
	Uptime    int64  `json:"uptime"`
	Error     string `json:"error"`
	Version   string `json:"version"`
	ProxyOn   bool   `json:"proxyOn"`
	ProxyAddr string `json:"proxyAddr"`
	MixedPort int    `json:"mixedPort"`
	APIPort   int    `json:"apiPort"`
	Mode      string `json:"mode"`
	BootErr   string `json:"bootErr"`
	DataDir   string `json:"dataDir"`
	Tun       bool   `json:"tun"`
	Admin     bool   `json:"admin"`
}

type ConnView struct {
	ID          string `json:"id"`
	Host        string `json:"host"`
	Dest        string `json:"dest"`
	DestIP      string `json:"destIP"`
	Process     string `json:"process"`
	ProcessPath string `json:"processPath"`
	Chains      string `json:"chains"`
	Rule        string `json:"rule"`
	Upload      int64  `json:"upload"`
	Download    int64  `json:"download"`
	Start       string `json:"start"`
	Network     string `json:"network"`
}

type TrafficView struct {
	UpTotal   int64 `json:"upTotal"`
	DownTotal int64 `json:"downTotal"`
	UpRate    int64 `json:"upRate"`
	DownRate  int64 `json:"downRate"`
}

// SubInfo 是订阅的元信息，给前端展示用。
type SubInfo struct {
	URL       string `json:"url"`
	Name      string `json:"name"`
	Nodes     int    `json:"nodes"`
	Groups    int    `json:"groups"`
	Rules     int    `json:"rules"`
	FetchedAt int64  `json:"fetchedAt"`
	Insecure  bool   `json:"insecure"`
	Upload    int64  `json:"upload"`
	Download  int64  `json:"download"`
	Total     int64  `json:"total"`
	Expire    int64  `json:"expire"`
	WebPage   string `json:"webPage"`
	UpdateHrs int    `json:"updateHrs"`
	Error     string `json:"error"`
}

// NodeView 是节点/策略组列表项。
type NodeView struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Delay int    `json:"delay"`
	Group bool   `json:"group"`
	Now   string `json:"now"`
	Count int    `json:"count"`
}

// ---------- 初始化 ----------

// 数据目录名。产品改名过两次：ProxDeck → NexusLink → RoutePilot。
// 旧目录在首次启动时由 migrateDataDir 按顺序搬过来，
// 谁在就搬谁——NexusLink 排前面，避免用更老的 ProxDeck 数据覆盖。
const (
	dataDirName = "RoutePilot"
)

var legacyDirNames = []string{"NexusLink", "ProxDeck"}

func dataDir() string {
	if dataDirOverride != "" {
		return dataDirOverride
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, dataDirName)
}

// migrateDataDir 把旧数据目录搬到新名字下。
// 只在「新目录不存在 + 旧目录存在」时执行一次，避免覆盖用户已有数据。
func (s *CoreService) migrateDataDir() {
	base, err := os.UserConfigDir()
	if err != nil {
		return
	}
	newDir := filepath.Join(base, dataDirName)

	if _, err := os.Stat(newDir); err == nil {
		return // 新目录已存在，什么都不动
	}
	// 按新旧程度依次找：改名链是 ProxDeck → NexusLink → RoutePilot，
	// 越靠前越新。两个旧目录都在时只搬 NexusLink。
	for _, legacy := range legacyDirNames {
		oldDir := filepath.Join(base, legacy)
		fi, err := os.Stat(oldDir)
		if err != nil || !fi.IsDir() {
			continue
		}

		// 同一卷内 rename 是原子的，最干净
		if err := os.Rename(oldDir, newDir); err == nil {
			return
		}
		// 跨卷或被占用时退化为逐个复制；只有全部成功才删旧目录，
		// 否则宁可留着让用户手动处理，也不能把没复制过去的数据删掉。
		if err := copyDirAll(oldDir, newDir); err != nil {
			s.dbg("data dir migrate skipped: %v", err)
			return
		}
		_ = os.RemoveAll(oldDir)
		return
	}
}

// copyDirAll 递归复制目录。
//
// 任何文件复制失败都必须反映到返回值：调用方据此决定是否删除旧目录。
// 之前失败一律 continue、永远返回 nil，随后 RemoveAll 旧目录，
// 被占用（比如旧实例没退干净的 mihomo.exe）的文件就这样丢了。
func copyDirAll(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	var failed []string
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDirAll(srcPath, dstPath); err != nil {
				failed = append(failed, e.Name()+"/")
			}
			continue
		}
		b, err := os.ReadFile(srcPath)
		if err != nil {
			failed = append(failed, e.Name())
			continue
		}
		// 保留原文件权限，但始终给所有者写权限：迁移前 config.yaml /
		// 订阅文件可能只有 0600，统一按 0644 重写会放宽权限；若源文件是
		// 只读的，也不能把目标文件复制成只读，否则后续保存会失败。
		mode := os.FileMode(0o644)
		if fi, statErr := e.Info(); statErr == nil && fi.Mode().Perm() != 0 {
			mode = fi.Mode().Perm() | 0o200
		}
		if err := os.WriteFile(dstPath, b, mode); err != nil {
			failed = append(failed, e.Name())
		}
	}
	if len(failed) > 0 {
		// 只报前几个，避免日志刷屏
		head := failed
		if len(head) > 3 {
			head = head[:3]
		}
		return fmt.Errorf("%d 个文件复制失败（%s），旧目录已保留",
			len(failed), strings.Join(head, ", "))
	}
	return nil
}

// ensureGeodata 把内置的 GeoIP / GeoSite 数据库释放到数据目录。
// 大小一致就跳过，避免每次启动都写十几兆。
func ensureGeodata(dir string) {
	files := map[string][]byte{
		"geoip.metadb": geoipEmbed,
		"geosite.dat":  geositeEmbed,
	}
	for name, data := range files {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Size() == int64(len(data)) {
			continue
		}
		_ = os.WriteFile(p, data, 0o644)
	}
}

// randomSecret 生成控制 API 的鉴权密钥。
// 熵不足时绝不回退到固定串——那等于把控制接口对本机所有程序敞开。
func randomSecret() (string, error) {
	b := make([]byte, 32)
	for i := 0; i < 3; i++ {
		if _, err := rand.Read(b); err == nil {
			return hex.EncodeToString(b), nil
		}
	}
	return "", fmt.Errorf("无法生成控制 API 密钥：系统随机源不可用")
}

// pickPort 返回一个尽量不冲突的端口：从 prefer 开始往上找。
func pickPort(prefer int) int {
	for p := prefer; p < prefer+60; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			_ = ln.Close()
			return p
		}
	}
	return prefer
}

// Boot 完成一次性准备：释放内核、生成配置、启动进程。
// 内核启动是异步的，前端靠轮询 Status 感知状态变化。
func (s *CoreService) Boot() {
	// 迁移必须在任何读写数据目录的操作之前完成
	s.migrateDataDir()

	dir := dataDir()
	s.mu.Lock()
	if s.booted {
		s.mu.Unlock()
		return
	}
	s.booted = true
	s.mu.Unlock()

	// 读上次的运行状态（模式、TUN、系统代理），启动后按需恢复。
	s.loadAppState()

	// 上次恢复失败留下的备份必须优先处理：先还原用户原始配置，
	// 再决定是否要按旧意图重新接管。
	s.recoverPendingProxyRestore()

	// 管理员进程启动时，把之前用 Run 键配置的普通自启升级为
	// HighestAvailable 计划任务，TUN 下次开机就不用再手动提权。
	if s.admin() {
		if err := autostart.UpgradeIfElevated(); err != nil {
			s.dbg("autostart upgrade failed: %v", err)
		}
	}
	// 上次进程异常退出可能留下被接管的 WinHTTP，提权启动时先还原。
	if err := s.restoreWinHTTPIfPending(); err != nil {
		s.dbg("boot: restore WinHTTP failed: %v", err)
		s.mu.RLock()
		pendingWinHTTP := s.winHTTPPending
		s.mu.RUnlock()
		if pendingWinHTTP {
			s.appendBootErr("上次的 WinHTTP 代理设置尚未还原，请以管理员身份重启完成还原")
		}
	}

	// GeoIP 用于地图上标注连接目标的位置。加载失败不影响主功能，
	// 只是地图上少一层"目标点"。
	if db, err := geoip.Load(geoipEmbed); err == nil {
		s.geoDB = db
	}

	mgr := kernel.NewManager(dir)
	s.mgr = mgr

	if _, err := mgr.EnsureBinary(embeddedKernel); err != nil {
		s.failBoot(err)
		return
	}
	// wintun.dll 是 TUN 模式的虚拟网卡驱动，提前放好，省得开启 TUN 时才发现缺文件。
	mgr.EnsureAux(map[string][]byte{"wintun.dll": wintunDLL})
	// 内核没有 GeoIP 数据库会去 GitHub 现下，国内基本卡死在这里。
	// 直接内置一份，保证断网/新机器也能首次启动。
	ensureGeodata(dir)

	sec, err := randomSecret()
	if err != nil {
		s.failBoot(err)
		return
	}

	s.mu.Lock()
	s.secret = sec
	s.mixedPort = pickPort(defaultMixedPort)
	s.apiPort = pickPort(defaultAPIPort)
	s.dnsPort = pickPort(defaultDNSPort)
	s.api = kernel.NewClient(fmt.Sprintf("127.0.0.1:%d", s.apiPort), s.secret)
	s.mu.Unlock()

	// 如果上次用过订阅，直接拿本地缓存起，不用等网络。
	s.loadSavedSubscription()
	s.loadProcRules()
	s.loadIgnored()
	_ = s.writeProcRulesYAML()

	confPath, err := configgen.Write(dir, s.options())
	if err != nil {
		s.failBoot(err)
		return
	}

	if err := mgr.Start(confPath); err != nil {
		s.failBoot(err)
		return
	}

	s.startAutoUpdate()
	s.startHistorySampler()
	s.startKernelWatchdog()
	s.startNetworkHealthCheck()
	s.afterStart()
}

// options 汇总当前端口与密钥，供生成配置使用。
func (s *CoreService) options() configgen.Options {
	s.mu.RLock()
	defer s.mu.RUnlock()
	mode := s.mode
	if mode == "" {
		mode = "rule"
	}

	// TUN 以当前期望态为准。restoreTUN 只表示"上次退出时开着"，
	// 用于启动阶段的一次性恢复，不能拿来生成配置——否则会话内开关
	// TUN 改不动配置，界面显示和实际行为会脱节。
	wantTUN := s.tunOn && s.admin()

	plan := buildProcessTargetPlan(s.procRules, s.subTargets, s.subProviders)

	return configgen.Options{
		MixedPort:          s.mixedPort,
		APIPort:            s.apiPort,
		DNSListen:          s.dnsPort,
		Secret:             s.secret,
		Mode:               mode,
		Tun:                wantTUN,
		ProcessRules:       true,
		ProcessCustomRules: plan.Rules,
		ProcessGroups:      plan.Groups,
		Base:               s.subConfig,
	}
}

// processTargetSelection 描述一个需要在内核就绪后执行的组选择。
type processTargetSelection struct {
	Group          string
	Target         string
	ProviderBacked bool
}

type processTargetPlan struct {
	Groups     []map[string]any
	Rules      []string
	Selections []processTargetSelection
}

// processTargetGroupName 为"进程指定出口"生成稳定的隐藏组名。
// 用目标名哈希而不是直接拼名字，避免逗号、中文等字符影响规则结构。
func processTargetGroupName(target string) string {
	sum := sha256.Sum256([]byte(target))
	return configgen.ProcessGroupPrefix + hex.EncodeToString(sum[:4])
}

// buildProcessTargetPlan 把进程规则映射成隐藏策略组 + PROCESS-NAME 规则。
//
// 目标分两类：
//   - 显式节点/策略组：配置解析阶段就存在，组里直接放 [target] 即可。
//   - provider 节点：解析阶段还不存在，组用 use 引用 provider，等内核
//     启动后再通过 API 把组选到目标节点；失败时回退 DIRECT。
func buildProcessTargetPlan(rules map[string]string, targets map[string]bool, providers []string) processTargetPlan {
	plan := processTargetPlan{}
	if len(rules) == 0 {
		return plan
	}

	procs := make([]string, 0, len(rules))
	for proc := range rules {
		procs = append(procs, proc)
	}
	sort.Strings(procs)

	groupsByTarget := map[string]bool{}
	for _, proc := range procs {
		policy := rules[proc]
		if !strings.HasPrefix(policy, processTargetPrefix) {
			continue
		}
		target := strings.TrimSpace(strings.TrimPrefix(policy, processTargetPrefix))
		if target == "" || strings.HasPrefix(target, configgen.ProcessGroupPrefix) {
			continue
		}
		if sanitizeProcName(proc) == "" {
			continue
		}

		group := processTargetGroupName(target)
		if !groupsByTarget[target] {
			g := map[string]any{"name": group, "type": "select"}
			if targets[target] {
				g["proxies"] = []any{target}
				plan.Selections = append(plan.Selections, processTargetSelection{Group: group, Target: target})
			} else if len(providers) > 0 {
				// provider 节点：先放 DIRECT 兜底，再引用所有 provider；
				// 内核就绪后选到具体节点，选不中就退回 DIRECT。
				use := make([]any, 0, len(providers))
				for _, p := range providers {
					use = append(use, p)
				}
				g["proxies"] = []any{"DIRECT"}
				g["use"] = use
				plan.Selections = append(plan.Selections, processTargetSelection{
					Group: group, Target: target, ProviderBacked: true,
				})
			} else {
				// 目标已不存在且没有 provider 兜底，忽略，进程回到跟随规则。
				continue
			}
			plan.Groups = append(plan.Groups, g)
			groupsByTarget[target] = true
		}
		plan.Rules = append(plan.Rules, fmt.Sprintf("PROCESS-NAME,%s,%s", proc, group))
	}
	return plan
}

// reloadConfig 让内核重新读取生成的配置，不重启进程。
func (s *CoreService) reloadConfig(path string) error {
	if s.api == nil {
		return fmt.Errorf("内核 API 未就绪")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.api.ReloadConfig(ctx, path)
}

// selectProcessTargets 把 provider 支撑的隐藏组选到具体节点。
// 显式节点/组在配置里只有单个成员，会自动选中；这里统一补一次，
// 失败时 provider 组退回 DIRECT，避免悄悄用第一个节点。
func (s *CoreService) selectProcessTargets() {
	if s.api == nil || s.mgr == nil || !s.mgr.IsRunning() {
		return
	}
	s.mu.RLock()
	plan := buildProcessTargetPlan(s.procRules, s.subTargets, s.subProviders)
	s.mu.RUnlock()
	if len(plan.Selections) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, sel := range plan.Selections {
		if err := s.api.Select(ctx, sel.Group, sel.Target); err != nil {
			s.dbg("select process target %s -> %s failed: %v", sel.Group, sel.Target, err)
			if sel.ProviderBacked {
				if fallbackErr := s.api.Select(ctx, sel.Group, "DIRECT"); fallbackErr != nil {
					s.dbg("fallback process target %s -> DIRECT failed: %v", sel.Group, fallbackErr)
				}
			}
		}
	}
}

// afterStart 等内核就绪后，把真实监听端口同步回来。
//
// mihomo 在端口被占用时会自动顺延（7890 → 7891），配置里写的端口因此
// 不一定是它真正监听的端口。如果系统代理指向一个没人监听的端口，
// 用户的表现就是"一开代理就断网"。所以必须以内核自报为准。
func (s *CoreService) afterStart() {
	if s.api == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for {
			if _, err := s.api.Version(ctx); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
		cfg, err := s.api.Configs(ctx)
		if err != nil || cfg.MixedPort <= 0 {
			return
		}
		s.mu.Lock()
		changed := s.mixedPort != cfg.MixedPort
		s.mixedPort = cfg.MixedPort
		// 端口变化时如果系统代理已开着，得跟着改正，否则立刻断网。
		proxyOn := s.proxyOn
		s.mu.Unlock()
		if changed && proxyOn {
			_ = sysproxy.Enable(fmt.Sprintf("127.0.0.1:%d", cfg.MixedPort))
		}

		// 校正系统代理：确保它指向内核这次真正监听的端口。
		s.reconcileProxy()

		// 恢复上次退出时的接管状态。
		// TUN 的配置在生成阶段已经带上，这里只同步标记；
		// 系统代理要显式开一次（reconcileProxy 只处理代理重启前后的状态，
		// 处理不了"用户上次主动开着、这次要重新打开"）。
		//
		// 这两个标志只在启动阶段消费一次。若不清零，会话中用户主动关掉的
		// TUN / 系统代理会在任意一次内核重启后按旧快照被重新打开；TUN 更糟：
		// 配置是按当前 tunOn 生成的（不含 TUN），这里却把 tunOn 置回 true，
		// 界面显示"已接管"，实际流量并没有走 TUN。
		s.mu.Lock()
		wantTUN := s.restoreTUN && s.admin()
		wantProxy := s.restoreProxy
		s.restoreTUN = false
		s.restoreProxy = false
		s.mu.Unlock()

		if wantTUN {
			s.mu.Lock()
			s.tunOn = true
			s.mu.Unlock()
		} else if wantProxy {
			_ = s.SetSystemProxy(true)
		}

		// 进程指定到 provider 节点的隐藏组需要显式选中；
		// 显式节点组也会统一补一次，保证与持久化映射一致。
		s.selectProcessTargets()
	}()
}

func (s *CoreService) setBootErr(msg string) {
	s.mu.Lock()
	s.bootErr = msg
	s.mu.Unlock()
}

// failBoot 记录启动错误，并回滚上次遗留在注册表里的系统代理接管记录。
//
// Boot 失败时 afterStart 不会执行，reconcileProxy 也就没有机会校正。
// 如果上次退出时系统代理仍指向本程序的内核端口，这次内核又没起来，
// 注册表里就留下一个指向死端口的代理——用户表现为"开机直接断网"，
// 而 Shutdown 只认内存里的 proxyOn，同样不会处理。
func (s *CoreService) failBoot(err error) {
	s.setBootErr(err.Error())
	if restoreErr := s.restoreSystemProxy(); restoreErr != nil {
		s.dbg("boot failure: restore system proxy failed: %v", restoreErr)
		s.appendBootErr(fmt.Sprintf("系统代理恢复失败，备份已保留: %v", restoreErr))
	}
	if restoreErr := s.restoreWinHTTPIfPending(); restoreErr != nil {
		s.dbg("boot failure: restore WinHTTP failed: %v", restoreErr)
		s.appendBootErr(fmt.Sprintf("WinHTTP 恢复失败，备份已保留: %v", restoreErr))
	}
}

// appendBootErr 保留已有启动错误，不做覆盖。
func (s *CoreService) appendBootErr(msg string) {
	s.mu.Lock()
	if s.bootErr == "" {
		s.bootErr = msg
	} else {
		s.bootErr += "；" + msg
	}
	s.mu.Unlock()
}

// recoverPendingProxyRestore 在启动阶段优先重试上次未完成的系统代理恢复。
// 成功则清除旧的 proxyOn 意图，避免 afterStart 又把代理打开。
func (s *CoreService) recoverPendingProxyRestore() {
	st, err := readProxyState()
	if err != nil {
		s.setBootErr("系统代理备份损坏，已保留原文件: " + err.Error())
		s.mu.Lock()
		s.restoreProxy = false
		s.mu.Unlock()
		return
	}
	if !st.Saved || !st.Pending {
		return
	}
	if err := s.restoreSystemProxy(); err != nil {
		s.appendBootErr("上次系统代理恢复未完成，备份已保留: " + err.Error())
		s.mu.Lock()
		s.restoreProxy = false
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.restoreProxy = false
	s.mu.Unlock()
	s.saveAppState()
}

func (s *CoreService) Attach(app *application.App) { s.app = app }

// Shutdown 在应用退出时调用。只有确实是我们开的代理才需要还原，
// 否则会把用户原本正在用的代理设置一并抹掉。
//
// 退出阶段的恢复失败无法阻止进程结束，但备份与 Pending 标记必须保留，
// 下次启动会优先重试，避免网络配置永久停留在死端口。
func (s *CoreService) Shutdown() {
	if err := s.restoreSystemProxy(); err != nil {
		s.dbg("shutdown: restore system proxy failed, backup kept: %v", err)
	}
	if err := s.restoreWinHTTPIfPending(); err != nil {
		s.dbg("shutdown: restore WinHTTP failed, backup kept: %v", err)
	}
	if s.mgr != nil {
		_ = s.mgr.Stop()
	}
}

// ---------- 状态 ----------

func (s *CoreService) Status() AppStatus {
	s.mu.RLock()
	mixed, apiPort, proxyOn, bootErr, tunOn, mode := s.mixedPort, s.apiPort, s.proxyOn, s.bootErr, s.tunOn, s.mode
	s.mu.RUnlock()
	if mode == "" {
		mode = "rule"
	}

	st := AppStatus{
		MixedPort: mixed,
		APIPort:   apiPort,
		ProxyOn:   proxyOn,
		ProxyAddr: fmt.Sprintf("127.0.0.1:%d", mixed),
		Mode:      mode,
		BootErr:   bootErr,
		DataDir:   dataDir(),
		Tun:       tunOn,
		Admin:     s.admin(),
	}
	if s.mgr != nil {
		ks := s.mgr.Status()
		st.State = string(ks.State)
		st.PID = ks.PID
		st.Uptime = ks.Uptime
		st.Error = ks.Error
	}
	if s.api != nil && st.State == "running" {
		st.Version = s.cachedVersion()
	}
	return st
}

// cachedVersion 返回内核版本，最多一分钟查一次。
// 前端每 1.2 秒轮询一次 Status，每次都打 /version 纯属浪费；
// 版本只在升级内核时变化，而升级路径会主动清掉缓存。
func (s *CoreService) cachedVersion() string {
	s.verMu.Lock()
	defer s.verMu.Unlock()
	if s.verCache != "" && time.Since(s.verCached) < time.Minute {
		return s.verCache
	}
	if s.api == nil {
		return s.verCache
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if v, err := s.api.Version(ctx); err == nil {
		s.verCache = v.Version
		s.verCached = time.Now()
	}
	return s.verCache
}

func (s *CoreService) clearVersionCache() {
	s.verMu.Lock()
	s.verCache = ""
	s.verCached = time.Time{}
	s.verMu.Unlock()
}

// ---------- 内核控制 ----------

func (s *CoreService) StartKernel() error {
	if s.mgr == nil {
		return fmt.Errorf("内核管理器未初始化")
	}
	confPath, err := configgen.Write(dataDir(), s.options())
	if err != nil {
		return err
	}
	if err := s.mgr.Start(confPath); err != nil {
		return err
	}
	s.clearVersionCache()
	s.invalidateConnCache()
	s.afterStart()
	return nil
}

func (s *CoreService) StopKernel() error {
	if s.mgr == nil {
		return nil
	}
	if pc, file, line, ok := runtime.Caller(1); ok {
		fn := runtime.FuncForPC(pc)
		name := "?"
		if fn != nil {
			name = fn.Name()
		}
		s.dbg("StopKernel <- %s (%s:%d)", name, filepath.Base(file), line)
	}
	// 先恢复系统代理，避免关了内核还挂着代理导致断网。
	// 恢复失败时必须中止停止流程：此时内核还在运行，代理仍可用；
	// 强行停内核只会留下一个指向死端口的系统代理。
	if err := s.SetSystemProxy(false); err != nil {
		return fmt.Errorf("无法恢复系统代理，已中止停止内核: %w", err)
	}
	s.clearVersionCache()
	s.invalidateConnCache()
	return s.mgr.Stop()
}

func (s *CoreService) RestartKernel() error {
	if s.mgr == nil {
		return fmt.Errorf("内核管理器未初始化")
	}
	// 记录触发方：内核反复重启时，只有调用栈能说明是谁在重启它。
	if pc, file, line, ok := runtime.Caller(1); ok {
		fn := runtime.FuncForPC(pc)
		name := "?"
		if fn != nil {
			name = fn.Name()
		}
		s.dbg("RestartKernel <- %s (%s:%d)", name, filepath.Base(file), line)
	}
	confPath, err := configgen.Write(dataDir(), s.options())
	if err != nil {
		return err
	}
	if err := s.mgr.Restart(confPath); err != nil {
		return err
	}
	s.clearVersionCache()
	s.invalidateConnCache()
	s.afterStart()
	return nil
}

// ---------- 系统代理 ----------

func (s *CoreService) proxyOps() systemProxyOps {
	if s.sys.Current != nil {
		return s.sys
	}
	return defaultSystemProxyOps()
}

func (s *CoreService) proxyStatePath() string {
	return filepath.Join(dataDir(), proxyStateFile)
}

// captureProxySnapshot 记录当前注册表状态，作为接管前的还原点。
func (s *CoreService) captureProxySnapshot(addr string) proxyBackup {
	ops := s.proxyOps()
	srv, enabled := ops.Current()
	return proxyBackup{
		Addr:          addr,
		Server:        srv,
		Override:      ops.Override(),
		AutoConfigURL: ops.AutoConfigURL(),
		Enabled:       enabled,
		Saved:         true,
	}
}

func (s *CoreService) SetSystemProxy(on bool) error {
	ops := s.proxyOps()
	s.mu.RLock()
	addr := fmt.Sprintf("127.0.0.1:%d", s.mixedPort)
	s.mu.RUnlock()

	if on {
		if !s.kernelRunning() && s.mgr != nil {
			return fmt.Errorf("请先启动内核再开启系统代理")
		}
		// TUN 已经在 IP 层接管全部流量，这时再开系统代理等于代理两遍。
		// 界面里的开关会禁用，但托盘菜单里的没禁，所以这里也必须拦一道。
		s.mu.RLock()
		tun := s.tunOn
		bk := s.backup
		s.mu.RUnlock()
		if tun {
			return fmt.Errorf("TUN 已开启，正在接管全部流量，请先关闭 TUN")
		}
		if bk.Saved && bk.Pending {
			return fmt.Errorf("上次系统代理恢复尚未完成，请先重试恢复或以管理员身份重启")
		}
		if !bk.Saved {
			bk = s.captureProxySnapshot(addr)
			// 备份文件丢失但注册表仍指向本程序端口：这不是一份可还原的
			// "原始配置"，而是上次崩溃留下的死代理。直接关掉，避免把它
			// 当成用户原配置保存，导致以后永远还原到死端口。
			if bk.Enabled && strings.EqualFold(bk.Server, addr) {
				_ = ops.Disable()
				return fmt.Errorf("检测到系统代理仍指向本程序但备份已丢失，已关闭遗留代理，请重新开启")
			}
		} else {
			bk.Addr = addr
		}

		if err := ops.Enable(addr); err != nil {
			// 写了一半也要尽量把注册表拉回原状，不能留下半接管状态。
			_ = s.applyProxySnapshot(bk)
			return fmt.Errorf("开启系统代理失败: %w", err)
		}
		// PAC 优先级高于手动代理，接管时必须移除，否则我们的地址根本不生效。
		// 原地址已经记在 backup 里，关闭代理时再写回去。
		if err := ops.ClearAutoConfigURL(); err != nil {
			_ = s.applyProxySnapshot(bk)
			return fmt.Errorf("清除系统 PAC 设置失败: %w", err)
		}

		// 备份必须落盘成功后才认为接管成功；否则崩溃后无法识别。
		if err := s.persistProxyState(bk); err != nil {
			_ = s.applyProxySnapshot(bk)
			return fmt.Errorf("保存系统代理备份失败: %w", err)
		}
		s.mu.Lock()
		s.backup = bk
		s.proxyOn = true
		s.mu.Unlock()
		s.saveAppState()

		// 管理员权限下同时接管 WinHTTP，让系统服务 / CLI 也走代理。
		s.takeOverWinHTTP(addr)
		return nil
	}

	if err := s.restoreSystemProxy(); err != nil {
		return err
	}
	if err := s.restoreWinHTTPIfPending(); err != nil {
		return err
	}
	s.saveAppState()
	return nil
}

// applyProxySnapshot 把备份里的原始设置写回注册表。
// 每个字段都要写，包括空值：接管时我们覆盖了 Override/PAC，
// 原值为空时也必须显式清空，否则用户的自定义配置会被永久污染。
func (s *CoreService) applyProxySnapshot(bk proxyBackup) error {
	if !bk.Saved {
		return nil
	}
	ops := s.proxyOps()
	var errs []error

	if bk.Enabled && bk.Server != "" {
		errs = append(errs, ops.Enable(bk.Server))
	} else {
		// 关闭状态也要把地址写回原值，否则注册表里会残留我们上次的端口。
		errs = append(errs, ops.SetServer(bk.Server), ops.Disable())
	}
	errs = append(errs, ops.SetOverride(bk.Override))
	if bk.AutoConfigURL != "" {
		errs = append(errs, ops.SetAutoConfigURL(bk.AutoConfigURL))
	} else {
		errs = append(errs, ops.ClearAutoConfigURL())
	}

	var filtered []error
	for _, err := range errs {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	return errors.Join(filtered...)
}

// verifyProxySnapshot 回读注册表，确认恢复动作真的生效。
// 只返回 nil 才算恢复成功，不能凭"写入没报错"就删除备份。
func (s *CoreService) verifyProxySnapshot(bk proxyBackup) error {
	if !bk.Saved {
		return nil
	}
	ops := s.proxyOps()
	cur, enabled := ops.Current()
	if enabled != bk.Enabled {
		return fmt.Errorf("系统代理启用状态不符：期望 %v，实际 %v", bk.Enabled, enabled)
	}
	if bk.Enabled && bk.Server != "" && !strings.EqualFold(cur, bk.Server) {
		return fmt.Errorf("系统代理地址不符：期望 %s，实际 %s", bk.Server, cur)
	}
	if !bk.Enabled && !strings.EqualFold(cur, bk.Server) {
		return fmt.Errorf("系统代理地址未还原：期望 %q，实际 %q", bk.Server, cur)
	}
	if got := ops.Override(); got != bk.Override {
		return fmt.Errorf("代理绕过列表不符")
	}
	if got := ops.AutoConfigURL(); got != bk.AutoConfigURL {
		return fmt.Errorf("PAC 地址不符：期望 %q，实际 %q", bk.AutoConfigURL, got)
	}
	return nil
}

// restoreSystemProxy 把系统代理还原成被接管之前的样子。
// 原本就指向别的代理软件时原样还回去，原本关着才关闭。
//
// 关键前提：只还原「确实是我们接管过」的系统代理。
// 没有接管记录时什么都不做——否则用户机器上正在跑的 Clash Verge、
// v2rayN 之类会被我们顺手关掉，表现为"点了停止内核，整台机器断网"。
//
// 恢复失败时备份必须保留，并标记 Pending，下次启动优先重试。
func (s *CoreService) restoreSystemProxy() error {
	s.mu.RLock()
	bk := s.backup
	s.mu.RUnlock()
	if !bk.Saved {
		var err error
		bk, err = readProxyState()
		if err != nil {
			return fmt.Errorf("系统代理备份损坏，已保留原文件: %w", err)
		}
	}
	if !bk.Saved {
		return nil
	}

	// 先持久化"恢复中"标记。这样即使恢复过程中崩溃，
	// 下次启动也只会重试恢复，而不会把旧端口当成仍然有效的接管。
	bk.Pending = true
	if err := s.persistProxyState(bk); err != nil {
		return fmt.Errorf("标记系统代理待恢复失败: %w", err)
	}
	s.mu.Lock()
	s.backup = bk
	s.mu.Unlock()

	if err := s.applyProxySnapshot(bk); err != nil {
		return fmt.Errorf("恢复系统代理失败（备份已保留）: %w", err)
	}
	if err := s.verifyProxySnapshot(bk); err != nil {
		return fmt.Errorf("恢复系统代理后校验失败（备份已保留）: %w", err)
	}
	if err := os.Remove(s.proxyStatePath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除系统代理备份失败（恢复已完成，下次将继续校验）: %w", err)
	}

	s.mu.Lock()
	s.backup = proxyBackup{}
	s.proxyOn = false
	s.mu.Unlock()
	s.saveAppState()
	return nil
}

// takeOverWinHTTP 备份并接管 WinHTTP 代理。WinHTTP 是机器级设置，
// 只有管理员权限才能写；普通权限下直接跳过，不影响 WinINET 系统代理。
func (s *CoreService) takeOverWinHTTP(addr string) {
	if !s.admin() {
		return
	}
	ops := s.proxyOps()

	s.mu.RLock()
	pending := s.winHTTPPending
	s.mu.RUnlock()
	if pending {
		// 上次的备份还没还原，绝不能覆盖；只把地址更新到当前端口。
		if err := ops.EnableWinHTTP(addr); err != nil {
			s.dbg("WinHTTP enable failed: %v", err)
		}
		return
	}

	raw, err := ops.WinHTTPRaw()
	if err != nil {
		s.dbg("WinHTTP backup failed: %v", err)
		return
	}
	if err := ops.EnableWinHTTP(addr); err != nil {
		s.dbg("WinHTTP enable failed: %v", err)
		return
	}

	s.mu.Lock()
	s.winHTTPBackup = raw
	s.winHTTPPending = true
	s.mu.Unlock()
	s.saveAppState()
}

// restoreWinHTTPIfPending 在管理员进程里还原 WinHTTP 设置。
// 普通权限下无法写 HKLM，返回错误并保留 pending 状态，等下次提权后继续还原。
func (s *CoreService) restoreWinHTTPIfPending() error {
	s.mu.RLock()
	pending := s.winHTTPPending
	backup := append([]byte(nil), s.winHTTPBackup...)
	s.mu.RUnlock()
	if !pending {
		return nil
	}

	if !s.admin() {
		return fmt.Errorf("WinHTTP 代理设置待还原，需要管理员权限，请以管理员身份重启")
	}
	if err := s.proxyOps().RestoreWinHTTP(backup); err != nil {
		return fmt.Errorf("还原 WinHTTP 设置失败（备份已保留）: %w", err)
	}

	s.mu.Lock()
	s.winHTTPPending = false
	s.winHTTPBackup = nil
	s.mu.Unlock()
	s.saveAppState()
	return nil
}

// readProxyState 从磁盘读接管记录。文件不存在返回零值；
// 内容损坏返回错误并保留原文件，避免静默覆盖。
func readProxyState() (proxyBackup, error) {
	b, err := os.ReadFile(filepath.Join(dataDir(), proxyStateFile))
	if err != nil {
		if os.IsNotExist(err) {
			return proxyBackup{}, nil
		}
		return proxyBackup{}, err
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	var st proxyBackup
	if err := json.Unmarshal(b, &st); err != nil {
		return proxyBackup{}, err
	}
	return st, nil
}

func (s *CoreService) persistProxyState(bk proxyBackup) error {
	mb, err := json.Marshal(bk)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.proxyStatePath(), mb, 0o600)
}

// atomicWriteFile 先写同目录临时文件并 Sync，再原子替换目标文件。
// 写一半崩溃时目标文件仍是上一份完整内容，不会留下截断的 JSON。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
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

// reconcileProxy 让系统代理始终指向内核的真实端口。
//
// 系统代理是注册表里的持久状态，而内核端口会因端口冲突而变。
// 只有“当前地址确实是我们上次写入的地址”才校正；如果用户或外部软件
// 已经把代理改成别的值，必须放弃接管，不能覆盖用户的新配置。
func (s *CoreService) reconcileProxy() {
	// TUN 已接管全部流量时，系统代理必须保持关闭。
	if s.TUNState() {
		return
	}
	st, err := readProxyState()
	if err != nil {
		s.dbg("read proxy state failed, leave registry untouched: %v", err)
		return
	}
	if !st.Saved || st.Pending {
		// Pending 由启动恢复流程处理，这里不能把它当成有效接管。
		return
	}

	ops := s.proxyOps()
	cur, enabled := ops.Current()
	if !enabled {
		// 用户自己把系统代理关了：还原地址、bypass、PAC 后撤掉接管记录，
		// 并同步 restoreProxy，避免下次启动又擅自打开。
		_ = ops.SetServer(st.Server)
		_ = ops.SetOverride(st.Override)
		if st.AutoConfigURL != "" {
			_ = ops.SetAutoConfigURL(st.AutoConfigURL)
		} else {
			_ = ops.ClearAutoConfigURL()
		}
		_ = os.Remove(s.proxyStatePath())
		s.mu.Lock()
		s.proxyOn = false
		s.backup = proxyBackup{}
		s.restoreProxy = false
		s.mu.Unlock()
		s.saveAppState()
		// 用户手动关掉系统代理时，WinHTTP 也应该跟着还原。
		if err := s.restoreWinHTTPIfPending(); err != nil {
			s.dbg("WinHTTP restore postponed: %v", err)
		}
		return
	}

	s.mu.RLock()
	want := fmt.Sprintf("127.0.0.1:%d", s.mixedPort)
	s.mu.RUnlock()
	if !strings.EqualFold(cur, st.Addr) && !strings.EqualFold(cur, want) {
		// 外部软件/用户改了系统代理。以当前注册表为准，放弃我们旧的备份，
		// 否则退出时会把用户的新设置覆盖回接管前的值。
		s.dbg("system proxy changed externally to %s, relinquish takeover", cur)
		_ = os.Remove(s.proxyStatePath())
		s.mu.Lock()
		s.proxyOn = false
		s.backup = proxyBackup{}
		s.restoreProxy = false
		s.mu.Unlock()
		s.saveAppState()
		return
	}

	s.mu.Lock()
	s.proxyOn = true
	s.backup = st
	s.mu.Unlock()

	if strings.EqualFold(cur, want) {
		if st.Addr != want {
			st.Addr = want
			if err := s.persistProxyState(st); err != nil {
				s.dbg("update proxy state failed: %v", err)
			}
		}
		return
	}
	// 注册表指向的是旧端口，校正到当前端口，并更新接管记录。
	if err := ops.Enable(want); err != nil {
		s.dbg("reconcile proxy to %s failed: %v", want, err)
		return
	}
	st.Addr = want
	if err := s.persistProxyState(st); err != nil {
		s.dbg("update proxy state failed: %v", err)
	}
}

func (s *CoreService) SystemProxyState() bool {
	_, enabled := s.proxyOps().Current()
	return enabled
}

// ---------- TUN ----------

// SetTUN 开关 TUN 模式。TUN 在 IP 层接管全部流量，
// 是代理"不读系统代理设置"的程序（游戏、UWP、命令行工具）的唯一办法。
func (s *CoreService) SetTUN(on bool) error {
	return s.setTUN(on, s.RestartKernel)
}

// setTUN 把内核重启抽成参数，便于故障注入测试验证回滚路径。
func (s *CoreService) setTUN(on bool, restart func() error) error {
	if on && !s.admin() {
		return fmt.Errorf("开启 TUN 需要管理员权限")
	}

	// 先记住原状态。内核重启失败时要整体回滚——
	// 不能出现"界面上写着开启、实际没启用"这种脱节。
	s.mu.RLock()
	prevTUN := s.tunOn
	prevProxyOn := s.proxyOn
	prevBackup := s.backup
	prevTunPrevProxy := s.tunPrevProxyOn
	s.mu.RUnlock()

	// 记录关闭系统代理前的真实注册表状态。TUN 开启失败时用它回滚，
	// 避免把用户/外部软件原本的代理设置弄丢。
	previousRegistry := s.captureProxySnapshot("")

	if on {
		// TUN 已经接管全部出站流量，此时如果系统代理还开着，
		// 浏览器会先连本地代理端口、再由 TUN 转发一次，等于代理两遍。
		// 先完整还原我们接管前的配置，再把系统代理关闭。
		if prevProxyOn || prevBackup.Saved {
			if err := s.restoreSystemProxy(); err != nil {
				// 恢复失败时不要继续切 TUN：否则用户既没了代理也没了原配置。
				s.mu.Lock()
				s.proxyOn = prevProxyOn
				s.backup = prevBackup
				s.mu.Unlock()
				return fmt.Errorf("切换 TUN 前恢复系统代理失败: %w", err)
			}
		}
		if err := s.proxyOps().Disable(); err != nil {
			_ = s.applyProxySnapshot(previousRegistry)
			s.mu.Lock()
			s.proxyOn = prevProxyOn
			s.backup = prevBackup
			s.mu.Unlock()
			return fmt.Errorf("关闭系统代理失败，已回滚: %w", err)
		}
		// WinHTTP 也是系统代理类接管，TUN 生效前必须还原，避免双代理。
		if err := s.restoreWinHTTPIfPending(); err != nil {
			_ = s.applyProxySnapshot(previousRegistry)
			s.mu.Lock()
			s.proxyOn = prevProxyOn
			s.backup = prevBackup
			s.mu.Unlock()
			return fmt.Errorf("切换 TUN 前还原 WinHTTP 失败: %w", err)
		}

		s.mu.Lock()
		s.tunOn = true
		s.proxyOn = false
		s.backup = proxyBackup{}
		// 只有"本来由我们接管"的系统代理才在关闭 TUN 后自动恢复。
		s.tunPrevProxyOn = prevProxyOn
		s.mu.Unlock()
	} else {
		s.mu.Lock()
		s.tunOn = false
		s.mu.Unlock()
	}

	if s.kernelRunning() {
		if err := restart(); err != nil {
			// 重启可能已经停掉旧内核。回滚时先确认内核是否还在跑：
			// 在跑就按原意图恢复 TUN；已经停了 TUN 实际就是关闭状态，
			// 不能把 tunOn 回滚成 true 造成 UI 与实际脱节。
			kernelAlive := s.kernelRunning()
			s.mu.Lock()
			s.tunOn = prevTUN && kernelAlive
			s.proxyOn = false
			s.backup = proxyBackup{}
			s.tunPrevProxyOn = prevTunPrevProxy
			s.mu.Unlock()
			s.saveAppState()

			if prevProxyOn {
				if kernelAlive {
					// 旧内核还在，可以重新接管；让 SetSystemProxy 重新采集
					// 当前（已还原的）注册表作为新的还原点。
					if e := s.SetSystemProxy(true); e != nil {
						s.dbg("TUN rollback re-enable proxy failed: %v", e)
						_ = s.applyProxySnapshot(prevBackup)
					}
				} else {
					_ = s.applyProxySnapshot(prevBackup)
				}
			} else {
				_ = s.applyProxySnapshot(previousRegistry)
			}
			return fmt.Errorf("切换失败，已回滚: %w", err)
		}
	}

	// 关闭 TUN 成功后，如果系统代理原本是我们在接管，就重新打开。
	if !on && s.tunPrevProxyOn {
		s.mu.Lock()
		s.tunPrevProxyOn = false
		s.mu.Unlock()
		if s.kernelRunning() {
			if err := s.SetSystemProxy(true); err != nil {
				s.saveAppState()
				return fmt.Errorf("TUN 已关闭，但恢复系统代理失败: %w", err)
			}
		}
	}

	s.saveAppState()
	return nil
}

func (s *CoreService) TUNState() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tunOn
}

// ProxyEnabled 给托盘菜单读当前状态（菜单项需要在打开时知道勾没勾）。
func (s *CoreService) ProxyEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.proxyOn
}

// Quitting 标记"用户是从托盘菜单主动退出的"，此时窗口关闭不再拦截。
func (s *CoreService) Quitting() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.quitting
}

func (s *CoreService) MarkQuitting() {
	s.mu.Lock()
	s.quitting = true
	s.mu.Unlock()
}

// ---------- 配置备份 ----------

// ConfigBundle 导出的配置包。只含用户配置，不含流量历史等运行数据。
type ConfigBundle struct {
	Version    int               `json:"version"`
	ExportedAt int64             `json:"exportedAt"`
	Subs       []subMeta         `json:"subs"`
	ProcRules  map[string]string `json:"procRules"`
	Ignored    []string          `json:"ignored"`
}

// ExportConfig 导出配置为 JSON 字符串，供前端保存成文件。
func (s *CoreService) ExportConfig() (string, error) {
	s.mu.RLock()
	bundle := ConfigBundle{
		Version:    1,
		ExportedAt: time.Now().Unix(),
		Subs:       s.subs,
		ProcRules:  s.procRules,
	}
	for k, v := range s.ignored {
		if v {
			bundle.Ignored = append(bundle.Ignored, k)
		}
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ImportConfig 从 JSON 导入配置。会覆盖当前订阅与进程规则。
func (s *CoreService) ImportConfig(jsonStr string) error {
	var bundle ConfigBundle
	if err := json.Unmarshal([]byte(jsonStr), &bundle); err != nil {
		return fmt.Errorf("配置格式不正确: %w", err)
	}
	if bundle.Version != 1 {
		return fmt.Errorf("不支持的配置版本: %d", bundle.Version)
	}

	s.mu.Lock()
	s.subs = bundle.Subs
	s.procRules = bundle.ProcRules
	s.ignored = make(map[string]bool)
	for _, n := range bundle.Ignored {
		s.ignored[n] = true
	}
	s.mu.Unlock()

	s.saveSubs()
	s.saveAppState()

	// 导入的进程分流规则必须立刻落进 provider 文件并重载内核，
	// 否则要等下一次别的操作触发写入才生效，表现为"导入完没反应"。
	if mb, err := json.MarshalIndent(s.ProcessRules(), "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dataDir(), procRulesJSON), mb, 0o644)
	}
	if mb, err := json.MarshalIndent(s.IgnoredProcesses(), "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dataDir(), procIgnoredJSON), mb, 0o644)
	}
	_ = s.writeProcRulesYAML()

	if s.mgr != nil && s.mgr.IsRunning() {
		if err := s.RestartKernel(); err != nil {
			return fmt.Errorf("配置已导入，但重载内核失败: %w", err)
		}
	}
	return nil
}

// ---------- 节点延迟历史 ----------

// DelaySample 是一次测速结果。
type DelaySample struct {
	Time  int64 `json:"t"` // Unix 秒
	Delay int32 `json:"d"` // 延迟毫秒，<=0 表示超时/失败
}

// delayHistoryKeep 每个节点保留最近 100 次测速。
const delayHistoryKeep = 100

// NodeDelayHistory 返回所有节点的延迟历史，按时间升序。
func (s *CoreService) NodeDelayHistory() map[string][]DelaySample {
	s.delayMu.Lock()
	defer s.delayMu.Unlock()
	out := make(map[string][]DelaySample, len(s.delayHistory))
	for k, v := range s.delayHistory {
		cp := make([]DelaySample, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// recordDelay 记录一次测速结果。容量到顶就丢最旧的。
func (s *CoreService) recordDelay(name string, delay int32) {
	s.delayMu.Lock()
	defer s.delayMu.Unlock()
	if s.delayHistory == nil {
		s.delayHistory = make(map[string][]DelaySample)
	}
	list := append(s.delayHistory[name], DelaySample{Time: time.Now().Unix(), Delay: delay})
	if len(list) > delayHistoryKeep {
		list = list[len(list)-delayHistoryKeep:]
	}
	s.delayHistory[name] = list
}

// ---------- 规则可视化 ----------

// RuleView 给前端展示的规则条目。
type RuleView struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
	Proxy   string `json:"proxy"`
}

// ListRules 返回当前生效的分流规则，顺序即匹配顺序。
func (s *CoreService) ListRules() ([]RuleView, error) {
	if s.api == nil {
		return nil, fmt.Errorf("内核未就绪")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rules, err := s.api.Rules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RuleView, 0, len(rules))
	for _, r := range rules {
		out = append(out, RuleView{Type: r.Type, Payload: r.Payload, Proxy: r.Proxy})
	}
	return out, nil
}

// startKernelWatchdog 内核崩溃自动重启。连续失败 3 次后放弃，避免死循环。
func (s *CoreService) startKernelWatchdog() {
	failCount := 0
	s.mgr.OnExit = func(reason string) {
		s.dbg("kernel exited unexpectedly: %s (quitting=%v)", reason, s.Quitting())
		if s.Quitting() {
			return
		}
		failCount++
		if failCount > 3 {
			s.app.Event.Emit("kernel-crashed", "内核连续崩溃 3 次，已停止自动重启，请查看数据目录下的 debug.log")
			return
		}
		// 第一次崩就悄悄拉起，不打扰用户；连着崩才值得弹通知。
		if failCount >= 2 {
			s.app.Event.Emit(
				"kernel-crashed",
				fmt.Sprintf("内核反复退出（第 %d 次：%s），正在重试", failCount, reason),
			)
		}
		s.dbg("kernel restart scheduled in %ds (failCount=%d)", failCount*2, failCount)
		time.Sleep(time.Duration(failCount*2) * time.Second)
		if s.Quitting() {
			return
		}
		if err := s.RestartKernel(); err == nil {
			failCount = 0
		}
	}
}

// probeTwice 对同一目标测两次：第一次失败隔 3 秒复测，两次都失败才算失败。
// 探测要求「5 秒内完成建连+TLS+HTTP」，节点繁忙时会偶发顶满超时，
// 而真实流量走长连接根本不受影响——实测约 20% 偶发 Timeout 却曾
// 连续两次撞线触发"网络不通"误报。复测一次过滤这种抖动。
func probeTwice(probe func() (int, error), retryDelay time.Duration) bool {
	if d, err := probe(); err == nil && d > 0 {
		return true
	}
	time.Sleep(retryDelay)
	d, err := probe()
	return err == nil && d > 0
}

// startNetworkHealthCheck 每 2 分钟测一次代理链路是否通。
// 连续 2 次失败才告警（避免瞬时波动误报）；告警只在恢复前发一次，
// 不再每 4 分钟重复弹窗。
func (s *CoreService) startNetworkHealthCheck() {
	failCount := 0
	alerted := false
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		// 首次检查等 30 秒，等内核完全就绪
		time.Sleep(30 * time.Second)
		for {
			select {
			case <-ticker.C:
			}
			if s.Quitting() {
				return
			}
			// 内核没在运行时检测没有意义，否则用户主动停掉内核后
			// 会一直收到"代理链路不通"的误报。
			if s.mgr == nil || !s.mgr.IsRunning() || s.api == nil {
				failCount, alerted = 0, false
				continue
			}

			s.mu.RLock()
			mode := s.mode
			group := configgen.MainProxyGroup(s.subConfig)
			s.mu.RUnlock()

			// 直连模式没有代理链路可测；没有可用策略组时也不该断言代理不通。
			// 测主策略组而不是 GLOBAL：GLOBAL 默认指向 DIRECT，测它等于测直连，
			// 代理真的坏了也发现不了。
			if mode == "direct" || group == "" {
				failCount, alerted = 0, false
				continue
			}

			ok := probeTwice(func() (int, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				return s.api.Delay(ctx, group, "https://www.gstatic.com/generate_204", 5000)
			}, 3*time.Second)
			if !ok {
				failCount++
				if failCount >= 2 && !alerted {
					alerted = true
					if s.app != nil {
						s.app.Event.Emit("network-degraded", "代理链路连续 2 次检测失败，网络可能不通")
					}
				}
			} else {
				failCount, alerted = 0, false
			}
		}
	}()
}

// ---------- 流量告警 ----------

// TrafficAlertLimit 日流量告警阈值（字节）。0 表示不告警。
func (s *CoreService) TrafficAlertLimit() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.alertLimit
}

// SetTrafficAlertLimit 设置日流量告警阈值（MB）。0 关闭告警。
func (s *CoreService) SetTrafficAlertLimit(mb int64) {
	if mb < 0 {
		mb = 0
	}
	s.mu.Lock()
	s.alertLimit = mb * 1024 * 1024
	s.mu.Unlock()
}

// checkTrafficAlertToday 检查今日累计流量是否超阈值。
//
// "当日只告警一次"按日期判断，而不是一个进程级的布尔值——否则程序
// 连开几天也只在第一天提醒一次。状态收进 CoreService 并加锁，
// 避免采样 goroutine 与设置调用并发读写。
func (s *CoreService) checkTrafficAlertToday() {
	limit := s.TrafficAlertLimit()
	if limit <= 0 || s.app == nil {
		return
	}

	now := time.Now()
	today := now.Format("2006-01-02")

	s.mu.RLock()
	already := s.alertSentDay == today
	s.mu.RUnlock()
	if already {
		return
	}

	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	s.histMu.Lock()
	var up, down int64
	for _, sample := range s.history {
		if sample.Time >= todayStart {
			up += sample.Up
			down += sample.Down
		}
	}
	s.histMu.Unlock()

	total := up + down
	if total < limit {
		return
	}

	// 再检查一次并置位，防止并发重复弹窗
	s.mu.Lock()
	if s.alertSentDay == today {
		s.mu.Unlock()
		return
	}
	s.alertSentDay = today
	s.mu.Unlock()

	s.app.Event.Emit("traffic-alert", fmt.Sprintf("今日流量已达 %s，超过设定阈值", formatBytes(total)))
}

// formatBytes 字节转人类可读。
func formatBytes(n int64) string {
	f := float64(n)
	switch {
	case f >= 1<<30:
		return fmt.Sprintf("%.2f GB", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.1f MB", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.0f KB", f/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ---------- 日志导出 ----------

// ExportLogs 返回当前日志内容（纯文本，每行一条）。
func (s *CoreService) ExportLogs() string {
	if s.mgr == nil {
		return ""
	}
	return s.mgr.Logs.Dump()
}

// ---------- 节点收藏 ----------

// HomeLocation 返回地图上"本机"的位置。
type HomeLocation struct {
	Lon   float64 `json:"lon"`
	Lat   float64 `json:"lat"`
	Label string  `json:"label"`
}

// HomeLocation 读取当前设置的本机坐标。
func (s *CoreService) HomeLocation() HomeLocation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	label := s.homeLabel
	if label == "" {
		label = "本机"
	}
	return HomeLocation{Lon: s.homeLon, Lat: s.homeLat, Label: label}
}

// SetHomeLocation 设置本机坐标。地图的起点和连线都以它为基准。
func (s *CoreService) SetHomeLocation(lon, lat float64, label string) error {
	if lon < -180 || lon > 180 {
		return fmt.Errorf("经度必须在 -180 ~ 180 之间")
	}
	if lat < -90 || lat > 90 {
		return fmt.Errorf("纬度必须在 -90 ~ 90 之间")
	}
	if strings.TrimSpace(label) == "" {
		label = "本机"
	}
	s.mu.Lock()
	s.homeLon, s.homeLat, s.homeLabel = lon, lat, label
	s.mu.Unlock()
	s.saveAppState()
	return nil
}

// FavoriteNodes 返回收藏的节点名列表。
func (s *CoreService) FavoriteNodes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.favorites))
	for name, v := range s.favorites {
		if v {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// IsFavorite 判断节点是否已收藏。
func (s *CoreService) IsFavorite(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.favorites[name]
}

// ToggleFavorite 切换收藏状态，返回切换后的状态。
func (s *CoreService) ToggleFavorite(name string) bool {
	s.mu.Lock()
	if s.favorites == nil {
		s.favorites = make(map[string]bool)
	}
	s.favorites[name] = !s.favorites[name]
	on := s.favorites[name]
	if !on {
		delete(s.favorites, name)
	}
	s.mu.Unlock()
	s.saveAppState()
	return on
}

// ---------- 托盘快捷操作 ----------

// Mode 返回当前代理模式。
func (s *CoreService) Mode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

// pickSelectorGroup 确定性地挑一个用户策略组。
//
// map 迭代顺序是随机的，"取第一个 Selector" 在多策略组订阅里会每次
// 选到不同的组，表现为托盘菜单偶尔选错节点。规则改为：
// 成员多的优先（通常是主选择组），成员数相同再按名字排序。
// mustContain 非空时只考虑包含该节点的组。
func pickSelectorGroup(proxies map[string]kernel.Proxy, mustContain string) string {
	names := make([]string, 0, len(proxies))
	for name, p := range proxies {
		if p.Type != "Selector" || builtinGroupNames[name] ||
			strings.HasPrefix(name, configgen.ProcessGroupPrefix) {
			continue
		}
		if mustContain != "" {
			ok := false
			for _, m := range p.All {
				if m == mustContain {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return ""
	}
	sort.Slice(names, func(i, j int) bool {
		li, lj := len(proxies[names[i]].All), len(proxies[names[j]].All)
		if li != lj {
			return li > lj
		}
		return names[i] < names[j]
	})
	return names[0]
}

// QuickNodes 返回 topN 个延迟最低的节点名和当前选中节点。
// 托盘菜单打开时调用，要快，只走已缓存的测速数据。
func (s *CoreService) QuickNodes(topN int) ([]string, string) {
	if s.api == nil {
		return nil, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxies, _, err := s.allProxies(ctx)
	if err != nil {
		return nil, ""
	}

	active := ""
	if group := pickSelectorGroup(proxies, ""); group != "" {
		active = proxies[group].Now
	}

	type nodeInfo struct {
		name  string
		delay int
	}
	var nodes []nodeInfo
	for name, p := range proxies {
		if builtinGroupNames[name] {
			continue
		}
		switch p.Type {
		case "Selector", "URLTest", "Fallback", "LoadBalance":
			continue
		}
		d := 99999
		s.delayMu.Lock()
		if hist := s.delayHistory[name]; len(hist) > 0 {
			last := hist[len(hist)-1]
			if last.Delay > 0 {
				d = int(last.Delay)
			}
		}
		s.delayMu.Unlock()
		nodes = append(nodes, nodeInfo{name, d})
	}
	sort.Slice(nodes, func(i, j int) bool {
		fi := s.IsFavorite(nodes[i].name)
		fj := s.IsFavorite(nodes[j].name)
		if fi != fj {
			return fi // 收藏的排前面
		}
		return nodes[i].delay < nodes[j].delay
	})
	if len(nodes) > topN {
		nodes = nodes[:topN]
	}
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.name
	}
	return names, active
}

// QuickSelectNode 快速切换当前出口节点。
func (s *CoreService) QuickSelectNode(name string) error {
	if s.api == nil {
		return fmt.Errorf("内核未就绪")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	proxies, err := s.api.Proxies(ctx)
	if err != nil {
		return err
	}
	// 优先选包含该节点的策略组，没有则退回主选择组
	group := pickSelectorGroup(proxies, name)
	if group == "" {
		group = pickSelectorGroup(proxies, "")
	}
	if group == "" {
		return fmt.Errorf("找不到策略组")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	return s.api.Select(ctx2, group, name)
}

// ---------- 内核版本更新 ----------

// CheckCoreUpdate 查询 mihomo 的最新 release，和当前内核版本对比。
// 版本号从内核的外部控制 API 读，读不到就返回空（前端显示"未知"）。
func (s *CoreService) CheckCoreUpdate() (*kernel.LatestInfo, error) {
	cur := ""
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if v, err := s.api.Version(ctx); err == nil {
		cur = v.Version
	}
	// 国内访问 GitHub 可能很慢，给 25 秒
	return kernel.CheckUpdate(cur, 25*time.Second)
}

// UpdateCore 下载新内核并替换，随后重启内核让新版本生效。
// 返回新内核的 sha256 前 12 位。
//
// url 参数只用于比对：真正下载的地址由后端重新向 GitHub 查询后决定。
// 否则前端（或任何能调用本方法的东西）传一个任意 URL 就能让我们
// 下载并执行任意程序——TUN 场景下还是管理员权限。
func (s *CoreService) UpdateCore(url string) (string, error) {
	if s.mgr == nil {
		return "", fmt.Errorf("内核管理器未初始化")
	}

	info, err := kernel.CheckUpdate(s.Status().Version, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("无法确认最新内核版本，已取消升级: %w", err)
	}
	// 已经是最新版就没必要停内核、断掉全部连接再换一个相同的文件。
	// Current 为空说明内核没在运行、读不到版本，此时不拦，交给用户决定。
	if info.Current != "" && !info.HasUpdate {
		return "", fmt.Errorf("当前已是最新内核版本 %s", info.Latest)
	}
	if url != "" && url != info.URL {
		return "", fmt.Errorf("下载地址与官方发布不一致，已拒绝")
	}

	// exe 被占用时替换会失败，先停内核
	if err := s.StopKernel(); err != nil {
		return "", fmt.Errorf("停止内核失败: %w", err)
	}

	sum, err := s.mgr.DownloadAndReplace(info.URL, info.Digest, 3*time.Minute)
	if err != nil {
		// 下载/替换失败要把服务拉回来，不能让用户断网
		_ = s.RestartKernel()
		return "", err
	}

	if err := s.RestartKernel(); err != nil {
		return sum, fmt.Errorf("内核已替换但重启失败: %w", err)
	}
	return sum, nil
}

// ResetCore 回退到内置内核：清除升级标记、删掉磁盘上的内核文件。
func (s *CoreService) ResetCore() error {
	if s.mgr == nil {
		return fmt.Errorf("内核管理器未初始化")
	}
	_ = s.StopKernel()
	kernel.ClearCustomMark(s.mgr.DataDir())
	_ = os.Remove(filepath.Join(s.mgr.DataDir(), "bin", "mihomo.exe"))
	return s.RestartKernel()
}

// CoreCustomized 磁盘上的内核是否已被手动升级（而非内置版本）。
func (s *CoreService) CoreCustomized() bool {
	if s.mgr == nil {
		return false
	}
	return kernel.IsCustomCore(s.mgr.DataDir())
}

// ---------- 开机自启 ----------

func (s *CoreService) AutoStart() bool {
	return autostart.Enabled()
}

func (s *CoreService) SetAutoStart(on bool) error {
	if err := autostart.Set(on, s.admin()); err != nil {
		return fmt.Errorf("设置开机自启失败: %w", err)
	}
	return nil
}

// IsAdmin 让界面知道当前有没有管理员权限。
func (s *CoreService) IsAdmin() bool {
	return s.admin()
}

// RelaunchElevated 以管理员身份重启自己。成功后当前进程退出。
func (s *CoreService) RelaunchElevated() error {
	if elevate.IsElevated() {
		return fmt.Errorf("已经是管理员权限")
	}
	if err := elevate.RelaunchAsAdmin(); err != nil {
		return err
	}
	// 先停内核、还原系统代理，再退出，把单实例锁让给新进程。
	s.Shutdown()
	go func() {
		time.Sleep(400 * time.Millisecond)
		os.Exit(0)
	}()
	return nil
}

// ---------- 连接 ----------

// connCacheTTL 是连接表缓存窗口。前端每轮会同时请求 Traffic 和 Connections，
// 两个接口各拉一次整表；500ms 内复用同一份响应，既省内核处理也省 IPC 序列化。
const connCacheTTL = 500 * time.Millisecond

// connections 返回连接表，带短缓存与并发合并。
// 锁覆盖整个请求过程：并发进来的第二个调用会等第一个完成，然后直接命中缓存。
func (s *CoreService) connections(ctx context.Context) (*kernel.ConnectionsResp, error) {
	if s.api == nil {
		return nil, fmt.Errorf("内核未就绪")
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.connCache != nil && time.Since(s.connCacheAt) < connCacheTTL {
		return s.connCache, nil
	}
	resp, err := s.api.Connections(ctx)
	if err != nil {
		return nil, err
	}
	s.connCache = resp
	s.connCacheAt = time.Now()
	return resp, nil
}

func (s *CoreService) invalidateConnCache() {
	s.connMu.Lock()
	s.connCache = nil
	s.connCacheAt = time.Time{}
	s.connMu.Unlock()
}

func (s *CoreService) Connections() ([]ConnView, error) {
	out := []ConnView{}
	if s.api == nil {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resp, err := s.connections(ctx)
	if err != nil {
		return out, err
	}
	for _, c := range resp.Connections {
		host := c.Metadata.Host
		if host == "" {
			host = c.Metadata.DestinationIP
		}
		proc := c.Metadata.Process
		if proc == "" && c.Metadata.ProcessPath != "" {
			proc = filepath.Base(c.Metadata.ProcessPath)
		}
		if proc == "" {
			proc = "—"
		}
		dest := c.Metadata.DestinationIP
		if c.Metadata.DestinationPort != "" {
			dest = net.JoinHostPort(c.Metadata.DestinationIP, c.Metadata.DestinationPort)
		}
		out = append(out, ConnView{
			ID:          c.ID,
			Host:        host,
			Dest:        dest,
			DestIP:      c.Metadata.DestinationIP,
			Process:     proc,
			ProcessPath: c.Metadata.ProcessPath,
			Chains:      strings.Join(c.Chains, " → "),
			Rule:        c.Rule,
			Upload:      c.Upload,
			Download:    c.Download,
			Start:       c.Start,
			Network:     c.Metadata.Network,
		})
	}
	return out, nil
}

func (s *CoreService) CloseConnection(id string) error {
	if s.api == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.api.CloseConnection(ctx, id)
	s.invalidateConnCache()
	return err
}

// CloseAllConnections 断开全部活动连接。
// 改完进程策略后想让老连接立刻套用新规则时，这是唯一的手段——
// mihomo 只在建连时匹配规则，已经建立的连接不会自动改道。
func (s *CoreService) CloseAllConnections() error {
	if s.api == nil {
		return fmt.Errorf("内核未就绪")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.api.CloseAllConnections(ctx)
	s.invalidateConnCache()
	return err
}

// ---------- 流量 ----------

// Traffic 返回累计流量与瞬时速率。速率由两次采样差分得出。
func (s *CoreService) Traffic() TrafficView {
	out := TrafficView{}
	if s.api == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := s.connections(ctx)
	if err != nil {
		return out
	}
	now := time.Now()
	s.trafficMu.Lock()
	if !s.lastTick.IsZero() {
		dt := now.Sub(s.lastTick).Seconds()
		if dt > 0.2 {
			s.rateUp = int64(float64(resp.UploadTotal-s.lastUp) / dt)
			s.rateDown = int64(float64(resp.DownloadTotal-s.lastDown) / dt)
			if s.rateUp < 0 {
				s.rateUp = 0
			}
			if s.rateDown < 0 {
				s.rateDown = 0
			}
		}
	}
	s.lastUp = resp.UploadTotal
	s.lastDown = resp.DownloadTotal
	s.lastTick = now
	up, down := s.rateUp, s.rateDown
	s.trafficMu.Unlock()

	out.UpTotal = resp.UploadTotal
	out.DownTotal = resp.DownloadTotal
	out.UpRate = up
	out.DownRate = down
	return out
}

// ---------- 流量历史 ----------

// TrafficSample 是一分钟的流量样本。
type TrafficSample struct {
	Time int64 `json:"t"` // 该分钟起点的 Unix 秒
	Up   int64 `json:"u"` // 上行字节
	Down int64 `json:"d"` // 下行字节
}

// historyKeep 最多保留 7 天的分钟样本：7*24*60 = 10080 条。
const historyKeep = 7 * 24 * 60

// TrafficHistory 返回最近的分钟级流量样本，按时间升序。
func (s *CoreService) TrafficHistory() []TrafficSample {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	out := make([]TrafficSample, len(s.history))
	copy(out, s.history)
	return out
}

// historyPath 流量历史的落盘路径。
func (s *CoreService) historyPath() string {
	return filepath.Join(dataDir(), "traffic-history.json")
}

// loadHistory 启动时读历史。
func (s *CoreService) loadHistory() {
	data, err := os.ReadFile(s.historyPath())
	if err != nil {
		return
	}
	var list []TrafficSample
	if err := json.Unmarshal(data, &list); err != nil {
		return
	}
	s.histMu.Lock()
	s.history = list
	s.histMu.Unlock()
}

// saveHistory 落盘。数据量不大（最多 1 万条），直接整体写。
func (s *CoreService) saveHistory() {
	s.histMu.Lock()
	data, err := json.Marshal(s.history)
	s.histMu.Unlock()
	if err != nil {
		return
	}
	_ = os.WriteFile(s.historyPath(), data, 0o644)
}

// appendSample 追加一条样本，并裁剪到保留窗口内。
func (s *CoreService) appendSample(sample TrafficSample) {
	s.histMu.Lock()
	s.history = append(s.history, sample)
	if len(s.history) > historyKeep {
		s.history = s.history[len(s.history)-historyKeep:]
	}
	s.histMu.Unlock()
}

// startHistorySampler 每分钟从 mihomo 读累计流量，差分出这一分钟的实际用量。
// 用累计值而不是瞬时速率累加，更准、也更省资源。
func (s *CoreService) startHistorySampler() {
	s.loadHistory()

	var lastUp, lastDown int64
	var lastMin int64

	ticker := time.NewTicker(time.Minute)
	go func() {
		defer ticker.Stop()
		for range ticker.C {
			if s.Quitting() {
				return
			}
			if s.api == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			resp, err := s.api.Connections(ctx)
			cancel()
			if err != nil {
				continue
			}

			now := time.Now()
			min := now.Truncate(time.Minute).Unix()

			// 第一次只记基线，不出样本
			if lastMin == 0 {
				lastUp, lastDown, lastMin = resp.UploadTotal, resp.DownloadTotal, min
				continue
			}

			up := resp.UploadTotal - lastUp
			down := resp.DownloadTotal - lastDown
			// mihomo 重启会把累计值清零，差值变负 —— 记 0 重新开始
			if up < 0 {
				up = 0
			}
			if down < 0 {
				down = 0
			}

			// 如果中间空了几分钟（比如睡眠），补空样本，避免曲线断层
			for t := lastMin + 60; t < min; t += 60 {
				s.appendSample(TrafficSample{Time: t, Up: 0, Down: 0})
			}

			s.appendSample(TrafficSample{Time: min, Up: up, Down: down})
			lastUp, lastDown, lastMin = resp.UploadTotal, resp.DownloadTotal, min

			// 检查当日流量告警（用今日累计值）
			s.checkTrafficAlertToday()

			s.saveHistory()
		}
	}()
}

// ---------- 模式与日志 ----------

func (s *CoreService) SetMode(mode string) error {
	if s.api == nil {
		return fmt.Errorf("内核未就绪")
	}
	switch mode {
	case "rule", "global", "direct":
	default:
		return fmt.Errorf("不支持的模式: %s", mode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 切到全局模式前检查 GLOBAL 组的出口。
	//
	// GLOBAL 是 mihomo 内置组，默认指向 DIRECT——直接切过去等于全机断网，
	// 用户看到的就是"点了没反应"（其实是网没了）。
	// 但只在它还是 DIRECT 时才兜底：用户自己选过出口就尊重他的选择，
	// 这也是 Clash Verge 的处理方式（他们把 GLOBAL 组置顶让用户自己挑）。
	if mode == "global" {
		if px, err := s.api.Proxies(ctx); err == nil {
			if g, ok := px["GLOBAL"]; ok && strings.EqualFold(g.Now, "DIRECT") {
				s.mu.RLock()
				group := configgen.MainProxyGroup(s.subConfig)
				s.mu.RUnlock()
				if group != "" {
					_ = s.api.Select(ctx, "GLOBAL", group)
				}
			}
		}
	}

	if err := s.api.SetMode(ctx, mode); err != nil {
		return err
	}

	s.mu.Lock()
	s.mode = mode
	s.mu.Unlock()
	s.saveAppState()
	return nil
}

func (s *CoreService) Logs() []string {
	if s.mgr == nil {
		return []string{}
	}
	return s.mgr.Logs.Snapshot()
}

// LogDelta 是增量日志的一次返回：新增行 + 最新序号 + 是否截断。
type LogDelta struct {
	Lines []string `json:"lines"`
	Seq   uint64   `json:"seq"`
	// Truncated 为 true 表示游标之后的部分已经被滚出缓冲区，
	// 本次返回的是当前全部残留，前端可据此提示"更早日志已丢弃"。
	Truncated bool `json:"truncated"`
}

// LogsSince 增量拉取日志。since 传 0 表示首次全量。
// 前端下一轮把上次的 Seq 带回来即可，避免每轮重复传整个缓冲。
func (s *CoreService) LogsSince(since uint64) LogDelta {
	if s.mgr == nil {
		return LogDelta{Lines: []string{}}
	}
	lines, seq, truncated := s.mgr.Logs.Since(since)
	return LogDelta{Lines: lines, Seq: seq, Truncated: truncated}
}

func (s *CoreService) ClearLogs() {
	if s.mgr != nil {
		s.mgr.Logs.Clear()
	}
}

// OpenDataDir 在资源管理器里打开数据目录，方便看配置文件。
func (s *CoreService) OpenDataDir() error {
	dir := dataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return exec.Command("explorer", dir).Start()
}

// ---------- 订阅 ----------

const (
	subConfigFile = "subscription.yaml"
	subMetaFile   = "subscription.json"
	subsFile      = "subs.json" // 多订阅列表
	appStateFile  = "app-state.json"
)

// appState 是用户上次退出时的运行状态，重启后自动恢复。
//
// 注意 TUN 的特殊性：开启它需要管理员权限，所以保存了 true
// 但这次是普通权限启动的，就只能保持关闭、提示用户以管理员重启。
type appState struct {
	TUN       bool            `json:"tun"`
	ProxyOn   bool            `json:"proxyOn"`
	Mode      string          `json:"mode"`
	WinX      int             `json:"winX"`
	WinY      int             `json:"winY"`
	WinW      int             `json:"winW"`
	WinH      int             `json:"winH"`
	Favorites map[string]bool `json:"favorites"`

	// TunPrevProxyOn 表示开 TUN 时临时关闭了由本程序接管的系统代理。
	// 关 TUN 后据此恢复，避免切换模式导致系统代理状态丢失。
	TunPrevProxyOn bool `json:"tunPrevProxyOn,omitempty"`

	// WinHTTP 接管备份。数据是注册表二进制值（base64 存储），
	// Pending=true 表示设置仍处于被接管状态，需要在管理员进程里还原。
	WinHTTPBackup  []byte `json:"winHttpBackup,omitempty"`
	WinHTTPPending bool   `json:"winHttpPending,omitempty"`

	// 地图本机位置（不填则用默认值）
	HomeLon   *float64 `json:"homeLon,omitempty"`
	HomeLat   *float64 `json:"homeLat,omitempty"`
	HomeLabel string   `json:"homeLabel,omitempty"`
}

type subMeta struct {
	URL       string `json:"url"`
	Name      string `json:"name"`
	FetchedAt int64  `json:"fetchedAt"`
	Insecure  bool   `json:"insecure"`
	Upload    int64  `json:"upload"`
	Download  int64  `json:"download"`
	Total     int64  `json:"total"`
	Expire    int64  `json:"expire"`
	WebPage   string `json:"webPage"`
	UpdateHrs int    `json:"updateHrs"`
}

// ---------- 多订阅 ----------

// subsStore 是订阅列表的磁盘格式。
type subsStore struct {
	Items  []subMeta `json:"items"`
	Active int       `json:"active"`
}

// subFileName 为每个订阅的原始配置起个稳定文件名。
// 命名规则统一放在 subscription 包里，base64 订阅的 file provider
// 会用同一个文件名定位原文。
func subFileName(rawURL string) string {
	return subscription.RawFileName(rawURL)
}

// writeSubscriptionFiles 落盘订阅原文，以及 base64 订阅给内核 provider
// 准备的解码后节点列表。两者成对写入，避免内核启动时 provider 文件缺失。
func (s *CoreService) writeSubscriptionFiles(url string, res *subscription.Result) error {
	dir := dataDir()
	if err := os.WriteFile(filepath.Join(dir, subFileName(url)), res.Raw, 0o600); err != nil {
		return fmt.Errorf("保存订阅配置失败: %w", err)
	}

	providerPath := filepath.Join(dir, subscription.ProviderFileName(url))
	if len(res.ProviderContent) > 0 {
		if err := os.WriteFile(providerPath, res.ProviderContent, 0o600); err != nil {
			return fmt.Errorf("保存订阅节点列表失败: %w", err)
		}
	} else {
		// 订阅从 base64 变成 Clash 时，清掉遗留的 provider 文件。
		_ = os.Remove(providerPath)
	}
	return nil
}

func (s *CoreService) loadSubs() {
	dir := dataDir()
	b, err := os.ReadFile(filepath.Join(dir, subsFile))
	if err != nil {
		// 没有多订阅文件：看有没有旧的单订阅文件，有就迁移成列表的第 0 个。
		s.migrateSingleSub()
		return
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	var st subsStore
	if err := json.Unmarshal(b, &st); err != nil {
		return
	}
	if len(st.Items) == 0 {
		return
	}
	if st.Active < 0 || st.Active >= len(st.Items) {
		st.Active = 0
	}
	s.mu.Lock()
	s.subs = st.Items
	s.activeIdx = st.Active
	s.mu.Unlock()
}

// migrateSingleSub 把旧版的单订阅文件搬到新的多订阅列表里，
// 让升级后不用重新填一遍地址。
func (s *CoreService) migrateSingleSub() {
	dir := dataDir()
	mb, err := os.ReadFile(filepath.Join(dir, subMetaFile))
	if err != nil {
		return
	}
	mb = bytes.TrimPrefix(mb, []byte{0xEF, 0xBB, 0xBF})
	var m subMeta
	if err := json.Unmarshal(mb, &m); err != nil || m.URL == "" {
		return
	}
	// 把当前那份原始配置搬到 per-subscription 文件。
	if cb, err := os.ReadFile(filepath.Join(dir, subConfigFile)); err == nil {
		_ = os.WriteFile(filepath.Join(dir, subFileName(m.URL)), cb, 0o600)
	}
	s.mu.Lock()
	s.subs = []subMeta{m}
	s.activeIdx = 0
	s.mu.Unlock()
	s.saveSubs()
}

func (s *CoreService) saveSubs() {
	s.mu.RLock()
	st := subsStore{Items: s.subs, Active: s.activeIdx}
	s.mu.RUnlock()
	if mb, err := json.MarshalIndent(st, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dataDir(), subsFile), mb, 0o600)
	}
}

// ListSubscriptions 返回所有订阅及当前激活的是哪个。
func (s *CoreService) ListSubscriptions() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.subs))
	for _, m := range s.subs {
		out = append(out, map[string]any{
			"url":       m.URL,
			"name":      m.Name,
			"fetchedAt": m.FetchedAt,
			"upload":    m.Upload,
			"download":  m.Download,
			"total":     m.Total,
			"expire":    m.Expire,
			// 缺这个字段前端就永远看不到"恢复证书校验"按钮，
			// 一旦点过忽略证书就无法再关掉——安全设置单项锁死。
			"insecure": m.Insecure,
			"active":   m.URL == s.subURL,
		})
	}
	return map[string]any{"items": out, "active": s.activeIdx}
}

// SwitchSubscription 切到另一个已保存的订阅。
func (s *CoreService) SwitchSubscription(idx int) error {
	s.mu.RLock()
	if idx < 0 || idx >= len(s.subs) {
		s.mu.RUnlock()
		return fmt.Errorf("订阅序号越界: %d", idx)
	}
	meta := s.subs[idx]
	s.mu.RUnlock()

	b, err := os.ReadFile(filepath.Join(dataDir(), subFileName(meta.URL)))
	if err != nil {
		return fmt.Errorf("找不到该订阅的本地配置，请点刷新重新拉取: %w", err)
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	res, err := subscription.Parse(b, meta.URL, meta.Insecure)
	if err != nil {
		return fmt.Errorf("订阅配置损坏: %w", err)
	}
	if err := s.writeSubscriptionFiles(meta.URL, res); err != nil {
		return err
	}

	info := subInfoFromResult(meta, res)

	s.mu.Lock()
	s.activeIdx = idx
	s.subURL = meta.URL
	s.subConfig = res.Config
	s.subRaw = b
	s.subInfo = info
	s.subTargets, s.subProviders = subscriptionTargets(res)
	s.mu.Unlock()

	s.saveSubs()
	_ = s.writeProcRulesYAML()
	if s.mgr != nil && s.mgr.IsRunning() {
		return s.RestartKernel()
	}
	return nil
}

// SwitchSubscriptionByURL 按 URL 切换订阅。理由同 RemoveSubscriptionByURL：
// 下标会漂移，URL 才是稳定标识。
func (s *CoreService) SwitchSubscriptionByURL(url string) error {
	url = strings.TrimSpace(url)
	s.mu.RLock()
	idx := -1
	for i, m := range s.subs {
		if m.URL == url {
			idx = i
			break
		}
	}
	s.mu.RUnlock()
	if idx < 0 {
		return fmt.Errorf("找不到该订阅（可能已被删除）")
	}
	return s.SwitchSubscription(idx)
}

// SetSubInsecure 为某个订阅开启/关闭"忽略证书校验"。
// 只改标记不重新拉取，调用方需要自己再触发一次刷新。
func (s *CoreService) SetSubInsecure(url string, insecure bool) error {
	url = strings.TrimSpace(url)
	s.mu.Lock()
	found := false
	for i := range s.subs {
		if s.subs[i].URL == url {
			s.subs[i].Insecure = insecure
			found = true
			break
		}
	}
	if s.subURL == url {
		s.subInfo.Insecure = insecure
	}
	s.mu.Unlock()
	if !found {
		return fmt.Errorf("找不到该订阅")
	}
	s.saveSubs()
	s.persistSubMeta()
	return nil
}

// CopySubscriptionURL 把订阅地址写进剪贴板。
// 手机端（iOS/Android）的代理客户端都是靠这条链接导入节点的，
// 手动从 UI 里选中复制很费劲，这里给个一键复制。
func (s *CoreService) CopySubscriptionURL(url string) error {
	url = strings.TrimSpace(url)
	if url == "" {
		return fmt.Errorf("订阅地址为空")
	}
	if s.app == nil || s.app.Clipboard == nil {
		return fmt.Errorf("剪贴板不可用")
	}
	if !s.app.Clipboard.SetText(url) {
		return fmt.Errorf("写入剪贴板失败")
	}
	return nil
}

// RemoveSubscription 删掉一个订阅。删的正好是激活的那个就自动切到剩余的第一个。
func (s *CoreService) RemoveSubscription(idx int) error {
	return s.removeSubscription(idx, "")
}

// RemoveSubscriptionByURL 按 URL 删除订阅。
// 下标会随列表变化（自动更新追加、别处删除）而失效，前端应优先用 URL 定位。
func (s *CoreService) RemoveSubscriptionByURL(url string) error {
	return s.removeSubscription(-1, strings.TrimSpace(url))
}

// removeSubscription 是删除的统一实现。expectURL 非空时在同一个锁内按 URL
// 重新定位下标，彻底避开"校验完到删除之间列表变了"的竞态。
func (s *CoreService) removeSubscription(idx int, expectURL string) error {
	s.mu.Lock()
	if expectURL != "" {
		idx = -1
		for i, m := range s.subs {
			if m.URL == expectURL {
				idx = i
				break
			}
		}
		if idx < 0 {
			s.mu.Unlock()
			return fmt.Errorf("找不到该订阅（可能已被删除）")
		}
	}
	if idx < 0 || idx >= len(s.subs) {
		n := len(s.subs)
		s.mu.Unlock()
		return fmt.Errorf("订阅序号越界: %d（当前共 %d 个）", idx, n)
	}
	removed := s.subs[idx]
	wasActive := removed.URL == s.subURL

	s.subs = append(s.subs[:idx], s.subs[idx+1:]...)
	if len(s.subs) == 0 {
		s.activeIdx = 0
		s.subURL = ""
		s.subConfig = nil
		s.subRaw = nil
		s.subInfo = SubInfo{}
		s.subTargets = nil
		s.subProviders = nil
	} else if wasActive {
		s.activeIdx = 0
	} else if idx < s.activeIdx {
		// 删掉排在激活订阅前面的条目，后面的都往前挪一位，
		// 不修这个下标就会指向另一个订阅。
		s.activeIdx--
	}
	remaining := len(s.subs)
	s.mu.Unlock()

	_ = os.Remove(filepath.Join(dataDir(), subFileName(removed.URL)))
	_ = os.Remove(filepath.Join(dataDir(), subscription.ProviderFileName(removed.URL)))
	s.saveSubs()

	// 删掉的不是激活订阅，当前配置不受影响，不用重启。
	if !wasActive {
		return nil
	}

	// 删的是当前订阅：切到剩余的第一个。
	if remaining > 0 {
		return s.SwitchSubscription(0)
	}
	_ = os.Remove(filepath.Join(dataDir(), subConfigFile))
	_ = os.Remove(filepath.Join(dataDir(), subMetaFile))
	s.mu.Lock()
	s.subs = []subMeta{}
	s.activeIdx = 0
	s.subURL = ""
	s.subConfig = nil
	s.subRaw = nil
	s.subInfo = SubInfo{}
	s.mu.Unlock()
	if s.mgr != nil && s.mgr.IsRunning() {
		return s.RestartKernel()
	}
	return nil
}

func subInfoFromMeta(m subMeta) SubInfo {
	return SubInfo{
		URL:       m.URL,
		Name:      m.Name,
		FetchedAt: m.FetchedAt,
		Insecure:  m.Insecure,
		Upload:    m.Upload,
		Download:  m.Download,
		Total:     m.Total,
		Expire:    m.Expire,
		WebPage:   m.WebPage,
		UpdateHrs: m.UpdateHrs,
	}
}

// subMetaFromResult 把解析结果映射成落盘的订阅元信息。
func subMetaFromResult(url string, res *subscription.Result) subMeta {
	return subMeta{
		URL:       url,
		Name:      res.Name,
		FetchedAt: res.FetchedAt,
		Insecure:  res.Insecure,
		Upload:    res.Upload,
		Download:  res.Download,
		Total:     res.Total,
		Expire:    res.Expire,
		WebPage:   res.WebPage,
		UpdateHrs: res.UpdateHrs,
	}
}

// subInfoFromResult 把解析结果映射成界面用的订阅信息。
// 节点/组/规则数都取解析结果，Clash 与 base64 订阅走同一套逻辑。
func subInfoFromResult(m subMeta, res *subscription.Result) SubInfo {
	info := subInfoFromMeta(m)
	info.Nodes = len(res.Nodes)
	info.Groups = len(res.Groups)
	info.Rules = res.Rules
	return info
}

// subscriptionTargets 汇总当前订阅可用的进程出口目标。
// base64 订阅的节点来自 provider，不能在配置解析阶段直接写进组，
// 只能记录策略组；节点目标交给 provider 支撑的隐藏组处理。
func subscriptionTargets(res *subscription.Result) (map[string]bool, []string) {
	targets := make(map[string]bool, len(res.Nodes)+len(res.Groups))
	if res.Format != "base64" {
		for _, n := range res.Nodes {
			targets[n.Name] = true
		}
	}
	for _, g := range res.Groups {
		targets[g] = true
	}
	return targets, append([]string(nil), res.Providers...)
}

// loadSavedSubscription 读取上次落盘的订阅，让应用离线也能直接起。
func (s *CoreService) loadSavedSubscription() {
	dir := dataDir()

	// 先把订阅列表载进来（含旧格式迁移）。
	s.loadSubs()

	s.mu.RLock()
	var meta subMeta
	if len(s.subs) > 0 {
		if s.activeIdx < 0 || s.activeIdx >= len(s.subs) {
			s.activeIdx = 0
		}
		meta = s.subs[s.activeIdx]
	}
	s.mu.RUnlock()

	// 列表为空就退回旧的单订阅文件。
	if meta.URL == "" {
		mb, err := os.ReadFile(filepath.Join(dir, subMetaFile))
		if err != nil {
			return
		}
		mb = bytes.TrimPrefix(mb, []byte{0xEF, 0xBB, 0xBF})
		if json.Unmarshal(mb, &meta) != nil || meta.URL == "" {
			return
		}
	}

	cb, err := os.ReadFile(filepath.Join(dir, subFileName(meta.URL)))
	if err != nil {
		// 旧布局：配置直接放在 subscription.yaml。
		cb, err = os.ReadFile(filepath.Join(dir, subConfigFile))
		if err != nil {
			return
		}
	}
	cb = bytes.TrimPrefix(cb, []byte{0xEF, 0xBB, 0xBF})
	res, err := subscription.Parse(cb, meta.URL, meta.Insecure)
	if err != nil {
		s.dbg("load saved subscription failed: %v", err)
		return
	}
	if err := s.writeSubscriptionFiles(meta.URL, res); err != nil {
		s.dbg("write saved subscription failed: %v", err)
		return
	}

	info := subInfoFromResult(meta, res)

	s.mu.Lock()
	s.subURL = meta.URL
	s.subConfig = res.Config
	s.subRaw = cb
	s.subInfo = info
	s.subTargets, s.subProviders = subscriptionTargets(res)
	s.mu.Unlock()
}

// Subscribe 拉取订阅、落盘，并重启内核让新节点生效。
func (s *CoreService) Subscribe(url string) error {
	url = strings.TrimSpace(url)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 只有用户为这个订阅主动开过"忽略证书校验"才降级，默认走严格校验。
	s.mu.RLock()
	allowInsecure := false
	for _, m := range s.subs {
		if m.URL == url {
			allowInsecure = m.Insecure
			break
		}
	}
	s.mu.RUnlock()

	res, err := subscription.Fetch(ctx, url, allowInsecure)
	if err != nil {
		s.mu.Lock()
		s.subInfo.Error = err.Error()
		s.mu.Unlock()
		return err
	}

	return s.applySubscription(url, res)
}

// applySubscription 把已拉取/解析好的订阅落盘并设为激活。
// 单独抽出来供自动更新复用：避免自动更新时再拉一次同样的内容。
func (s *CoreService) applySubscription(url string, res *subscription.Result) error {
	dir := dataDir()
	if err := s.writeSubscriptionFiles(url, res); err != nil {
		return err
	}
	// 同时维护一份"当前订阅"，兼容旧路径与迁移。
	_ = os.WriteFile(filepath.Join(dir, subConfigFile), res.Raw, 0o600)

	meta := subMetaFromResult(url, res)
	if mb, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, subMetaFile), mb, 0o600)
	}

	s.mu.Lock()
	// 已存在就更新，不存在就追加为新订阅并设为激活。
	idx := -1
	for i, m := range s.subs {
		if m.URL == url {
			idx = i
			break
		}
	}
	if idx >= 0 {
		s.subs[idx] = meta
	} else {
		s.subs = append(s.subs, meta)
		idx = len(s.subs) - 1
	}
	s.activeIdx = idx
	s.subURL = url
	s.subConfig = res.Config
	s.subRaw = res.Raw
	s.subInfo = subInfoFromResult(meta, res)
	s.subTargets, s.subProviders = subscriptionTargets(res)
	s.mu.Unlock()

	s.saveSubs()

	// 订阅里的组名可能变了，进程规则的出口要跟着重写。
	_ = s.writeProcRulesYAML()

	// 内核没在跑就只落盘，等用户自己启动。
	if s.mgr != nil && s.mgr.IsRunning() {
		return s.RestartKernel()
	}
	return nil
}

// Subscription 返回当前订阅的元信息。
func (s *CoreService) Subscription() SubInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.subInfo
}

// RefreshSubscription 用已保存的地址重新拉取。订阅内容会变（换节点、改规则），
// 所以这个操作是必需的而不是锦上添花。
func (s *CoreService) RefreshSubscription() error {
	s.mu.RLock()
	url := s.subURL
	s.mu.RUnlock()
	if url == "" {
		return fmt.Errorf("还没有添加订阅")
	}
	return s.Subscribe(url)
}

// ClearSubscription 移除订阅，回到内置的直连配置。
func (s *CoreService) ClearSubscription() error {
	dir := dataDir()
	s.mu.RLock()
	url := s.subURL
	s.mu.RUnlock()
	_ = os.Remove(filepath.Join(dir, subConfigFile))
	_ = os.Remove(filepath.Join(dir, subMetaFile))
	if url != "" {
		_ = os.Remove(filepath.Join(dir, subscription.ProviderFileName(url)))
	}

	s.mu.Lock()
	s.subURL = ""
	s.subConfig = nil
	s.subInfo = SubInfo{}
	s.mu.Unlock()

	if s.mgr != nil && s.mgr.IsRunning() {
		return s.RestartKernel()
	}
	return nil
}

// allProxies 合并内核 /proxies 与 /providers/proxies。
// base64 订阅的节点只存在于 provider 里，不合并就看不到节点、
// 批量测速和托盘快速切换也会漏掉它们。owner 记录节点属于哪个 provider。
func (s *CoreService) allProxies(ctx context.Context) (map[string]kernel.Proxy, map[string]string, error) {
	if s.api == nil {
		return nil, nil, fmt.Errorf("内核未就绪")
	}
	proxies, err := s.api.Proxies(ctx)
	if err != nil {
		return nil, nil, err
	}
	providers, err := s.api.ProviderProxies(ctx)
	if err != nil {
		// 老版本内核没有 provider 接口时退化为普通节点，不影响主功能。
		return proxies, nil, nil
	}
	owner := make(map[string]string)
	for providerName, list := range providers {
		for _, p := range list {
			if _, exists := proxies[p.Name]; !exists {
				proxies[p.Name] = p
			}
			owner[p.Name] = providerName
		}
	}
	return proxies, owner, nil
}

// Nodes 从内核读取实时节点与策略组，附带最近一次延迟。
func (s *CoreService) Nodes() ([]NodeView, error) {
	out := []NodeView{}
	if s.api == nil {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	proxies, _, err := s.allProxies(ctx)
	if err != nil {
		return out, err
	}

	groupTypes := map[string]bool{
		"Selector": true, "URLTest": true, "Fallback": true, "LoadBalance": true,
	}
	for _, p := range proxies {
		// 进程自定义出口的隐藏组不当作普通策略组展示。
		if strings.HasPrefix(p.Name, configgen.ProcessGroupPrefix) {
			continue
		}
		delay := 0
		if len(p.History) > 0 {
			delay = p.History[len(p.History)-1].Delay
		}
		out = append(out, NodeView{
			Name:  p.Name,
			Type:  p.Type,
			Delay: delay,
			Group: groupTypes[p.Type],
			Now:   p.Now,
			Count: len(p.All),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// SelectNode 在策略组里切到指定节点。
func (s *CoreService) SelectNode(group, name string) error {
	if s.api == nil {
		return fmt.Errorf("内核未就绪")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	return s.api.Select(ctx, group, name)
}

// TestNode 对单个节点发起延迟测试，返回毫秒；失败返回 -1。
func (s *CoreService) TestNode(name string) int {
	if s.api == nil {
		return -1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	d, err := s.api.Delay(ctx, name, "https://www.gstatic.com/generate_204", 5000)
	if err == nil && d > 0 {
		return d
	}

	// base64 订阅的节点不在 /proxies 里，普通测速会 404，
	// 需要按所属 provider 走专门的单节点测速接口。
	_, owner, oerr := s.allProxies(ctx)
	if oerr != nil || owner[name] == "" {
		return -1
	}
	d, err = s.api.DelayProviderProxy(ctx, owner[name], name, "https://www.gstatic.com/generate_204", 5000)
	if err != nil || d <= 0 {
		return -1
	}
	return d
}

// TestAllNodes 并发测试所有叶子节点的延迟，返回 节点名 → 毫秒（-1 表示不通）。
//
// 并发度限制在 8：机场一般有 30+ 节点，全放开会把本地连接表打满，
// 而且测出来的延迟也不准（互相抢带宽）。
func (s *CoreService) TestAllNodes() map[string]int {
	out := map[string]int{}
	if s.api == nil {
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	proxies, owner, err := s.allProxies(ctx)
	cancel()
	if err != nil {
		return out
	}

	var names []string
	for name, p := range proxies {
		if builtinGroupNames[name] {
			continue
		}
		switch p.Type {
		case "Selector", "URLTest", "Fallback", "LoadBalance":
			continue // 策略组不是节点
		}
		names = append(names, name)
	}
	sort.Strings(names)

	const concurrency = 8
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func(n string) {
			defer wg.Done()
			defer func() { <-sem }()
			c, cc := context.WithTimeout(context.Background(), 15*time.Second)
			defer cc()

			var d int
			var err error
			if provider := owner[n]; provider != "" {
				d, err = s.api.DelayProviderProxy(c, provider, n, "https://www.gstatic.com/generate_204", 5000)
			} else {
				d, err = s.api.Delay(c, n, "https://www.gstatic.com/generate_204", 5000)
			}

			mu.Lock()
			var v int
			if err != nil || d <= 0 {
				v = -1
			} else {
				v = d
			}
			out[n] = v
			mu.Unlock()
			s.recordDelay(n, int32(v))
		}(name)
	}
	wg.Wait()
	return out
}

// ResolveTargets 把一批 IP 解析成地理位置，给地图标注连接目标用。
// 返回 {ip: {"country": "US", "lon": -98, "lat": 39}}，查不到的不含在内。
func (s *CoreService) ResolveTargets(ips []string) map[string]any {
	out := map[string]any{}
	if s.geoDB == nil || len(ips) == 0 {
		return out
	}
	// 去重。一批连接经常指向同一个 CDN IP，没必要查多次。
	seen := make(map[string]bool, len(ips))
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true

		cc := s.geoDB.Country(ip)
		if cc == "" {
			continue
		}
		lon, lat, ok := geoip.Location(cc)
		if !ok {
			continue
		}
		out[ip] = map[string]any{"country": cc, "lon": lon, "lat": lat}
	}
	return out
}

// ---------- 按程序分流 ----------

const (
	procRulesJSON   = "process-rules.json"
	procIgnoredJSON = "process-ignored.json"
	procDirectYAML  = "process-direct.yaml"
	procProxyYAML   = "process-proxy.yaml"
	procDirectProv  = "pp-direct"
	procProxyProv   = "pp-proxy"
)

// 常见系统进程，永远不用提示。
// 用户对 svchost.exe 做选择没有意义，只会制造噪音。
var systemProcWhitelist = map[string]bool{
	"system": true, "svchost.exe": true, "services.exe": true,
	"lsass.exe": true, "csrss.exe": true, "wininit.exe": true,
	"winlogon.exe": true, "dllhost.exe": true, "runtimebroker.exe": true,
	"smartscreen.exe": true, "msedgewebview2.exe": true, "conhost.exe": true,
	"fontdrvhost.exe": true, "audiodg.exe": true, "sihost.exe": true,
	"dwm.exe": true, "securityhealthservice.exe": true, "msmpeng.exe": true,
	"searchindexer.exe": true, "wermgr.exe": true, "taskhostw.exe": true,
	"explorer.exe": true, "ctfmon.exe": true,
}

// mihomo 内置的组/节点名，测速和分流时都要排除掉。
var builtinGroupNames = map[string]bool{
	"GLOBAL": true, "DIRECT": true, "REJECT": true, "REJECT-DROP": true,
	"COMPATIBLE": true, "PASS": true, "PASS-RULE": true, "PROXY": true,
}

// 策略组识别放在 configgen 里，配置生成和规则渲染都要用同一套判断。

func (s *CoreService) loadProcRules() {
	b, err := os.ReadFile(filepath.Join(dataDir(), procRulesJSON))
	if err != nil {
		return
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return
	}
	s.mu.Lock()
	s.procRules = m
	s.mu.Unlock()
}

// loadIgnored 读取"用户已经选择不再提示"的进程列表。
// 这个列表是必要的：否则每次开机重启应用，老进程又会被当成新的问一遍。
func (s *CoreService) loadIgnored() {
	b, err := os.ReadFile(filepath.Join(dataDir(), procIgnoredJSON))
	if err != nil {
		return
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	var m []string
	if err := json.Unmarshal(b, &m); err != nil {
		return
	}
	s.mu.Lock()
	s.ignored = make(map[string]bool, len(m))
	for _, p := range m {
		s.ignored[p] = true
	}
	s.mu.Unlock()
}

// IgnoreProcess 把某个程序加入忽略名单，之后不再提示。
func (s *CoreService) IgnoreProcess(proc string) error {
	proc = sanitizeProcName(proc)
	if proc == "" {
		return fmt.Errorf("进程名为空或含有非法字符")
	}
	s.mu.Lock()
	if s.ignored == nil {
		s.ignored = map[string]bool{}
	}
	s.ignored[proc] = true
	snapshot := make([]string, 0, len(s.ignored))
	for p := range s.ignored {
		snapshot = append(snapshot, p)
	}
	s.mu.Unlock()
	sort.Strings(snapshot)
	if mb, err := json.MarshalIndent(snapshot, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dataDir(), procIgnoredJSON), mb, 0o644)
	}
	return nil
}

func (s *CoreService) IgnoredProcesses() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.ignored))
	for p := range s.ignored {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// sanitizeProcName 校验进程名是否能安全地写进 PROCESS-NAME 规则。
//
// 规则格式是逗号分隔的（PROCESS-NAME,<name>,<policy>），所以进程名里
// 不能有逗号和换行。Windows 文件名是**允许**逗号的，`a,b.exe` 是合法
// 文件名——如果直接拼进规则文件，会被解析成三段，规则结构就毁了。
//
// 返回空串表示这个进程名不能用。
func sanitizeProcName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.ContainsAny(name, ",\n\r\t\x00") {
		return ""
	}
	// 进程名不该带路径分隔符，带了说明来源异常。
	if strings.ContainsAny(name, `/\`) {
		return ""
	}
	return name
}

// writeProcRulesYAML 把进程策略渲染成两个 rule-provider 文件。
//
// 为什么拆成两个：mihomo 的 classical provider 里 payload **不能自带出口策略**，
// 实测把 `PROCESS-NAME,curl.exe,REJECT` 写进去会被完全忽略、一律走外层的策略。
// 所以按"直连 / 代理"分文件，出口由配置里的 RULE-SET 指定。
func (s *CoreService) writeProcRulesYAML() error {
	s.mu.RLock()
	snapshot := make(map[string]string, len(s.procRules))
	for k, v := range s.procRules {
		snapshot[k] = v
	}
	s.mu.RUnlock()

	var proxyList, directList []string
	for proc, policy := range snapshot {
		switch policy {
		case "proxy":
			proxyList = append(proxyList, proc)
		case "direct":
			directList = append(directList, proc)
		}
	}
	sort.Strings(proxyList)
	sort.Strings(directList)

	render := func(names []string) string {
		lines := make([]string, 0, len(names))
		for _, n := range names {
			// 再校验一次。规则文件是拼字符串的，脏数据进去会破坏整张表。
			if sanitizeProcName(n) == "" {
				continue
			}
			lines = append(lines, "  - PROCESS-NAME,"+n)
		}
		if len(lines) == 0 {
			return "payload: []\n"
		}
		return "payload:\n" + strings.Join(lines, "\n") + "\n"
	}

	dir := dataDir()
	if err := os.WriteFile(filepath.Join(dir, procProxyYAML), []byte(render(proxyList)), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, procDirectYAML), []byte(render(directList)), 0o644)
}

// ---------- 运行状态持久化 ----------

func (s *CoreService) loadAppState() {
	b, err := os.ReadFile(filepath.Join(dataDir(), appStateFile))
	if err != nil {
		s.mu.Lock()
		s.mode = "rule"
		s.mu.Unlock()
		return
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	var st appState
	if err := json.Unmarshal(b, &st); err != nil {
		return
	}
	s.mu.Lock()
	s.restoreTUN = st.TUN
	// tunOn 是配置生成的实际依据，启动时必须跟着恢复态同步，
	// 否则上次开着 TUN 的用户这次启动配置里会没有 TUN。
	s.tunOn = st.TUN
	// 非管理员启动建不了虚拟网卡，界面不能显示"TUN 已开启"。
	// restoreTUN 保留原意，等用户提权重启后仍能按上次的意图恢复。
	if !s.admin() {
		s.tunOn = false
	}
	s.restoreProxy = st.ProxyOn
	s.tunPrevProxyOn = st.TunPrevProxyOn
	s.winHTTPPending = st.WinHTTPPending
	s.winHTTPBackup = append([]byte(nil), st.WinHTTPBackup...)
	s.winX, s.winY, s.winW, s.winH = st.WinX, st.WinY, st.WinW, st.WinH
	if st.HomeLon != nil && st.HomeLat != nil {
		s.homeLon, s.homeLat = *st.HomeLon, *st.HomeLat
	}
	if st.HomeLabel != "" {
		s.homeLabel = st.HomeLabel
	}
	if st.Favorites != nil {
		s.favorites = st.Favorites
	} else {
		s.favorites = make(map[string]bool)
	}
	switch st.Mode {
	case "rule", "global", "direct":
		s.mode = st.Mode
	default:
		s.mode = "rule"
	}
	s.mu.Unlock()
}

// dbg 把关键生命周期事件追加到 debug.log。
// GUI 进程没有控制台，内核反复重启这类问题事后只能靠这份记录定位。
func (s *CoreService) dbg(format string, a ...any) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("01-02 15:04:05"), fmt.Sprintf(format, a...))
	f, err := os.OpenFile(filepath.Join(dataDir(), "debug.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > 2<<20 {
		// 超过 2MB 重置，避免无限增长
		_ = f.Truncate(0)
	}
	_, _ = f.WriteString(line)
}

// saveAppState 实时落盘。不攒到退出时再写——崩溃也能恢复上次的状态。
func (s *CoreService) saveAppState() {
	s.mu.RLock()
	lon, lat := s.homeLon, s.homeLat
	// 拷贝一份收藏表。直接传 map 引用会让 json.Marshal 在锁外读取，
	// 与并发的 ToggleFavorite 写入构成 map 并发读写，极少数情况下会直接 panic。
	favorites := make(map[string]bool, len(s.favorites))
	for k, v := range s.favorites {
		favorites[k] = v
	}
	winBackup := append([]byte(nil), s.winHTTPBackup...)
	st := appState{
		TUN:            s.tunOn,
		ProxyOn:        s.proxyOn,
		Mode:           s.mode,
		TunPrevProxyOn: s.tunPrevProxyOn,
		WinX:           s.winX,
		WinY:           s.winY,
		WinW:           s.winW,
		WinH:           s.winH,
		Favorites:      favorites,
		WinHTTPBackup:  winBackup,
		WinHTTPPending: s.winHTTPPending,
		HomeLon:        &lon,
		HomeLat:        &lat,
		HomeLabel:      s.homeLabel,
	}
	s.mu.RUnlock()
	if st.Mode == "" {
		st.Mode = "rule"
	}
	if mb, err := json.MarshalIndent(st, "", "  "); err == nil {
		if err := writeFileAtomic(filepath.Join(dataDir(), appStateFile), mb, 0o600); err != nil {
			s.dbg("save app state failed: %v", err)
		}
	}
}

// SaveWindowBounds 记住窗口位置和大小。
func (s *CoreService) SaveWindowBounds(x, y, w, h int) {
	s.mu.Lock()
	s.winX, s.winY, s.winW, s.winH = x, y, w, h
	s.mu.Unlock()
	s.saveAppState()
}

// WindowBounds 返回上次保存的窗口位置和大小。
func (s *CoreService) WindowBounds() (x, y, w, h int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.winX, s.winY, s.winW, s.winH
}

// ---------- 订阅自动更新 ----------

// persistSubMeta 把订阅元信息写盘。自动更新时不拉配置、也要刷新已用流量，
// 所以这条路径必须能单独写 meta。
func (s *CoreService) persistSubMeta() {
	s.mu.RLock()
	m := subMeta{
		URL:       s.subURL,
		Name:      s.subInfo.Name,
		FetchedAt: s.subInfo.FetchedAt,
		Insecure:  s.subInfo.Insecure,
		Upload:    s.subInfo.Upload,
		Download:  s.subInfo.Download,
		Total:     s.subInfo.Total,
		Expire:    s.subInfo.Expire,
		WebPage:   s.subInfo.WebPage,
		UpdateHrs: s.subInfo.UpdateHrs,
	}
	s.mu.RUnlock()
	if mb, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dataDir(), subMetaFile), mb, 0o600)
	}
}

// startAutoUpdate 定期检查订阅是否该更新了。
// 机场通过 profile-update-interval 头告诉我们拉取间隔（通常 24 小时），
// 我们用 10 分钟的粗粒度去对表，够了。
func (s *CoreService) startAutoUpdate() {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			if s.Quitting() {
				return
			}
			s.maybeAutoRefresh()
		}
	}()
}

// maybeAutoRefresh 检查全部订阅的更新间隔，而不只是当前激活的那个。
// 非激活订阅也各自更新原文与配额信息，切换过去时直接就是新内容；
// 只有激活订阅内容真的变了才重启内核，避免无谓断连。
func (s *CoreService) maybeAutoRefresh() {
	s.mu.RLock()
	subs := append([]subMeta(nil), s.subs...)
	s.mu.RUnlock()

	for _, m := range subs {
		if m.URL == "" || m.UpdateHrs <= 0 || m.FetchedAt == 0 {
			continue
		}
		if time.Since(time.UnixMilli(m.FetchedAt)) < time.Duration(m.UpdateHrs)*time.Hour {
			continue
		}
		if s.Quitting() {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		res, err := subscription.Fetch(ctx, m.URL, m.Insecure)
		cancel()
		if err != nil {
			s.dbg("auto refresh skipped (%s): %v", m.URL, err)
			continue
		}

		// 用户可能在等待网络期间切走了订阅，别把旧的激活订阅又抢回来。
		s.mu.RLock()
		active := s.subURL
		old := append([]byte(nil), s.subRaw...)
		s.mu.RUnlock()

		if m.URL != active {
			s.applyInactiveSubscription(m.URL, res)
			continue
		}
		if bytes.Equal(old, res.Raw) {
			s.updateActiveSubMeta(m.URL, res)
			continue
		}
		if err := s.applySubscription(m.URL, res); err != nil {
			s.dbg("auto refresh apply failed: %v", err)
			continue
		}
		// 让前端知道这次是自动更新，不是用户手动刷新。
		if s.app != nil {
			s.app.Event.Emit("sub-auto-updated", res.Name)
		}
	}
}

// updateActiveSubMeta 内容没变时只刷新配额与时间戳，不重启内核。
func (s *CoreService) updateActiveSubMeta(url string, res *subscription.Result) {
	meta := subMetaFromResult(url, res)
	s.mu.Lock()
	for i := range s.subs {
		if s.subs[i].URL == url {
			s.subs[i] = meta
			break
		}
	}
	if s.subURL == url {
		s.subInfo.Name = meta.Name
		s.subInfo.FetchedAt = meta.FetchedAt
		s.subInfo.Upload = meta.Upload
		s.subInfo.Download = meta.Download
		s.subInfo.Total = meta.Total
		s.subInfo.Expire = meta.Expire
		s.subInfo.UpdateHrs = meta.UpdateHrs
	}
	s.mu.Unlock()

	s.saveSubs()
	s.persistSubMeta()
}

// applyInactiveSubscription 更新非激活订阅的原文与元信息，不动内核。
func (s *CoreService) applyInactiveSubscription(url string, res *subscription.Result) {
	if err := s.writeSubscriptionFiles(url, res); err != nil {
		s.dbg("auto refresh save failed (%s): %v", url, err)
		return
	}

	meta := subMetaFromResult(url, res)
	s.mu.Lock()
	idx := -1
	for i := range s.subs {
		if s.subs[i].URL == url {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return
	}
	s.subs[idx] = meta
	becameActive := s.subURL == url
	s.mu.Unlock()

	if becameActive {
		// 等待网络期间用户切到了这个订阅，需要走激活路径重启内核。
		if err := s.applySubscription(url, res); err != nil {
			s.dbg("auto refresh activate failed: %v", err)
		}
		return
	}
	s.saveSubs()
}

// ProcessRules 返回当前的进程分流表。
func (s *CoreService) ProcessRules() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.procRules))
	for k, v := range s.procRules {
		out[k] = v
	}
	return out
}

// SetProcessRule 设置某个程序的分流策略。
// policy 取 "proxy" / "direct" / "follow"，或 "target:<节点/策略组名>"。
// follow 表示删除该记录、回到订阅规则。
//
// 返回值是"补断开的旧连接条数"。mihomo 只在建连那一刻匹配规则，已建立的
// 连接不会重新判定，所以改完策略必须把该进程的老连接掐掉，用户才能立刻看到效果。
func (s *CoreService) SetProcessRule(proc, policy string) (int, error) {
	proc = sanitizeProcName(proc)
	if proc == "" {
		return 0, fmt.Errorf("进程名为空或含有非法字符")
	}

	target := ""
	switch {
	case policy == "proxy" || policy == "direct":
	case policy == "follow" || policy == "":
		policy = "follow"
	case strings.HasPrefix(policy, processTargetPrefix):
		target = strings.TrimSpace(strings.TrimPrefix(policy, processTargetPrefix))
		if err := s.validateProcessTarget(target); err != nil {
			return 0, err
		}
		policy = processTargetPrefix + target
	default:
		return 0, fmt.Errorf("不支持的策略: %s", policy)
	}

	if policy == "proxy" {
		s.mu.RLock()
		group := configgen.MainProxyGroup(s.subConfig)
		s.mu.RUnlock()
		if group == "" {
			return 0, fmt.Errorf("还没有可用节点，请先添加订阅")
		}
	}

	s.mu.RLock()
	oldPolicy := s.procRules[proc]
	s.mu.RUnlock()
	if oldPolicy == policy {
		return 0, nil
	}

	s.mu.Lock()
	if s.procRules == nil {
		s.procRules = map[string]string{}
	}
	if policy == "follow" {
		delete(s.procRules, proc)
	} else {
		s.procRules[proc] = policy
	}
	snapshot := make(map[string]string, len(s.procRules))
	for k, v := range s.procRules {
		snapshot[k] = v
	}
	s.mu.Unlock()

	if mb, err := json.MarshalIndent(snapshot, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dataDir(), procRulesJSON), mb, 0o644)
	}
	if err := s.writeProcRulesYAML(); err != nil {
		return 0, fmt.Errorf("写规则文件失败: %w", err)
	}

	if s.api == nil || s.mgr == nil || !s.mgr.IsRunning() {
		return 0, nil
	}

	// 只要变更涉及"指定节点"，就必须改 proxy-groups / rules，
	// 通过配置重载生效；纯 direct/proxy 仍走 provider 热刷新。
	targetInvolved := strings.HasPrefix(oldPolicy, processTargetPrefix) ||
		strings.HasPrefix(policy, processTargetPrefix)
	if targetInvolved {
		confPath, err := configgen.Write(dataDir(), s.options())
		if err != nil {
			return 0, fmt.Errorf("规则已保存，但生成配置失败: %w", err)
		}
		if err := s.reloadConfig(confPath); err != nil {
			s.dbg("reload config failed, fallback to kernel restart: %v", err)
			if restartErr := s.RestartKernel(); restartErr != nil {
				return 0, fmt.Errorf("规则已保存，但重载内核失败: %v（重启也失败: %w）", err, restartErr)
			}
		}
		s.selectProcessTargets()
		return s.closeProcessConnections(proc), nil
	}

	// 热刷新：只重新加载这两个 rule-provider，不重载整个配置，
	// 已经建立的连接一条都不会断。
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for _, name := range []string{procDirectProv, procProxyProv} {
		if err := s.api.UpdateRuleProvider(ctx, name); err != nil {
			return 0, fmt.Errorf("规则已保存，但热刷新失败（重启内核可生效）: %w", err)
		}
	}
	return s.closeProcessConnections(proc), nil
}

// validateProcessTarget 校验指定出口是否存在。
// 内核没运行时只能依据当前订阅解析结果判断。
func (s *CoreService) validateProcessTarget(target string) error {
	if target == "" {
		return fmt.Errorf("指定的节点名为空")
	}
	if strings.ContainsAny(target, "\n\r\t\x00") {
		return fmt.Errorf("节点名含有非法字符")
	}
	if strings.HasPrefix(target, configgen.ProcessGroupPrefix) {
		return fmt.Errorf("不能把进程指向内部隐藏策略组")
	}

	if s.api != nil && s.mgr != nil && s.mgr.IsRunning() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		proxies, _, err := s.allProxies(ctx)
		if err != nil {
			return fmt.Errorf("读取节点列表失败: %w", err)
		}
		if _, ok := proxies[target]; !ok {
			return fmt.Errorf("找不到节点或策略组: %s", target)
		}
		return nil
	}

	s.mu.RLock()
	explicit := s.subTargets[target]
	hasProvider := len(s.subProviders) > 0
	s.mu.RUnlock()
	if !explicit && !hasProvider {
		return fmt.Errorf("找不到节点或策略组: %s", target)
	}
	return nil
}

// closeProcessConnections 掐掉某个进程的旧连接，让新规则立即生效。
func (s *CoreService) closeProcessConnections(proc string) int {
	if s.api == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	closed := 0
	resp, err := s.api.Connections(ctx)
	if err != nil {
		return 0
	}
	for _, c := range resp.Connections {
		name := c.Metadata.Process
		if name == "" && c.Metadata.ProcessPath != "" {
			name = filepath.Base(c.Metadata.ProcessPath)
		}
		if !strings.EqualFold(name, proc) {
			continue
		}
		if err := s.api.CloseConnection(ctx, c.ID); err == nil {
			closed++
		}
	}
	s.invalidateConnCache()
	return closed
}
