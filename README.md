# hytray

**Windows 系统托盘 Go 库：让没有窗口的常驻进程「看得见、点得到、停得掉」。**

零业务依赖（标准库 + `golang.org/x/sys/windows`），不占端口、不含任何业务配置，图标由调用方注入。
提供两套菜单样式——系统原生与自绘玻璃——运行时可切换。

```bash
go get github.com/hymuse/hytray
```

## 快速开始

```go
import tray "github.com/hymuse/hytray/pkg/tray"

//go:embed tray.ico
var icon []byte

func main() {
    err := tray.Run(tray.Options{
        Icon:    icon,
        Tooltip: "我的服务 · 右键出菜单",
        Style:   tray.StyleNative, // 或 StyleGlass（自绘玻璃，仅 Windows）
        Menu: func() []tray.MenuItem {
            return []tray.MenuItem{
                {ID: 1, Label: "打开管理台"},
                {ID: 2, Label: "运行中", Disabled: true, Dot: tray.DotRun},
                {Separator: true},
                {ID: 3, Label: "退出"},
            }
        },
        OnMenu: func(id int) { /* 处理点击 */ },
    })
    if err != nil { /* ... */ }
}
```

`go run ./cmd/tray-demo` 可直接跑最小示例（图标 + 菜单 + 子菜单 + 自检钩子）。

## 两套菜单样式

| | `StyleNative`（默认） | `StyleGlass` |
|---|---|---|
| 绘制 | `TrackPopupMenuEx`，系统画 | 自绘半透明玻璃窗 |
| 观感 | 跟随系统主题 | 毛玻璃、圆角、状态色点与发光选中标记 |
| 子菜单 | 支持 | 不支持（遇到 `Children` 整份降级原生） |
| 快捷键列 | 支持 | 忽略 |
| 无障碍 | 系统原生支持 | 自绘无 UIA——**投政企/等保场景请用原生** |
| 平台 | Windows | 仅 Windows；不可用时自动降级原生 |

零值 `Style` = 原生。`SetStyle` 可运行时热切。`Dot` 字段只在玻璃样式下生效。

## 能力

- 图标按 `SM_CXSMICON` 多档选尺寸；换屏 / 改缩放后重选（`WM_DISPLAYCHANGE`）
- Explorer 重启后自动重挂图标（`TaskbarCreated`）
- Shell 通知退避重试（注册失败不死循环）
- 系统明暗跟随（`WM_SETTINGCHANGE` / `WM_THEMECHANGED`）
- 通知节流、分级气泡通知、通知点击回调
- 单实例互斥、V4 回调语义（键盘可达 / 中键 / 悬停）
- `ValidateMenu` 自检菜单树结构
- 非 Windows 平台返回 `ErrUnsupported`（build tag 隔离，交叉编译不碰 Win32 符号）

完整能力清单与行为语义见 [`docs/TRAY-CONTRACT.md`](docs/TRAY-CONTRACT.md)，API 覆盖表见
[`docs/API-COVERAGE.md`](docs/API-COVERAGE.md)，用法详见 [`docs/USAGE.md`](docs/USAGE.md)。

## 设计取舍

- **不引第三方托盘库**：`getlantern/systray` 为一颗图标带进 8 个间接依赖；`fyne.io/systray`
  要 cgo，破坏单二进制与交叉编译。直接调 Win32 更轻、更可控。
- **内核不认业务**：端口、服务名、任务名、URL 都不进库；图标以 `[]byte` 传入，库不 embed 资源。
- **不做跨平台**：macOS 的 `NSMenu`、Linux 的 DBusMenu 由系统渲染，自绘复刻不了；库服务的对象
  本身强绑 Windows。
- **玻璃引擎只在 Windows**：自绘部分来自三十轮截图验收的实现，本仓只维护不重写。

## 目录

```
pkg/tray/          库代码（唯一 import 入口）
cmd/tray-demo/     可运行示例
docs/              契约 / 用法 / API 覆盖表
```

## 要求

- Go 1.27.3+
- Windows（原生菜单在其它平台返回 `ErrUnsupported`）

## License

MIT
