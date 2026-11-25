//go:build windows

package tray

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Win32 常量（只取本包用到的，别照抄整份头文件）。
const (
	wmApp           = 0x8000
	msgCallback     = wmApp + 1 // 托盘图标事件
	msgQuit         = wmApp + 2 // 请求退出（Quit 发）
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmNull          = 0x0000
	wmDestroy       = 0x0002
	wmDisplayChange = 0x007E // 分辨率 / 显示器拓扑变了（含换屏、缩放变化）
	wmSettingChange = 0x001A // 系统设置变了
	wmThemeChange   = 0x031A // 主题已改变（明暗切换时也会来）

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2
	// nimSetVersion 把回调语义切到 V4：lParam 低 16 位是事件、高 16 位是图标 ID，
	// 单击变成 NIN_SELECT、右键变成 WM_CONTEXTMENU，**且键盘选中 NIN_KEYSELECT 才开始送达**。
	nimSetVersion      = 4
	notifyIconVersion4 = 4

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4
	nifInfo    = 0x10
	// nifShowTip（NIF_SHOWTIP）：V4 语义下 shell 默认不弹标准 tooltip（留给应用自绘
	// NIN_POPUPOPEN），必须显式带这条才显示——不设的话 tooltip 注册再对也不显示。
	// 权威值 = MSDN NOTIFYICONDATAW.uFlags：NIF_SHOWTIP 0x00000080（不是 0x40）。
	nifShowTip = 0x80

	// NIIF_* 与 tray.go 里 BalloonKind 的枚举值一一对应（BalloonNone=0 … BalloonError=3）。
	niifNone    = 0x0
	niifInfo    = 0x1
	niifWarning = 0x2
	niifError   = 0x3

	// V4 的事件码。注意这些都在 WM_USER 之上，与鼠标消息不在一个区间。
	ninSelect           = 0x0400 // 左键单击（V4）
	ninKeySelect        = 0x0401 // 键盘选中（Win+B → 方向键 → Enter），只有 V4 有
	ninBalloonShow      = 0x0402
	ninBalloonHide      = 0x0403
	ninBalloonTimeout   = 0x0404
	ninBalloonUserClick = 0x0405 // 用户点了气泡通知
	ninPopupOpen        = 0x0406 // 鼠标悬停图标（V4 tooltip 诊断用）
	ninPopupClose       = 0x0407

	wmContextMenu = 0x007B // V4 下右键走这个，而不是 WM_RBUTTONUP
	wmMButtonUp   = 0x0208

	idiApplication = 32512
	iconVersion300 = 0x00030000

	maxTipChars = 127 // V2 的 szTip 是 128 个 UTF-16 码元，含结尾 0

	// smCxSmIcon = SM_CXSMICON（系统小图标宽度；100% DPI 为 16，150% 为 24，200% 为 32）。
	smCxSmIcon = 49
)

// iconFlags 是 NOTIFYICONDATA.uFlags 的唯一来源，抽成函数供守卫测试钉住。
func iconFlags() uint32 { return nifMessage | nifIcon | nifTip | nifShowTip }

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	// pGetProcAddress 用来按**序号**取 uxtheme 的未文档化导出（见 applyTheme）。
	pGetProcAddress = kernel32.NewProc("GetProcAddress")

	pRegisterClassEx          = user32.NewProc("RegisterClassExW")
	pCreateWindowEx           = user32.NewProc("CreateWindowExW")
	pDefWindowProc            = user32.NewProc("DefWindowProcW")
	pGetMessage               = user32.NewProc("GetMessageW")
	pTranslateMessage         = user32.NewProc("TranslateMessage")
	pDispatchMessage          = user32.NewProc("DispatchMessageW")
	pPostMessage              = user32.NewProc("PostMessageW")
	pPostQuitMessage          = user32.NewProc("PostQuitMessage")
	pDestroyWindow            = user32.NewProc("DestroyWindow")
	pGetCursorPos             = user32.NewProc("GetCursorPos")
	pSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	pCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	pDestroyIcon              = user32.NewProc("DestroyIcon")
	pLoadIcon                 = user32.NewProc("LoadIconW")
	pGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	pRegisterWindowMessage    = user32.NewProc("RegisterWindowMessageW")
	pSetProcessDpiAwareness   = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetProcessDPIAware       = user32.NewProc("SetProcessDPIAware")
	pShellNotifyIcon          = shell32.NewProc("Shell_NotifyIconW")
	pShellNotifyIconGetRect   = shell32.NewProc("Shell_NotifyIconGetRect")
	pGetModuleHandle          = kernel32.NewProc("GetModuleHandleW")
)

func getSystemMetrics(idx int) int32 {
	r, _, _ := pGetSystemMetrics.Call(uintptr(idx))
	return int32(r)
}

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type point struct{ x, y int32 }

type msgT struct {
	hwnd     windows.Handle
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       point
	lPrivate uint32
}

// notifyIconData 是 NOTIFYICONDATAW 的**完整 Vista+ 布局**（含 guidItem / hBalloonIcon）。
//
// V1（到 szTip，64 字符）只能显示 tooltip；要弹系统通知必须有 szInfo / szInfoTitle。
// ★升级时 szTip 从 64 变 128 字符 —— 只加字段不扩 szTip 会整体错位，
// Windows 会把 szInfoTitle 当 tooltip 读。cbSize 一律由 unsafe.Sizeof 给出。
//
// ★2026-11-16：原实现止于 dwInfoFlags（V3，64 位 = 952），缺尾部 guidItem(16) +
// hBalloonIcon(8)。Win11 新任务栏按 cbSize 判定结构版本，尺寸不足会忽略依赖完整结构的
// 标志（如 NIF_SHOWTIP）。补齐后 64 位 = 976，与微软 NOTIFYICONDATAW 完全一致。
type notifyIconData struct {
	cbSize           uint32
	hWnd             windows.Handle
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            windows.Handle
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32 // 与 uTimeout 共用；NIM_ADD / NIM_SETVERSION 时写 4
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte       // GUID（本包用 uID 标识，保留全 0；仅占位保证 cbSize 完整）
	hBalloonIcon     windows.Handle // 自定义气球图标（保留 0 = 用默认）
}

// taskbarCreatedMsg explorer.exe 重启时广播的消息号（0 = 还没注册成功）。
//
// 见契约 C-7：explorer 重启会带走所有 Shell_NotifyIcon 注册 —— 进程还活着，图标却没了，
// 保活任务只看进程死活，救不回来。必须收到这条消息后重新 NIM_ADD。
var taskbarCreatedMsg atomic.Uint32

// backoffDelays 首次注册失败后的重试间隔（契约 C-10）。
//
// 为什么是这个量级：开机自启时托盘常比资源管理器先起来，Shell_NotifyIcon 内部
// SendMessageTimeout 最长阻塞 4 秒，所以第一次失败很正常，等 4 秒再试通常就好了；
// 后面指数拉长，最多试 5 次（总时长约 2 分钟），再失败就放弃并记日志 —— 无限重试会掩盖真问题。
var backoffDelays = []time.Duration{
	4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second,
}

// state 是消息泵线程与外部调用之间唯一共享的可变状态。
var (
	mu      sync.Mutex
	current *state
)

type state struct {
	opts         Options
	hwnd         windows.Handle
	icon         windows.Handle
	iconBytes    []byte // 原始图标字节：换屏 / SetIcon 后要按新 DPI 重新选档
	nid          notifyIconData
	version      atomic.Uint32 // 回调语义版本：4 = V4（键盘可达），0 = 退回 V0
	ready        atomic.Bool   // 图标是否已成功挂上（退避重试期间为 false）
	alive        atomic.Bool   // 窗口是否还有效（收到 Quit / WM_DESTROY 后置 false）
	style        atomic.Int32  // 菜单样式，运行时可切（SetStyle）
	lastPopupLog atomic.Uint64 // hover 诊断日志节流时间戳（UnixNano，0=从未记）
	lastOpen     atomic.Int64  // 上次 OnOpen 触发时刻（UnixNano，0=从未）；双击去重，见 shouldFireOpen
}

// openDebounceWindow 双击去重窗口。
//
// ★ 契约 §「左键回调 = 图标左键单击 / 双击」要求**一次物理动作只回调一次**。
// 但 Windows 通知协议下，一次物理双击会被 shell 拆成多个逻辑事件：
//   - V4：两次独立的 NIN_SELECT(0x400)，中间夹一个 WM_LBUTTONDBLCLK(0x203)
//   - V0：WM_LBUTTONUP + WM_LBUTTONDBLCLK + WM_LBUTTONUP
//
// eventOpensPanel 按**事件码**过滤只能挡住「同一次点击的补发消息」，挡不住
// 第二击那个合法事件 ⇒ OnOpen 被调两次 ⇒ 消费者开两个标签（hymuse 2026-11-21 实测）。
//
// 根治：按**时间窗**去重 —— 窗口内的后续「打开」事件一律不触发 OnOpen。
// 500ms 对齐 Windows 系统双击阈值（GetDoubleClickTime 默认 500）；两次独立
// 单击的正常人节奏远大于 500ms，不会误伤。
//
// 放在库内核（而非各消费者应用层）：契约是库承诺的，所有消费者都该拿到正确语义；
// 否则每个用 hytray 的项目都要自己写一遍节流，还容易漏。
const openDebounceWindow = 500 * time.Millisecond

// shouldFireOpen 是否该触发 OnOpen：事件码判定（eventOpensPanel）+ 时间窗去重。
//
// now/last 均为 UnixNano。纯函数，单测钉住「一次双击只回调一次 + 慢单击不被吞」。
// 时钟回拨（now < last）时放行：正常双击不可能是负间隔，别让系统调时把图标点死。
// window 由调用方传入（生产传 openDebounceWindow），便于单测反向自证「窗口=0 必放行」
// 时调的是**同一个真身**，不靠复制实现。
func shouldFireOpen(version, event uint32, now, last int64, window time.Duration) bool {
	if !eventOpensPanel(version, event) {
		return false
	}
	if last <= 0 || now < last {
		return true
	}
	return now-last >= int64(window)
}

// notifyIconIdentifier 是 NOTIFYICONIDENTIFIER（Shell_NotifyIconGetRect 的入参）。
//
// ⚠️ 只在 64 位 Windows 上正确：cbSize(4) + pad(4) + hWnd(8) + uID(4) + pad(4) + guid(16) = 40。
// 32 位下 sizeof 不同，本仓不分发 32 位产物，真需要时按 GOARCH 补一份。
type notifyIconIdentifier struct {
	cbSize uint32
	_      uint32
	hWnd   windows.Handle
	uID    uint32
	_      uint32
	guid   [16]byte
}

// wndProcCb 必须**包级创建一次**：syscall.NewCallback 每次调用都会占用一个回调槽，泄漏不起。
var wndProcCb = syscall.NewCallback(wndProc)

const wndClassName = "HyTrayWnd"

// Supported 本平台支持托盘（Windows）。
const supported = true

func run(opts Options) error {
	runtime.LockOSThread() // 窗口与消息泵必须在同一线程
	defer runtime.UnlockOSThread()

	if len(opts.Icon) == 0 {
		return fmt.Errorf("tray: Options.Icon 为空 —— 图标必须由调用方提供")
	}

	// ★必须在建窗与读取任何度量之前声明 DPI 感知。
	//
	// 踩过的坑（2026-11-16 首次跑 demo 发现）：不声明时进程是 DPI-unaware，
	// GetSystemMetrics(SM_CXSMICON) 永远返回 96dpi 的 16px —— 哪怕屏幕是 150%，
	// 于是 C-8 的选档逻辑全体失效，图标被系统按 16→24 拉伸，糊。
	enableDPIAwareness()

	// 菜单明暗也要在建窗之前定（进程级设置）。
	applyTheme(opts.Theme)

	hInst, _, _ := pGetModuleHandle.Call(0)
	if hInst == 0 {
		return fmt.Errorf("tray: GetModuleHandle 失败")
	}

	clsName, err := syscall.UTF16PtrFromString(wndClassName)
	if err != nil {
		return err
	}
	wc := wndClassEx{
		cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		lpfnWndProc:   wndProcCb,
		hInstance:     windows.Handle(hInst),
		lpszClassName: clsName,
	}
	if r, _, e := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("tray: RegisterClassEx 失败：%v", e)
	}

	// TaskbarCreated 必须在建窗**之前**注册：它是一条全局消息号，
	// 注册失败（极少）时 taskbarCreatedMsg 保持 0，恢复能力自然失效但不会误判别的消息。
	if name, err := syscall.UTF16PtrFromString("TaskbarCreated"); err == nil {
		if r, _, _ := pRegisterWindowMessage.Call(uintptr(unsafe.Pointer(name))); r != 0 {
			taskbarCreatedMsg.Store(uint32(r))
		}
	}

	// 隐藏的顶层窗口：不能用 message-only 窗口 —— TrackPopupMenu 需要能 SetForegroundWindow，
	// 而 message-only 窗口没有前台资格，菜单会弹完不消失。
	hwnd, _, e := pCreateWindowEx.Call(
		0,                                // dwExStyle
		uintptr(unsafe.Pointer(clsName)), // lpClassName
		uintptr(unsafe.Pointer(clsName)), // lpWindowName
		0,                                // dwStyle
		0, 0, 0, 0,                       // x, y, w, h
		0,     // hWndParent
		0,     // hMenu ← 少这一位，hInstance 会被当成菜单句柄（Invalid menu handle）
		hInst, // hInstance
		0,     // lpParam
	)
	if hwnd == 0 {
		return fmt.Errorf("tray: CreateWindowEx 失败：%v", e)
	}

	icon, iconSrc := loadIcon(opts.Icon)
	style := opts.Style
	if style == StyleGlass && !glassAvailable() {
		Logf("StyleGlass 不可用（本构建未含玻璃引擎），已降级 StyleNative")
		style = StyleNative
	}
	st := &state{
		opts:      opts,
		hwnd:      windows.Handle(hwnd),
		icon:      windows.Handle(icon),
		iconBytes: opts.Icon,
	}
	st.style.Store(int32(style))
	st.alive.Store(true)
	mu.Lock()
	current = st
	mu.Unlock()

	st.nid = notifyIconData{
		cbSize:           uint32(unsafe.Sizeof(notifyIconData{})),
		hWnd:             st.hwnd,
		uID:              1,
		uFlags:           iconFlags(),
		uCallbackMessage: msgCallback,
		hIcon:            st.icon,
	}
	// MSDN 示例：NIM_ADD 时 uVersion 就要写好（与 NIM_SETVERSION 一致），
	// 让 shell 在注册那一刻就按 V4 解析。之前 ADD 时留 0、事后 SETVERSION 补 4，
	// Win11 上可能导致 NIF_SHOWTIP 在注册阶段被按旧语义忽略。
	if !opts.V0Compat {
		st.nid.uVersion = notifyIconVersion4
	}
	copy(st.nid.szTip[:], syscall.StringToUTF16(trimTip(opts.Tooltip))) // 末尾自带 0
	Logf("窗口就绪 hwnd=%#x；%s；HICON=%#x；style=%d", hwnd, iconSrc, icon, style)

	// 注册放到 goroutine：Shell_NotifyIcon 内部有 SendMessageTimeout，最长阻塞 4 秒，
	// 堵住这里等于托盘启动就慢 4 秒（契约 C-10）。
	go addIconWithRetry(st)

	var m msgT
	for {
		r, _, e := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(r) {
		case -1:
			return fmt.Errorf("tray: GetMessage 出错：%v", e)
		case 0:
			return nil // WM_QUIT
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// addIconWithRetry 先试一次，失败转退避重试（契约 C-10）。
//
// 为什么首轮失败不返回错误：开机自启时托盘常比资源管理器先起来，此时失败是正常时序，
// 不是配置错误 —— 直接退出等于开机没有图标。
func addIconWithRetry(st *state) {
	if addIcon(st) {
		return
	}
	Logf("首次 Shell_NotifyIcon(NIM_ADD) 失败，转入退避重试")
	for i, d := range backoffDelays {
		time.Sleep(d)
		if !st.alive.Load() {
			return // 托盘已退出，别再折腾
		}
		if addIcon(st) {
			Logf("NIM_ADD 第 %d 次重试成功（等了 %s）", i+1, d)
			return
		}
	}
	Logf("NIM_ADD 重试 %d 次仍失败，本会话不会出现托盘图标（资源管理器没跑？）", len(backoffDelays))
}

// addIcon 挂图标 → 切 V4 回调语义 → 首次成功时触发 OnReady。
//
// 三步绑在一起是因为它们必须同生共死：Explorer 重启后重挂也得重新 SETVERSION，
// 否则新 shell 又退回 V0 语义（键盘选中就没了）。
func addIcon(st *state) bool {
	// 先删再添：任务栏里可能有上一次的残留条目，直接 ADD 会返回失败。
	shellNotify(nimDelete, &st.nid)
	if !shellNotify(nimAdd, &st.nid) {
		return false
	}

	// 默认 V4：键盘选中 / 中键 + 标准 tooltip 兼得。NIM_ADD 时已写 uVersion=4
	// （见上方初始化），这里再补一次 NIM_SETVERSION（MSDN 要求每次 ADD 都调）。
	// 标准 tooltip 靠完整结构(976)+NIF_SHOWTIP 保证，不是靠退回 V0。
	if !st.opts.V0Compat {
		nid := st.nid
		nid.uVersion = notifyIconVersion4
		if shellNotify(nimSetVersion, &nid) {
			st.version.Store(notifyIconVersion4)
			Logf("回调版本已切到 V4（键盘选中 / 中键 + 标准 tooltip 兼得）")
		} else {
			st.version.Store(0)
			Logf("NIM_SETVERSION 失败，保持 V0 语义")
		}
	} else {
		st.version.Store(0)
		Logf("V0Compat：保持 V0 回调语义（逃生舱，无键盘选中）")
	}

	if !st.ready.Swap(true) {
		Logf("Shell_NotifyIcon(NIM_ADD) 成功，图标已在托盘（tooltip=%q）", trimTip(st.opts.Tooltip))
		if st.opts.OnReady != nil {
			safeCall(st.opts.OnReady)
		}
	}
	return true
}

// readdAfterExplorerRestart 处理 TaskbarCreated（契约 C-7）。
//
// 顺序很重要：旧 shell 的登记随 shell 一起消失了，但**新 shell 可能仍认为 (hwnd, uid) 存在**，
// 此时直接 NIM_ADD 会失败 ⇒ 先 DELETE 清残留再 ADD（踩过的坑）。
func readdAfterExplorerRestart(st *state) {
	// 顺序很重要：旧 shell 的登记随 shell 一起消失了，但**新 shell 可能仍认为 (hwnd, uid) 存在**，
	// 此时直接 NIM_ADD 会失败 ⇒ addIcon 内部先 DELETE 再 ADD，并重设 V4 版本。
	if addIcon(st) {
		Logf("Explorer 重启后图标已重挂")
		return
	}
	Logf("Explorer 重启后重挂失败，转入退避重试")
	addIconWithRetry(st)
}

func quit() {
	mu.Lock()
	st := current
	mu.Unlock()
	if st == nil {
		return
	}
	pPostMessage.Call(uintptr(st.hwnd), msgQuit, 0, 0)
}

// SetTooltip 更新悬停提示（可在任意 goroutine 调）。
func SetTooltip(text string) {
	mu.Lock()
	st := current
	mu.Unlock()
	if st == nil {
		return
	}
	mu.Lock()
	copy(st.nid.szTip[:], syscall.StringToUTF16(trimTip(text)))
	nid := st.nid
	mu.Unlock()
	shellNotify(nimModify, &nid)
}

// trayEvent 把回调 lParam 解析成事件码。
//
// V4 与 V0 的差别就在这一行：V4 的 lParam = 图标ID<<16 | 事件码，
// V0 的 lParam 就是鼠标消息本身。抽成纯函数才能单测 —— 解析错了的后果是
// 「右键没反应」或「左键变成弹菜单」，而且只有真机点才知道。
func trayEvent(version uint32, lParam uintptr) uint32 {
	if version >= notifyIconVersion4 {
		return uint32(lParam & 0xffff)
	}
	return uint32(lParam)
}

// eventOpensPanel 判断某事件在给定回调版本下是否该触发 OnOpen（打开管理台）。
//
// ★ 2026-11-20 双标签根因：旧实现把 NIN_SELECT / WM_LBUTTONUP / WM_LBUTTONDBLCLK
// 三个事件一视同仁都触发 OnOpen。V4 语义下单次左键只有 NIN_SELECT，Win11 实测
// 同一次点击还会补发 WM_LBUTTONUP，旧映射于是当成第二次点击 ⇒ 开两个标签。
// 收窄为「按版本认事件」后，同一击的两个事件只有对的那一个生效。
func eventOpensPanel(version, event uint32) bool {
	if version >= notifyIconVersion4 {
		return event == ninSelect
	}
	return event == wmLButtonUp || event == wmLButtonDblClk
}

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	mu.Lock()
	st := current
	mu.Unlock()

	// TaskbarCreated 是运行时注册的动态消息号，必须比对值而不是常量。
	if tm := taskbarCreatedMsg.Load(); tm != 0 && msg == uintptr(tm) {
		Logf("收到 TaskbarCreated：Explorer 已重启")
		if st != nil {
			go readdAfterExplorerRestart(st) // 别在窗口过程里做会阻塞的 Shell 调用
		}
		return 0
	}

	switch msg {
	case msgCallback:
		if st == nil {
			return 0
		}
		// V4：低 16 位是事件码、高 16 位是图标 ID；V0：lParam 直接就是鼠标消息。
		event := trayEvent(st.version.Load(), lParam)
		switch event {
		case ninSelect, wmLButtonUp, wmLButtonDblClk:
			// 按版本分流 + 双击时间窗去重（见 shouldFireOpen）：
			//   - eventOpensPanel 挡「同一次点击的补发消息」（V4 的 WM_LBUTTONUP 等）
			//   - 时间窗挡「双击的第二击那个合法打开事件」（V4 的第二个 NIN_SELECT）
			// 两层缺一不可：只按事件码过滤挡不住第二个合法 0x400（2026-11-21 实测）。
			ver := st.version.Load()
			now := time.Now().UnixNano()
			last := st.lastOpen.Load()
			if !shouldFireOpen(ver, event, now, last, openDebounceWindow) {
				if eventOpensPanel(ver, event) {
					Logf("双击第二击：距上次 %dms，节流丢弃（version=%d event=%#x）",
						time.Duration(now-last).Milliseconds(), ver, event)
				} else {
					Logf("忽略重复左键事件：version=%d event=%#x（本版本只认 NIN_SELECT=%#x）", ver, event, ninSelect)
				}
				break
			}
			// CAS 更新时间戳：消息泵单线程，但 OnOpen 可能间接触发重入，
			// 保证只有一个胜者继续触发回调，不会两个都通过检查后各调一次。
			if !st.lastOpen.CompareAndSwap(last, now) {
				break
			}
			Logf("左键事件：version=%d event=%#x lParam=%#x", ver, event, lParam)
			if st.opts.OnOpen != nil {
				safeCall(st.opts.OnOpen)
			}
		case wmContextMenu, wmRButtonUp:
			// V4 下右键走 WM_CONTEXTMENU（菜单键、键盘触发也走它），V0 下才是 WM_RBUTTONUP。
			showMenuFor(st)
		case ninKeySelect:
			// 键盘选中（Win+B → 方向键 → Enter）。没有鼠标坐标，菜单得贴着图标弹。
			showMenuNearIcon(st)
		case ninBalloonUserClick:
			// 用户点了气泡通知 —— 这是通知最有价值的用法（点「已在托盘」直接开管理台）。
			if st.opts.OnBalloonClick != nil {
				safeCall(st.opts.OnBalloonClick)
			}
		case ninPopupOpen:
			// V4 tooltip 健康探针：NIF_SHOWTIP 生效时 shell 弹标准 tooltip、
			// **不发** NIN_POPUPOPEN；它来了 = shell 把悬停当「应用自绘 UI」信号，
			// 典型因 cbSize 非完整 976 或 NIM_ADD 时 uVersion 没写 4。
			// 2026-11-16 实证：952 + ADD 时 uVersion=0 ⇒ 连发 POPUPOPEN、tooltip 不弹；
			// 三处按 MSDN 对齐后 ⇒ tooltip 弹、POPUPOPEN 不再来。节流避免刷屏。
			if st.lastPopupLog.Load() == 0 || time.Since(time.Unix(0, int64(st.lastPopupLog.Load()))) > 10*time.Second {
				Logf("hover 命中托盘图标（NIN_POPUPOPEN），tooltip=%q uFlags=0x%x cbSize=%d",
					trimTip(st.opts.Tooltip), st.nid.uFlags, st.nid.cbSize)
				st.lastPopupLog.Store(uint64(time.Now().UnixNano()))
			}
		case wmMButtonUp:
			Logf("中键点击（暂不暴露回调）")
		}
		return 0
	case wmSettingChange, wmThemeChange:
		// 系统明暗切换会来这两条（不同 Windows 版本侧重不同）。
		//
		// 光在启动时调一次 SetPreferredAppMode 不够 —— 菜单主题有缓存，进程不重启就一直是旧色。
		// 实测（2026-11-16）：切成浅色后菜单仍是深色，退出重开才变。
		//
		// 不解析 lParam 里的 "ImmersiveColorSet"：那需要 unsafe 把 uintptr 转指针（vet 会报警），
		// 而 applyTheme 本身幂等、开销只有一次注册表读 —— 宁可多刷几次，不碰不安全的指针转换。
		if st != nil {
			which := "WM_SETTINGCHANGE"
			if msg == wmThemeChange {
				which = "WM_THEMECHANGED"
			}
			// 日志只说"收到哪条消息"，不断言"明暗变了" —— WM_SETTINGCHANGE 很宽泛，
			// 任何系统设置变更都会来，是不是主题变了由 applyTheme 读注册表后自己判断。
			Logf("收到 %s，重应用菜单主题", which)
			go applyTheme(st.opts.Theme)
		}
		return 0
	case wmDisplayChange:
		// 换屏 / 改缩放后系统小图标尺寸变了：原来按 16px 档做的图标在 200% 屏上会被拉伸。
		// 走 goroutine 是因为里面要调 Shell_NotifyIcon（可能阻塞）。
		if st != nil {
			go reloadIconForDPI(st)
		}
		return 0
	case msgQuit:
		Logf("收到退出请求，摘除托盘图标")
		if st != nil {
			st.alive.Store(false)
			shellNotify(nimDelete, &st.nid)
			if st.icon != 0 {
				pDestroyIcon.Call(uintptr(st.icon))
			}
		}
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		pPostQuitMessage.Call(0)
		if st != nil {
			st.alive.Store(false)
			mu.Lock()
			current = nil
			mu.Unlock()
		}
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, msg, wParam, lParam)
	return r
}

// enableDPIAwareness 声明本进程为 Per-Monitor V2 DPI 感知。
//
// 为什么库要替宿主进程做这件事：托盘进程通常是独立的无界面 exe，DPI 感知只影响
// 图标尺寸的度量，不会干扰任何业务逻辑；不做则 GetSystemMetrics 恒返回 96dpi 值，
// C-8 的选档直接失效（高 DPI 下图标被拉伸变糊）。
//
// 幂等：宿主已经声明过时调用会返回 E_ACCESSDENIED，忽略即可。
var dpiOnce sync.Once

func enableDPIAwareness() {
	dpiOnce.Do(enableDPIAwarenessOnce)
}

func enableDPIAwarenessOnce() {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (HANDLE)-4
	if r, _, _ := pSetProcessDpiAwareness.Call(^uintptr(3)); r != 0 {
		return
	}
	// Win7/8 只有这个（system DPI aware），有总比没有强。
	if r, _, _ := pSetProcessDPIAware.Call(); r != 0 {
		return
	}
	Logf("DPI 感知声明失败，高 DPI 下图标可能被拉伸")
}

// loadIcon 从调用方给的 ICO 字节里挑**最贴近系统小图标尺寸**的那一条做成 HICON。
// 直接把整份 .ico 喂给 CreateIconFromResourceEx 会失败 —— 它要的是单条目里的 DIB，
// 不含文件头的 ICONDIR/ICONDIRENTRY。
//
// 为什么按 SM_CXSMICON 选档而不取最大档（某实现改进后）：48px 交给 Windows
// 缩到 16px 是 1:3，锯齿明显、看着发虚。按系统目标选「不小于目标的最小档」：
// 100% DPI 命中 16px 原尺寸、150% 命中 24px、200% 命中 32px，都是 1:1 或接近 1:1。
//
// ⚠️ 前提是进程声明过 DPI 感知（见 enableDPIAwareness）—— 否则 target 恒为 16。
func loadIcon(ico []byte) (uintptr, string) {
	if len(ico) < 6 {
		return fallbackIcon("ICO 字节缺失")
	}
	if binary.LittleEndian.Uint16(ico[2:4]) != 1 { // type 必须 1 = icon
		return fallbackIcon("ICO 类型不是 1")
	}
	count := int(binary.LittleEndian.Uint16(ico[4:6]))

	type entry struct{ w, off, ln int }
	var cands []entry
	for i := 0; i < count; i++ {
		off := 6 + 16*i
		if off+16 > len(ico) {
			break
		}
		w := int(ico[off]) // 0 表示 256
		if w == 0 {
			w = 256
		}
		size := int(binary.LittleEndian.Uint32(ico[off+8 : off+12]))
		dataOff := int(binary.LittleEndian.Uint32(ico[off+12 : off+16]))
		if dataOff+size > len(ico) || size == 0 {
			continue
		}
		cands = append(cands, entry{w, dataOff, size})
	}
	if len(cands) == 0 {
		return fallbackIcon("ICO 里没有可用条目")
	}

	target := int(getSystemMetrics(smCxSmIcon))
	widths := make([]int, len(cands))
	for i, c := range cands {
		widths[i] = c.w
	}
	pick := cands[pickIconIndex(widths, target)]

	// 7 个参数一个都不能省（实测）：少传的三个位置拿到的是
	// **栈上残留值**，不是 0 ⇒ cxDesired/cyDesired 变成垃圾尺寸，图标创建随机失败、托盘显示空白。
	// cx=cy=0 = 用资源自带尺寸，Flags=0 = LR_DEFAULTCOLOR。
	h, _, _ := pCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&ico[pick.off])), uintptr(pick.ln), 1, iconVersion300,
		0, 0, 0,
	)
	src := fmt.Sprintf("ICO %dpx（共 %d 条，系统目标 %dpx）", pick.w, count, target)
	if h == 0 {
		return fallbackIcon(src + " · CreateIconFromResourceEx 失败")
	}
	return h, src
}

// pickIconIndex 在候选宽度里选「不小于 target 的最小档」；全比 target 小则取最大档。
//
// 这套选档是为了别让 Windows 做 1:3 缩放（48→16 锯齿明显）：100% DPI 命中 16px、
// 150% 命中 24px、200% 命中 32px，都是 1:1 或接近 1:1。抽成纯函数才能单测。
func pickIconIndex(widths []int, target int) int {
	best := -1
	for i, w := range widths {
		if w >= target && (best < 0 || w < widths[best]) {
			best = i
		}
	}
	if best < 0 {
		for i, w := range widths {
			if best < 0 || w > widths[best] {
				best = i
			}
		}
	}
	return best
}

// fallbackIcon 回落系统默认程序图标：至少有个可点的东西，比托盘一片空白强。
func fallbackIcon(reason string) (uintptr, string) {
	r, _, _ := pLoadIcon.Call(0, idiApplication)
	return r, reason + " · 已回落系统默认图标"
}

// reloadIconForDPI 显示器拓扑或缩放变化后，按新的 SM_CXSMICON 重新选档（契约 C-14）。
//
// 为什么必须做：笔记本 150% + 外接 100% 是很常见的组合，把托盘图标从高 DPI 屏拖到低 DPI 屏
// （或反之）后，原来的档位就不再合适 —— 32px 档缩到 16 会糊，16px 档拉到 32 会虚。
func reloadIconForDPI(st *state) {
	mu.Lock()
	ico := st.iconBytes
	mu.Unlock()
	if len(ico) == 0 {
		return
	}
	h, src := loadIcon(ico)
	if h == 0 {
		return
	}
	mu.Lock()
	old := st.icon
	st.icon = windows.Handle(h)
	st.nid.hIcon = st.icon
	nid := st.nid
	mu.Unlock()

	if !shellNotify(nimModify, &nid) {
		pDestroyIcon.Call(h)
		mu.Lock()
		st.icon = old
		st.nid.hIcon = old
		mu.Unlock()
		return
	}
	if old != 0 && old != st.icon {
		pDestroyIcon.Call(uintptr(old))
	}
	Logf("显示器变化，图标已按新 DPI 重选：%s", src)
}

// SetStyle 运行时切换菜单样式（下次右键生效，可在任意 goroutine 调）。
//
// 用途：让用户自己选「原生还是玻璃」并存进配置 —— 有人就喜欢系统原生的利落，
// 有人要玻璃的观感。注意玻璃不支持子菜单，切之前先确认菜单树是扁平的
// （含 Children 时玻璃会整份降级原生，看起来像「没切换」）。
func SetStyle(s Style) {
	mu.Lock()
	st := current
	mu.Unlock()
	if st == nil {
		Logf("托盘未运行，SetStyle 忽略")
		return
	}
	if s == StyleGlass && !glassSupported {
		Logf("本构建未含玻璃引擎，SetStyle(StyleGlass) 降级为原生")
		s = StyleNative
	}
	st.style.Store(int32(s))
	Logf("菜单样式已切换为 %d", int(s))
}

// SetIcon 运行时替换托盘图标（状态变化时用，比如「运行中 / 已停止」换图）。
//
// 传的是完整 ICO 字节，内部重新按当前 DPI 选档 —— 换屏（不同缩放的显示器）后
// 再调一次就能拿到合适的档位。旧 HICON 会被释放，调用方不用管生命周期。
func SetIcon(ico []byte) error {
	mu.Lock()
	st := current
	mu.Unlock()
	if st == nil {
		return errors.New("tray: 托盘未运行，无法替换图标")
	}
	if len(ico) == 0 {
		return errors.New("tray: 图标字节为空")
	}
	h, src := loadIcon(ico)
	if h == 0 {
		return fmt.Errorf("tray: 图标创建失败（%s）", src)
	}

	mu.Lock()
	old := st.icon
	st.icon = windows.Handle(h)
	st.iconBytes = ico // 记下新字节：换屏后要按这个重选档
	st.nid.hIcon = st.icon
	nid := st.nid
	mu.Unlock()

	if !shellNotify(nimModify, &nid) {
		// 换图失败也别把新 HICON 泄漏掉；界面上还是旧图标，下次成功再换。
		pDestroyIcon.Call(h)
		mu.Lock()
		st.icon = old
		st.nid.hIcon = old
		mu.Unlock()
		return errors.New("tray: Shell_NotifyIcon(NIM_MODIFY) 失败，图标未替换")
	}
	if old != 0 && old != st.icon {
		pDestroyIcon.Call(uintptr(old))
	}
	Logf("图标已替换：%s", src)
	return nil
}

// Balloon 弹一条信息级通知（保留旧签名，等价于 ShowBalloon(..., BalloonInfo)）。
func Balloon(title, text string) { ShowBalloon(title, text, BalloonInfo) }

// ShowBalloon 按级别弹一条系统通知（Win10+ 会走通知中心）。
//
// 托盘图标找不到时这是唯一能让用户看见「它在这儿」的手段 —— 通知就是从图标位置冒出来的。
// ★调用方必须自己节流（契约 C-9），库提供 Throttle 工具，不要每次都弹。
func ShowBalloon(title, text string, kind BalloonKind) {
	mu.Lock()
	st := current
	mu.Unlock()
	if st == nil {
		return
	}
	mu.Lock()
	nid := st.nid // 复制：只改本次要用的字段，别污染正式状态
	mu.Unlock()

	var info uint32
	switch kind { // 枚举值与 Win32 的 NIIF_* 一一对应，见 tray.go 的 BalloonKind 定义
	case BalloonWarn:
		info = niifWarning
	case BalloonError:
		info = niifError
	case BalloonNone:
		info = niifNone
	default:
		info = niifInfo
	}
	nid.uFlags = nifInfo
	copy(nid.szInfoTitle[:], syscall.StringToUTF16(trimUTF16(title, 63)))
	copy(nid.szInfo[:], syscall.StringToUTF16(trimUTF16(text, 255)))
	nid.dwInfoFlags = info
	shellNotify(nimModify, &nid)
}

func trimUTF16(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}

func trimTip(s string) string { return trimUTF16(s, maxTipChars) }

// shellNotify 调 Shell_NotifyIcon；资源管理器刚重启时会短暂失败，重试一次再放弃。
func shellNotify(action uint32, nid *notifyIconData) bool {
	for attempt := 0; attempt < 2; attempt++ {
		if r, _, _ := pShellNotifyIcon.Call(uintptr(action), uintptr(unsafe.Pointer(nid))); r != 0 {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// safeCall 挡住回调里的 panic：窗口过程是 C 回调，panic 会直接把进程带走，
// 而托盘只是「附属 UI」，不该有这个杀伤力。
func safeCall(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			Logf("回调 panic 已拦下：%v", r)
		}
	}()
	fn()
}

// SingleInstance 用命名互斥体保证只有一份托盘：返回 ok=false 表示已经有一份在跑。
//
// 为什么不用端口占位/锁文件：托盘可能由计划任务、手动、开机启动三处拉起，
// 互斥体是 Windows 上唯一「进程死了自动释放」的判据（锁文件会留残留，端口会被别人占）。
func SingleInstance(name string) (bool, func()) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return true, func() {} // 名字都编不出来：宁可放行，也别让托盘起不来
	}
	h, err := windows.CreateMutex(nil, false, p)
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			if h != 0 {
				windows.CloseHandle(h)
			}
			return false, func() {}
		}
		return true, func() {} // 其它错误（权限等）不拦启动
	}
	return true, func() { windows.CloseHandle(h) }
}

// 未文档化的 uxtheme 导出（只按序号存在，没有名字）。
//
// ★这些值和序号**都不在微软公开文档里**，是社区逆向 + 我们实测的产物：
//
//	· 序号 135 / 136：dumpbin 看 uxtheme 导出表（无名导出）、反汇编 Explorer 的调用点
//	· 枚举语义：rounk-ctrl 的 "Win32 Dark Mode" gist、Microsoft Q&A 上 Castorix31 的实验帖、
//	  Emacs 2026-08 的 w32fns.c 补丁（三处一致：Default=0 AllowDark=1 ForceDark=2 ForceLight=3）
//	· 我们自己的实测（2026-11-16）：2 = 强制深色有效、3 = 强制浅色有效、
//	  1 = 允许深色但「深→浅」跟随、「浅→深」不变（不对称，所以没用它）
//
// 既非官方契约就可能随 Windows 版本变 —— 取不到序号必须能静默降级，且序号要钉进测试
// （TestUxthemeOrdinals），否则写错只会"悄悄不变色"，极难排查。
const (
	uxthemeSetPreferredAppMode = 135
	uxthemeFlushMenuThemes     = 136

	pmDefault    = 0
	pmAllowDark  = 1 // 允许深色：跟随系统设置**自动**切换
	pmForceDark  = 2 // 强制深色：钉死，系统切回浅色也不变（实测 2026-11-16 踩过）
	pmForceLight = 3
)

// applyTheme 让原生菜单跟随系统明暗（契约 C-17）。
//
// 背景：Win32 原生菜单**默认恒为浅色** —— 系统深色模式下任务栏是深的、菜单是白的，
// 深色屏配白菜单很突兀。微软没有公开这个开关，只能调 uxtheme.dll 的未文档化导出：
//
//	135 = SetPreferredAppMode(PreferredAppMode)  0=Default 1=AllowDark 2=ForceDark 3=ForceLight
//	136 = FlushMenuThemes()
//
// ★坑：不能用 windows.NewProc("#135") —— 那会把 "#135" 当函数名交给 GetProcAddress，
// 而它只支持「把序号当指针值传」（C 的 MAKEINTRESOURCE(135)）。这里直接把 135 作为参数
// 传给 GetProcAddress，序号 < 65536 时 Windows 会自动按序号查。
//
// 未文档化 = 序号可能随版本变，所以取不到就静默放弃（菜单退回浅色，功能不受影响）。
// 社区把这条当标准做法（Emacs 2026-08 的 w32fns.c 补丁同样处理）。
func applyTheme(mode Theme) {
	want := -1
	switch mode {
	case ThemeDark:
		want = pmForceDark
	case ThemeLight:
		want = pmForceLight
	default: // ThemeAuto
		// ★按**当前**系统状态显式指定 ForceDark / ForceLight，不要用 AllowDark。
		//
		// 实测（2026-11-16，两个方向都试过）：
		//   · AllowDark 在启动时（系统深色）能出深色菜单；
		//   · 系统切浅 → 收到消息重设 AllowDark → 菜单变浅 ✅；
		//   · 系统切回深 → 重设 AllowDark → 菜单**仍是浅色** ❌（不对称）。
		// 而显式 Force* 每次按实际状态指定，两个方向都稳。
		if systemPrefersDark() {
			want = pmForceDark
		} else {
			want = pmForceLight
		}
	}

	h, err := windows.LoadLibrary("uxtheme.dll")
	if err != nil {
		Logf("加载 uxtheme 失败，菜单保持系统默认：%v", err)
		return
	}
	addr, _, _ := pGetProcAddress.Call(uintptr(h), uxthemeSetPreferredAppMode)
	if addr == 0 {
		Logf("uxtheme 没有 ordinal %d 导出，菜单保持系统默认浅色", uxthemeSetPreferredAppMode)
		return
	}
	prev, _, _ := syscall.SyscallN(addr, uintptr(want))
	Logf("菜单明暗已设为 %d（原模式 %d）", want, prev)
	if flush, _, _ := pGetProcAddress.Call(uintptr(h), uxthemeFlushMenuThemes); flush != 0 {
		syscall.SyscallN(flush)
	}
}

// systemPrefersDark 读「应用模式」是否为深色。
//
// 读的是 AppsUseLightTheme 而不是 SystemUsesLightTheme：前者管 Win32 应用（菜单/对话框），
// 后者管任务栏/开始菜单。两者可以不一致 —— 只读后者会出现「任务栏深色但菜单按浅色处理」。
func systemPrefersDark() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	if err != nil {
		return false
	}
	return v == 0
}
