//go:build windows

package tray

import (
	"syscall"
	"unsafe"
)

// 原生菜单（TrackPopupMenu）相关常量与方法。
const (
	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfDisabled  = 0x0002
	mfChecked   = 0x0008
	mfSeparator = 0x0800
	mfPopup     = 0x0010

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
	tpmNoNotify    = 0x0080
)

var (
	pCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	pAppendMenu       = user32.NewProc("AppendMenuW")
	pTrackPopupMenuEx = user32.NewProc("TrackPopupMenuEx")
	pDestroyMenu      = user32.NewProc("DestroyMenu")
	pGetMenuItemCount = user32.NewProc("GetMenuItemCount")
	pGetMenuItemID    = user32.NewProc("GetMenuItemID")
	pGetSubMenu       = user32.NewProc("GetSubMenu")
)

// glassSupported 本构建是否含自绘玻璃菜单引擎。
//
// 恒含（menu_glass_windows.go，2026-11-16 合并），合并时只做了三处适配：
// 删掉重复的 systemPrefersDark / DPI 声明、调用改指向本包的 enableDPIAwareness。
// 渲染代码一个像素没动。
const glassSupported = true

func glassAvailable() bool { return glassSupported }

// showMenuFor 按样式分派菜单渲染（鼠标触发：弹在光标处）。
func showMenuFor(st *state) { showMenuAt(st, nil) }

// showMenuNearIcon 键盘触发（NIN_KEYSELECT）时用：菜单贴着图标弹。
//
// 为什么不能复用光标坐标：键盘操作时光标可能停在屏幕任意角落，
// 菜单跑到那儿会让人以为点错了东西。取不到图标矩形（图标被折进 ^ 溢出区时也会失败）
// 就退回光标位置。
func showMenuNearIcon(st *state) {
	if pt, ok := iconRectPoint(st); ok {
		showMenuAt(st, &pt)
		return
	}
	Logf("取不到图标矩形（可能被折进 ^ 溢出区），菜单退回光标位置")
	showMenuAt(st, nil)
}

// showMenuAt 是两种样式的统一入口：先求值一次菜单，再决定用哪套渲染。
//
// at == nil 表示弹在光标处；非 nil 用于键盘触发时贴着图标弹（玻璃样式不支持指定坐标，
// 它自带定位逻辑）。
func showMenuAt(st *state, at *point) {
	// ★重入闸：玻璃菜单用自己的**嵌套消息泵**（GetMessage 抓全线程消息）。
	// 菜单显示期间若托盘又收到 msgCallback（连点右键 / 切换样式时的残留消息），
	// 嵌套泵会把 msgCallback 派发给托盘 wndProc → 再次进到这里 → 递归弹菜单。
	//
	// 症状（2026-11-16 实测）：从玻璃切回原生后菜单「卡死在屏幕上」——
	// 就是递归里两层菜单互相抢着关、谁也没关干净。
	//
	// curMenu != nil 表示玻璃菜单正在显示；此时直接丢弃新的弹菜单请求。
	// 原生菜单走 TrackPopupMenuEx 自带模态泵、不存在这个重入问题，所以只在玻璃分支拦。
	// 直接看 curMenu（不看当前 style）—— 切换样式的 onPick 里 style 会变 Native，
	// 但那一刻玻璃菜单可能还没走完卸载，仍要拦住重入。
	menuMu.Lock()
	busy := curMenu != nil
	menuMu.Unlock()
	if busy {
		Logf("玻璃菜单正在显示，忽略重复的弹菜单请求（防重入）")
		return
	}

	items := []MenuItem{}
	if st.opts.Menu != nil {
		items = st.opts.Menu() // 每次右键重新求值（契约 C-1）
	}

	if Style(st.style.Load()) == StyleGlass && glassSupported {
		if menuHasChildren(items) {
			// 自绘引擎没有子菜单实现。混着画会出两套交互，
			// 所以整份菜单降级原生 —— 契约 §2.4 写明了这条边界。
			Logf("菜单含子菜单，玻璃样式不支持（§2.4），整份降级为原生菜单")
		} else if err := showCustomMenu(items, func(id int) {
			if st.opts.OnMenu != nil {
				safeCall(func() { st.opts.OnMenu(id) })
			}
		}); err == nil {
			return
		} else {
			Logf("玻璃菜单渲染失败（%v），降级原生", err)
		}
	}
	showNativeMenuItems(st, items, at)
}

// menuHasChildren 判断菜单树里有没有子菜单（玻璃样式不支持）。
func menuHasChildren(items []MenuItem) bool {
	for _, it := range items {
		if len(it.Children) > 0 {
			return true
		}
	}
	return false
}

// iconRectPoint 用 Shell_NotifyIconGetRect 拿图标在屏幕上的矩形，返回菜单应弹出的点（图标左下角）。
func iconRectPoint(st *state) (point, bool) {
	id := notifyIconIdentifier{
		cbSize: uint32(unsafe.Sizeof(notifyIconIdentifier{})),
		hWnd:   st.hwnd,
		uID:    1,
	}
	var rc struct{ left, top, right, bottom int32 }
	r, _, _ := pShellNotifyIconGetRect.Call(
		uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(&rc)),
	)
	if int32(r) != 0 { // S_OK = 0
		return point{}, false
	}
	return point{x: rc.left, y: rc.bottom}, true
}

// buildPopup 递归把 items 填进 hMenu。
//
// 子菜单用 MF_POPUP 挂：此时 AppendMenu 的第 4 个参数是**子菜单句柄**而不是命令 ID。
// 子菜单句柄一旦挂到父菜单上就归父菜单管，别再单独 DestroyMenu（会崩）。
// 空子菜单会显示成一个点了没反应的灰条 —— 契约 C-12 要求调用方不要生成这种项。
func buildPopup(hMenu uintptr, items []MenuItem) {
	for _, it := range items {
		if it.Separator {
			pAppendMenu.Call(hMenu, mfSeparator, 0, 0)
			continue
		}
		if len(it.Children) > 0 {
			hSub, _, _ := pCreatePopupMenu.Call()
			if hSub == 0 {
				continue
			}
			buildPopup(hSub, it.Children)
			label, err := syscall.UTF16PtrFromString(it.Label)
			if err != nil {
				pDestroyMenu.Call(hSub) // 还没挂上去，得自己收拾
				continue
			}
			flags := uintptr(mfPopup)
			if it.Disabled {
				flags |= mfGrayed | mfDisabled
			}
			pAppendMenu.Call(hMenu, flags, hSub, uintptr(unsafe.Pointer(label)))
			continue
		}
		flags := uintptr(mfString)
		if it.Disabled {
			flags |= mfGrayed | mfDisabled
		}
		if it.Checked {
			flags |= mfChecked
		}
		label, err := syscall.UTF16PtrFromString(menuLabel(it))
		if err != nil {
			continue
		}
		pAppendMenu.Call(hMenu, flags, uintptr(it.ID), uintptr(unsafe.Pointer(label)))
	}
}

// menuLabel 拼出实际显示的文本：Label + (\t + Accel)。
//
// 原生菜单把 \t 之后的内容按右列对齐显示 —— 这是系统行为，不用自己算宽度。
// 空 Accel 不拼，避免尾部多一个空的右列（那会多出一条空白）。
func menuLabel(it MenuItem) string {
	if it.Accel == "" {
		return it.Label
	}
	return it.Label + "\t" + it.Accel
}

// showNativeMenu 弹出系统原生右键菜单。
// 用 TPM_RETURNCMD 同步取回命令 ID，省掉 WM_COMMAND 那条路。
//
// at == nil 表示弹在光标处（鼠标触发）；非 nil 用于键盘触发时贴着图标弹。
func showNativeMenuItems(st *state, items []MenuItem, at *point) {
	hMenu, _, _ := pCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer pDestroyMenu.Call(hMenu)

	buildPopup(hMenu, items)

	var pt point
	if at != nil {
		pt = *at
	} else {
		pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	}
	// 先抢前台再弹，弹完补一条 WM_NULL —— 缺这两步菜单会「关不掉」（Win32 老毛病）。
	pSetForegroundWindow.Call(uintptr(st.hwnd))
	Logf("弹出菜单：%d 项 @ (%d,%d)", len(items), pt.x, pt.y)
	cmd, _, _ := pTrackPopupMenuEx.Call(
		hMenu,
		tpmReturnCmd|tpmRightButton|tpmNoNotify,
		uintptr(pt.x), uintptr(pt.y),
		uintptr(st.hwnd), 0,
	)
	pPostMessage.Call(uintptr(st.hwnd), wmNull, 0, 0)

	// 记一条点击日志：「菜单点了没反应」光看代码判不出来 —— 到底是没选中（cmd=0）、
	// 还是 ID 没对上，只有实际值能说清。
	if cmd == 0 {
		Logf("菜单：未选中（点空白或点了禁用项）")
		return
	}
	id := int(int32(cmd))
	Logf("菜单点击 id=%d", id)
	if st.opts.OnMenu != nil {
		safeCall(func() { st.opts.OnMenu(id) })
	}
}
