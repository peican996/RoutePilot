package kernel

import (
	"bytes"
	"fmt"
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
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateError    State = "error"
)

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
type Manager struct {
	mu sync.Mutex

	exePath string
	dataDir string
	cmd     *exec.Cmd
	state   State
	lastErr string
	started time.Time
	exited  chan struct{}

	// stopping 标记「主动 Stop/Restart」。主动 kill 时 cmd.Wait() 也会返回
	// 非 nil（"signal: killed"/"exit status 1"），不能当成崩溃处理，
	// 否则每次正常重启都会误报内核异常、还会连锁触发看门狗无限重启。
	stopping bool

	// OnExit 内核进程意外退出时的回调（正常 Stop 不触发）。
	OnExit func(reason string)

	Logs *LogBuffer
}

// NewManager 创建管理器。dataDir 用于存放内核二进制、配置文件与缓存。
func NewManager(dataDir string) *Manager {
	return &Manager{
		dataDir: dataDir,
		state:   StateStopped,
		Logs:    NewLogBuffer(400),
	}
}

func (m *Manager) DataDir() string { return m.dataDir }

func (m *Manager) ExePath() string { return m.exePath }

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
func (m *Manager) Start(configPath string) error {
	m.mu.Lock()
	if m.cmd != nil {
		m.mu.Unlock()
		return fmt.Errorf("内核已在运行")
	}
	if m.exePath == "" {
		m.mu.Unlock()
		return fmt.Errorf("内核尚未就绪，请先调用 EnsureBinary")
	}
	exe := m.exePath
	m.state = StateStarting
	m.lastErr = ""
	m.stopping = false
	m.mu.Unlock()

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
		m.mu.Lock()
		m.state = StateError
		m.lastErr = err.Error()
		m.mu.Unlock()
		return fmt.Errorf("启动内核失败: %w", err)
	}

	// 挂在 Job 上，本进程被强杀时内核会一并结束，不留孤儿。
	attachToJob(cmd.Process.Pid)

	exited := make(chan struct{})
	m.mu.Lock()
	m.cmd = cmd
	m.state = StateRunning
	m.started = time.Now()
	m.exited = exited
	m.mu.Unlock()

	// 唯一的 Wait 调用点。进程自然退出时在这里收尾。
	go func() {
		err := cmd.Wait()
		if w.file != nil {
			_ = w.file.Close()
		}
		close(exited)
		m.mu.Lock()
		if m.cmd != cmd {
			m.mu.Unlock()
			return
		}
		m.cmd = nil
		m.started = time.Time{}
		intentional := m.stopping
		m.stopping = false
		if err != nil && !intentional {
			m.state = StateError
			m.lastErr = err.Error()
		} else {
			m.state = StateStopped
			m.lastErr = ""
		}
		cb := m.OnExit
		reason := m.lastErr
		m.mu.Unlock()

		// 只把「非主动停止」的异常退出上报为崩溃
		if cb != nil && err != nil && !intentional {
			cb(reason)
		}
	}()

	return nil
}

// Stop 结束内核进程，并等待它真正退出，避免端口未释放导致重启失败。
func (m *Manager) Stop() error {
	m.mu.Lock()
	cmd := m.cmd
	exited := m.exited
	// 先标记为主动停止，Wait goroutine 看到后不会当成崩溃上报
	m.stopping = true
	m.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		m.mu.Lock()
		m.stopping = false
		m.mu.Unlock()
		return nil
	}
	_ = cmd.Process.Kill()

	if exited != nil {
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			m.mu.Lock()
			m.stopping = false
			m.mu.Unlock()
			return fmt.Errorf("内核未能在 5 秒内退出")
		}
	}

	// exited 是在收尾逻辑之前 close 的，这里再等 cmd 句柄真正清空，
	// 否则紧接着的 Start 会撞上「内核已在运行」。
	for i := 0; i < 100; i++ {
		m.mu.Lock()
		done := m.cmd == nil
		m.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// Restart 先停再起。
func (m *Manager) Restart(configPath string) error {
	if err := m.Stop(); err != nil {
		return err
	}
	time.Sleep(150 * time.Millisecond)
	return m.Start(configPath)
}

func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmd != nil
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
	if m.cmd != nil && m.cmd.Process != nil {
		st.PID = m.cmd.Process.Pid
	}
	if !m.started.IsZero() {
		st.StartedAt = m.started.UnixMilli()
		st.Uptime = int64(time.Since(m.started).Seconds())
	}
	return st
}
