// demo 用最小可运行例子自证内核：图标能不能挂上、菜单能不能弹出、子菜单是不是空灰条。
//
//	go run ./examples/demo
//
// 跑起来后看任务栏右下角（Win11 会折进 ^ 溢出区）。右键出菜单，点「退出」结束。
// 排障日志在 %TEMP%\hytray-demo.log —— 托盘出问题时先看它，别猜。
package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tray "github.com/hymuse/hytray/pkg/tray"
)

//go:embed assets/tray.ico
var icoBytes []byte

const (
	idStatus = 1
	idToggle = 10
	idIcon   = 11
	idWarn   = 12
	idStyle  = 13
	idA      = 201
	idB      = 202
	idDeep   = 301
	idQuit   = 99
)

func main() {
	logPath := filepath.Join(os.TempDir(), "hytray-demo.log")
	lf, err := os.Create(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "建日志失败：%v\n", err)
		os.Exit(1)
	}
	defer lf.Close()
	tray.Logf = func(format string, a ...any) {
		fmt.Fprintf(lf, time.Now().Format("15:04:05.000")+" "+format+"\n", a...)
	}
	fmt.Printf("托盘日志：%s\n", logPath)

	// 单实例：计划任务、手动、开机启动三处都可能拉起托盘，互斥体是唯一可靠判据。
	ok, release := tray.SingleInstance(`Local\hytray-demo`)
	if !ok {
		fmt.Println("已经有一份在跑了 —— 看任务栏右下角的隐藏图标区（^ 箭头里）")
		return
	}
	defer release()

	// HYTRAY_DEMO_GLASS=1 用自绘玻璃菜单（第二种样式）。玻璃不支持子菜单，
	// 所以这个模式下换成扁平菜单 —— 也能顺便验证「含子菜单时自动降级原生」。
	glass := os.Getenv("HYTRAY_DEMO_GLASS") != ""
	// HYTRAY_DEMO_GLASS_SUB=1：玻璃样式 + 带子菜单的菜单，用来验证「自动降级原生」
	glassWithSub := os.Getenv("HYTRAY_DEMO_GLASS_SUB") != ""
	menuStyle := tray.StyleNative
	if glass || glassWithSub {
		menuStyle = tray.StyleGlass
	}

	on := true
	pick := "A"

	// 通知节流（契约 C-9）：24h 内只弹一次。时间戳落盘，进程重启也不会重弹 ——
	// 托盘由计划任务保活，「重启就重弹一条通知」是真实骚扰。
	th := tray.NewThrottle(filepath.Join(os.TempDir(), "hytray-demo.stamp"), 24*time.Hour)

	// 自证契约 C-4：菜单树 ID 唯一、可点击项有 ID。这类错肉眼看不出来。
	if err := tray.ValidateMenu(demoMenu(&on, &pick)); err != nil {
		fmt.Fprintf(os.Stderr, "菜单结构不合契约：%v\n", err)
		os.Exit(1)
	}

	// 调试钩子：HYTRAY_DEMO_AUTOCHECK=1 时启动后自动跑一遍 SetTooltip / SetIcon / ShowBalloon，
	// 用来在无人值守的情况下自证这三个 API（观感验收还是要人看）。
	// HYTRAY_DEMO_AUTOBACK=1：玻璃启动后自动切回原生（复现切换卡死用）。
	if os.Getenv("HYTRAY_DEMO_AUTOBACK") != "" {
		go func() {
			time.Sleep(1500 * time.Millisecond)
			glass = false
			tray.SetStyle(tray.StyleNative)
			tray.Logf("已自动切回原生样式")
		}()
	}

	// HYTRAY_DEMO_AUTOTOGGLE=1：启动后自动切到玻璃，用来无人值守验证 SetStyle。
	if os.Getenv("HYTRAY_DEMO_AUTOTOGGLE") != "" {
		go func() {
			time.Sleep(2 * time.Second)
			glass = true
			tray.SetStyle(tray.StyleGlass)
			tray.Logf("已自动切换到玻璃样式")
		}()
	}

	if os.Getenv("HYTRAY_DEMO_AUTOCHECK") != "" {
		go func() {
			time.Sleep(1500 * time.Millisecond)
			tray.SetTooltip("hytray demo · 自检改过 tooltip")
			if err := tray.SetIcon(icoBytes); err != nil {
				tray.Logf("自检 SetIcon 失败：%v", err)
			} else {
				tray.Logf("自检 SetIcon 成功")
			}
			tray.ShowBalloon("自检通知", "这是一条 BalloonWarn 级别通知", tray.BalloonWarn)
			tray.Logf("自检完成")
		}()
	}

	err = tray.Run(tray.Options{
		Icon:    icoBytes,
		Tooltip: "hytray demo · 右键出菜单",
		Style:   menuStyle,
		Theme:   tray.ThemeAuto, // 跟随系统明暗：深色系统下用深色菜单
		Menu: func() []tray.MenuItem {
			if glass && !glassWithSub {
				return demoMenuFlat(&on, &pick)
			}
			return demoMenu(&on, &pick)
		},
		OnMenu: func(id int) {
			tray.Logf("OnMenu 收到 id=%d", id)
			switch id {
			case idToggle:
				on = !on
				tray.SetTooltip(fmt.Sprintf("hytray demo · 开关=%v", on))
			case idA:
				pick = "A"
			case idB:
				pick = "B"
			case idDeep:
				tray.Logf("点了深层项")
			case idIcon:
				// 运行时换图标：真实项目里用来表达「运行中 / 已停止」。
				if err := tray.SetIcon(icoBytes); err != nil {
					tray.Logf("SetIcon 失败：%v", err)
				}
			case idWarn:
				tray.ShowBalloon("测试警告", "这是一条 BalloonWarn 级别的通知", tray.BalloonWarn)
			case idStyle:
				// 运行时切样式（下次右键生效）：一份 demo 就能对比两种观感。
				glass = !glass
				if glass {
					tray.SetStyle(tray.StyleGlass)
				} else {
					tray.SetStyle(tray.StyleNative)
				}
				tray.Logf("样式已切到 %v（glass=%v）", map[bool]string{true: "玻璃", false: "原生"}[glass], glass)
			case idQuit:
				tray.Logf("用户点退出")
				tray.Quit()
			}
		},
		OnOpen: func() { tray.Logf("左键：demo 没有主界面，只记一条") },
		OnReady: func() {
			if th.Allow() {
				tray.ShowBalloon("hytray demo 已就绪",
					"右键出菜单；图标找不到就点 ^ 展开", tray.BalloonInfo)
			} else {
				tray.Logf("「已就绪」通知已节流（24h 内弹过）")
			}
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "托盘退出：%v\n", err)
		os.Exit(1)
	}
	fmt.Println("已退出")
}

// demoMenu 每次右键重新求值（契约 C-1）—— 所以开关与勾选态是实时的。
func demoMenu(on *bool, pick *string) []tray.MenuItem {
	state, dot := "已停止", tray.DotIdle
	if *on {
		state, dot = "运行中 · demo", tray.DotRun
	}
	return []tray.MenuItem{
		{ID: idStatus, Label: state, Disabled: true, Dot: dot},
		{Separator: true},
		{ID: idToggle, Label: "切换开关", Checked: *on},
		{ID: idIcon, Label: "换一次图标"},
		{ID: idWarn, Label: "弹一条警告通知"},
		{ID: idStyle, Label: "切换为玻璃菜单"},
		{Label: "子菜单示例", Children: []tray.MenuItem{
			{ID: idA, Label: "选项 A", Checked: *pick == "A"},
			{ID: idB, Label: "选项 B", Checked: *pick == "B"},
			{Separator: true},
			{Label: "更深一层", Children: []tray.MenuItem{
				{ID: idDeep, Label: "深层项（验证多层递归）"},
			}},
		}},
		{Separator: true},
		{ID: idQuit, Label: "退出"},
	}
}

// demoMenuFlat 给玻璃样式用的扁平菜单：无子菜单（自绘引擎不支持）、无快捷键列。
func demoMenuFlat(on *bool, pick *string) []tray.MenuItem {
	state, dot := "已停止", tray.DotIdle
	if *on {
		state, dot = "运行中 · demo", tray.DotRun
	}
	return []tray.MenuItem{
		{ID: idStatus, Label: state, Disabled: true, Dot: dot},
		{Separator: true},
		{ID: idToggle, Label: "切换开关", Checked: *on},
		{ID: idA, Label: "选项 A", Checked: *pick == "A"},
		{ID: idB, Label: "选项 B", Checked: *pick == "B"},
		{ID: idDeep, Label: "警告状态（不可点）", Disabled: true, Dot: tray.DotWarn},
		{Separator: true},
		{ID: idWarn, Label: "弹一条警告通知"},
		{ID: idStyle, Label: "切换为原生菜单"},
		{Separator: true},
		{ID: idQuit, Label: "退出"},
	}
}
