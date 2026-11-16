//go:build windows

package tray

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// tooltip 上限守卫（契约 §2.1）：szTip 是 128 个 UTF-16 码元（含结尾 0），超长必须截断。
//
// 历史坑：曾有实现的注释写「≤ 63 码元」（那是 V1 的 szTip）——
// 契约按 127 统一，这条守卫钉住它。
func TestTrimTip(t *testing.T) {
	long := strings.Repeat("中", 200)
	if got := trimTip(long); len([]rune(got)) != maxTipChars {
		t.Errorf("超长 tooltip 应截断到 %d 码元，得到 %d", maxTipChars, len([]rune(got)))
	}
	if maxTipChars != 127 {
		t.Errorf("maxTipChars = %d，契约要求 127（szTip 128 WCHAR 含结尾）", maxTipChars)
	}
	if got := trimTip("  x  "); got != "x" {
		t.Errorf("应裁剪首尾空白，得到 %q", got)
	}
}

// 退避序列守卫（契约 C-10）：单调递增、首档不小于 1 秒、次数有上限。
//
// 首档为什么不小于 1 秒：Shell_NotifyIcon 内部 SendMessageTimeout 最长阻塞 4 秒，
// 开机自启时资源管理器常比托盘晚起来，密集重试只是白等。
func TestBackoffDelays(t *testing.T) {
	if n := len(backoffDelays); n == 0 || n > 6 {
		t.Fatalf("重试次数应在 1~6 之间，当前 %d", n)
	}
	if backoffDelays[0] < time.Second {
		t.Errorf("首次重试间隔 %v 太短", backoffDelays[0])
	}
	for i := 1; i < len(backoffDelays); i++ {
		if backoffDelays[i] <= backoffDelays[i-1] {
			t.Errorf("退避未递增：%v", backoffDelays)
		}
	}
}

// DPI 图标选档守卫（契约 C-8）。
func TestPickIconIndex(t *testing.T) {
	widths := []int{16, 24, 32, 48}
	cases := []struct{ target, want int }{
		{16, 16}, // 100% DPI：原尺寸命中
		{24, 24}, // 150%
		{32, 32}, // 200%
		{20, 24}, // 介于两档之间 ⇒ 取不小于目标的最小档
		{64, 48}, // 超过最大档 ⇒ 取最大（缩总比空白强）
	}
	for _, c := range cases {
		if got := widths[pickIconIndex(widths, c.target)]; got != c.want {
			t.Errorf("target=%d：选中 %dpx，要 %dpx", c.target, got, c.want)
		}
	}

	// 只有一档时无论 target 多大都选它
	single := []int{48}
	if got := single[pickIconIndex(single, 16)]; got != 48 {
		t.Errorf("单档 ICO：选中 %d，要 48", got)
	}

	// 全比 target 小：取最大
	small := []int{16, 20}
	if got := small[pickIconIndex(small, 32)]; got != 20 {
		t.Errorf("全比目标小：选中 %d，要 20", got)
	}
}

// V4 事件解析守卫。
//
// 为什么必须单测：解析错了的现象是「右键没反应」或「左键弹出菜单」，
// 而这两件事只有真机点下去才知道 —— 编码阶段能钉死就别留给人工。
func TestTrayEventV4(t *testing.T) {
	const iconID = 1
	mk := func(ev uint32) uintptr { return uintptr(iconID)<<16 | uintptr(ev) }

	cases := []struct {
		name string
		ver  uint32
		lp   uintptr
		want uint32
	}{
		{"V4 单击 → NIN_SELECT", notifyIconVersion4, mk(ninSelect), ninSelect},
		{"V4 键盘选中 → NIN_KEYSELECT", notifyIconVersion4, mk(ninKeySelect), ninKeySelect},
		{"V4 右键 → WM_CONTEXTMENU", notifyIconVersion4, mk(wmContextMenu), wmContextMenu},
		{"V4 双击 → WM_LBUTTONDBLCLK", notifyIconVersion4, mk(wmLButtonDblClk), wmLButtonDblClk},
		{"V4 通知点击 → NIN_BALLOONUSERCLICK", notifyIconVersion4, mk(ninBalloonUserClick), ninBalloonUserClick},
		// V0：lParam 就是消息本身，不做掩码
		{"V0 右键 → WM_RBUTTONUP", 0, uintptr(wmRButtonUp), wmRButtonUp},
		{"V0 左键 → WM_LBUTTONUP", 0, uintptr(wmLButtonUp), wmLButtonUp},
	}
	for _, c := range cases {
		if got := trayEvent(c.ver, c.lp); got != c.want {
			t.Errorf("%s：得到 %#x，要 %#x", c.name, got, c.want)
		}
	}
}

// V4 的高 16 位（图标 ID）不得干扰事件判断 —— 多图标场景下 ID 会变大。
func TestTrayEventIgnoresHighBits(t *testing.T) {
	for _, iconID := range []uintptr{1, 7, 0xFFFF} {
		lp := iconID<<16 | uintptr(wmContextMenu)
		if got := trayEvent(notifyIconVersion4, lp); got != wmContextMenu {
			t.Errorf("图标 ID=%d 时解析成 %#x，要 %#x", iconID, got, wmContextMenu)
		}
	}
}

// 主题跟随守卫（契约 C-17）。
//
// 关键点：读的必须是 AppsUseLightTheme（管 Win32 应用/菜单），
// 不是 SystemUsesLightTheme（管任务栏/开始菜单）—— 两者可以不一致，
// 读错了就会出现「任务栏已经深色，菜单还按浅色处理」。
func TestSystemPrefersDarkMatchesAppsKey(t *testing.T) {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		t.Skipf("读不到 Personalize 注册表，跳过：%v", err)
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	if err != nil {
		t.Skipf("没有 AppsUseLightTheme 值，跳过：%v", err)
	}
	if got, want := systemPrefersDark(), v == 0; got != want {
		t.Errorf("systemPrefersDark()=%v，但 AppsUseLightTheme=%d（应为 %v）", got, v, want)
	}
}

// 未文档化导出的序号必须钉死：写错了不会报错，只会悄悄不变色，很难查。
func TestUxthemeOrdinals(t *testing.T) {
	if uxthemeSetPreferredAppMode != 135 {
		t.Errorf("SetPreferredAppMode 序号应为 135，当前 %d", uxthemeSetPreferredAppMode)
	}
	if uxthemeFlushMenuThemes != 136 {
		t.Errorf("FlushMenuThemes 序号应为 136，当前 %d", uxthemeFlushMenuThemes)
	}
	if pmForceDark != 2 || pmForceLight != 3 {
		t.Errorf("PreferredAppMode 枚举不对：ForceDark=%d ForceLight=%d", pmForceDark, pmForceLight)
	}
}
