package kernel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// State 表示内核进程的生命周期状态。
//
// 状态转换只允许在 Manager 的锁内完成：
//
//	Stopped/Failed → Starting → Running
//	Starting → Failed（启动失败，进程已清理）
//	Running → Stopping → Stopped
//	Running → Failed（异常退出）
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateFailed   State = "failed"
)

// 生命周期错误。调用方用 errors.Is 判断具体类别，不要靠字符串匹配。
var (
	ErrAlreadyRunning  = errors.New("内核已在运行")
	ErrLifecycleBusy   = errors.New("内核正在停止，请稍后重试")
	ErrNotReady        = errors.New("内核尚未就绪")
	ErrManagerClosed   = errors.New("内核管理器已关闭")
	ErrPortInUse       = errors.New("控制 API 端口被占用")
	ErrStartTimeout    = errors.New("内核启动超时")
	ErrAPIUnreachable  = errors.New("内核进程已启动但控制 API 不可访问")
	ErrProcessExited   = errors.New("内核进程在启动完成前退出")
	ErrStopTimeout     = errors.New("内核未能在限定时间内退出")
	ErrJobObjectFailed = errors.New("内核进程未能纳入 Job Object")
)

// ExitInfo 描述一次异常进程退出。回调只在「非主动且非零退出」时触发，
// 因此这里不包含 Intentional 字段：收到它就代表需要上层决定是否恢复。
type ExitInfo struct {
	Reason    string
	ExitCode  int
	Uptime    time.Duration
	StartedAt time.Time
	At        time.Time
}

const (
	// stopTimeout 是等待进程真正退出的上限。超过后状态进入 Failed，
	// 由上层决定是否重试，而不是假装 Stop 成功。
	stopTimeout = 5 * time.Second
	// readyPollInterval 是启动就绪探测的轮询间隔。
	readyPollInterval = 200 * time.Millisecond
)

// processRun 是一次启动产生的全部资源。cmd/PID/退出信号/清理完成信号
// 绑定在同一个对象上，避免新旧两次启动互相覆盖字段。
type processRun struct {
	cmd     *exec.Cmd
	pid     int
	started time.Time
	exited  chan struct{} // Wait 返回后关闭
	done    chan struct{} // 收尾逻辑执行完毕后关闭
	// intentional 由 Stop/Restart/启动失败清理写入，受 Manager.mu 保护。
	intentional bool
	// exitErr 在 close(exited) 前写入，读取方需等待 done。
	exitErr error
	// jobAttached 表示该进程已成功加入 Manager 的 Job Object。
	jobAttached bool
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// LogBuffer 是一个固定容量的环形日志缓冲，用于在 UI 里回看内核输出。
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
	// seq 是最后一条日志的全局序号。前端记住它做增量拉取：
	// 每次只取比自己旧的序号更新的部分，避免 1.2 秒一轮重复传 400 行。
	seq uint64
}

func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = 400
	}
	return &LogBuffer{lines: make([]string, 0, max), max: max}
}

func (b *LogBuffer) Add(s string) {
	s = strings.TrimSpace(ansiRe.ReplaceAllString(s, ""))
	if s == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, s)
	b.seq++
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
}

// Since 返回 seq 之后的日志、当前最新序号，以及是否发生了截断。
// 游标落后于缓冲区起点时（日志已经滚掉），返回整段缓冲并置 truncated，
// 让前端能提示"更早的日志已丢弃"，而不是以为中间没丢东西。
func (b *LogBuffer) Since(seq uint64) ([]string, uint64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if seq >= b.seq {
		return []string{}, b.seq, false
	}
	n := int(b.seq - seq)
	truncated := false
	if n > len(b.lines) {
		n = len(b.lines)
		truncated = true
	}
	out := make([]string, n)
	copy(out, b.lines[len(b.lines)-n:])
	return out, b.seq, truncated
}

func (b *LogBuffer) Snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}

func (b *LogBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = b.lines[:0]
	b.seq = 0
}

// Dump 返回全部日志（纯文本，换行分隔）。
func (b *LogBuffer) Dump() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.lines, "\n")
}

// openKernelLog 打开追加写的内核日志。超过 4MB 先滚一份 .1，
// 避免长期运行时无限增长。
func openKernelLog(dataDir string) *os.File {
	if dataDir == "" {
		return nil
	}
	p := filepath.Join(dataDir, "kernel.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > 4<<20 {
		_ = os.Remove(p + ".1")
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	return f
}

// logWriter 把内核的原始输出按行切分后丢进缓冲区。
// 同时落一份到 kernel.log——内核崩溃时内存日志会随重启清空，
// 只有磁盘上这份能事后复盘。
type logWriter struct {
	buf     *LogBuffer
	file    *os.File
	mu      sync.Mutex
	pending []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		_, _ = w.file.Write(p)
	}
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		w.buf.Add(string(w.pending[:i]))
		w.pending = w.pending[i+1:]
	}
	return len(p), nil
}

// Status 是内核状态的对外快照，直接序列化给前端。
type Status struct {
	State     State  `json:"state"`
	PID       int    `json:"pid"`
	StartedAt int64  `json:"startedAt"`
	Uptime    int64  `json:"uptime"`
	Error     string `json:"error"`
	ExePath   string `json:"exePath"`
	DataDir   string `json:"dataDir"`
}

// Manager 负责 mihomo 子进程的完整生命周期：落盘二进制、启动、退出、崩溃捕获。
//
// 并发模型：
//   - opMu 串行化 Start/Stop/Restart/Close。同一时刻只有一个生命周期事务，
//     Stop 与 Start 并发时按到达顺序线性化，不会交叉执行。
//   - mu 只保护状态快照与当前 run，不做耗时操作，Status 轮询不会被阻塞。
type Manager struct {
	opMu sync.Mutex

	mu      sync.Mutex
	exePath string
	dataDir string
	state   State
	run     *processRun
	lastErr string
	closed  bool

	// 启动就绪校验：addr 是控制 API 监听地址。probe 成功才把状态推进到
	// Running；仅进程存活不代表启动成功。
	readyAddr    string
	readyProbe   func(context.Context) error
	readyTimeout time.Duration

	// job 拥有「本进程退出即杀死内核」的 Job 句柄。生命周期与 Manager 一致，
	// 只有 Close 才关闭；Stop/Restart 不销毁它，避免重启时反复创建。
	job *jobObject

	// OnExit 只在「非主动、非零退出」时触发。用户 Stop/Restart 与正常
	// 退出（exit code 0）都不会回调。
	OnExit func(ExitInfo)

	// lastBackup 是最近一次成功替换前的内核备份路径。
	// 升级后的首次启动失败时用它回滚；只在同一次进程内有效。
	lastBackup string

	// hook 仅用于测试注入确定性 barrier（process-started / before-running）。
	// 生产环境为 nil，不参与生命周期判断。
	hook func(stage string)

	Logs *LogBuffer
}

// NewManager 创建管理器。dataDir 用于存放内核二进制、配置文件与缓存。
func NewManager(dataDir string) *Manager {
	return &Manager{
		dataDir:      dataDir,
		state:        StateStopped,
		readyTimeout: 15 * time.Second,
		job:          newJobObject(),
		Logs:         NewLogBuffer(400),
	}
}

// SetStartupValidation 配置启动就绪校验。
//
// addr 是控制 API 的监听地址（如 127.0.0.1:19090）；probe 应实际请求
// 该 API（例如 /version）。只有 probe 成功，Start 才会返回成功并把状态
// 置为 Running；超时后按「端口冲突 / 启动超时 / API 不可访问」分类报错。
func (m *Manager) SetStartupValidation(addr string, probe func(context.Context) error, timeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readyAddr = addr
	m.readyProbe = probe
	if timeout > 0 {
		m.readyTimeout = timeout
	}
}

func (m *Manager) DataDir() string { return m.dataDir }

func (m *Manager) ExePath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exePath
}

// EnsureAux 释放内核运行所需的附属文件（Windows 上是 wintun.dll）。
// 同时写到数据目录和 bin 目录：mihomo 查找驱动时会先看工作目录，
// 再看 exe 所在目录，两处都放最省心。
func (m *Manager) EnsureAux(files map[string][]byte) {
	dirs := []string{m.dataDir, filepath.Join(m.dataDir, "bin")}
	for name, data := range files {
		for _, d := range dirs {
			p := filepath.Join(d, name)
			if st, err := os.Stat(p); err == nil && st.Size() == int64(len(data)) {
				continue
			}
			_ = os.WriteFile(p, data, 0o644)
		}
	}
}

// Start 以指定的配置文件启动内核。
//
// 这是一个完整事务：互斥、启动进程、纳入 Job、等待控制 API 就绪。
// 任何一步失败都会杀掉本次进程并把状态置为 Failed，不会留下半启动状态。
func (m *Manager) Start(configPath string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.startLocked(configPath)
}

func (m *Manager) startLocked(configPath string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	if m.run != nil || m.state == StateRunning || m.state == StateStarting {
		m.mu.Unlock()
		return fmt.Errorf("%w（当前状态 %s）", ErrAlreadyRunning, m.state)
	}
	if m.state == StateStopping {
		m.mu.Unlock()
		return ErrLifecycleBusy
	}
	if m.exePath == "" {
		m.mu.Unlock()
		return fmt.Errorf("%w，请先调用 EnsureBinary", ErrNotReady)
	}
	exe := m.exePath
	addr, probe, timeout := m.readyAddr, m.readyProbe, m.readyTimeout
	m.state = StateStarting
	m.lastErr = ""
	m.mu.Unlock()

	// 端口预检查只能作为快速失败提示，不能保证随后一定能绑定；
	// 真正的结论以启动后的 API 探测为准。
	if addr != "" {
		if err := checkAddrFree(addr); err != nil {
			return m.failStart(fmt.Errorf("%w：%s（%v）", ErrPortInUse, addr, err))
		}
	}

	cmd := exec.Command(exe, "-d", m.dataDir, "-f", configPath)
	cmd.Dir = m.dataDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	w := &logWriter{buf: m.Logs, file: openKernelLog(m.dataDir)}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		if w.file != nil {
			_ = w.file.Close()
		}
		return m.failStart(fmt.Errorf("启动内核失败: %w", err))
	}

	// 挂在 Job 上，本进程被强杀时内核会一并结束，不留孤儿。
	// 分配失败不再静默降级：无法保证进程受管时直接终止本次启动。
	if m.job != nil {
		if err := m.job.assign(cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if w.file != nil {
				_ = w.file.Close()
			}
			return m.failStart(fmt.Errorf("%w：%v", ErrJobObjectFailed, err))
		}
	}

	r := &processRun{
		cmd:         cmd,
		pid:         cmd.Process.Pid,
		started:     time.Now(),
		exited:      make(chan struct{}),
		done:        make(chan struct{}),
		jobAttached: m.job != nil,
	}
	m.mu.Lock()
	m.run = r
	m.mu.Unlock()

	// 唯一的 Wait 调用点。进程自然退出时在这里收尾。
	go m.waitProcess(r, w)

	m.callHook("process-started")

	// 进程存活不等于启动成功：必须等控制 API 真正可用。
	if probe != nil {
		if err := m.awaitReady(r, probe, timeout); err != nil {
			return m.abortStart(r, err)
		}
	}

	m.callHook("before-running")

	m.mu.Lock()
	if m.run != r {
		// 进程在探测成功与状态切换之间退出：这仍然是一次启动失败。
		if m.state != StateFailed {
			m.state = StateFailed
			m.lastErr = exitReason(r.exitErr)
		}
		m.mu.Unlock()
		return fmt.Errorf("%w：%s", ErrProcessExited, exitReason(r.exitErr))
	}
	// 仅比较 m.run 还不够：waitProcess 先 close(exited) 再抢 m.mu，
	// 这中间存在窗口——进程已经退出，但 m.run 仍指向 r。
	// 此时若直接提交 Running，会向调用方谎报"启动成功"。
	// exited 已关闭说明 cmd.Wait 返回、r.exitErr 已写入（happens-before），
	// 必须在锁内显式检查，不能靠等待或 sleep。
	select {
	case <-r.exited:
		// 先摘掉 run，避免随后 waitProcess 拿到锁时把状态改回 Stopped，
		// 或让 Stop/Start 误以为这个已退出的进程仍然受管。
		if m.run == r {
			m.run = nil
		}
		m.state = StateFailed
		m.lastErr = exitReason(r.exitErr)
		m.mu.Unlock()
		return fmt.Errorf("%w：%s", ErrProcessExited, exitReason(r.exitErr))
	default:
	}
	m.state = StateRunning
	m.lastErr = ""
	m.mu.Unlock()
	return nil
}

// awaitReady 轮询探测控制 API，直到成功、进程退出或超时。
func (m *Manager) awaitReady(r *processRun, probe func(context.Context) error, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		if err := probe(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-r.exited:
			<-r.done
			return fmt.Errorf("%w：%s", ErrProcessExited, exitReason(r.exitErr))
		case <-ctx.Done():
			return m.classifyStartFailure(lastErr)
		case <-ticker.C:
		}
	}
}

// classifyStartFailure 在启动超时后区分三类失败，便于用户直接定位：
// 端口被别的程序占住 / 进程在监听但 API 不健康 / 什么都没起来。
func (m *Manager) classifyStartFailure(cause error) error {
	m.mu.Lock()
	addr, timeout := m.readyAddr, m.readyTimeout
	m.mu.Unlock()

	if addr != "" {
		// 先尝试建连：能连上说明端口上是我们的（或别的）监听者，
		// 但 HTTP 层没有正常响应，属于 API 不可访问。
		if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
			_ = conn.Close()
			return fmt.Errorf("%w：%s 有监听但健康检查失败（%v）", ErrAPIUnreachable, addr, cause)
		}
		if err := bindAddr(addr); err != nil {
			return fmt.Errorf("%w：%s（%v）", ErrPortInUse, addr, err)
		}
	}
	return fmt.Errorf("%w：%s 内未就绪（%v）", ErrStartTimeout, timeout, cause)
}

// abortStart 清理一次失败的启动：标记为主动结束、杀掉进程并等待收尾，
// 最后把状态置为 Failed。
func (m *Manager) abortStart(r *processRun, cause error) error {
	m.mu.Lock()
	if m.run == r {
		r.intentional = true
		m.state = StateStopping
	}
	m.mu.Unlock()

	_ = r.cmd.Process.Kill()

	cleaned := true
	select {
	case <-r.done:
	case <-time.After(stopTimeout):
		cleaned = false
	}

	m.mu.Lock()
	// 只有确认进程已收尾才清空 run；否则保留句柄，阻止后续 Start
	// 在旧进程仍存活时再拉起第二个实例。
	if cleaned && m.run == r {
		m.run = nil
	}
	m.state = StateFailed
	m.lastErr = cause.Error()
	m.mu.Unlock()
	return cause
}

// failStart 记录未产生进程的启动失败。
func (m *Manager) failStart(err error) error {
	m.mu.Lock()
	if m.run == nil && m.state == StateStarting {
		m.state = StateFailed
	}
	m.lastErr = err.Error()
	m.mu.Unlock()
	return err
}

// waitProcess 是唯一的 cmd.Wait 调用点，负责回收全部进程资源。
func (m *Manager) waitProcess(r *processRun, w *logWriter) {
	err := r.cmd.Wait()
	if w.file != nil {
		_ = w.file.Close()
	}
	r.exitErr = err
	close(r.exited)

	m.mu.Lock()
	current := m.run == r
	intentional := r.intentional
	if current {
		m.run = nil
		// 用户主动停止、或进程以 0 正常退出，都不算崩溃。
		if intentional || err == nil {
			m.state = StateStopped
			m.lastErr = ""
		} else {
			m.state = StateFailed
			m.lastErr = err.Error()
		}
	}
	cb := m.OnExit
	m.mu.Unlock()

	// done 关闭后，Stop/abortStart 可以确定 cmd、PID 已清空。
	close(r.done)

	// 只上报异常退出：主动停止与正常退出都不触发自动恢复。
	if current && cb != nil && err != nil && !intentional {
		cb(ExitInfo{
			Reason:    err.Error(),
			ExitCode:  exitCode(err),
			Uptime:    time.Since(r.started),
			StartedAt: r.started,
			At:        time.Now(),
		})
	}
}

// Stop 结束内核进程，并等待资源真正回收。
func (m *Manager) Stop() error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.stopLocked()
}

func (m *Manager) stopLocked() error {
	m.mu.Lock()
	if (m.state == StateStopped || m.state == StateFailed) && m.run == nil {
		m.mu.Unlock()
		return nil
	}
	r := m.run
	if r == nil {
		m.state = StateStopped
		m.lastErr = ""
		m.mu.Unlock()
		return nil
	}
	// 先标记为主动停止，Wait goroutine 看到后不会当成崩溃上报。
	r.intentional = true
	m.state = StateStopping
	m.mu.Unlock()

	killErr := r.cmd.Process.Kill()
	// 进程可能已经自行退出；Kill 返回的错误交给 Wait 收尾判断，
	// 只有等待超时时才把它作为诊断信息返回。

	select {
	case <-r.done:
	case <-time.After(stopTimeout):
		m.mu.Lock()
		if m.run == r {
			m.state = StateFailed
			m.lastErr = fmt.Sprintf("停止内核超时（kill: %v）", killErr)
		}
		m.mu.Unlock()
		return fmt.Errorf("%w（kill: %v）", ErrStopTimeout, killErr)
	}

	m.mu.Lock()
	if m.run == nil && m.state != StateFailed {
		m.state = StateStopped
		m.lastErr = ""
	}
	m.mu.Unlock()
	return nil
}

// Restart 是一个完整事务：Stop 与新 Start 之间不会被其他生命周期操作插入。
func (m *Manager) Restart(configPath string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.stopLocked(); err != nil {
		return err
	}
	return m.startLocked(configPath)
}

// Close 停止内核并关闭 Job 句柄。关闭后 Manager 不再接受 Start。
func (m *Manager) Close() error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	stopErr := m.stopLocked()
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()

	var jobErr error
	if m.job != nil {
		jobErr = m.job.close()
	}
	if stopErr != nil {
		return stopErr
	}
	return jobErr
}

func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state == StateRunning
}

// JobAttached 返回当前进程是否已成功加入 Job Object。
func (m *Manager) JobAttached() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.run != nil && m.run.jobAttached
}

func (m *Manager) callHook(stage string) {
	if m.hook != nil {
		m.hook(stage)
	}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		State:   m.state,
		Error:   m.lastErr,
		ExePath: m.exePath,
		DataDir: m.dataDir,
	}
	if m.run != nil {
		st.PID = m.run.pid
		st.StartedAt = m.run.started.UnixMilli()
		st.Uptime = int64(time.Since(m.run.started).Seconds())
	}
	return st
}

// checkAddrFree 用于启动前快速判断端口占用。
//
// 绑定失败不一定代表别的程序在监听：Windows 上 TIME_WAIT 也会让刚释放的
// 端口短暂无法绑定。只有在确实能建立 TCP 连接时才判定为被占用，避免内核
// 刚重启就误报端口冲突。
func checkAddrFree(addr string) error {
	bindErr := bindAddr(addr)
	if bindErr == nil {
		return nil
	}
	conn, dialErr := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return bindErr
	}
	return nil
}

// bindAddr 尝试独占绑定地址，返回底层错误。
func bindAddr(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func exitReason(err error) string {
	if err == nil {
		return "exit code 0"
	}
	return err.Error()
}
