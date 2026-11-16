# 用法

> hytray 是 **Go 库**，不是可执行程序——没有 `hytray.exe` 可以双击，`cmd/tray-demo` 只是例子。
> 快速开始见 [README](../README.md)，本文是完整用法与设计约束。

## 为什么是库，不是独立 exe

**核心原因：托盘的菜单是「活的」。**

菜单要实时反映服务状态（运行中 / 已停止 / 端口被谁占着）。只有**真正管着服务的那个进程**知道
这些；独立 exe 不知道，只能靠 IPC 去问——等于为了省一次 import，造一套进程间协议。

| | **库（import）** | 独立 exe + 配置 |
|---|---|---|
| 进程数 | 托盘是主程序的子命令，还是一个 exe | 再多一个 tray 进程 |
| 状态从哪来 | 同进程直接读缓存 | 必须 IPC 往返 |
| 菜单 | 每次右键现算（契约 C-1） | 静态配置，只能定时刷 |
| 图标 / 文案 | 代码里写，类型安全 | 配置文件，改了要重启 |
| 启动时序 | 天然同步 | 要处理「托盘先起 / 服务先起」 |
| 分发 | 不变 | 多发一个，版本要同步 |

## 完整示例

```go
import (
    _ "embed"
    tray "github.com/hymuse/hytray/pkg/tray"   // 包名是 tray，路径带 pkg
)

//go:embed assets/tray.ico
var icoBytes []byte

func runTray() error {
    if !tray.Supported() {
        return tray.ErrUnsupported
    }
    ok, release := tray.SingleInstance(`Local\mytool-tray`)
    if !ok {
        return fmt.Errorf("托盘已经在跑了")
    }
    defer release()

    tray.Logf = func(format string, a ...any) {          // 隐身启动没 stdout，落文件
        logToFile(fmt.Sprintf(format, a...))
    }

    // 通知节流：24h 内只弹一次（契约 C-9）。时间戳落盘，进程重启也不会重弹。
    th := tray.NewThrottle(filepath.Join(dataDir, ".balloon-stamp"), 24*time.Hour)

    return tray.Run(tray.Options{
        Icon:    icoBytes,                                // 项目自己的图标（16/24/32/48 四档）
        Tooltip: "mytool · 运行中",
        Style:   tray.StyleNative,

        Menu: func() []tray.MenuItem {                    // 每次右键重新求值（C-1），禁止 IO（C-2）
            return []tray.MenuItem{
                {ID: idStatus, Label: statusText(), Disabled: true, Dot: tray.DotRun},
                {Separator: true},
                {ID: idOpen, Label: "打开管理台", Disabled: !ready},
                {ID: idToggle, Label: toggleLabel()},
                {Label: "高级", Children: []tray.MenuItem{  // 子菜单：低频项收进来
                    {ID: idLog, Label: "打开日志"},
                    {ID: idReload, Label: "重载配置"},
                }},
            }
        },
        OnMenu: func(id int) {                            // UI 线程，禁止阻塞（C-3）
            switch id {
            case idOpen:
                openURL(panelURL())
            case idToggle:
                go toggleService()                        // 起 goroutine，并立刻改 tooltip
            }
        },
        OnOpen:  func() { openURL(panelURL()) },
        OnReady: func() {                                 // 图标真的挂上时才会调用
            if th.Allow() {
                tray.ShowBalloon("mytool 已在托盘", "右键出菜单", tray.BalloonInfo)
            }
        },
    })
}
```

运行中还能换图标、样式与 tooltip：

```go
tray.SetStyle(tray.StyleGlass)  // 下次右键生效；用户偏好可以存配置，启动时传给 Options.Style
```

```go
tray.SetIcon(icoStopped)                     // 换成「已停止」图标（内部重选 DPI 档、释放旧 HICON）
tray.SetTooltip("mytool · 已停止")           // 任意 goroutine 可调
tray.ShowBalloon("启动失败", err.Error(), tray.BalloonError)
```

**上线前跑一次结构校验**（ID 重复这类错肉眼看不出来）：

```go
if err := tray.ValidateMenu(myMenu()); err != nil {
    return fmt.Errorf("菜单结构不合契约：%w", err)
}
```

## 菜单设计建议

hytray **不预设任何菜单项**。`Options.Menu` 是唯一来源，返回空就右键无菜单——菜单必须反映
业务实时状态，而内核不认识这些。

推荐结构：

```
运行中 · :8080 · dev          ← 状态行，Disabled，一眼看出在不在跑
─────────────────────
打开管理台
停止服务                     ← 危险动作，在错误状态下要 Disabled
─────────────────────
高级 ›                        ← 低频项收进子菜单（一级 ≤ 12 项，见 C-12）
─────────────────────
退出                          ← 服务型项目可省略（C-11）
```

四条规矩：① 首项永远是状态行；② 分隔线按「状态 / 主操作 / 次要 / 退出」分组；
③ 一级不超过 12 项；④ 文案自解释，关键信息不许只放 tooltip（Linux 上没有 tooltip）。

## 接入清单

### 必做 —— 缺一个都上不了线

| # | 事项 | 落点 / 代码 |
|---|---|---|
| 1 | 加依赖 | `go get github.com/hymuse/hytray@v0.7.4` |
| 2 | 准备**四档**图标 16/24/32/48 | `assets/tray.ico` + `//go:embed`（三档在 150% DPI 下会拉伸，见 C-8） |
| 3 | 托盘入口 | `cmd/<你的应用>/tray.go`，见下方骨架 |
| 4 | 单实例 | `tray.SingleInstance(`Local\<你的应用>-tray`)` |
| 5 | 日志钩子 | `tray.Logf = 写文件` —— 托盘常被服务/计划任务隐身拉起，没有 stdout |
| 6 | 菜单结构守卫 | 项目测试里跑 `tray.ValidateMenu(buildMenu())` |
| 7 | 自启指向 | 开机自启动作改成 `<你的应用>.exe tray` |

```go
// cmd/<你的应用>/tray.go 的最小骨架
func runTray() error {
    if !tray.Supported() {
        return tray.ErrUnsupported
    }
    ok, release := tray.SingleInstance(`Local\myproj-tray`)
    if !ok {
        return nil // 已经有一份在跑，安静退出
    }
    defer release()

    tray.Logf = func(f string, a ...any) { logToFile(fmt.Sprintf(f, a...)) }
    th := tray.NewThrottle(filepath.Join(dataDir, ".balloon-stamp"), 24*time.Hour)

    return tray.Run(tray.Options{
        Icon:    icoBytes,
        Tooltip: tooltipText(),        // ≤127 码元
        Theme:   tray.ThemeAuto,       // 深色系统下菜单不发白
        Menu:    buildMenu,            // 每次右键重新求值
        OnMenu:  handleMenu,
        OnOpen:  func() { openURL(panelURL()) },
        OnReady: func() {
            if th.Allow() {
                tray.ShowBalloon("已就绪", "右键出菜单", tray.BalloonInfo)
            }
        },
        OnBalloonClick: func() { openURL(panelURL()) },
    })
}
```

### 建议 —— 不影响能跑，但影响体验

| # | 事项 | 为什么值得做 |
|---|---|---|
| 8 | `Theme` | 不设的话深色系统下菜单是白的（Win32 默认行为） |
| 9 | `Throttle` | 托盘被保活反复拉起时，通知一天只弹一次 |
| 10 | `SetIcon` | 运行中 / 已停止换图标，比改文字直观 |
| 11 | `OnBalloonClick` | 点通知直接进管理台，而不是让用户去 `^` 里找图标 |
| 12 | 危险项的 `Disabled` | 端口被占时禁用「启动」，比点了报错友好 |

### 验收 —— 发版前逐条过

完整清单见 [TRAY-CONTRACT.md](TRAY-CONTRACT.md) §5，重点三条：

```
□ 150% DPI 下图标不糊（C-8）
□ Win+B 选中图标按 Enter 能弹菜单（C-16）
□ 系统深色模式下菜单是深色的（C-17，Win32 默认不会变）
```

## 两个常见误解

**托盘不是独立程序。** 推荐做成主程序的一个子命令（如 `<你的应用>.exe tray`），与 CLI 共享
同一套状态读取代码——否则「托盘显示的地址」和「CLI 连的地址」可能对不上。

**托盘不是 GUI 框架。** 它不提供输入框、列表、进度条。需要这些就开管理台——
契约 §1.2 写明了「需要输入参数 / 需要看大量信息」的都不该放托盘。
