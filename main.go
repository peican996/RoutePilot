package main

import (
	"embed"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"golang.org/x/sys/windows"
)

// 前端产物直接打进二进制，分发时只需要一个 exe。
//
//go:embed all:frontend/dist
var assets embed.FS

// 内核同样内置。首次运行时释放到用户数据目录。
//
//go:embed assets/mihomo.exe
var embeddedKernel []byte

// GeoIP / GeoSite 数据库。干净机器上 mihomo 会去 GitHub 现下载，
// 在国内会长时间卡住并导致内核不监听端口，所以这里一并内置。
//
//go:embed assets/geo/geoip.metadb
var geoipEmbed []byte

//go:embed assets/geo/geosite.dat
var geositeEmbed []byte

// wintun.dll 是 TUN 模式在 Windows 上的虚拟网卡驱动，mihomo 运行时需要。
//
//go:embed assets/wintun.dll
var wintunDLL []byte

//go:embed build/appicon.png
var trayIcon []byte

//go:embed build/appicon-green.png
var trayIconGreen []byte

//go:embed build/appicon-amber.png
var trayIconAmber []byte

//go:embed build/appicon-red.png
var trayIconRed []byte

// 单实例判定：命名互斥体是权威依据，监听端口作为补充。
//
// 早期只用一个 TCP 端口做锁，任意本地程序先占住 47914 就能让我们
// 永远起不来（重试 4.5 秒后静默退出）。互斥体只在本产品自己的实例
// 之间冲突，不受其他程序影响。
//
// 注意：CreateMutexW 之后要靠 LastError 判断"是否已存在"，而 LastError
// 是线程局部的、Go 的 goroutine 会在系统调用之间迁移线程。必须用
// runtime.LockOSThread 把当前 goroutine 钉住，否则读到的可能是别的
// 线程的 LastError——这也是最初放弃互斥体方案的真实原因。
const instanceMutexName = "Local\\RoutePilot.SingleInstance"

// 端口锁保留作补充判据：互斥体在极端情况下（权限/会话隔离）可能失效。
const instanceLockPort = "127.0.0.1:47914"

var (
	instanceLn     net.Listener
	instanceMutex  windows.Handle
	procCreateMx   = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateMutexW")
	procMessageBox = windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW")
)

func init() {
	// 注册自定义事件，绑定生成器会据此产生强类型前端 API。
	// Emit 的事件必须在这里注册，否则前端 Events.On 收不到。
	application.RegisterEvent[string]("sub-auto-updated")
	application.RegisterEvent[string]("kernel-crashed")
	application.RegisterEvent[string]("network-degraded")
	application.RegisterEvent[string]("traffic-alert")
}

// acquireInstanceMutex 尝试创建命名互斥体。
// 返回 false 表示已有实例在跑，或创建失败。
func acquireInstanceMutex() bool {
	namePtr, err := windows.UTF16PtrFromString(instanceMutexName)
	if err != nil {
		return false
	}

	runtime.LockOSThread()
	h, _, lastErr := procCreateMx.Call(0, 0, uintptr(unsafe.Pointer(namePtr)))
	runtime.UnlockOSThread()

	if h == 0 {
		return false
	}
	if lastErr == windows.ERROR_ALREADY_EXISTS {
		_ = windows.CloseHandle(windows.Handle(h))
		return false
	}
	instanceMutex = windows.Handle(h)
	return true
}

func acquireSingleInstance() bool {
	// 提权重启时旧进程还没退干净，给几秒宽限。
	for i := 0; i < 30; i++ {
		if acquireInstanceMutex() {
			break
		}
		if i == 29 {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}

	// 端口只是补充信号：拿不到不影响运行（我们并不需要这个端口），
	// 能拿到就多一层保险。
	if ln, err := net.Listen("tcp", instanceLockPort); err == nil {
		instanceLn = ln
	}
	return true
}

// showAlreadyRunning 启动失败时给个可见提示，而不是静默退出。
func showAlreadyRunning() {
	text, _ := windows.UTF16PtrFromString("RoutePilot 已经在运行了。\n\n请从系统托盘打开，或先退出已有实例。")
	title, _ := windows.UTF16PtrFromString("RoutePilot")
	procMessageBox.Call(0,
		uintptr(unsafe.Pointer(text)),
		uintptr(unsafe.Pointer(title)),
		uintptr(0x40), // MB_ICONINFORMATION
	)
}

func main() {
	if !acquireSingleInstance() {
		log.Println("RoutePilot 已经在运行了")
		showAlreadyRunning()
		return
	}

	svc := NewCoreService()
	svc.Boot()

	// 主窗口句柄。拦截 WM_CLOSE 时要用，但窗口在 app 创建之后才有，
	// 所以先声明变量，靠闭包在真正收到消息时取值。
	var mainWindow *application.WebviewWindow
	var saveBounds func()

	app := application.New(application.Options{
		Name:        "RoutePilot",
		Description: "个人定制的代理控制台",
		Icon:        trayIcon,
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler: noCacheHTML(application.AssetFileServerFS(assets)),
		},
		Windows: application.WindowsOptions{
			// 关键：最后一个窗口关闭时**不要**自动退出。
			// 这个默认是 false，导致点 X 就整个应用退掉了——
			// 而代理软件必须常驻，否则用户误关窗口就断网。
			DisableQuitOnLastWindowClosed: true,

			// 直接在消息层拦 WM_CLOSE。比依赖 Wails 的 WindowClosing 事件更可靠：
			// 事件的 Cancel 只是设个标志，实测拦不住退出。
			WndProcInterceptor: func(hwnd uintptr, msg uint32, wParam, lParam uintptr) (uintptr, bool) {
				const wmClose = 0x0010
				if msg == wmClose && mainWindow != nil && !svc.Quitting() {
					saveBounds()
					mainWindow.Hide()
					return 0, true
				}
				return 0, false
			},
		},
		Mac: application.MacOptions{
			// 关窗口不退出应用——代理得继续跑。
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})

	svc.Attach(app)

	mainWindow = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "RoutePilot",
		Width:            1080,
		Height:           720,
		BackgroundColour: application.NewRGB(14, 16, 20),
		URL:              "/",
	})

	// 恢复上次的窗口位置
	if x, y, w, h := svc.WindowBounds(); w > 0 && h > 0 {
		mainWindow.SetPosition(x, y)
		mainWindow.SetSize(w, h)
	}

	// Wails v3 的窗口选项没有 Icon 字段，用 Win32 API 把 rsrc 嵌入的图标
	// 设到标题栏/任务栏。窗口句柄要等 app.Run() 启动后才就绪，
	// 所以延迟到 goroutine 里设。
	go func() {
		for i := 0; i < 20; i++ {
			time.Sleep(250 * time.Millisecond)
			if hwnd := uintptr(mainWindow.NativeWindow()); hwnd != 0 {
				setWindowIcon(hwnd)
				return
			}
		}
	}()

	// 窗口隐藏时记住位置
	saveBounds = func() {
		b := mainWindow.Bounds()
		svc.SaveWindowBounds(b.X, b.Y, b.Width, b.Height)
	}

	// 双保险：WindowClosing 事件也拦一次。两条路径都走隐藏，不销毁窗口。
	mainWindow.OnWindowEvent(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if svc.Quitting() {
			return
		}
		e.Cancel()
		saveBounds()
		mainWindow.Hide()
	})

	setupTray(app, svc, mainWindow)

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}

	svc.Shutdown()
}

// noCacheHTML 给 HTML 响应加 no-cache。
//
// WebView2 会把 index.html 缓存住，而 index.html 里引用的是带 hash 的 JS 文件名。
// 结果就是：JS 文件名变了，但 html 还是旧的 → 永远加载旧 bundle，
// 改了前端代码看不到任何变化。只对 html 关缓存，静态资源仍走正常缓存。
func noCacheHTML(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "" || p == "/" || strings.HasSuffix(p, ".html") {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		}
		next.ServeHTTP(w, r)
	})
}

// setWindowIcon 用 Win32 API 把 exe 里 rsrc 嵌入的图标设到窗口标题栏。
// Wails v3 的 WebviewWindowOptions 没有 Icon 字段，只能走这条路。
// 图标资源 ID 是 rsrc 默认的 1（-ico 生成的第一个图标组）。
func setWindowIcon(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	var (
		user32              = syscall.NewLazyDLL("user32.dll")
		kernel32            = syscall.NewLazyDLL("kernel32.dll")
		procGetModuleHandle = kernel32.NewProc("GetModuleHandleW")
		procLoadImage       = user32.NewProc("LoadImageW")
		procSendMessage     = user32.NewProc("SendMessageW")
	)
	const (
		wmSetIcon     = 0x0080
		iconSmall     = 0
		iconBig       = 1
		imageIcon     = 1
		lrDefaultSize = 0x00000040
	)
	hInst, _, _ := procGetModuleHandle.Call(0)
	hIcon, _, _ := procLoadImage.Call(hInst, 1, imageIcon, 0, 0, lrDefaultSize)
	if hIcon == 0 {
		return
	}
	procSendMessage.Call(hwnd, wmSetIcon, iconBig, hIcon)
	procSendMessage.Call(hwnd, wmSetIcon, iconSmall, hIcon)
}

// setupTray 建托盘图标和右键菜单。代理软件必须常驻，
// 用户不会为了保持代理而一直开着窗口。
func setupTray(app *application.App, svc *CoreService, win *application.WebviewWindow) {
	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon)
	tray.SetTooltip("RoutePilot")

	menu := app.NewMenu()

	show := menu.Add("显示窗口")
	show.OnClick(func(ctx *application.Context) {
		win.Show()
		win.Focus()
	})

	menu.AddSeparator()

	proxyItem := menu.AddCheckbox("系统代理", svc.ProxyEnabled())
	proxyItem.OnClick(func(ctx *application.Context) {
		on := !svc.ProxyEnabled()
		if err := svc.SetSystemProxy(on); err != nil {
			log.Println("托盘切换系统代理失败:", err)
			proxyItem.SetChecked(!on)
			return
		}
		proxyItem.SetChecked(on)
	})

	tunItem := menu.AddCheckbox("TUN 模式", svc.TUNState())
	tunItem.OnClick(func(ctx *application.Context) {
		on := !svc.TUNState()
		if err := svc.SetTUN(on); err != nil {
			log.Println("托盘切换 TUN 失败:", err)
			tunItem.SetChecked(!on)
			return
		}
		tunItem.SetChecked(on)
	})

	// ---- 模式切换子菜单 ----
	menu.AddSeparator()
	modeMenu := menu.AddSubmenu("代理模式")
	ruleItem := modeMenu.AddRadio("规则模式", svc.Mode() == "rule")
	globalItem := modeMenu.AddRadio("全局模式", svc.Mode() == "global")
	directItem := modeMenu.AddRadio("直连模式", svc.Mode() == "direct")
	setMode := func(mode string, item *application.MenuItem) {
		if err := svc.SetMode(mode); err != nil {
			log.Println("托盘切换模式失败:", err)
			return
		}
		ruleItem.SetChecked(mode == "rule")
		globalItem.SetChecked(mode == "global")
		directItem.SetChecked(mode == "direct")
	}
	ruleItem.OnClick(func(ctx *application.Context) { setMode("rule", ruleItem) })
	globalItem.OnClick(func(ctx *application.Context) { setMode("global", globalItem) })
	directItem.OnClick(func(ctx *application.Context) { setMode("direct", directItem) })

	// ---- 快速节点切换 ----
	menu.AddSeparator()
	nodeMenu := menu.AddSubmenu("切换节点")
	// 启动时构建节点列表（取延迟最低的 8 个）
	names, active := svc.QuickNodes(8)
	if len(names) == 0 {
		nodeMenu.Add("暂无节点（先在主窗口测速）")
	} else {
		for _, n := range names {
			item := nodeMenu.AddRadio(n, n == active)
			name := n
			item.OnClick(func(ctx *application.Context) {
				if err := svc.QuickSelectNode(name); err != nil {
					log.Println("托盘切换节点失败:", err)
				}
			})
		}
	}

	menu.AddSeparator()

	quit := menu.Add("退出 RoutePilot")
	quit.OnClick(func(ctx *application.Context) {
		svc.MarkQuitting()
		app.Quit()
	})

	tray.SetMenu(menu)
	tray.OnClick(func() {
		win.Show()
		win.Focus()
	})

	// 托盘图标状态：绿=运行中，黄=告警，红=错误/停止
	go func() {
		lastState := ""
		for {
			time.Sleep(10 * time.Second)
			if svc.Quitting() {
				return
			}
			st := svc.Status()
			var icon []byte
			var label string
			switch {
			case st.State != "running":
				icon = trayIconRed
				label = "内核未运行"
			case st.Error != "":
				icon = trayIconAmber
				label = "异常"
			default:
				icon = trayIconGreen
				label = fmt.Sprintf("运行中 · %s", st.Mode)
			}
			if label != lastState {
				tray.SetIcon(icon)
				tray.SetTooltip("RoutePilot — " + label)
				lastState = label
			}
		}
	}()
}
