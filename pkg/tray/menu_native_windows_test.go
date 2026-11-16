//go:build windows

package tray

import "testing"

// 菜单树构建守卫：递归挂子菜单是最容易「静默失败」的一段 ——
// MF_POPUP 挂错了会得到一条点了没反应的灰条，或者子菜单句柄被提前 Destroy 导致崩溃。
// 这里真的建一个 HMENU 出来数条目，不靠肉眼看代码。
func TestBuildPopupTree(t *testing.T) {
	hMenu, _, _ := pCreatePopupMenu.Call()
	if hMenu == 0 {
		t.Fatal("CreatePopupMenu 失败")
	}
	defer pDestroyMenu.Call(hMenu)

	items := []MenuItem{
		{ID: 1, Label: "状态", Disabled: true, Dot: DotRun},
		{Separator: true},
		{ID: 2, Label: "打开"},
		{Label: "节点", Children: []MenuItem{
			{ID: 201, Label: "香港"},
			{ID: 202, Label: "东京", Checked: true},
			{Separator: true},
			{Label: "更多", Children: []MenuItem{
				{ID: 301, Label: "深层"},
			}},
		}},
		{Separator: true},
		{ID: 99, Label: "退出"},
	}
	buildPopup(hMenu, items)

	// 一级应有 7 条（含 2 条分隔线与 2 条子菜单锚点）
	n, _, _ := pGetMenuItemCount.Call(hMenu)
	if int(int32(n)) != len(items) {
		t.Fatalf("一级条目数 = %d，要 %d", int32(n), len(items))
	}

	// 第 4 项（index 3）是「节点」子菜单 ⇒ 必须能取到子菜单句柄且条目数对
	sub, _, _ := pGetSubMenu.Call(hMenu, 3)
	if sub == 0 {
		t.Fatal("「节点」没有挂成子菜单（取不到子菜单句柄）")
	}
	sn, _, _ := pGetMenuItemCount.Call(sub)
	if int(int32(sn)) != 4 {
		t.Fatalf("「节点」子菜单条目数 = %d，要 4", int32(sn))
	}

	// 子菜单里的第 4 项又是子菜单「更多」⇒ 验证多层递归
	subsub, _, _ := pGetSubMenu.Call(sub, 3)
	if subsub == 0 {
		t.Fatal("「更多」没有挂成子菜单（多层递归断了）")
	}
	ssn, _, _ := pGetMenuItemCount.Call(subsub)
	if int(int32(ssn)) != 1 {
		t.Fatalf("「更多」条目数 = %d，要 1", int32(ssn))
	}
}

// 分隔线不得占用命令 ID：AppendMenu 的分隔线项 ID 恒为 0，
// 若误用 mfString 加分隔线，会变成一条可点的空白项。
func TestBuildPopupSeparator(t *testing.T) {
	hMenu, _, _ := pCreatePopupMenu.Call()
	if hMenu == 0 {
		t.Fatal("CreatePopupMenu 失败")
	}
	defer pDestroyMenu.Call(hMenu)

	buildPopup(hMenu, []MenuItem{{ID: 1, Label: "a"}, {Separator: true}})
	n, _, _ := pGetMenuItemCount.Call(hMenu)
	if int(int32(n)) != 2 {
		t.Fatalf("条目数 = %d，要 2（分隔线也算一条）", int32(n))
	}
	// index 1 是分隔线：取它的 ID 应为 0
	id, _, _ := pGetMenuItemID.Call(hMenu, 1)
	if int32(id) != 0 {
		t.Errorf("分隔线 ID = %d，要 0", int32(id))
	}
}

// 快捷键拼装守卫（契约 §2.4）：Accel 由库拼 \t，项目不许手拼。
//
// 空 Accel 必须不拼 —— 尾部多一个 \t 会在菜单里多出一条空白右列。
func TestMenuLabel(t *testing.T) {
	if got := menuLabel(MenuItem{Label: "熄屏"}); got != "熄屏" {
		t.Errorf("无 Accel 时 label = %q，要裸 Label", got)
	}
	if got := menuLabel(MenuItem{Label: "熄屏", Accel: "Ctrl+Alt+S"}); got != "熄屏\tCtrl+Alt+S" {
		t.Errorf("有 Accel 时 label = %q，要 \"熄屏\tCtrl+Alt+S\"", got)
	}
}

// 带 Accel 的菜单项构建后条目数不变（\t 是同一项内部的列分隔，不是新条目）。
func TestBuildPopupWithAccel(t *testing.T) {
	hMenu, _, _ := pCreatePopupMenu.Call()
	if hMenu == 0 {
		t.Fatal("CreatePopupMenu 失败")
	}
	defer pDestroyMenu.Call(hMenu)

	items := []MenuItem{
		{ID: 1, Label: "熄屏", Accel: "Ctrl+Alt+S"},
		{ID: 2, Label: "锁屏+熄屏", Accel: "Ctrl+Alt+L"},
	}
	buildPopup(hMenu, items)
	n, _, _ := pGetMenuItemCount.Call(hMenu)
	if int(int32(n)) != 2 {
		t.Fatalf("条目数 = %d，要 2", int32(n))
	}
}
