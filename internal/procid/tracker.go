package procid

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProcInfo 是一次系统进程快照中的单个进程。
//
// PID 只在快照有效期内可信：同一个 PID 在进程退出后可能被复用，
// 因此必须结合 Created 判断是否仍是同一个进程，绝不能把 PID 永久当作身份。
type ProcInfo struct {
	PID     uint32
	Name    string
	Path    string
	Created time.Time
	// FileID 是 exe 文件的卷序列号 + 文件索引，用于识别「同路径但文件已升级」。
	FileID string
}

// Source 提供当前系统进程快照。
type Source interface {
	List() ([]ProcInfo, error)
}

// SourceFunc 让普通函数满足 Source。
type SourceFunc func() ([]ProcInfo, error)

func (f SourceFunc) List() ([]ProcInfo, error) { return f() }

// LookupState 描述按名称解析进程的结果。
type LookupState int

const (
	LookupMissing LookupState = iota
	LookupUnique
	LookupAmbiguous
)

// LookupResult 是按名称解析的结果。
//
// Unique 时可以安全地采用 Info.Path；Ambiguous 表示同名进程不止一个，
// 调用方必须降级为名称匹配，并在日志中说明无法唯一定位。
type LookupResult struct {
	State      LookupState
	Info       ProcInfo
	Candidates int
}

// Tracker 维护短生命周期的进程快照缓存，用于补齐 mihomo 缺失的进程路径，
// 并在 PID 复用、进程退出或文件身份变化时及时更新。
//
// 缓存只用于「连接 → 身份」的解析，不参与规则持久化：规则键永远是可跨
// 重启的路径/名称，不会写入 PID 或创建时间。
type Tracker struct {
	src  Source
	ttl  time.Duration
	now  func() time.Time
	file func(path string) string
	logf func(format string, args ...any)

	mu        sync.Mutex
	loaded    bool
	fetchedAt time.Time
	byPID     map[uint32]ProcInfo
	byName    map[string][]uint32
	// verify 在采纳快照路径前实时校验进程实例是否仍是快照里的那个
	// （生产环境用 OpenProcess+GetProcessTimes 核对创建时间），
	// 消除 TTL 窗口内同名进程退出/被新实例替换导致的错误归属。
	verify func(ProcInfo) bool
}

// NewTracker 创建缓存。ttl <= 0 时默认 5 秒。
func NewTracker(src Source, ttl time.Duration) *Tracker {
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	return &Tracker{
		src:    src,
		ttl:    ttl,
		now:    time.Now,
		byPID:  map[uint32]ProcInfo{},
		byName: map[string][]uint32{},
	}
}

// SetFileIDFunc 注入文件身份读取函数（生产环境为 Windows 文件索引）。
func (t *Tracker) SetFileIDFunc(fn func(path string) string) {
	t.mu.Lock()
	t.file = fn
	t.mu.Unlock()
}

// SetVerifyFunc 注入进程实例校验函数（生产环境实时核对 PID 的创建时间）。
// 注入后 LookupName 在采纳唯一匹配的路径前会先确认快照没有过期失真。
func (t *Tracker) SetVerifyFunc(fn func(ProcInfo) bool) {
	t.mu.Lock()
	t.verify = fn
	t.mu.Unlock()
}

// SetLogger 注入诊断日志。
func (t *Tracker) SetLogger(fn func(format string, args ...any)) {
	t.mu.Lock()
	t.logf = fn
	t.mu.Unlock()
}

// LookupName 解析同名进程。缓存过期时先刷新快照。
func (t *Tracker) LookupName(name string) LookupResult {
	key, err := NormalizeName(name)
	if err != nil {
		return LookupResult{State: LookupMissing}
	}
	key = strings.ToLower(key)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.refreshLocked(false)

	pids := t.byName[key]
	switch len(pids) {
	case 0:
		return LookupResult{State: LookupMissing}
	case 1:
		info, ok := t.byPID[pids[0]]
		if !ok {
			return LookupResult{State: LookupMissing}
		}
		// 快照最长有 TTL 的陈旧期：同名进程若恰好在此期间退出、被
		// 不同路径的新实例替换，直接采用快照路径会把连接挂到错误路径，
		// 用户据此一键建规则就会持久化错误的 PROCESS-PATH。
		// 先实时校验该 PID 仍是快照里的进程实例，不一致就强制刷新
		// 一次重查；仍不一致按"无法唯一定位"降级为名称匹配。
		if t.verify != nil && !t.verify(info) {
			if err := t.refreshLocked(true); err == nil {
				if again, ok2 := t.byName[key]; ok2 && len(again) == 1 {
					if info2, ok3 := t.byPID[again[0]]; ok3 {
						info = info2
					}
				}
			}
			if t.verify != nil && !t.verify(info) {
				t.log("名称 %q 的快照实例已失效，降级为名称匹配", name)
				return LookupResult{State: LookupAmbiguous, Candidates: len(pids), Info: info}
			}
		}
		if info.FileID == "" && t.file != nil && info.Path != "" {
			info.FileID = t.file(info.Path)
			t.byPID[info.PID] = info
		}
		return LookupResult{State: LookupUnique, Info: info, Candidates: 1}
	default:
		return LookupResult{State: LookupAmbiguous, Candidates: len(pids)}
	}
}

// LookupPID 返回 PID 对应的最新快照信息。
func (t *Tracker) LookupPID(pid uint32) (ProcInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.refreshLocked(false)
	info, ok := t.byPID[pid]
	return info, ok
}

// Refresh 强制刷新一次快照，供启动与测试使用。
func (t *Tracker) Refresh() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.refreshLocked(true)
}

func (t *Tracker) refreshLocked(force bool) error {
	now := t.now()
	if !force && t.loaded && now.Sub(t.fetchedAt) < t.ttl {
		return nil
	}
	if t.src == nil {
		return nil
	}
	infos, err := t.src.List()
	if err != nil {
		// 保留上一份快照，避免一次查询失败让所有进程身份突然降级。
		// 但必须显式记录：调用方据此知道这次解析可能基于陈旧快照。
		t.log("进程快照刷新失败，沿用上一份快照: %v", err)
		return err
	}

	byPID := make(map[uint32]ProcInfo, len(infos))
	byName := make(map[string][]uint32)
	reused := 0
	for _, info := range infos {
		if info.PID == 0 {
			continue
		}
		name := info.Name
		if name == "" {
			name = filepath.Base(info.Path)
		}
		if name == "" {
			continue
		}
		info.Name = name
		byPID[info.PID] = info
		key := strings.ToLower(name)
		byName[key] = append(byName[key], info.PID)

		// PID 复用：同一 PID 的创建时间变了，说明已经是另一个进程。
		if old, ok := t.byPID[info.PID]; ok && !old.Created.Equal(info.Created) {
			reused++
			t.log("检测到 PID 复用: pid=%d %s → %s", info.PID, old.Name, info.Name)
		}
		// 同路径文件身份变化通常意味着程序被升级/替换。
		if old, ok := t.byPID[info.PID]; ok && !old.Created.Equal(info.Created) &&
			old.Path != "" && old.Path == info.Path && old.FileID != "" && info.FileID != "" &&
			old.FileID != info.FileID {
			t.log("进程文件身份变化: pid=%d path=%s", info.PID, info.Path)
		}
	}
	for _, pids := range byName {
		sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	}

	removed := 0
	if t.loaded {
		for pid := range t.byPID {
			if _, ok := byPID[pid]; !ok {
				removed++
			}
		}
	}
	t.byPID = byPID
	t.byName = byName
	t.loaded = true
	t.fetchedAt = now
	if removed > 0 {
		t.log("已清理退出的进程缓存: %d 个", removed)
	}
	if reused > 0 {
		t.log("本次快照发现 %d 个 PID 复用，相关缓存已更新", reused)
	}
	return nil
}

func (t *Tracker) log(format string, args ...any) {
	if t.logf != nil {
		t.logf(format, args...)
	}
}

// Describe 返回便于排查的身份摘要。
func (p ProcInfo) Describe() string {
	return fmt.Sprintf("pid=%d name=%s path=%s created=%s file=%s",
		p.PID, p.Name, p.Path, p.Created.Format(time.RFC3339), p.FileID)
}
