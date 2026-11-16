package tray

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 菜单树守卫（契约 C-4 / C-12）：ID 重复与空 Label 肉眼看不出来，
// 只有右键点下去才发现「点了没反应」或「点 A 触发了 B」。
func TestValidateMenu(t *testing.T) {
	valid := []MenuItem{
		{ID: 1, Label: "状态", Disabled: true, Dot: DotRun},
		{Separator: true},
		{ID: 2, Label: "打开"},
		{Label: "节点", Children: []MenuItem{
			{ID: 201, Label: "香港"},
			{ID: 202, Label: "东京", Checked: true},
			{Separator: true},
			{ID: 210, Label: "重新测速"},
		}},
	}
	if err := ValidateMenu(valid); err != nil {
		t.Fatalf("合法菜单被判错：%v", err)
	}

	cases := []struct {
		name string
		menu []MenuItem
		want string // 期望错误里包含的片段
	}{
		{"ID 重复", []MenuItem{{ID: 1, Label: "a"}, {ID: 1, Label: "b"}}, "重复"},
		{"父子 ID 冲突", []MenuItem{
			{ID: 5, Label: "a"},
			{Label: "sub", Children: []MenuItem{{ID: 5, Label: "b"}}},
		}, "重复"},
		{"可点击项缺 ID", []MenuItem{{Label: "没 ID"}}, "缺少 ID"},
		{"空 Label", []MenuItem{{ID: 3}}, "Label 为空"},
		{"分隔线带 ID", []MenuItem{{Separator: true, ID: 9}}, "分隔线"},
		{"分隔线带 Label", []MenuItem{{Separator: true, Label: "×"}}, "分隔线"},
		{"Label 里手拼 \\t", []MenuItem{{ID: 7, Label: "退出\tCtrl+Q"}}, "Accel 字段"},
		{"Accel 里又拼 \\t", []MenuItem{{ID: 8, Label: "退出", Accel: "\tCtrl+Q"}}, "Accel 里不要"},
	}
	for _, c := range cases {
		err := ValidateMenu(c.menu)
		if err == nil {
			t.Errorf("%s：应报错却没有", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误信息 = %q，应包含 %q", c.name, err.Error(), c.want)
		}
	}
}

// 子菜单项（带 Children）的 ID 被忽略，允许为 0：父项只当锚点，
// 点它不会触发回调。
func TestValidateMenuSubmenuIDOptional(t *testing.T) {
	menu := []MenuItem{{Label: "地区", Children: []MenuItem{{ID: 301, Label: "自动"}}}}
	if err := ValidateMenu(menu); err != nil {
		t.Fatalf("带子菜单的父项 ID=0 应合法：%v", err)
	}
}

// 深度上限：契约建议 ≤ 2 层，这里放宽到 4 层兜底（Linux DBusMenu 深层嵌套体验差）。
func TestValidateMenuDepth(t *testing.T) {
	leaf := []MenuItem{{ID: 100, Label: "leaf"}}
	for i := 0; i < 5; i++ { // 嵌套 5 层，超过上限
		leaf = []MenuItem{{Label: "n", Children: leaf}}
	}
	if err := ValidateMenu(leaf); err == nil {
		t.Error("超过 4 层应报错")
	}
}

// 通知节流守卫（契约 C-9）：多份实现各写一份，迟早写出三种行为。
//
// 反向验证：把 Allow() 里的间隔比较去掉，后三条断言立刻 FAIL。
func TestThrottle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", ".stamp")
	base := time.Date(2026, 11, 16, 10, 0, 0, 0, time.Local)

	th := NewThrottle(p, 24*time.Hour)
	th.now = func() time.Time { return base }

	if !th.Allow() {
		t.Fatal("首次应放行")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("放行后应写入时间戳（父目录要自动创建）：%v", err)
	}
	if th.Allow() {
		t.Error("同一时刻第二次不该放行")
	}

	th.now = func() time.Time { return base.Add(time.Hour) }
	if th.Allow() {
		t.Error("1h 后不该放行")
	}
	th.now = func() time.Time { return base.Add(23 * time.Hour) }
	if th.Allow() {
		t.Error("23h 后不该放行")
	}
	th.now = func() time.Time { return base.Add(25 * time.Hour) }
	if !th.Allow() {
		t.Error("25h 后应放行")
	}
}

// 时间戳异常（时钟回拨 / 拷贝了别的机器的 .local / 文件被写坏）一律按「没弹过」处理：
// 宁可多弹一次，也不要永久静默 —— 后者没有任何现象，排不出来。
func TestThrottleBrokenStamp(t *testing.T) {
	base := time.Date(2026, 11, 16, 10, 0, 0, 0, time.Local)

	future := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(future, []byte(strconv.FormatInt(base.Add(72*time.Hour).Unix(), 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	th := NewThrottle(future, 24*time.Hour)
	th.now = func() time.Time { return base }
	if !th.Allow() {
		t.Error("时间戳在未来应按过期处理（放行）")
	}

	junk := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(junk, []byte("not-a-timestamp"), 0o644); err != nil {
		t.Fatal(err)
	}
	th2 := NewThrottle(junk, 24*time.Hour)
	th2.now = func() time.Time { return base }
	if !th2.Allow() {
		t.Error("时间戳不可解析应按没弹过处理（放行）")
	}

	// nil 接收者放行：调用方不必为「没配节流」写分支
	var nilTh *Throttle
	if !nilTh.Allow() {
		t.Error("nil Throttle 应放行")
	}
}

// BalloonKind 的枚举值必须与 Win32 的 NIIF_* 一一对应，否则通知会显示成别的级别图标。
func TestBalloonKindMapping(t *testing.T) {
	want := map[BalloonKind]int{
		BalloonNone:  0,
		BalloonInfo:  1,
		BalloonWarn:  2,
		BalloonError: 3,
	}
	for k, v := range want {
		if int(k) != v {
			t.Errorf("BalloonKind %d 期望枚举值 %d（要与 NIIF_* 对齐）", int(k), v)
		}
	}
}
