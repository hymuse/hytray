# 系统托盘 API 能力对照（Windows）

> 数据源：`Shell_NotifyIcon` + `NOTIFYICONDATA`（含 V4 语义）、`Shell_NotifyIconGetRect`、  
> `RegisterWindowMessage("TaskbarCreated")`。对照对象：hytray v0.7.4。
>
> 这份文档的用途有两个：① 项目评估「这个需求 hytray 能不能做」；② 决定新能力要不要进库时，  
> 先看这里有没有、为什么没有。

> [!IMPORTANT]
> **Win11 没有新增托盘 API。** `Shell_NotifyIcon` 还是 1995 年那套接口，Win11 只换了视觉
> （Fluent 菜单、通知中心）。所以「用满 Win11 的托盘能力」这个说法本身不成立 —— 能力面与 Win10
> 完全相同，差别只在观感和图标是否被折进 `^`（见 §7）。

## 1. 图标管理

| 能力           | Windows API                             | hytray      | 说明                                     |
| ------------ | --------------------------------------- | ----------- | -------------------------------------- |
| 添加 / 修改 / 删除 | `NIM_ADD` / `NIM_MODIFY` / `NIM_DELETE` | ✅           |                                        |
| 运行时换图        | 改 `hIcon` 后 `NIM_MODIFY`                | ✅ `SetIcon` | 内部重选 DPI 档，旧 HICON 自动释放                |
| 按系统小图标尺寸选档   | `GetSystemMetrics(SM_CXSMICON)`         | ✅           | 需配合 DPI 感知（见 §4）                       |
| 换屏 / 改缩放后重选档 | `WM_DISPLAYCHANGE`                      | ✅ C-14      |                                        |
| **一个进程多个图标** | 同 hWnd 不同 `uID`                         | ❌           | 固定 `uID=1`。真实需求未出现（一个服务一颗图标）           |
| **临时隐藏图标**   | `NIS_HIDDEN`（`dwState`）                 | ❌           | 有需求时加 `SetHidden(bool)`；注意隐藏 ≠ 删除，位置会变 |
| 共享图标句柄       | `NIS_SHAREDICON`                        | ❌           | 仅老程序兼容用，新代码无意义                         |

## 2. 交互

| 能力        | 事件                    | hytray     | 说明                                 |
| --------- | --------------------- | ---------- | ---------------------------------- |
| 左键单击      | `NIN_SELECT`（V4）      | ✅ `OnOpen` | V0 下是 `WM_LBUTTONUP`，两种都接          |
| 左键双击      | `WM_LBUTTONDBLCLK`    | ✅ `OnOpen` | 与单击同一个回调                           |
| 右键        | `WM_CONTEXTMENU`（V4）  | ✅ 弹菜单      | V0 下是 `WM_RBUTTONUP`，两种都接          |
| **键盘选中**  | `NIN_KEYSELECT`（仅 V4） | ✅ 弹菜单      | `Win+B` → 方向键 → `Enter`。v0.5.0 起支持 |
| 中键        | `WM_MBUTTONUP`        | ⚠️ 收到但无回调  | 打日志；有项目要「中键快速动作」时加 `OnMiddleClick` |
| 悬停移入 / 移出 | `WM_MOUSEMOVE`（V4）    | ❌          | 需要「悬停即展开」这类交互时才做                   |
| Tooltip   | `NIF_TIP` / `szTip`   | ✅          | 上限 127 个 UTF-16 码元，超长截断            |
| 菜单内键盘导航   | `TrackPopupMenu` 内建   | ✅          | ↑↓ / Esc / 首字母跳转，系统实现              |

## 3. 通知（气泡）

| 能力               | API                                            | hytray             | 说明                                                    |
| ---------------- | ---------------------------------------------- | ------------------ | ----------------------------------------------------- |
| 气泡通知             | `NIF_INFO` + `szInfo` / `szInfoTitle`          | ✅ `ShowBalloon`    | 标题 63 / 正文 255 码元（含结尾，安全上限）                           |
| 级别图标             | `dwInfoFlags` = `NIIF_INFO/WARNING/ERROR/NONE` | ✅ `BalloonKind`    | 枚举值与 Win32 一一对应                                       |
| **点击通知的回调**      | `NIN_BALLOONUSERCLICK`（V4）                     | ✅ `OnBalloonClick` | v0.5.0 起支持。最典型用法：点「已在托盘」直接开管理台                        |
| 通知显示 / 隐藏 / 超时事件 | `NIN_BALLOONSHOW` / `HIDE` / `TIMEOUT`         | ❌                  | 用到时再加                                                 |
| 关闭提示音            | `NIIF_NOSOUND`                                 | ❌                  | **Win10+ 已失效**：走通知中心后提示音由系统设置控制                       |
| 大图标              | `NIIF_LARGE_ICON`                              | ❌                  | **Win10+ 已失效**：通知中心不用 `hIcon`，图标由应用身份（AUMID）决定，见 §3.1 |



> ⚠️ Win10+ 的通知走**通知中心**，`uTimeout` 由系统接管，「气泡停留几秒」不可控。

### 3.1 通知在通知中心里显示成什么样（应用身份 AUMID）

Win10/11 的通知由**通知中心**渲染，**`hIcon` 完全不参与** —— 图标和应用名由「应用身份」（AppUserModelID）决定。

实测（2026-11-16，读 `%LOCALAPPDATA%/Microsoft/Windows/Notifications/wpndatabase.db` 的  
`NotificationHandler` 表）：hytray demo 发的通知**没有注册任何 handler** —— 系统里只有 AAD、  
ScreenClipping 这类系统应用的 AUMID。也就是说它只能以「exe 文件名 + 通用图标」的样子出现。

要显示成「你的应用名」+ 自己的图标，需要两件事**同时**成立：

| 步骤           | 谁做     | 内容                                                       |
| ------------ | ------ | -------------------------------------------------------- |
| ① 进程声明 AUMID | 代码（一行） | `SetCurrentProcessExplicitAppUserModelID("MyApp.Tray")` |
| ② 开始菜单快捷方式   | 安装器    | 快捷方式的 AppUserModelID 属性 = 同一字符串，且快捷方式带图标                 |

只有 ① 没有 ②：仍显示 exe 名（但通知可以归组）。绿色版、计划任务拉起这类没有开始菜单快捷方式的
应用，通知就只能以 exe 名出现——这是系统行为，不是库能解决的。

## 4. 周边 API

| 能力            | API                                       | hytray             | 说明                                                                   |
| ------------- | ----------------------------------------- | ------------------ | -------------------------------------------------------------------- |
| 图标在屏幕上的矩形     | `Shell_NotifyIconGetRect`                 | ✅                  | 键盘触发菜单时用来贴着图标定位；图标被折进 `^` 时可能失败，退回光标位置                               |
| Explorer 重启恢复 | `RegisterWindowMessage("TaskbarCreated")` | ✅ C-7              | 先 `NIM_DELETE` 再 `NIM_ADD`                                           |
| 回调语义版本        | `NIM_SETVERSION`                          | ✅ V4               | 失败自动退回 V0 并记日志                                                       |
| 注册表声明 DPI 感知  | `SetProcessDpiAwarenessContext`           | ✅                  | 不声明会让 §1 的选档失效                                                       |
| 单实例           | 命名互斥体（不是托盘 API）                           | ✅ `SingleInstance` |                                                                      |
| **强制图标常显**    | ——                                        | ——                 | **系统没有这个 API**：Win10/11 是否折进 `^` 由用户的「任务栏设置」决定，程序无法强制。这是托盘最常见的用户困惑来源 |

### 4.1 深色菜单（未文档化 API）

Win32 的 `TrackPopupMenu` **不会跟随深色模式**，默认恒为浅色。系统深色模式下任务栏是深的、
菜单是白的 —— 这不是设置错了，是平台行为。

唯一的开关是 `uxtheme.dll` 的两个**未文档化导出**（只有序号，没有名字）：

| 序号 | 函数 | 参数 |
|---|---|---|
| 135 | `SetPreferredAppMode` | 0=Default 1=AllowDark **2=ForceDark** 3=ForceLight |
| 136 | `FlushMenuThemes` | 无 |

三个约束：① 必须在创建任何窗口**之前**调用；② 是**进程级**设置；③ 序号可能随 Windows 版本变
（Emacs 2026-08 的 `w32fns.c` 补丁也是按这个序号调的，社区目前一致）。

**Go 的坑**：`windows.NewProc("#135")` 不行 —— `GetProcAddress` 不接受 `#N` 字符串写法，
必须把序号当指针值传（C 的 `MAKEINTRESOURCE(135)`）：

```go
addr, _, _ := pGetProcAddress.Call(uintptr(hUxtheme), 135) // 135 < 65536 ⇒ 按序号查
syscall.SyscallN(addr, 2)                                  // ForceDark
```

判断该不该变深：读 `AppsUseLightTheme`（0 = 深色）。**不要**读 `SystemUsesLightTheme` ——
后者管任务栏，两者可以不一致（本机实测就出现过两个键同为 0 但菜单仍是白的场景）。

## 5. 回调语义：V0 vs V4

`NIM_SETVERSION` 决定 `uCallbackMessage` 的 `lParam` 怎么读。hytray 用 V4，失败退回 V0。

|            | V0             | V4                              |
| ---------- | -------------- | ------------------------------- |
| `lParam`   | 就是鼠标消息         | **低 16 位 = 事件码，高 16 位 = 图标 ID** |
| 左键单击       | `WM_LBUTTONUP` | `NIN_SELECT`(0x400)             |
| 右键         | `WM_RBUTTONUP` | `WM_CONTEXTMENU`(0x7B)          |
| 键盘选中       | **不发送**        | `NIN_KEYSELECT`(0x401)          |
| 通知点击       | 不发送            | `NIN_BALLOONUSERCLICK`(0x405)   |
| 悬停         | 不发送            | `WM_MOUSEMOVE`                  |
| `szTip` 上限 | 64 码元          | **128**（127 可用）                 |
| 坐标         | `GetCursorPos` | 事件里带（`wParam` 低/高 16 位是 x/y）    |

**为什么必须升 V4**：不升就只有鼠标能用 —— 键盘用户在 `Win+B` 里选中图标按 Enter 是**完全没反应**的，  
而这条路径在鼠标用户身上永远暴露不出来。

## 6. 未覆盖清单（每条写明为什么、什么时候重开）

| 能力                                 | 为什么不做                                      | 重开条件               |
| ---------------------------------- | ------------------------------------------ | ------------------ |
| 多图标（多 `uID`）                       | 真实需求未出现（一个服务一颗图标）                          | 出现「一个进程要管理多个对象」的需求 |
| `NIS_HIDDEN`                       | 隐藏后图标位置会变，用户更难找；有需求时更该用 Tooltip 说明         | 出现「临时静默但保留状态」的场景   |
| 中键回调                               | 无消费者                                       | 某项目要「中键一键动作」       |
| 悬停事件                               | 无消费者，且悬停频繁触发、易误操作                          | 出现「悬停展开面板」类交互      |
| 通知的超时 / 显示 / 隐藏事件                  | 只在做「通知点击后跳转 + 未点击则重试」这类编排时才需要              | 出现通知编排需求           |
| `NIIF_NOSOUND` / `NIIF_LARGE_ICON` | **不是「没做」，是 Win10+ 上已失效**（通知中心接管了图标与声音）     | 不重开                |
| 菜单项图标                              | 见契约 §2.4：手搓 `MIIM_BITMAP` 成本 ≈ 改布局参数，且零消费者 | 两个项目同时要            |
| 菜单动画                               | `TrackPopupMenu` 由系统绘制，库无法干预               | 玻璃引擎进库时            |

## 7. 平台行为差异（同代码不同表现）

| 项      | Win10      | Win11                       |
| ------ | ---------- | --------------------------- |
| 菜单外观   | 经典（方角、无阴影） | Fluent（圆角、轻阴影）              |
| 图标默认位置 | 通知区直接可见    | **折进 `^` 溢出区**（新注册的图标默认进溢出） |
| 通知     | 气泡贴图标      | 通知中心（右上角浮出）                 |
| 深色菜单   | 跟随「应用模式」   | 同左                          |

**结论**：同一份 `StyleNative` 代码在 Win10 和 Win11 上观感不同，这是**系统行为不是缺陷**；  
需要一致观感只能自绘（=玻璃引擎，见契约 §2.4）。
