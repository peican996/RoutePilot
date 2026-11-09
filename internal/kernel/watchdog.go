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
// 状态（全部受 mu 保护，互斥且明确）：
//
//	stopped    —— 永久停止（Shutdown/MarkQuitting），之后不再做任何事
//	paused     —— 用户手动停止内核，等待 Resume
//	exhausted  —— 超过重启上限，等待用户手动启动后 Resume 清除
//	restarting —— Restart 回调正在执行
//	timer      —— 至多一个待执行的重启定时器
//
// 并发约束：
//   - 同一时刻最多一个定时器。定时器带序号，被 Stop/Pause/重排替换后，
//     旧回调即使已经触发也会因序号不匹配而被丢弃，绝不会启动内核。
//   - restarting 期间收到的新退出事件只记账（pendingAgain），不另排定时器；
//     本轮重启结束后由 fire 统一决定是否继续恢复，避免一次故障叠加多次重启。
//   - Restart 返回错误与期间 OnExit 报告的退出视为同一次故障，只计一次。
type Watchdog struct {
	opts WatchdogOptions

	mu         sync.Mutex
	crashes    []time.Time
	timer      *time.Timer
	timerSeq   uint64
	stopped    bool
	paused     bool
	exhausted  bool
	restarting bool
	// pendingAgain 表示 restarting 期间又收到了退出事件，需在本轮结束后处理。
	pendingAgain bool
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

	if w.stopped || w.paused || w.exhausted {
		w.logf("看门狗已停止/暂停/熔断，忽略退出事件: %s", info.Reason)
		return
	}

	w.recordCrashLocked(info)

	if w.restarting {
		// 重启回调执行期间新内核又崩了：先记账，本轮结束后再统一安排。
		// 这里绝不能排定时器，否则会与正在进行的重启叠加成两条恢复链。
		w.pendingAgain = true
		w.logf("重启进行中收到新的退出事件，合并到本轮处理: %s", info.Reason)
		return
	}

	if w.overLimitLocked() {
		w.exhaustLocked(info.Reason)
		return
	}
	if w.timer != nil {
		// 已有待执行的重启：这是同一次故障的重复退出事件，不叠加。
		w.logf("忽略重复退出事件：已有待执行的重启（窗口内第 %d 次）", len(w.crashes))
		return
	}

	attempt := len(w.crashes)
	delay := w.scheduleLocked(attempt)
	w.logf("内核异常退出，第 %d 次自动重启将在 %s 后执行: %s", attempt, delay, info.Reason)
	if w.opts.OnRetry != nil {
		cb := w.opts.OnRetry
		go cb(attempt, delay, info)
	}
}

// recordCrashLocked 记录一次失败。调用方必须持有 w.mu。
func (w *Watchdog) recordCrashLocked(info ExitInfo) {
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
}

// fire 执行一次重启。失败会作为一次新失败继续退避，直到上限。
func (w *Watchdog) fire(seq uint64) {
	w.mu.Lock()
	// 旧定时器（已被 Stop/Pause/重排替换）不得再启动内核。
	if seq != w.timerSeq {
		w.mu.Unlock()
		return
	}
	w.timer = nil
	w.timerSeq++ // 让本次定时器彻底失效
	if w.stopped || w.paused || w.exhausted {
		w.mu.Unlock()
		return
	}
	if w.restarting {
		// 理论不可达：restarting 期间不排定时器。保守放弃本次触发。
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
	defer w.mu.Unlock()
	w.restarting = false

	// 重启期间被 Stop/Pause/熔断：不得再安排任何恢复。
	if w.stopped || w.paused || w.exhausted {
		w.pendingAgain = false
		return
	}

	if err != nil {
		w.logf("自动重启失败: %v", err)
		// 重启期间若已经收到 OnExit，说明同一次故障已经记过一次，
		// 这里不能再记，否则一次故障会被放大成两次、提前耗尽重试额度。
		if !w.pendingAgain {
			now := time.Now()
			w.pruneLocked(now)
			w.crashes = append(w.crashes, now)
		}
		w.pendingAgain = false
		if w.overLimitLocked() {
			w.exhaustLocked(err.Error())
			return
		}
		attempt := len(w.crashes)
		delay := w.scheduleLocked(attempt)
		w.logf("将在 %s 后重试启动（窗口内第 %d 次）", delay, attempt)
		return
	}

	// 重启成功，但期间新进程已经崩溃：必须继续恢复，不能把事件吞掉。
	if w.pendingAgain {
		w.pendingAgain = false
		if w.overLimitLocked() {
			w.exhaustLocked("重启后立即再次退出")
			return
		}
		attempt := len(w.crashes)
		delay := w.scheduleLocked(attempt)
		w.logf("新内核启动后再次退出，将在 %s 后重试（窗口内第 %d 次）", delay, attempt)
	}
}

// Stop 永久停止看门狗。用户主动退出时必须先调用，避免退出过程触发重启。
func (w *Watchdog) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	w.pendingAgain = false
	w.stopTimerLocked()
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
	w.pendingAgain = false
	w.stopTimerLocked()
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
	w.pendingAgain = false
	w.stopTimerLocked()
}

// Exhausted 返回是否已因超过上限而停止自动恢复。
func (w *Watchdog) Exhausted() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exhausted
}

func (w *Watchdog) overLimitLocked() bool {
	return len(w.crashes) > w.opts.Config.MaxRestarts
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
	w.pendingAgain = false
	w.stopTimerLocked()
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

// stopTimerLocked 停掉并作废当前定时器。递增序号确保已经触发的旧回调失效。
func (w *Watchdog) stopTimerLocked() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.timerSeq++
}

// scheduleLocked 安排下一次重启，返回等待时长。
// 排新定时器前一定先作废旧定时器，保证同一时刻最多一个。
func (w *Watchdog) scheduleLocked(attempt int) time.Duration {
	w.stopTimerLocked()
	seq := w.timerSeq
	delay := backoffDelay(w.opts.Config, attempt)
	w.timer = time.AfterFunc(delay, func() { w.fire(seq) })
	return delay
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
