package kernel

import (
	"fmt"
	"sync"
	"time"
)

// WatchdogConfig 定义自动恢复策略。
//
// Window 是失败统计窗口：只有窗口内的退出才计入连续失败。
// StableAfter 表示一次运行持续多久后视为「已稳定」，再次崩溃时清空旧账，
// 而不是启动成功后立刻清零——后者会让「启动即崩」被无限放大。
type WatchdogConfig struct {
	Window      time.Duration
	MaxRestarts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	StableAfter time.Duration
}

// WatchdogOptions 是 Watchdog 的依赖注入项。
type WatchdogOptions struct {
	Config      WatchdogConfig
	Restart     func() error
	OnRetry     func(attempt int, delay time.Duration, info ExitInfo)
	OnExhausted func(attempts int, message string)
	Logf        func(format string, args ...any)
}

// Watchdog 负责在进程异常退出后按策略重启。
//
// 并发安全：ReportExit 可能来自进程收尾 goroutine，Stop/Reset 来自 UI 线程；
// 所有状态都在 mu 内维护。同一时刻最多只有一个待执行的重启定时器，多个退出
// 事件不会叠加出多个重启流程。
type Watchdog struct {
	opts WatchdogOptions

	mu         sync.Mutex
	crashes    []time.Time
	timer      *time.Timer
	stopped    bool
	paused     bool
	exhausted  bool
	restarting bool
}

// NewWatchdog 创建看门狗。未配置的字段使用保守默认值。
func NewWatchdog(opts WatchdogOptions) *Watchdog {
	cfg := opts.Config
	if cfg.Window <= 0 {
		cfg.Window = 10 * time.Minute
	}
	if cfg.MaxRestarts <= 0 {
		cfg.MaxRestarts = 5
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 2 * time.Second
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = time.Minute
	}
	if cfg.MaxDelay < cfg.BaseDelay {
		cfg.MaxDelay = cfg.BaseDelay
	}
	if cfg.StableAfter <= 0 {
		cfg.StableAfter = time.Minute
	}
	opts.Config = cfg
	return &Watchdog{opts: opts}
}

// ReportExit 接收一次异常退出（由 Manager.OnExit 调用）。
//
// 上层必须保证主动 Stop/Restart 与正常退出不会走到这里；Watchdog 不重复
// 判断意图，只负责次数统计、退避与上限。
func (w *Watchdog) ReportExit(info ExitInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.paused {
		return
	}
	w.recordLocked(info)
}

// recordLocked 记录一次失败并决定是否安排重启。调用方必须持有 w.mu。
func (w *Watchdog) recordLocked(info ExitInfo) {
	now := info.At
	if now.IsZero() {
		now = time.Now()
	}

	// 长稳运行后的崩溃是新账，不继承旧窗口内的失败次数。
	if info.Uptime >= w.opts.Config.StableAfter {
		w.crashes = nil
	}
	w.pruneLocked(now)
	w.crashes = append(w.crashes, now)

	if len(w.crashes) > w.opts.Config.MaxRestarts {
		w.exhaustLocked(info.Reason)
		return
	}
	// 已有待执行的重启：说明这是同一次故障的重复退出事件，不再叠加。
	if w.timer != nil {
		w.logf("忽略重复退出事件：已有待执行的重启（窗口内第 %d 次）", len(w.crashes))
		return
	}

	attempt := len(w.crashes)
	delay := backoffDelay(w.opts.Config, attempt)
	w.scheduleLocked(delay)
	w.logf("内核异常退出，第 %d 次自动重启将在 %s 后执行: %s", attempt, delay, info.Reason)
	if w.opts.OnRetry != nil {
		cb := w.opts.OnRetry
		go cb(attempt, delay, info)
	}
}

// fire 执行一次重启。失败会作为一次新失败继续退避，直到上限。
func (w *Watchdog) fire() {
	w.mu.Lock()
	if w.stopped || w.paused || w.exhausted {
		w.timer = nil
		w.mu.Unlock()
		return
	}
	w.timer = nil
	if w.restarting {
		w.mu.Unlock()
		return
	}
	w.restarting = true
	restart := w.opts.Restart
	w.mu.Unlock()

	var err error
	if restart != nil {
		err = restart()
	}

	w.mu.Lock()
	w.restarting = false
	if w.stopped || w.paused {
		w.mu.Unlock()
		return
	}
	if err == nil {
		w.mu.Unlock()
		return
	}

	now := time.Now()
	w.logf("自动重启失败: %v", err)
	w.pruneLocked(now)
	w.crashes = append(w.crashes, now)
	if len(w.crashes) > w.opts.Config.MaxRestarts {
		w.exhaustLocked(err.Error())
		w.mu.Unlock()
		return
	}
	delay := backoffDelay(w.opts.Config, len(w.crashes))
	w.scheduleLocked(delay)
	w.logf("将在 %s 后重试启动（窗口内第 %d 次）", delay, len(w.crashes))
	w.mu.Unlock()
}

// Stop 永久停止看门狗。用户主动退出时必须先调用，避免退出过程触发重启。
func (w *Watchdog) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// Pause 暂停自动恢复并取消待执行的重启。用户手动停止内核时调用：
// 即使崩溃事件与停止操作并发，也不能在停止之后又把内核拉起来。
func (w *Watchdog) Pause() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.paused = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// Resume 在用户手动启动成功后恢复自动恢复，并清除旧的失败记录。
// 看门狗自己发起的重启不调用它，否则「启动即崩」会被反复清零而无限重试。
func (w *Watchdog) Resume() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.paused = false
	w.exhausted = false
	w.crashes = nil
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// Exhausted 返回是否已因超过上限而停止自动恢复。
func (w *Watchdog) Exhausted() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exhausted
}

func (w *Watchdog) pruneLocked(now time.Time) {
	cutoff := now.Add(-w.opts.Config.Window)
	kept := w.crashes[:0]
	for _, t := range w.crashes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	w.crashes = kept
}

func (w *Watchdog) exhaustLocked(reason string) {
	if w.exhausted {
		return
	}
	w.exhausted = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	msg := fmt.Sprintf(
		"内核在 %s 内连续异常退出 %d 次，已停止自动重启：%s",
		w.opts.Config.Window, len(w.crashes), reason,
	)
	w.logf("%s", msg)
	if w.opts.OnExhausted != nil {
		cb := w.opts.OnExhausted
		attempts := len(w.crashes)
		go cb(attempts, msg)
	}
}

func (w *Watchdog) scheduleLocked(delay time.Duration) {
	w.timer = time.AfterFunc(delay, w.fire)
}

func (w *Watchdog) logf(format string, args ...any) {
	if w.opts.Logf != nil {
		w.opts.Logf(format, args...)
	}
}

// backoffDelay 返回第 attempt 次重启的等待时间（指数退避，有上限）。
func backoffDelay(cfg WatchdogConfig, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := cfg.BaseDelay
	for i := 1; i < attempt; i++ {
		if d >= cfg.MaxDelay/2 {
			return cfg.MaxDelay
		}
		d *= 2
	}
	if d > cfg.MaxDelay {
		return cfg.MaxDelay
	}
	return d
}
