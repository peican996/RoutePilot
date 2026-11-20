// Package safego 提供带 panic 兜底的后台任务启动器。
//
// 为什么需要它：Go 里 goroutine 内未捕获的 panic 会终止整个进程，
// 而不是只结束那个 goroutine。对本程序来说，后果是窗口直接消失、
// 系统代理残留在已死的内核端口上——用户完全不知道发生了什么。
//
// 后台任务处理的恰恰是最容易触发 panic 的输入：机场返回的畸形订阅、
// 进程快照、注册表状态。一个下标越界就足以让整个代理客户端退出。
package safego

import (
	"fmt"
	"runtime/debug"
)

// Reporter 在后台任务 panic 时被调用。name 是任务名，err 是 recover 到的值，
// stack 是 panic 发生时的栈。实现方负责落日志/通知界面，不能阻塞太久。
type Reporter func(name string, err any, stack []byte)

// Go 异步启动一个受保护的任务。
//
// 即使 fn 内部 panic，进程也不会退出：recover 后调用 report，然后该 goroutine 正常结束。
// 用于一次性任务（设置窗口图标、退出前清理等）。
func Go(name string, fn func(), report Reporter) {
	go func() {
		defer protect(name, report)
		fn()
	}()
}

// Run 同步执行一轮受保护的任务，panic 在这里被吃掉并上报。
//
// 用于周期性循环的循环体：把每轮逻辑包进 Run，某一轮 panic 只影响这一轮，
// 定时代码在外层继续跑。若把整个循环包进 Go，一次 panic 后循环就永远停了。
func Run(name string, fn func(), report Reporter) {
	defer protect(name, report)
	fn()
}

// protect 是统一兜底逻辑。必须作为 defer 直接调用，
// recover 才能捕获到调用方的 panic。
func protect(name string, report Reporter) {
	r := recover()
	if r == nil {
		return
	}
	// 栈要在 recover 之后、defer 返回之前取，晚了就没了。
	stack := debug.Stack()
	if report == nil {
		return
	}
	// 上报方自己再 panic 也不能往上抛，否则等于没兜底。
	func() {
		defer func() { _ = recover() }()
		report(name, r, stack)
	}()
}

// Describe 生成一句可读的失败描述，供日志与界面共用。
func Describe(name string, err any) string {
	return fmt.Sprintf("后台任务「%s」异常：%v（已自动恢复，功能可能不完整）", name, err)
}
