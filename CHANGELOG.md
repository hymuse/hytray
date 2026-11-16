# Changelog

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)；版本号遵循 SemVer。

## [v0.7.4] - 2026-11-16

首个公开发布版本。

- **双菜单样式**：`StyleNative`（系统原生，支持子菜单与快捷键列）与 `StyleGlass`
  （自绘液态玻璃：半透明底板、圆角、状态色点、发光选中标记、明暗跟随），
  运行时 `SetStyle` 热切；玻璃遇子菜单整份降级原生
- **V4 回调语义**（`NIM_SETVERSION`）：键盘可达（`Win+B` → Enter）、通知点击回调、
  事件带坐标；升级失败自动退回 V0 并记日志
- **图标质量**：DPI 感知声明 + 按 `SM_CXSMICON` 多档选尺寸 + `WM_DISPLAYCHANGE`
  换屏后重选档
- **可靠性**：Explorer 重启恢复（`TaskbarCreated`，先删再挂）、Shell 注册退避重试、
  单实例互斥
- **通知**：分级气泡（Info / Warn / Error）、`OnBalloonClick`、落盘式节流
  （24h 内只弹一次）
- **周边**：`ValidateMenu` 菜单结构自检、运行时 `SetIcon` / `SetTooltip`、
  非 Windows 平台返回 `ErrUnsupported`（交叉编译不碰 Win32 符号）
