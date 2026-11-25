//go:build windows

package tray

import (
	"strings"
	"testing"
	"time"
	"unsafe"

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

// uFlags 守卫：NIF_SHOWTIP（MSDN 权威值 0x80）必须在 uFlags 里。
// V4 下标准 tooltip 靠它开启（MSDN 明定），不是靠退回 V0。
//
// 历史坑（两次）：
//
//	① 迁移到 V4 后只 copy 了 szTip、没带 NIF_SHOWTIP —— 悬停不弹；
//	② 补 flag 时误写成 0x40，且守卫自证「nifShowTip==0x40」= 假绿，修了等于没修。
//
// 本测试用硬编码 0x80（MSDN NOTIFYICONDATAW.uFlags）钉住真值，不引用被测常量自证。
// 反向验证：把 iconFlags 里的 nifShowTip 删掉、或把常量改回 0x40，本测试立刻 FAIL。
func TestIconFlagsShowTip(t *testing.T) {
	const msdnNifShowTip = 0x80 // 外部权威值，故意不引用 nifShowTip
	flags := iconFlags()
	if flags&msdnNifShowTip == 0 {
		t.Errorf("uFlags 必须含 NIF_SHOWTIP(0x80)，当前 0x%x", flags)
	}
	if flags&nifTip == 0 {
		t.Errorf("uFlags 必须含 NIF_TIP(0x4)，当前 0x%x", flags)
	}
	if nifShowTip != msdnNifShowTip {
		t.Errorf("nifShowTip 应为 MSDN NIF_SHOWTIP=0x80，当前 0x%x", nifShowTip)
	}
}

// 完整结构守卫：NOTIFYICONDATAW 必须是完整 Vista+ 布局（64 位 = 976）。
//
// 历史坑：原结构止于 dwInfoFlags（952），缺 guidItem(16)+hBalloonIcon(8)。
// Win11 新 XAML 任务栏按 cbSize 判定结构版本，尺寸不足会忽略 NIF_SHOWTIP。
// 硬编码 976（MSDN 完整 sizeof），反向验证：删掉任一尾部字段本测试立刻 FAIL。
func TestNotifyIconDataFullSize(t *testing.T) {
	const msdnFullSize = 976 // 64-bit NOTIFYICONDATAW
	if got := unsafe.Sizeof(notifyIconData{}); got != msdnFullSize {
		t.Errorf("sizeof(notifyIconData)=%d，MSDN 完整 NOTIFYICONDATAW 应为 %d", got, msdnFullSize)
	}
}

// 默认语义守卫：Options 零值必须走 V4（V0Compat=false）。
//
// 目标 = 键盘选中 + 标准 tooltip 兼得，默认就得是 V4；V0Compat=true 只是逃生舱。
// 反向验证：把 V0Compat 默认改成 true（即零值走 V0）本测试 FAIL。
func TestV4ByDefault(t *testing.T) {
	var opts Options
	if opts.V0Compat {
		t.Error("Options 零值 V0Compat 必须为 false（默认走 V4），当前为 true")
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

// 左键事件按版本分流守卫（2026-11-20 双标签根因）。
//
// 回归背景：旧实现把 NIN_SELECT / WM_LBUTTONUP / WM_LBUTTONDBLCLK 三个事件
// 全部映射到 OnOpen。V4 下 Win11 同一次单击同时送达 NIN_SELECT + WM_LBUTTONUP，
// 于是 openURL 被调两次 ⇒ 浏览器开两个相同标签（截图实锤 + tray.log 成对记录）。
//
// 正向规则：V4 只认 NIN_SELECT；V0 才认鼠标消息。两条路径互不串味。
func TestEventOpensPanel(t *testing.T) {
	cases := []struct {
		name    string
		version uint32
		event   uint32
		want    bool
	}{
		// V4：单击只有 NIN_SELECT 生效；补发的 WM_LBUTTONUP 必须被忽略。
		{"V4 单击 NIN_SELECT → 开", notifyIconVersion4, ninSelect, true},
		{"V4 补发 WM_LBUTTONUP → 忽略（双标签根因）", notifyIconVersion4, wmLButtonUp, false},
		{"V4 遗留 WM_LBUTTONDBLCLK → 忽略", notifyIconVersion4, wmLButtonDblClk, false},
		// V0：老式鼠标消息仍生效。
		{"V0 左键 WM_LBUTTONUP → 开", 0, wmLButtonUp, true},
		{"V0 双击 WM_LBUTTONDBLCLK → 开", 0, wmLButtonDblClk, true},
		{"V0 不认 V4 的 NIN_SELECT", 0, ninSelect, false},
	}
	for _, c := range cases {
		if got := eventOpensPanel(c.version, c.event); got != c.want {
			t.Errorf("%s：得到 %v，要 %v", c.name, got, c.want)
		}
	}
}

// 双击只触发一次 OnOpen 守卫（2026-11-21 hymuse 实测「单击开一次，双击开两次」）。
//
// 两层过滤缺一不可：
//   - eventOpensPanel：按事件码挡同一次点击的补发消息（原守卫，保留）
//   - shouldFireOpen 的时间窗：挡双击第二击那个**合法**打开事件
//
// 反向验证：把 openDebounceWindow 改成 0，「双击 80ms / 300ms」两条立刻 FAIL。
func TestShouldFireOpenDebounce(t *testing.T) {
	const base int64 = 1_700_000_000_000_000_000
	ms := func(n int64) int64 { return base + n*int64(time.Millisecond) }

	cases := []struct {
		name    string
		version uint32
		event   uint32
		now     int64
		last    int64
		want    bool
	}{
		// V4：双击 = 两次独立 NIN_SELECT，第二击必须被时间窗挡掉
		{"V4 首次点击", notifyIconVersion4, ninSelect, base, 0, true},
		{"V4 双击第二击 80ms → 丢弃", notifyIconVersion4, ninSelect, ms(80), ms(0), false},
		{"V4 双击第二击 300ms → 丢弃", notifyIconVersion4, ninSelect, ms(300), ms(0), false},
		{"V4 间隔 499ms → 丢弃", notifyIconVersion4, ninSelect, ms(499), ms(0), false},
		{"V4 边界 500ms → 放行", notifyIconVersion4, ninSelect, ms(500), ms(0), true},
		{"V4 慢单击 1.2s → 放行", notifyIconVersion4, ninSelect, ms(1200), ms(0), true},
		{"V4 补发 LBUTTONUP → 丢弃（原守卫不回退）", notifyIconVersion4, wmLButtonUp, ms(1200), ms(0), false},
		{"V4 DBLCLK 标记 → 丢弃（原守卫不回退）", notifyIconVersion4, wmLButtonDblClk, ms(1200), ms(0), false},
		// V0：双击 = LBUTTONUP + DBLCLK + LBUTTONUP，同样按时间窗去重
		{"V0 首次左键 LBUTTONUP", 0, wmLButtonUp, base, 0, true},
		{"V0 双击 LBUTTONUP 第二击 60ms → 丢弃", 0, wmLButtonUp, ms(60), ms(0), false},
		{"V0 DBLCLK 与首击同窗口 → 丢弃", 0, wmLButtonDblClk, ms(70), ms(0), false},
		{"V0 慢单击 1.2s → 放行", 0, wmLButtonUp, ms(1200), ms(0), true},
		// 时钟回拨别把图标点死
		{"时钟回拨 now<last → 放行", notifyIconVersion4, ninSelect, ms(0), ms(100), true},
	}
	for _, c := range cases {
		if got := shouldFireOpen(c.version, c.event, c.now, c.last, openDebounceWindow); got != c.want {
			t.Errorf("%s：version=%d event=%#x now=%d last=%d 得到 %v，要 %v",
				c.name, c.version, c.event, c.now, c.last, got, c.want)
		}
	}

	// 反向自证：窗口=0 时原本被挡的两条必须放行（调真身，不复制实现）
	for _, c := range cases[1:3] {
		if !shouldFireOpen(c.version, c.event, c.now, c.last, 0) {
			t.Errorf("窗口=0 时 %s 应放行（说明该用例确实由时间窗挡住）", c.name)
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
