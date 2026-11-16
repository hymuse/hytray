//go:build windows

package tray

import "testing"

// 玻璃菜单嵌套泵的重入守卫（2026-11-16 切换卡死修复）。
//
// 反向验证：若 menuPumpShouldDrop 不再拦 msgCallback，玻璃菜单显示期间来一次右键，
// 托盘 msgCallback 会被派到 wndProc → 在嵌套泵里又弹一层菜单 → 卡死。
func TestMenuPumpShouldDrop(t *testing.T) {
	cases := []struct {
		name string
		msg  uint32
		want bool
	}{
		{"托盘图标事件必须丢", msgCallback, true},
		{"托盘退出请求必须丢", msgQuit, true},
		{"菜单关闭消息由调用方处理，不在这丢", wmCloseMenu, false},
		{"鼠标钩子消息照常派发（否则点外关闭失效）", wmLButtonDown, false},
		{"鼠标移动照常派发（hover 高亮）", wmMousemove, false},
		{"键盘照常派发（↑↓/Enter/Esc）", wmKeyDown, false},
		{"绘制照常派发", 0x000F, false},
	}
	for _, c := range cases {
		if got := menuPumpShouldDrop(c.msg); got != c.want {
			t.Errorf("%s：msg=%#x 得到 %v，要 %v", c.name, c.msg, got, c.want)
		}
	}
}
