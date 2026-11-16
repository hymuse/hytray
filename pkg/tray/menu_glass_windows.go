//go:build windows

package tray

// 自绘托盘菜单：微信 / UU / 火绒那一类「好看」的右键菜单都不是系统
// TrackPopupMenu —— 那是 Win32 经典样式。它们全是**应用自绘弹出窗**，且带液态玻璃质感。
//
// 本文件的实现口径（延续零依赖纪律，全部标准库 + x/sys）：
//   - **UpdateLayeredWindow 逐像素 alpha**：菜单底板（半透明圆角矩形）由 Go 直接写
//     BGRA 位图生成 —— GDI 绘制的像素 alpha 恒 0（不透明），手写的底板才带透明度。
//   - **ACCENT_ACRYLICBLURBEHIND**（未公开但广泛使用的稳定 ABI，TranslucentTB 同款）
//     给窗口垫真模糊；垫上后底板可以调淡（透过它看到模糊背景）= 液态玻璃。
//     垫不上（老系统 / API 拒绝）就加厚底板，退化为半透明无模糊，观感仍可接受。
//   - 亮暗跟随系统（AppsUseLightTheme 注册表）。
//   - 键盘导航（↑↓/Enter/Esc）+ 失焦即收（微信式）。
//   - 嵌套消息泵：showCustomMenu 在托盘泵线程里再跑一层 GetMessage，WM_QUIT 转投外层。
//
// ⚠️ 血泪史（按顺序踩的雷，动这里之前先读）：
//   1. SetWindowPos 提 z 序必须 NOSIZE|NOMOVE|NOACTIVATE 三件套 —— 缺 NOMOVE 菜单瞬移 (0,0)。
//   2. GDI 画的 brush 不 SelectObject 进 DC = 用 stock WHITE_BRUSH（悬停白条吞字）。
//   3. SystemBackdrop(38) 路线整窗隐形：GDI 像素 alpha 恒 0，backdrop 上什么都看不见
//      —— 这就是为什么最终走 ULW 手写 alpha，而不是 DWM backdrop。

import (
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"strconv"

	"math"

	"image"
	"image/png"
	"os"
)

var (
	dwmapi    = windows.NewLazySystemDLL("dwmapi.dll")
	pDwmSetWA = dwmapi.NewProc("DwmSetWindowAttribute")
)

const (
	dwmwaUseImmersiveDarkMode = 20
	wcaAccentPolicy           = 19 // SetWindowCompositionAttribute 的 WCA_ACCENT_POLICY
	accentAcrylic             = 4  // ACCENT_ENABLE_ACRYLICBLURBEHIND
)

// 菜单窗口补充常量。
const (
	wmMousemove   = 0x0200
	wmLButtonDown = 0x0201
	wmRButtonDown = 0x0204
	wmKeyDown     = 0x0100
	wmKillFocus   = 0x0008
	wmEraseBkgnd  = 0x0014
	wmMouseLeave  = 0x02A2
	wmCloseMenu   = wmApp + 3 // 关闭自绘菜单（pump 截获，不进窗口过程）
	whMouseLL     = 14        // WH_MOUSE_LL：全局低级鼠标钩子（无需前台焦点）

	vkEscape = 0x1B
	vkUp     = 0x26
	vkDown   = 0x28
	vkReturn = 0x0D

	wsPopup        = 0x80000000
	wsExTopmost    = 0x00000008
	wsExToolWnd    = 0x00000080
	wsExLayered    = 0x00080000
	wsExNoActivate = 0x08000000 // 不抢焦点：托盘从溢出面板右键时，面板不会被我们挤掉
	swpNoSize      = 0x0001
	swpNoMove      = 0x0002 // ⚠️ 漏了它 SetWindowPos 会把窗口挪到 (0,0)（实测雷）
	swpNoActivate  = 0x0010
	swShownoact    = 8
	monitorNearest = 2
	ulwAlpha       = 2 // ULW_ALPHA
	lwaAlpha       = 2 // SetLayeredWindowAttributes 的 LWA_ALPHA（RDP 兼容回落）

	logpixelSX   = 88
	tmeLeave     = 2
	dtSingleline = 0x20
	dtVcenter    = 0x4
	dtNoprefix   = 0x800
	qualityAA    = 4 // ANTIALIASED_QUALITY（透明底上 ClearType 有彩边）
	stockNullBr  = 5 // NULL_BRUSH
	penNull      = 18
	psSolid      = 0
	biRGB        = 0
)

var (
	gdi32 = windows.NewLazySystemDLL("gdi32.dll")

	pGetDeviceCaps      = gdi32.NewProc("GetDeviceCaps")
	pCreateFontW        = gdi32.NewProc("CreateFontW")
	pSelectObject       = gdi32.NewProc("SelectObject")
	pDeleteObjectGdi    = gdi32.NewProc("DeleteObject")
	pCreateSolidBrushG  = gdi32.NewProc("CreateSolidBrush")
	pSetBkMode          = gdi32.NewProc("SetBkMode")
	pSetTextColorGdi    = gdi32.NewProc("SetTextColor")
	pRoundRect          = gdi32.NewProc("RoundRect")
	pCreatePen          = gdi32.NewProc("CreatePen")
	pGetStockObject     = gdi32.NewProc("GetStockObject")
	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pDeleteDC           = gdi32.NewProc("DeleteDC")
	pGetTextExtent32    = gdi32.NewProc("GetTextExtentPoint32W")
	pCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	pUpdateLayeredWin   = user32.NewProc("UpdateLayeredWindow")
	pBeginPaintProc     = user32.NewProc("BeginPaint")
	pEndPaintProc       = user32.NewProc("EndPaint")
	pBitBltProc         = gdi32.NewProc("BitBlt")
	pCreateRoundRectRgn = gdi32.NewProc("CreateRoundRectRgn")
	pSetWindowRgnProc   = user32.NewProc("SetWindowRgn")
	pSetLayeredWinAttr  = user32.NewProc("SetLayeredWindowAttributes")

	pSetFocusProc       = user32.NewProc("SetFocus")
	pLoadCursor         = user32.NewProc("LoadCursorW")
	pSetWindowsHookExW  = user32.NewProc("SetWindowsHookExW")
	pUnhookWinHookExW   = user32.NewProc("UnhookWindowsHookEx")
	pCallNextHookEx     = user32.NewProc("CallNextHookEx")
	pSetWindowPosProc   = user32.NewProc("SetWindowPos")
	pSetProcDpiCtx      = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetWinCompAttr     = user32.NewProc("SetWindowCompositionAttribute")
	pTrackMouseEvent    = user32.NewProc("TrackMouseEvent")
	pGetMonitorInfoW    = user32.NewProc("GetMonitorInfoW")
	pMonitorFromPoint   = user32.NewProc("MonitorFromPoint")
	pGetDCProc          = user32.NewProc("GetDC")
	pReleaseDCProc      = user32.NewProc("ReleaseDC")
	pFillRectProc       = user32.NewProc("FillRect")
	pDrawTextProc       = user32.NewProc("DrawTextW")
	pInvalidateRectProc = user32.NewProc("InvalidateRect")
	pValidateRectProc   = user32.NewProc("ValidateRect")
	pGetClientRectProc  = user32.NewProc("GetClientRect")
	pDestroyWindowProc  = user32.NewProc("DestroyWindow")
	pShowWindowProc     = user32.NewProc("ShowWindow")
	pGetModuleHandleP   = kernel32.NewProc("GetModuleHandleW")
)

// 颜色（Go 侧字面量，不走 web 的令牌守卫）：COLORREF = 0x00BBGGRR。
type menuColors struct {
	text, disabled, separator, hover, border uint32
}

// dotColor 状态点语义色（COLORREF）。暗色主题用亮色档（深板上要跳出来），
// 亮色主题用深色档（白板上要压得住）。
func dotColor(k DotKind, dark bool) uint32 {
	if dark {
		switch k {
		case DotRun:
			return 0x006CE02E // #2EE06C —— 饱和度 79% / hue 141°：#A8F792（41%/107°）
			// 和 #45E87C（70%/137°）都还偏「薄荷黄绿」，实测这个值才像「运行中」的绿
		case DotWarn:
			return 0x0024BFFB // #FBBF24
		case DotIdle:
			return 0x008C8C8C
		}
		return 0
	}
	switch k {
	case DotRun:
		return 0x003D8015 // #15803D
	case DotWarn:
		return 0x000953B4 // #B45309
	case DotIdle:
		return 0x00A0A0A0
	}
	return 0
}

func menuPalette(dark bool) menuColors {
	if dark {
		// Win11 暗色上下文菜单近似：文字 #FFFFFF，hover #3F3F3F
		return menuColors{text: 0x00FFFFFF, disabled: 0x009E9E9E, separator: 0x00454545, hover: 0x003F3F3F, border: 0x00454545}
	}
	// 亮色：文字 #1B1B1B，hover #EFEFEF
	return menuColors{text: 0x001B1B1B, disabled: 0x00A0A0A0, separator: 0x00D5D5D5, hover: 0x00EFEFEF, border: 0x00C9C9C9}
}

// segDist2 返回点 (px,py) 到线段 (ax,ay)-(bx,by) 的最短距离平方。
// 退化线段（首尾同点）自动按点处理。画选中态发光竖线时算到线段的距离。
func segDist2(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	len2 := dx*dx + dy*dy
	t := 0.0
	if len2 > 0 {
		t = ((px-ax)*dx + (py-ay)*dy) / len2
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
	}
	qx, qy := px-(ax+t*dx), py-(ay+t*dy)
	return qx*qx + qy*qy
}

// menuState 菜单窗口运行时状态（只在菜单泵线程动）。
type menuState struct {
	items      []MenuItem
	hwnd       windows.Handle
	font       uintptr
	fontBig    uintptr // 2x 画布用字体（超采样：22px → 44px）
	dark       bool
	plateBuf   []byte  // 液态玻璃底板缓存（抓屏+blur+暗化+圆角，生成一次）
	plateLuma  float64 // 底板平均亮度（hover / 分隔线按它派生一档差，固定灰值会失效）
	blur       bool    //（遗留字段，ACCENT 已弃用）
	scale      int     // dpi / 96
	width      int
	itemH      int
	sepH       int
	padV       int
	hover      int // -1 = 无
	result     int // 选中项 ID；0 = 未选中（点空白 / Esc / 失焦）
	closed     bool
	fallback   bool // ULW 失败（RDP 场景必失败）→ SLWA + 普通绘制
	radius     int  // 底板圆角物理像素（外部点击判定按圆角板，不是窗口矩形）
	shad       int  // 外投影外扩物理像素（窗口比板大一圈，多出来的是阴影区）
	posX, posY int32
}

var (
	menuMu      sync.Mutex
	curMenu     *menuState
	menuProc    = syscall.NewCallback(menuWndProc)
	menuClsOnce sync.Once
)

// PreviewMenu 在光标位置弹一次自绘菜单（独立于托盘图标），供 --menu-preview
// 截图与点选验证。选中 / 关闭后返回。
func PreviewMenu(items []MenuItem, onPick func(id int)) error {
	return showCustomMenu(items, onPick)
}

// menuOpenAt 测试钩子：非空时菜单弹在指定坐标而不是光标处（--menu-preview --pos）。
var menuOpenAt *point

// PreviewMenuAt 在指定坐标弹一次自绘菜单（验证用）。
func PreviewMenuAt(items []MenuItem, x, y int, onPick func(id int)) error {
	p := point{x: int32(x), y: int32(y)}
	menuOpenAt = &p
	defer func() { menuOpenAt = nil }()
	// 调试钩子：HYMUSE_PREVIEW_HOVER=N 让预览菜单初始悬停第 N 项（验证 hover 块渲染）
	if hv := os.Getenv("HYMUSE_PREVIEW_HOVER"); hv != "" {
		if n, e := strconv.Atoi(hv); e == nil && n >= 0 && n < len(items) {
			showCustomMenuWithHover = n
		}
	}
	return showCustomMenu(items, onPick)
}

// showCustomMenuWithHover：预览调试用初始悬停项（-1 = 无）；showCustomMenu 里消费后复位。
var showCustomMenuWithHover = -1

// showCustomMenu 自绘菜单主入口：量尺寸 → 建窗 → 嵌套泵 → 清理 → 回调。
// 在托盘泵线程调用（showMenu 已在该线程）。
func showCustomMenu(items []MenuItem, onPick func(id int)) error {
	if len(items) == 0 {
		return nil
	}
	enableDPIAwareness() // 预览路径不经过 tray.Run，必须在这里也设（教训）
	menuClsOnce.Do(registerMenuClass)

	s := &menuState{items: items, dark: systemPrefersDark(), hover: showCustomMenuWithHover}
	showCustomMenuWithHover = -1 // 消费后复位（调试钩子只影响一次）
	if !s.measureAndLayout() {
		return nil
	}

	menuMu.Lock()
	if curMenu != nil { // 已有菜单在弹：先关旧的，避免双窗抢焦点
		old := curMenu
		pPostMessage.Call(uintptr(old.hwnd), wmCloseMenu, 0, 0)
	}
	curMenu = s
	menuMu.Unlock()

	if err := s.createWindow(); err != nil {
		menuMu.Lock()
		curMenu = nil
		menuMu.Unlock()
		return err
	}

	// 「点击菜单外关闭」：**WH_MOUSE_LL 全局低级鼠标钩子**。
	// ⚠️ 不能用 SetCapture —— MSDN：「只有前台窗口才能捕获鼠标」，NOACTIVATE 的后台
	//    菜单窗 capture 静默失效（两轮实测点外不关的真凶）。低级钩子无需
	//    焦点、只观察不拦截（外部的点击照常送达目标窗口，不吞首次点击），菜单关闭即卸载。
	installMouseHook()
	defer uninstallMouseHook()

	s.pump()
	pDestroyWindowProc.Call(uintptr(s.hwnd))

	menuMu.Lock()
	curMenu = nil
	menuMu.Unlock()

	if id := s.result; id != 0 && onPick != nil {
		safeCall(func() { onPick(id) })
	}
	return nil
}

// registerMenuClass 注册菜单窗口类（进程级一次）。
// ⚠️ hCursor 必须设 IDC_ARROW —— 窗口类没光标时鼠标悬停显示的是「忙碌转圈」
// （实测：悬停菜单一直转圈）。
func registerMenuClass() {
	name, _ := syscall.UTF16PtrFromString("HymuseTrayMenuWnd")
	hcur, _, _ := pLoadCursor.Call(0, 32512) // IDC_ARROW
	wc := wndClassEx{
		cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		lpfnWndProc:   menuProc,
		hInstance:     windows.Handle(moduleHandle()),
		hCursor:       windows.Handle(hcur),
		lpszClassName: name,
	}
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
}

// measureAndLayout 量文本、算窗口尺寸与绘制常量。
func (s *menuState) measureAndLayout() bool {
	hdc, _, _ := pGetDCProc.Call(0)
	defer pReleaseDCProc.Call(0, hdc)
	dpi, _, _ := pGetDeviceCaps.Call(hdc, logpixelSX)
	if dpi == 0 {
		dpi = 96
	}
	s.scale = int(dpi*100) / 96 // 千分比缩放（100=100%，125=125%，150=150%）—— 整数除法会把 1.5x 截成 1x（实测「面板文字太小」）
	if s.scale < 100 {
		s.scale = 100
	}
	// 微软雅黑 UI 9pt：中文屏显最稳；负高度 = 字符高度；灰阶抗锯齿（透明底上 ClearType 有彩边）
	s.font, _, _ = pCreateFontW.Call(
		uintptr(-11*int(dpi)/72), 0, 0, 0, 400, 0, 0, 0, 1 /*DEFAULT_CHARSET*/, 0, 0, qualityAA, 0,
		uintptr(unsafe.Pointer(utf16Ptr("Microsoft YaHei UI"))))
	if s.font == 0 {
		return false
	}
	// 2x 画布专用字体（高度 ×ss）：同一字号在 2x 位图上绘制，降采样后边缘更细腻
	s.fontBig, _, _ = pCreateFontW.Call(
		uintptr(-11*int(dpi)*ss/72), 0, 0, 0, 400, 0, 0, 0, 1 /*DEFAULT_CHARSET*/, 0, 0, qualityAA, 0,
		uintptr(unsafe.Pointer(utf16Ptr("Microsoft YaHei UI"))))

	pSelectObject.Call(hdc, s.font)
	s.itemH = 40 * s.scale / 100
	s.sepH = 10 * s.scale / 100
	s.padV = 6 * s.scale / 100

	maxW := 0
	for _, it := range s.items {
		if it.Separator {
			continue
		}
		var sz size
		u := utf16Ptr(it.Label)
		pGetTextExtent32.Call(hdc, uintptr(unsafe.Pointer(u)), uintptr(utf16Count(it.Label)), uintptr(unsafe.Pointer(&sz)))
		if int(sz.cx) > maxW {
			maxW = int(sz.cx)
		}
	}
	// 左右各留 20*scale（hover 内缩 6 + 文字 14），下限 180*scale 保住「菜单」体感
	w := maxW + 40*s.scale/100
	if w < 180*s.scale/100 {
		w = 180 * s.scale / 100
	}
	if w > 340*s.scale/100 {
		w = 340 * s.scale / 100
	}
	s.width = w
	return true
}

type size struct{ cx, cy int32 }

func utf16Ptr(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}
func utf16Count(s string) int { return len([]rune(s)) }

// createWindow 建分层无边框窗：光标处弹出（menuOpenAt 覆盖，验证用）、贴屏幕边缘翻转、
// ACCENT acrylic 真模糊垫底、DWM 暗色。
func (s *menuState) createWindow() error {
	var pt point
	if menuOpenAt != nil {
		pt = *menuOpenAt
	} else {
		pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	}
	s.posX, s.posY = pt.x, pt.y

	var itemsH int
	for _, it := range s.items {
		if it.Separator {
			itemsH += s.sepH
		} else {
			itemsH += s.itemH
		}
	}
	w, h := s.width, itemsH+2*s.padV

	// 光标点为锚，右/下放不下就往左/上收，并夹回工作区（避开任务栏）。
	// 贴边留**呼吸间距**（12 逻辑像素）：翻转后底边精确贴 rcWork.bottom 会与任务栏
	// 紧贴在一起（截图实测「面板下方贴一起了」）。
	gap := 12 * s.scale / 100
	if mi := monitorInfoAt(int(pt.x), int(pt.y)); mi != nil {
		Logf("菜单定位：光标(%d,%d) rcWork=(%d,%d)-(%d,%d)", int(pt.x), int(pt.y),
			int(mi.rcWork.left), int(mi.rcWork.top), int(mi.rcWork.right), int(mi.rcWork.bottom))
		if int(pt.x)+w > int(mi.rcWork.right) {
			pt.x = int32(int(mi.rcWork.right) - w - gap)
		}
		if int(pt.y)+h > int(mi.rcWork.bottom) {
			pt.y = int32(int(mi.rcWork.bottom) - h - gap)
		}
		if int(pt.x) < int(mi.rcWork.left) {
			pt.x = mi.rcWork.left
		}
		if int(pt.y) < int(mi.rcWork.top) {
			pt.y = mi.rcWork.top
		}
	}
	// 兜底：无论如何夹回**虚拟屏**（全部显示器的并集）—— 多屏布局假设失效时的最后防线，
	// 保证菜单底边永不越过任何屏幕物理边界（实测「下方超出屏幕」的保底修复）。
	vx, _, _ := pGetSystemMetrics.Call(76) // SM_XVIRTUALSCREEN
	vy, _, _ := pGetSystemMetrics.Call(77) // SM_YVIRTUALSCREEN
	vw, _, _ := pGetSystemMetrics.Call(78) // SM_CXVIRTUALSCREEN
	vh, _, _ := pGetSystemMetrics.Call(79) // SM_CYVIRTUALSCREEN
	if vw > 0 && vh > 0 {
		if int(pt.x)+w > int(vx)+int(vw) {
			pt.x = int32(int(vx) + int(vw) - w)
		}
		if int(pt.y)+h > int(vy)+int(vh) {
			pt.y = int32(int(vy) + int(vh) - h)
		}
		if int(pt.x) < int(vx) {
			pt.x = int32(vx)
		}
		if int(pt.y) < int(vy) {
			pt.y = int32(vy)
		}
	}

	cls, _ := syscall.UTF16PtrFromString("HymuseTrayMenuWnd")
	s.radius = 16 * s.scale / 100 // 外部点击判定按圆角板（wndProc 用）
	// **外投影外扩**（实测）：ULW 位图必须等于窗口，所以窗口比板大一圈，
	// 多出来的 pad 区在 paintLayered 里画柔和阴影。没有这层阴影，玻璃永远像
	// 贴在背景上的色块（实测「没有玻璃边缘质感」）。
	s.shad = 14 * s.scale / 100
	s.posX, s.posY = pt.x-int32(s.shad), pt.y-int32(s.shad)
	hwnd, _, err := pCreateWindowEx.Call(
		uintptr(wsExTopmost|wsExToolWnd|wsExLayered|wsExNoActivate),
		uintptr(unsafe.Pointer(cls)), 0,
		uintptr(wsPopup),
		uintptr(s.posX), uintptr(s.posY), uintptr(w+2*s.shad), uintptr(h+2*s.shad),
		0, 0, moduleHandle(), 0)
	if hwnd == 0 {
		return err
	}
	s.hwnd = windows.Handle(hwnd)

	dark := uintptr(boolToInt(s.dark))
	pDwmSetWA.Call(hwnd, dwmwaUseImmersiveDarkMode, uintptr(unsafe.Pointer(&dark)), 1)
	// ACCENT acrylic 垫真模糊；垫上 ⇒ 底板调淡透出模糊 = 液态玻璃
	// （pblend=NULL 才是 ULW GEN_FAILURE 的元凶，ACCENT 无辜，恢复启用）
	_ = enableBlur // ACCENT 已弃用（对 ULW 不渲染）—— 玻璃改为抓屏自糊（paintLayered ①）

	pShowWindowProc.Call(hwnd, swShownoact)
	pSetWindowPosProc.Call(hwnd, ^uintptr(0) /*HWND_TOPMOST*/, 0, 0, 0, 0,
		uintptr(swpNoSize|swpNoMove|swpNoActivate))
	// **不抢焦点**（NOACTIVATE，也不 SetForegroundWindow/SetFocus）：用户从托盘溢出面板
	// 右键时，面板「失焦即关」—— 菜单一抢焦点面板就被挤掉（截图实测：
	// 「我还没选择呢，托盘会自动关闭」）。改用 WH_MOUSE_LL 低级钩子（见 showCustomMenu）：
	// 菜单照常收 hover/点击；点外关闭由 WH_MOUSE_LL 钩子负责（showCustomMenu）。
	s.paintLayered()
	Logf("菜单窗口 %dx%d @ (%d,%d) itemH=%d sepH=%d padV=%d scale=%d dark=%v",
		w, h, int(pt.x), int(pt.y), s.itemH, s.sepH, s.padV, s.scale, s.dark)
	return nil
}

// pump 菜单嵌套消息泵：直到菜单关闭。WM_QUIT 转投外层托盘泵。
//
// ★托盘图标事件在菜单显示期间必须丢弃（2026-11-16 卡死修复）：
// GetMessage(0,0,0) 抓的是**当前线程所有窗口**的消息 —— 包括托盘隐藏窗口的 msgCallback
// （右键/左键/键盘选中）。若不拦就 DispatchMessage，托盘 msgCallback 会被派到托盘 wndProc
// → 再次 showMenuFor → 在嵌套泵里又弹一层菜单 → 两层互相抢着关 = 卡死在屏幕上。
//
// 只丢 msgCallback / msgQuit 这两条托盘专属消息；**其它一律照常派发** ——
// 特别是 WH_MOUSE_LL 全局钩子的回调消息（hwnd=0），丢了会让「点菜单外关闭」失效。
func (s *menuState) pump() {
	for {
		var m msgT
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			if int32(r) == 0 { // WM_QUIT：托盘要退，转投外层
				pPostQuitMessage.Call(uintptr(m.wParam))
			}
			return
		}
		if m.message == wmCloseMenu {
			if m.hwnd == s.hwnd {
				return
			}
			continue // 别的菜单的关闭消息，忽略
		}
		// ★托盘图标事件：菜单显示期间丢弃，杜绝重入弹菜单
		if menuPumpShouldDrop(m.message) {
			Logf("菜单泵丢弃托盘事件 msg=%#x（防重入）", m.message)
			continue
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// menuPumpShouldDrop 判断玻璃菜单的嵌套泵是否该丢弃这条消息。
//
// 抽成纯函数是为了能单测：这条判断错了的后果是「切样式时卡死」或「点外关闭失效」，
// 都只能靠真机点出来，编码阶段必须钉死。
//
//   - msgCallback（托盘图标事件）/ msgQuit：菜单显示期间必须丢弃，否则派到托盘 wndProc
//     会重入弹菜单（两层菜单互相抢着关 = 卡死）
//   - wmCloseMenu：由调用方单独处理（只关自己那个 hwnd）
//   - 其它（含 WH_MOUSE_LL 钩子消息 hwnd=0、键盘、鼠标点击）：照常派发
func menuPumpShouldDrop(message uint32) bool {
	return message == msgCallback || message == msgQuit
}

func (s *menuState) close() {
	if s.closed {
		return
	}
	s.closed = true
	pPostMessage.Call(uintptr(s.hwnd), wmCloseMenu, 0, 0)
}

// menuWndProc 菜单窗口过程：绘制 / 悬停 / 点选 / 键盘 / 失焦关闭。
func menuWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	menuMu.Lock()
	s := curMenu
	menuMu.Unlock()
	if s == nil || s.hwnd != windows.Handle(hwnd) {
		r, _, _ := pDefWindowProc.Call(hwnd, msg, wParam, lParam)
		return r
	}

	switch msg {
	case 0x000F: // wmPaint
		// paintLayered 内嵌 ULW 失败回落（RDP 场景）：每次绘制自动尝试 ULW，
		// 失败就地 BitBlt —— 自愈式，无需按模式路由
		s.paintLayered()
		pValidateRectProc.Call(hwnd, 0)
		return 0
	case wmEraseBkgnd:
		return 1 // ULW 全权绘制，不擦背景
	case wmMousemove:
		_, y := s.hitFromLParam(lParam)
		idx, onItem := s.itemAt(y - s.shad)
		if idx != s.hover || !onItem {
			s.hover = idx
			pInvalidateRectProc.Call(hwnd, 0, 0)
		}
		tme := trackMouseEvent{cbSize: uint32(unsafe.Sizeof(trackMouseEvent{})), dwFlags: tmeLeave, hwndTrack: windows.Handle(hwnd)}
		pTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
		return 0
	case wmMouseLeave:
		if s.hover != -1 {
			s.hover = -1
			pInvalidateRectProc.Call(hwnd, 0, 0)
		}
		return 0
	case wmLButtonDown, wmRButtonDown:
		// 「点了菜单外」的
		// 判定按**圆角板**而不是窗口矩形 —— 窗口矩形比可见底板大（圆角外是透明区），
		// 按矩形算用户点底板旁的空隙会没反应（实测「点外面面板不消失」）。
		x, y := s.hitFromLParam(lParam)
		if !s.onPlate(x, y) {
			s.close()
		}
		return 0
	case wmLButtonUp, wmRButtonUp:
		x, y := s.hitFromLParam(lParam)
		if !s.onPlate(x, y) {
			return 0 // 窗外松开：上面 DOWN 已关菜单
		}
		idx, onItem := s.itemAt(y - s.shad)
		if onItem && idx >= 0 && !s.items[idx].Disabled && !s.items[idx].Separator {
			s.result = s.items[idx].ID
			s.close()
		}
		return 0
	case wmKeyDown:
		switch int(wParam) {
		case vkDown:
			s.stepHover(1)
		case vkUp:
			s.stepHover(-1)
		case vkReturn:
			if s.hover >= 0 && !s.items[s.hover].Disabled && !s.items[s.hover].Separator {
				s.result = s.items[s.hover].ID
			}
			s.close()
		case vkEscape:
			s.close()
		}
		return 0
	case wmKillFocus:
		// 菜单不抢焦点（NOACTIVATE），此消息理论上不再到达；保留兜底。
		// ⚠️ 验证模式（menuOpenAt 非空）豁免：后台拉起的进程拿不稳前台，焦点 2 秒内就被
		//    外部应用抢走 → 菜单还没被点就自关。
		if menuOpenAt == nil {
			Logf("菜单 WM_KILLFOCUS —— 失焦关闭")
			s.close()
		}
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, msg, wParam, lParam)
	return r
}

// stepHover 键盘移动悬停（跳过分隔线与禁用项，环绕）。
func (s *menuState) stepHover(dir int) {
	if len(s.items) == 0 {
		return
	}
	i := s.hover
	for n := 0; n < len(s.items); n++ {
		i += dir
		if i < 0 {
			i = len(s.items) - 1
		}
		if i >= len(s.items) {
			i = 0
		}
		if !s.items[i].Separator && !s.items[i].Disabled {
			s.hover = i
			pInvalidateRectProc.Call(uintptr(s.hwnd), 0, 0)
			return
		}
	}
}

// itemAt 客户区 y → 菜单项下标；落在分隔线 / 空隙返回 onItem=false。
func (s *menuState) itemAt(y int) (int, bool) {
	cur := s.padV
	for i, it := range s.items {
		if it.Separator {
			cur += s.sepH
			continue
		}
		if y >= cur && y < cur+s.itemH {
			return i, true
		}
		cur += s.itemH
	}
	return -1, false
}

func (s *menuState) hitFromLParam(lParam uintptr) (int, int) {
	return int(int16(uint16(lParam & 0xffff))), int(int16(uint16((lParam >> 16) & 0xffff)))
}

// onPlate 客户区坐标是否落在**可见圆角板**上。窗口比板大一圈（外投影 pad 区），
// 所有命中判定都必须先减掉 pad —— 否则点在阴影上也会被当成「板内」。
func (s *menuState) onPlate(x, y int) bool {
	if s.shad > 0 {
		x -= s.shad
		y -= s.shad
	}
	return insideRounded(s.width, s.clientH()-2*s.shad, s.radius, x, y)
}

// clientH 客户区高度（窗外判定用；捕获期间鼠标消息坐标可以越出窗口）。
func (s *menuState) clientH() int {
	var rc rect
	pGetClientRectProc.Call(uintptr(s.hwnd), uintptr(unsafe.Pointer(&rc)))
	return int(rc.bottom - rc.top)
}

type trackMouseEvent struct {
	cbSize      uint32
	dwFlags     uint32
	hwndTrack   windows.Handle
	dwHoverTime uint32
}

// accentPolicy / WINDOWCOMPOSITIONATTRIBDATA：SetWindowCompositionAttribute 的未公开结构
// （TranslucentTB 等广泛使用的稳定 ABI）。
type accentPolicy struct {
	accentState   uint32 // 4 = ACCENT_ENABLE_ACRYLICBLURBEHIND
	accentFlags   uint32
	gradientColor uint32 // ABGR 染色（含 alpha）
	animationId   uint32
}

type winCompAttribData struct {
	attrib     uint32
	data       unsafe.Pointer
	sizeOfData uintptr
}

// enableBlur 给窗口垫 ACCENT acrylic 真模糊。成功 ⇒ 底板调淡透出模糊。
func enableBlur(hwnd uintptr, dark bool) bool {
	// tint 染色偏淡（alpha 0x66 ≈ 40%）：底板自身有透明度，模糊是主角
	tint := uint32(0x66EFEFEF) // 亮
	if dark {
		tint = uint32(0x661B1B1B)
	}
	pol := accentPolicy{accentState: accentAcrylic, accentFlags: 2, gradientColor: tint}
	data := winCompAttribData{attrib: wcaAccentPolicy, data: unsafe.Pointer(&pol), sizeOfData: uintptr(unsafe.Sizeof(pol))}
	r, _, _ := pSetWinCompAttr.Call(hwnd, uintptr(unsafe.Pointer(&data)))
	return r != 0
}

// bitmapInfoHeader：CreateDIBSection 的 32bpp top-down 描述。
type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32 // 负值 = top-down
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type bitmapInfo struct {
	header bitmapInfoHeader
}

// blendFunc：BLENDFUNCTION（ULW 的 pblend）。
type blendFunc struct {
	sourceOver  byte // AC_SRC_OVER = 0
	flags       byte
	constAlpha  byte
	alphaFormat byte // AC_SRC_ALPHA = 1（位图带 per-pixel alpha）
}

// ss 超采样倍数：绘制在 2x 画布上，最后 box 降采样到物理像素。
//
// **为什么必须超采样**（实测）：ULW 位图按物理像素 1:1 上屏，150% 屏上 24px 半径的
// 圆角弧只有 ~38 个采样点、正文 CJK 只有 22px 高 —— GDI 灰阶 AA 的 1px 覆盖度在这种
// 采样密度下必然表现为「弧段台阶 + 笔画发虚」（双主题截图实测）。
// 2x 绘制后 2x2 box 降采样 = 每像素 4 个子采样 + 4px 邻域平均，等效 4 倍采样密度，
// 弧线与文字边缘一次到位。菜单位图只有几百 KB，一次性开销可忽略。
const ss = 2

// paintLayered 手写 ARGB 位图 + UpdateLayeredWindow：
//  1. 底板 = 抓屏快照（1x，只能物理像素）→ box blur → 单向压暗 → 放大到 2x →
//     圆角软边 + 顶光（Go 手写 BGRA，生成一次缓存）；
//  2. GDI 在 **2x 画布**上画 hover / 分隔线 / 文字（字体也是 2x）；
//  3. alpha 反推 → 2x2 box 降采样到 1x → premultiply → UpdateLayeredWindow 提交 DWM。
func (s *menuState) paintLayered() {
	hdcScreen, _, _ := pGetDCProc.Call(0)
	defer pReleaseDCProc.Call(0, hdcScreen)

	var rc rect
	pGetClientRectProc.Call(uintptr(s.hwnd), uintptr(unsafe.Pointer(&rc)))
	w, h := int(rc.right-rc.left), int(rc.bottom-rc.top)
	if w <= 0 || h <= 0 {
		return
	}
	W, H := w*ss, h*ss // 超采样画布尺寸（含外投影 pad 区）
	padBig := s.shad * ss
	cw := w - 2*s.shad // 板内容宽（不含 pad）

	newDIB := func(cw, ch int) (uintptr, unsafe.Pointer) {
		var bmi bitmapInfo
		bmi.header.biSize = uint32(unsafe.Sizeof(bmi.header))
		bmi.header.biWidth = int32(cw)
		bmi.header.biHeight = -int32(ch) // top-down
		bmi.header.biPlanes = 1
		bmi.header.biBitCount = 32
		bmi.header.biCompression = biRGB
		var bits unsafe.Pointer
		bmp, _, _ := pCreateDIBSection.Call(0, uintptr(unsafe.Pointer(&bmi)), biRGB, uintptr(unsafe.Pointer(&bits)), 0, 0)
		return bmp, bits
	}

	// 1x：交给 ULW 的最终位图（也是底板抓屏的落点）
	bmpS, bitsS := newDIB(w, h)
	if bmpS == 0 || bitsS == nil {
		return
	}
	defer pDeleteObjectGdi.Call(bmpS)
	memS, _, _ := pCreateCompatibleDC.Call(0)
	defer pDeleteDC.Call(memS)
	pSelectObject.Call(memS, bmpS)

	// 2x：实际绘制画布
	bmpB, bitsB := newDIB(W, H)
	if bmpB == 0 || bitsB == nil {
		return
	}
	defer pDeleteObjectGdi.Call(bmpB)
	memB, _, _ := pCreateCompatibleDC.Call(0)
	defer pDeleteDC.Call(memB)
	pSelectObject.Call(memB, bmpB)
	if s.fontBig != 0 {
		pSelectObject.Call(memB, s.fontBig)
	} else {
		pSelectObject.Call(memB, s.font)
	}
	pSetBkMode.Call(memB, 1 /*TRANSPARENT*/)

	big := (*[1 << 24]byte)(bitsB)[:W*H*4]
	sml := (*[1 << 24]byte)(bitsS)[:w*h*4]
	radius := 16 * s.scale / 100 // 物理像素圆角（16 逻辑 pt，150% 屏 = 24px）
	radiusBig := radius * ss

	// ① 底板 = 真液态玻璃：抓屏（菜单区域当前桌面内容）→ Go 手写 box blur → 单向压暗 →
	//   放大 2x → 圆角软边 + 顶光。**不依赖 ACCENT** —— ACCENT_ACRYLIC 对 ULW 分层窗
	//   不渲染（实测：调用成功但视觉无效，实测截图逮住「玻璃变实色」）。
	//   自己抓屏自己糊才是能落地的路，且 RDP 下同样可用。
	//   底板生成一次即缓存（hover 重绘只换内容，不重抓屏 —— 菜单存活期背景变化不追踪，
	//   与 Win11 系统菜单行为一致）。
	if s.plateBuf == nil {
		screenDC, _, _ := pGetDCProc.Call(0)
		pBitBltProc.Call(memS, 0, 0, uintptr(w), uintptr(h), screenDC,
			uintptr(s.posX), uintptr(s.posY), 0x00CC0020 /*SRCCOPY*/)
		pReleaseDCProc.Call(0, screenDC)
		boxBlur(bitsS, w, h, 21*s.scale/100) // 强模糊（21 逻辑半径 ≈ 苹果 backdrop 强度）
		// **底板色调 = 单向压暗 + 亮度压缩**（重构，取代 双向色差 delta）
		//
		// 旧方案的致命伤（明暗双主题实测）：暗背景 +52 把板抬到中灰（luma≈85），
		// 白字对比只剩 170 —— 观感「暗背景下文字不够亮」，板还像一块灰塑料，背景色相
		// 也被 delta 这个加法项洗成中性灰。苹果深色玻璃是**单向**的：背景再暗只透出一点、
		// 再亮也压成深色，板恒落在窄暗区间 ⇒ 白字对比恒 ≥190。色相靠**逐通道比值缩放**
		// 保留（比值不动 RGB 之间的比例，加法 delta 必然把色团拉向中性灰）。
		//
		// 映射：targetLuma = base + blurredLuma·span
		//   dark  : 20 + luma·0.18 → 纯黑背景 22 / 白墙背景 51
		//   light : 232 + luma·0.08 → 纯黑背景 232 / 白墙背景 252
		// span 同时承担「动态范围压缩」：背景里的强亮斑只传导 18% 到底板，
		// 不会在板上形成同亮度亮斑（的「局部融入背景」不再发生）。
		base, span := 20.0, 0.18
		tr, tg, tb := 0x14, 0x14, 0x16
		if !s.dark {
			base, span = 232.0, 0.08
			tr, tg, tb = 0xF0, 0xF0, 0xF2
		}
		const tintMix = 0.30 // 主题 tint 比例（玻璃基色）
		var lsum float64
		for i := 0; i < w*h; i++ {
			o := i * 4
			if i%w < s.shad || i%w >= w-s.shad || i/w < s.shad || i/w >= h-s.shad {
				continue // pad 区是桌面内容，不参与板均亮度
			}
			vb := float64(sml[o+0])*(1-tintMix) + float64(tb)*tintMix
			vg := float64(sml[o+1])*(1-tintMix) + float64(tg)*tintMix
			vr := float64(sml[o+2])*(1-tintMix) + float64(tr)*tintMix
			l := 0.299*vr + 0.587*vg + 0.114*vb
			target := base + l*span
			if l < 1 {
				vb, vg, vr = target, target, target // 无背景内容（纯黑）：直接给底色
			} else {
				k := target / l
				vb, vg, vr = vb*k, vg*k, vr*k
			}
			// 色团回补：比值缩放在暗区会把色度绝对值压小 → 映射后按 luma 保持再提一档
			// 饱和，玻璃后方的颜色才「透得出来」（苹果玻璃后的色是更艳的，不是更灰的）。
			l2 := 0.299*vr + 0.587*vg + 0.114*vb
			sml[o+0] = clampByte(l2 + (vb-l2)*1.4)
			sml[o+1] = clampByte(l2 + (vg-l2)*1.4)
			sml[o+2] = clampByte(l2 + (vr-l2)*1.4)
			sml[o+3] = 0xFF
			lsum += 0.299*float64(sml[o+2]) + 0.587*float64(sml[o+1]) + 0.114*float64(sml[o+0])
		}
		s.plateLuma = lsum / float64(w*h)
		// 放大到 2x 画布（最近邻）：底板是重模糊图，2x2 块在降采样时被平均掉，无损。
		for y := 0; y < H; y++ {
			si := (y / ss) * w
			for x := 0; x < W; x++ {
				so := (si + x/ss) * 4
				do := (y*W + x) * 4
				big[do+0] = sml[so+0]
				big[do+1] = sml[so+1]
				big[do+2] = sml[so+2]
				big[do+3] = 0xFF
			}
		}
		softCorners(bitsB, W, H, radiusBig, padBig)
		// **顶部内缘 specular**（重塑）：苹果液态玻璃的「顶光」是一条贴着轮廓
		// 走的**细亮线**（顶端最亮、越往下越弱），不是一片 18px 宽的灰白渐变 ——
		// 那版在暗板上就是一条脏灰光晕，实测反馈「顶部的光不好看」。
		// 亮线中心内移 1.2 逻辑 px（贴边界会让轮廓发硬）；左右两侧按离侧边距离衰减，
		// 只有顶边与顶角是亮的（真实玻璃的反射只出现在朝上的面）。
		// **外投影**（实测）：板外 pad 区画柔和阴影（黑色 + 二次衰减 alpha）。
		// 只写羽化带以外的区域（d > 3），不碰板的边缘过渡带 —— 否则会盖掉 AA。
		// 降采样后阴影自然平滑；最外缘衰减到 0，窗口边界无硬切。
		alphaMax := 0.46
		for py := 0; py < H; py++ {
			for px := 0; px < W; px++ {
				o := (py*W + px) * 4
				d := plateD(W, H, radiusBig, padBig+3, px, py)
				if d <= 3 {
					continue // 板内 / 羽化带：交给 softCorners
				}
				t := 1 - (d-3)/float64(padBig)
				if t <= 0 {
					continue // pad 最外缘：完全透明
				}
				a := alphaMax * t * t // 二次衰减 ≈ 高斯阴影的形状
				big[o+0] = 0
				big[o+1] = 0
				big[o+2] = 0
				big[o+3] = byte(a * 255)
			}
		}
		// **整圈内缘高光 + 顶部加强**（实测）：
		// 的 sideFade 让高光只存在于顶部 2R 带、两侧直接砍到 0 —— 观感就是
		// 「上方多出来一段白线」（截图实测）。玻璃的边缘反光是**闭合一圈**的：
		// 整圈统一 10%，顶部 4R 带内再叠最多 14% 并平滑渐隐。峰值靠过渡带宽度表达，
		// 不靠亮度堆（0.48 那版会读成一条生硬的白边）。
		// **高光必须从最外缘起算**（rimIn = 0）：内移 1.2px 的后果是高光外侧
		// 留了一条纯板色的窄带 —— 它比高光暗、又比背景暗，观感就是「高光外有一圈黑边」，
		// 玻璃边缘完全没了（实测指正）。真实玻璃的边缘反光紧贴外缘。
		rimW := 3.0 * float64(s.scale) / 100 * float64(ss)
		// 峰值落在**板外 1.5 物理 px**（rimIn 取负）：让 alpha 羽化带里的像素也带上高光色，
		// premultiply 后合成结果就是「背景→亮→玻璃体」的单调过渡。若峰值停在板内，
		// 最外一圈永远是纯板色，观感仍是「高光外面套了一圈暗边」。
		rimIn := -3.0
		topBoost := 4.0 * float64(radiusBig)
		for py := 0; py < H; py++ {
			// 整圈 0.18 是「能读出玻璃厚度」的下限：再低就只剩一圈灰过渡（实测
			// 反馈「没有玻璃边缘质感」）；顶部 4R 带再叠 0.12 平滑收口。
			strength := 0.18
			if float64(py) < topBoost {
				t := 1 - float64(py)/topBoost
				strength += 0.12 * t * t * (3 - 2*t) // smoothstep：顶部额外高光平滑收口
			}
			for px := 0; px < W; px++ {
				o := (py*W + px) * 4
				if big[o+3] == 0 {
					continue
				}
				d := -plateD(W, H, radiusBig, padBig+3, px, py) - rimIn
				if d < 0 || d > rimW {
					continue // 板外 / 亮线带之外
				}
				f := 1 - d/rimW
				f = f * f * (3 - 2*f) // smoothstep：线性衰减在峰值处有硬折线，看着就是「一条白边」
				c := strength * f
				big[o+0] = clampByte(float64(big[o+0])*(1-c) + 0xFF*c)
				big[o+1] = clampByte(float64(big[o+1])*(1-c) + 0xFF*c)
				big[o+2] = clampByte(float64(big[o+2])*(1-c) + 0xFF*c)
			}
		}
		// 整圈描边仍不上（实测调整）：分界由**一档灰差（单向压暗）+
		// 顶部 specular** 承担 —— 暗背景上板比背景亮一档、亮背景上暗一档，两个方向都有轮廓。
		s.plateBuf = append([]byte(nil), big...)
	} else {
		copy(big, s.plateBuf)
	}

	c := menuPalette(s.dark)
	// hover / 分隔线按**板均亮度**派生一档差 —— 底板亮度现在随背景映射（dark 20~66 /
	// light 232~252），menuPalette 里那套 Win11 固定灰值会在区间两端失效
	//（0x454545 分隔线画在 0x424242 板上 = 完全消失，的同类坑）。
	adj := 22.0
	if !s.dark {
		adj = -20.0
	}
	hv := clampByte(s.plateLuma + adj)
	c.hover = uint32(hv)<<16 | uint32(hv)<<8 | uint32(hv) // COLORREF 0x00BBGGRR（R=G=B）
	sv := clampByte(s.plateLuma + adj + 4)
	c.separator = uint32(sv)<<16 | uint32(sv)<<8 | uint32(sv)

	// ② 内容（GDI，不透明像素）。所有几何量 ×ss 落到 2x 画布。
	x0 := 6*s.scale/100*ss + padBig  // hover 高亮内缩（含外投影偏移）
	tx := 18*s.scale/100*ss + padBig // 文字起点
	hoverW := (cw - 12*s.scale/100) * ss
	itemH := s.itemH * ss
	sepH := s.sepH * ss
	y := s.padV*ss + padBig

	for i, it := range s.items {
		if it.Separator {
			sepBrush, _, _ := pCreateSolidBrushG.Call(uintptr(c.separator))
			sr := rect{left: int32(6*s.scale/100*ss + padBig), right: int32((cw-6*s.scale/100)*ss + padBig),
				top: int32(y + sepH/2 - ss), bottom: int32(y + sepH/2)} // ss 行高 = 降采样后 1px 实线
			pFillRectProc.Call(memB, uintptr(unsafe.Pointer(&sr)), sepBrush)
			pDeleteObjectGdi.Call(sepBrush)
			y += sepH
			continue
		}
		color := c.text
		if it.Disabled {
			color = c.disabled
		}
		if s.hover == i && !it.Disabled {
			// 悬停高亮：圆角矩形。⚠️ brush 必须 SelectObject 进 DC —— 不选进去用的就是
			// stock WHITE_BRUSH（截图实测：白底白字把悬停项整个吞掉）。
			hb, _, _ := pCreateSolidBrushG.Call(uintptr(c.hover))
			pen, _, _ := pCreatePen.Call(penNull, 0, 0)
			oldBrush, _, _ := pSelectObject.Call(memB, hb)
			oldPen, _, _ := pSelectObject.Call(memB, pen)
			pRoundRect.Call(memB,
				uintptr(x0), uintptr(y+2*s.scale/100*ss), uintptr(x0+hoverW), uintptr(y+itemH-2*s.scale/100*ss),
				uintptr(6*s.scale/100*ss), uintptr(6*s.scale/100*ss))
			pSelectObject.Call(memB, oldBrush)
			pSelectObject.Call(memB, oldPen)
			pDeleteObjectGdi.Call(pen)
			pDeleteObjectGdi.Call(hb)
		}
		pSetTextColorGdi.Call(memB, uintptr(color))
		label := utf16Ptr(it.Label)
		txLine := tx
		// ★所有项文字统一左对齐到 28ux（定稿）：左侧留固定图标槽，
		// 有标记（圆点/竖线）就填、没有就空着 —— 空槽也占位，文字左缘成一条直线。
		// 旧实现只给有标记的项让位 ⇒ 无标记项文字靠左 19px，选中态切换时文字还会左右跳。
		// 圆点与竖线互斥（不会同列叠），但两者都在 x=17ux 中心线，28ux 起字不会盖字。
		txLine = 28*s.scale/100*ss + padBig
		tr := rect{left: int32(txLine), right: int32((cw-10*s.scale/100)*ss + padBig), top: int32(y), bottom: int32(y + itemH)}
		pDrawTextProc.Call(memB, uintptr(unsafe.Pointer(label)), uintptr(utf16Count(it.Label)),
			uintptr(unsafe.Pointer(&tr)), uintptr(dtSingleline|dtVcenter|dtNoprefix))
		// **发光圆点**（实测）：不用字符 —— 字符做不出光晕、大小随字体漂移、
		// 也没法和文字基线精确对齐。直接往画布 RGB 上叠一个实心圆 + 径向光晕：
		// 底板不透明，光就是「叠在玻璃上的光」；alpha 保持 255，避开后面的亮度反推。
		hasDot := it.Dot != DotNone && !it.Checked // 与 Checked 互斥：有竖线就不画点
		if hasDot {
			if dc := dotColor(it.Dot, s.dark); dc != 0 {
				cr := float64(dc & 0xFF) // COLORREF 0x00BBGGRR
				cg := float64((dc >> 8) & 0xFF)
				cb := float64((dc >> 16) & 0xFF)
				// 点中心 17 逻辑 px（左缘 ≈13.4）：12 那版贴着面板边，视觉上「点挂在框上」
				cx := 17*s.scale/100*ss + padBig
				cyD := y + itemH/2
				rad := 3.6 * float64(s.scale) / 100 * float64(ss)
				gr := rad * 3.2 // 光晕外径
				glowA := 0.50
				if it.Dot == DotIdle {
					glowA = 0.14 // 已停止：灰点不发光
				}
				for py := cyD - int(gr) - 1; py <= cyD+int(gr)+1; py++ {
					if py < 0 || py >= H {
						continue
					}
					for px := cx - int(gr) - 1; px <= cx+int(gr)+1; px++ {
						if px < 0 || px >= W {
							continue
						}
						dist := math.Hypot(float64(px-cx), float64(py-cyD))
						if dist > gr {
							continue
						}
						var a float64
						if dist <= rad {
							a = 1
						} else {
							t := 1 - (dist-rad)/(gr-rad)
							a = glowA * t * t * t // 三次衰减：中心实心、外圈柔和消失
						}
						o := (py*W + px) * 4
						big[o+0] = clampByte(float64(big[o+0])*(1-a) + cb*a)
						big[o+1] = clampByte(float64(big[o+1])*(1-a) + cg*a)
						big[o+2] = clampByte(float64(big[o+2])*(1-a) + cr*a)
						big[o+3] = 0xFF
					}
				}
			}
		}
		if it.Checked {
			// 选中态：一条彩色短粗竖线 + 与状态圆点同款的径向发光（定稿，
			// 替掉矢量勾）。形状用竖线而非圆点 —— 圆点已归状态行（DotRun/Warn/Idle），
			// 得让「选中」和「现在什么状态」在形状上分开，不然语义撞。
			// 颜色复用 dotColor(DotRun) 的语义绿；明暗主题各自取亮/暗档。
			//
			// ★不能走 ③ 亮度反推：绿 #2EE06C 的 luma≈158 ≥ 140，会被当白字反推成白色
			//   丢色。所以和发光圆点一样：叠完 RGB 直接写 alpha=0xFF（实心叠光），
			//   ③ 遇到 alpha!=0 会跳过，颜色得以保留。
			if dc := dotColor(DotRun, s.dark); dc != 0 {
				cr := float64(dc & 0xFF) // COLORREF 0x00BBGGRR
				cg := float64((dc >> 8) & 0xFF)
				cb := float64((dc >> 16) & 0xFF)
				ux := s.scale / 100 * ss // 逻辑 1px 对应的物理像素（超采样后）
				// ★表达式必须和状态圆点逐字一致：17*s.scale/100*ss 在 scale=150 时是
				// ((17*150)/100)*2=50；写成 17*ux（ux=s.scale/100*ss=2）只有 34 —— 整数
				// 除法顺序不同 ⇒ 差 16 物理 px，看着像「没对齐」（实测逮住）。
				cx := 17*s.scale/100*ss + padBig // 竖线中心 x：与状态圆点同一条中心线
				cy := y + itemH/2                // 垂直居中
				half := 5 * ux                   // 竖线半高 5 逻辑 px（总高 10，短粗）
				th2 := 2 * ux                    // 半线宽 2 逻辑 px（总宽 4，粗）
				glow := th2 + 3*ux               // 光晕外径（5ux）；文字已让位到 28ux，不盖字
				glowA := 0.55                    // 光晕峰值（比状态点的 0.50 略亮，强调选中）
				segX, segY0, segY1 := float64(cx), float64(cy-half), float64(cy+half)
				x0, x1 := cx-glow-2, cx+glow+2
				y0, y1 := cy-half-glow-2, cy+half+glow+2
				for py := y0; py <= y1; py++ {
					if py < 0 || py >= H {
						continue
					}
					for px := x0; px <= x1; px++ {
						if px < 0 || px >= W {
							continue
						}
						d := math.Sqrt(segDist2(float64(px), float64(py), segX, segY0, segX, segY1))
						if d > float64(glow) {
							continue
						}
						var a float64
						if d <= float64(th2) {
							a = 1 // 线芯：实心语义绿
						} else {
							t := 1 - (d-float64(th2))/float64(glow-th2)
							a = glowA * t * t * t // 三次衰减：柔和光晕，同状态圆点
						}
						o := (py*W + px) * 4
						big[o+0] = byte(float64(big[o+0])*(1-a) + cb*a)
						big[o+1] = byte(float64(big[o+1])*(1-a) + cg*a)
						big[o+2] = byte(float64(big[o+2])*(1-a) + cr*a)
						big[o+3] = 0xFF // 实心叠光，避开 ③ 反推
					}
				}
			}
		}
		y += itemH
	}

	// ③ alpha 修复（premultiplied 陷阱）+ **文字亮度反推**：
	//   GDI 写像素不碰 alpha 字节（恒 0）—— 不补 alpha 文字会被 DWM 整个丢弃。
	//   但 GDI 的灰阶 AA 边缘像素是「白×覆盖率+底板×(1-覆盖率)」的**实心预混灰**，
	//   直接补 255 会让文字边缘发糊 = 实测「文字不够亮」。
	//   正解：比底板亮的像素按亮度差**反推 alpha**（RGB 回推纯白源色，premultiply
	//   pass 再统一预乘）→ 文字中心不透明纯白、边缘正确半透明过渡；
	//   比底板暗的内容（hover 块/分隔线）保持实心补全。
	//   gamma 0.82：灰阶 AA 在 22px CJK 上边缘偏淡，压一点 gamma 让字更实
	//   （实测「文字分辨率不够」）。
	for py := 0; py < H; py++ {
		for px := 0; px < W; px++ {
			i := py*W + px
			o := i * 4
			if big[o+3] != 0 || !insideRounded(W-2*(padBig+8), H-2*(padBig+8), radiusBig-padBig-8, px-padBig-8, py-padBig-8) {
				continue
			}
			luma := 0.299*float64(big[o+2]) + 0.587*float64(big[o+1]) + 0.114*float64(big[o+0])
			pl := 0.299*float64(s.plateBuf[o+2]) + 0.587*float64(s.plateBuf[o+1]) + 0.114*float64(s.plateBuf[o+0])
			if luma > pl+8 && luma >= 140 {
				// 比底板亮**且足够亮**（luma≥140 = 白字本体/其 AA 边缘）→ 亮度反推 alpha。
				// ⚠️ 只判 luma>pl 会误伤 hover 块（0x3F）：画在暗底板（pl<55）上时
				// 「比底板亮」→ alpha 被反推成 16/255、RGB 回推爆成白 → hover 块变成
				// 「几乎透明的白纱」透出后面图标（特写实测）。
				a := math.Pow((luma-pl)/(255-pl), 0.82)
				if a > 1 {
					a = 1
				}
				ab := byte(a * 255)
				big[o+3] = ab
				for ch := 0; ch < 3; ch++ {
					v := float64(big[o+ch]) * 255 / float64(ab) // 回推未预乘源色
					big[o+ch] = clampByte(v)
				}
			} else {
				big[o+3] = 0xFF // 实心内容（hover 块/分隔线/disabled 及一切暗色内容）
			}
		}
	}

	// ③.5 2x2 box 降采样到物理像素（在 premultiply **之前**做：预乘值不能直接平均）
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sb, sg, sr, sa int
			for dy := 0; dy < ss; dy++ {
				row := (y*ss + dy) * W
				for dx := 0; dx < ss; dx++ {
					o := (row + x*ss + dx) * 4
					sb += int(big[o+0])
					sg += int(big[o+1])
					sr += int(big[o+2])
					sa += int(big[o+3])
				}
			}
			do := (y*w + x) * 4
			n := ss * ss
			sml[do+0] = byte(sb / n)
			sml[do+1] = byte(sg / n)
			sml[do+2] = byte(sr / n)
			sml[do+3] = byte(sa / n)
		}
	}

	// ③.6 premultiply 收尾（锯齿真凶之一）：DWM 的分层窗位图要求 **premultiplied alpha**
	//   （RGB ≤ A）。底板羽化带的像素 alpha<255 而 RGB 未乘 —— DWM 按 premultiplied 解读
	//   会把边缘像素当「超亮」，表现为圆角一圈发白毛刺（截图实测）。
	//   alpha=255 的像素乘完不变，板内不受影响；alpha=0 乘完归零（圆角外彻底透明）。
	for i := 0; i < w*h; i++ {
		a := uint32(sml[i*4+3])
		if a == 255 {
			continue
		}
		sml[i*4+0] = byte(uint32(sml[i*4+0]) * a / 255)
		sml[i*4+1] = byte(uint32(sml[i*4+1]) * a / 255)
		sml[i*4+2] = byte(uint32(sml[i*4+2]) * a / 255)
	}

	// ④ 提交 DWM：pptDst 传当前位置。
	// ⚠️ 两个已知雷：hdcDst 必须给屏幕 DC（NULL 报 ERROR_GEN_FAILURE）；
	// pblend 也要给全（AC_SRC_OVER + AC_SRC_ALPHA），NULL 同样报 GEN_FAILURE。
	ptDst := point{x: s.posX, y: s.posY}
	ptSrc := point{}
	sz := size{cx: int32(w), cy: int32(h)}
	blend := blendFunc{sourceOver: 0, flags: 0, constAlpha: 255, alphaFormat: 1 /*AC_SRC_ALPHA*/}
	// 调试钩子：HYMUSE_TRAY_FORCE_FALLBACK=1 强制走回落路径（本地验证 RDP 兼容分支用）
	forceFallback := os.Getenv("HYMUSE_TRAY_FORCE_FALLBACK") != ""
	r, _, lastErr := pUpdateLayeredWin.Call(uintptr(s.hwnd), hdcScreen,
		uintptr(unsafe.Pointer(&ptDst)), uintptr(unsafe.Pointer(&sz)), memS,
		uintptr(unsafe.Pointer(&ptSrc)), 0, uintptr(unsafe.Pointer(&blend)), ulwAlpha)
	if r == 0 || forceFallback {
		// **RDP/headless 下 ULW 必失败 GEN_FAILURE(31)**，
		// 本地再正常也会在远程桌面会话里菜单隐形。回落路线（真机验证过）：
		// SetLayeredWindowAttributes(LWA_ALPHA) + SetWindowRgn 圆角裁切 + 普通 BitBlt。
		Logf("ULW 失败(%v) → 回落 SLWA+普通绘制（RDP 兼容）", lastErr)
		s.fallback = true
		hrgn, _, _ := pCreateRoundRectRgn.Call(uintptr(s.shad), uintptr(s.shad),
			uintptr(w-s.shad), uintptr(h-s.shad), uintptr(radius), uintptr(radius))
		pSetWindowRgnProc.Call(uintptr(s.hwnd), hrgn, 1)
		pSetLayeredWinAttr.Call(uintptr(s.hwnd), 0, uintptr(0xE0), lwaAlpha)
		// 把已画好的内容直接 BitBlt 到窗口 DC（alpha 忽略 = 实色；圆角外黑已被 Rgn 裁掉）。
		// 校验由 wmPaint 分支的 ValidateRect 统一处理（否则死循环重绘）。
		hdcW, _, _ := pGetDCProc.Call(uintptr(s.hwnd))
		pBitBltProc.Call(hdcW, 0, 0, uintptr(w), uintptr(h), memS, 0, 0, 0x00CC0020 /*SRCCOPY*/)
		pReleaseDCProc.Call(uintptr(s.hwnd), hdcW)
	}

	// 验证钩子：--pos 模式下把位图 dump 成 PNG（看 alpha / 文字到底画没画上）
	if menuOpenAt != nil {
		dumpDIB(bitsS, w, h)
	}
}

// insideRounded 圆角矩形判定（alpha 修复用，radius 内缩 3px 避开羽化带）。
func insideRounded(w, h, r, x, y int) bool {
	if x < 0 || y < 0 || x >= w || y >= h {
		return false
	}
	inX := x < r || x >= w-r
	inY := y < r || y >= h-r
	if inX && inY {
		cx := r
		if x >= w-r {
			cx = w - r
		}
		cy := r
		if y >= h-r {
			cy = h - r
		}
		dx, dy := x-cx, y-cy
		return dx*dx+dy*dy <= r*r
	}
	return true
}

// boxBlur 分离两 pass 盒式模糊（BGRA，只糊 RGB；alpha 通道保持）。
// 菜单尺寸 ~300×530，两 pass 纯 Go 循环毫秒级，只在底板生成时跑一次。
func boxBlur(bits unsafe.Pointer, w, h, r int) {
	if r < 1 || w <= 0 || h <= 0 {
		return
	}
	buf := (*[1 << 24]byte)(bits)[:w*h*4]
	tmp := make([]byte, w*h*4)
	pass := func(src, dst []byte, w, h, r int, horizontal bool) {
		inv := 1.0 / float64(2*r+1)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				var sr, sg, sb float64
				for k := -r; k <= r; k++ {
					sx, sy := x, y
					if horizontal {
						sx = x + k
					} else {
						sy = y + k
					}
					if sx < 0 {
						sx = 0
					}
					if sx >= w {
						sx = w - 1
					}
					if sy < 0 {
						sy = 0
					}
					if sy >= h {
						sy = h - 1
					}
					o := (sy*w + sx) * 4
					sb += float64(src[o+0])
					sg += float64(src[o+1])
					sr += float64(src[o+2])
				}
				o := (y*w + x) * 4
				dst[o+0] = byte(sb * inv)
				dst[o+1] = byte(sg * inv)
				dst[o+2] = byte(sr * inv)
				dst[o+3] = src[o+3]
			}
		}
	}
	pass(buf, tmp, w, h, r, true)
	pass(tmp, buf, w, h, r, false)
}

// plateD 到**内缩 inset 像素**轮廓的有符号距离（板内 < 0）。
//
// ⚠️ 为什么必须内缩（实测）：SDF 矩形与位图边界重合时，alpha 羽化带整条落在
// 位图之外被裁掉 —— 最外一列像素直接是 alpha=255 的板色，屏幕上表现为 242→51 的
// **一像素硬切**（剖面实测）。这正是「SDF 了为什么圆角还有锯齿感」的最后一环：
// 弧段只有部分像素落在带内、直边完全没有 AA。内缩 3（2x px）后羽化带一半在位图内，
// 直边与弧段获得同一条覆盖度曲线。
func plateD(W, H, radius, inset, px, py int) float64 {
	// ⚠️ 半径**不能**跟着 inset 一起减：inset 是「把板矩形从窗口/画布内缩」，
	// 板自己的圆角半径不变。写成 radius-inset 时 inset=pad+3（45）会把 48 的半径
	// 削成 3 —— 圆角直接变直角（实测右上角）。
	return plateSDF(float64(W-2*inset), float64(H-2*inset), float64(radius),
		float64(px-inset), float64(py-inset))
}

// softCorners 圆角软边抗锯齿（距离场）：d≥0.5 全显 / |d|<0.5 线性羽化 / 其余 alpha=0。
// softCorners 底板全轮廓软边（SDF）：**2px 渐变带**（色差加大后边缘对比
// 强，原 1px 羽化扛不住 → 实测「圆角还是有锯齿」）。圆弧与直边同一条数学曲线，
// 全轮廓均匀平滑。
// alpha = 255 × (aaW - d) / aaW：d ≤ -2 全实，d ∈ [-2,2] 线性渐隐，d ≥ 2 全透。
// RGB 保持未乘（统一由 premultiply pass 处理）。
func softCorners(bits unsafe.Pointer, w, h, radius, pad int) {
	buf := (*[1 << 24]byte)(bits)[:w*h*4]
	const aaW = 5.0  // 2x 画布上 5px ≈ 物理 2.5px 过渡带（超采样后无需再放宽）
	inset := pad + 3 // 2x px：边界内缩，让羽化带一半落在位图内（直边才有 AA）
	for py := 0; py < h; py++ {
		for px := 0; px < w; px++ {
			o := (py*w + px) * 4
			d := plateD(w, h, radius, inset, px, py)
			if d >= aaW {
				buf[o+3] = 0 // 板外彻底透明
				continue
			}
			a := (aaW - d) / aaW
			if a > 1 {
				a = 1
			}
			buf[o+3] = byte(a * 255)
		}
	}
}

// edgePlate 底板内缘白描边（SDF 抗锯齿）：按每像素到圆角矩形轮廓的**有符号距离**
// 计算白线覆盖度 —— 弧段与直边同一条数学曲线，天然平滑连贯（截图实测
// 「圆角白边锯齿不连贯」= 像素阶梯判定无 AA）。
// 白线画在软边羽化带内侧（线中心 = 板边界内 0.5px，半宽 0.75px + 0.5px AA 过渡），
// alpha 恒 255，锯齿修复用**亮度渐变**（premultiply pass 不受影响）。
func edgePlate(bits unsafe.Pointer, w, h, radius int) {
	buf := (*[1 << 24]byte)(bits)[:w*h*4]
	hw := 1.75 // 白线半宽：全亮段 1.5px —— **必须盖住弧段的像素离散抖动带**（±0.7px），
	dC := -1.0 // 否则弧段像素在「全亮/渐变/羽化」间交错 = 特写实测里的强弱不匀（实测）
	for py := 0; py < h; py++ {
		for px := 0; px < w; px++ {
			o := (py*w + px) * 4
			if buf[o+3] != 0xFF {
				continue // 只描不透明像素（羽化带本身保持平滑过渡）
			}
			d := plateSDF(float64(w), float64(h), float64(radius), float64(px), float64(py))
			cov := hw + 0.5 - math.Abs(d-dC)
			if cov <= 0 {
				continue
			}
			if cov > 1 {
				cov = 1
			}
			// 白线覆盖底板：亮度混合（alpha 恒 255，premultiply pass 不受影响）
			c := cov
			buf[o+0] = byte(float64(buf[o+0])*(1-c) + 0xFF*c)
			buf[o+1] = byte(float64(buf[o+1])*(1-c) + 0xFF*c)
			buf[o+2] = byte(float64(buf[o+2])*(1-c) + 0xFF*c)
		}
	}
}

// plateSDF 圆角矩形有符号距离（iq 经典公式）：板内 < 0，板外 > 0，边界 = 0。
// 像素中心按 +0.5 取样。
func plateSDF(w, h, r, x, y float64) float64 {
	cx, cy := w/2, h/2
	hx, hy := w/2-r, h/2-r
	qx := math.Abs(x+0.5-cx) - hx
	qy := math.Abs(y+0.5-cy) - hy
	dx := math.Max(qx, 0)
	dy := math.Max(qy, 0)
	return math.Hypot(dx, dy) + math.Min(math.Max(qx, qy), 0) - r
}

func clampByte(v float64) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func dumpDIB(bits unsafe.Pointer, w, h int) {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	buf := (*[1 << 24]byte)(bits)[:w*h*4]
	for i := 0; i < w*h; i++ {
		a := uint32(buf[i*4+3])
		img.Pix[i*4+3] = byte(a)
		if a > 0 {
			img.Pix[i*4+0] = byte(uint32(buf[i*4+2]) * 255 / a) // R
			img.Pix[i*4+1] = byte(uint32(buf[i*4+1]) * 255 / a) // G
			img.Pix[i*4+2] = byte(uint32(buf[i*4+0]) * 255 / a) // B
		}
	}
	f, err := os.Create(".local/menu-dump.png")
	if err == nil {
		_ = png.Encode(f, img)
		_ = f.Close()
		Logf("位图已 dump：.local/menu-dump.png")
	}
}

type rect struct{ left, top, right, bottom int32 }

type monitorInfo struct {
	cbSize    uint32
	rcMonitor rect
	rcWork    rect
	dwFlags   uint32
}

// monitorInfoAt 屏幕工作区（避开任务栏）。
// ⚠️ MonitorFromPoint 的 POINT 是**按值**传参（8 字节直接进寄存器）—— 传指针会被系统
// 当成坐标解读（实测：翻转判定失效，菜单溢出屏幕下沿）。拼成单个 uintptr 传。
func monitorInfoAt(x, y int) *monitorInfo {
	packed := uintptr(uint32(x)) | uintptr(uint32(y))<<32
	hm, _, _ := pMonitorFromPoint.Call(packed, monitorNearest)
	if hm == 0 {
		return nil
	}
	mi := &monitorInfo{cbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	if r, _, _ := pGetMonitorInfoW.Call(hm, uintptr(unsafe.Pointer(mi))); r == 0 {
		return nil
	}
	return mi
}

func boolToInt(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}

// moduleHandle 进程实例句柄（多次调用，包掉多返回值）。
func moduleHandle() uintptr {
	h, _, _ := pGetModuleHandleP.Call(0)
	return h
}

var mouseHookHandle uintptr

// mouseHookProcFn 低级鼠标钩子：观察鼠标按下事件，落在圆角板外 → 关闭菜单。
// **只观察不拦截**（返回 CallNextHookEx 链），外部的点击照常送达目标窗口。
// 位置取 GetCursorPos（点击瞬间光标 = 钩子 pt；避开 uintptr→Pointer 转换的 vet 告警）。
func mouseHookProcFn(nCode, wParam, lParam uintptr) uintptr {
	if nCode >= 0 && (wParam == wmLButtonDown || wParam == wmRButtonDown) {
		menuMu.Lock()
		s := curMenu
		menuMu.Unlock()
		if s != nil {
			var cp point
			pGetCursorPos.Call(uintptr(unsafe.Pointer(&cp)))
			x := int(cp.x) - int(s.posX)
			y := int(cp.y) - int(s.posY)
			if !s.onPlate(x, y) {
				s.close() // 点在圆角板外：收菜单（不吞这次点击）
			}
		}
	}
	r, _, _ := pCallNextHookEx.Call(0, nCode, wParam, lParam)
	return r
}

var mouseHookProc = syscall.NewCallback(mouseHookProcFn)

func installMouseHook() {
	h, _, err := pSetWindowsHookExW.Call(whMouseLL, mouseHookProc, 0, 0)
	if h == 0 {
		Logf("安装低级鼠标钩子失败 err=%v（点外关闭将失效，菜单其余功能不受影响）", err)
		return
	}
	mouseHookHandle = h
}

func uninstallMouseHook() {
	if mouseHookHandle != 0 {
		pUnhookWinHookExW.Call(mouseHookHandle)
		mouseHookHandle = 0
	}
}
