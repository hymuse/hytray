// Package tray 系统托盘图标（Windows 实现，其它平台返回 ErrUnsupported）。
//
// 本包的 API 与行为语义由 docs/TRAY-CONTRACT.md 定义 —— 那是唯一真源，
// 实现与契约冲突时改实现，不是改契约。
//
// 为什么自己调 Win32 而不引第三方托盘库（先经自有项目验证的路线）：
// `getlantern/systray` 为了一颗图标带进 8 个间接依赖（golog / ops / bpool …），
// `fyne.io/systray` 要 cgo（macOS/Linux），两者都与本仓「单二进制、只依赖标准库 +
// x/sys」的口径不符。托盘真正需要的只有五个 Win32 面：Shell_NotifyIcon、一个隐藏窗口
// 收鼠标消息、一个弹出菜单、一张 HICON、一个消息泵 —— 手写约 500 行。
//
// ⚠️ 平台边界：本包只在 `*_windows.go` 里实现，非 Windows 走 `tray_other.go` 的桩，
// `GOOS=linux go vet ./...` 不会碰到任何 user32 符号。
package tray

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrUnsupported 当前平台没有托盘实现（非 Windows）。
var ErrUnsupported = errors.New("tray: 系统托盘目前只支持 Windows")

// Style 菜单渲染风格。
type Style int

const (
	// StyleNative 系统原生菜单（TrackPopupMenu）：跟随系统主题、零绘制代码、三平台都支持。
	StyleNative Style = iota

	// StyleGlass 自绘半透明玻璃菜单：观感优先，仅 Windows 可用
	// （macOS 的 NSMenu 与 Linux 的 DBusMenu 都由系统渲染，复刻不了 —— 见契约 §4）。
	//
	// 能力边界（与 StyleNative 的差异，见契约 §2.4）：
	//   · **不支持子菜单** —— 菜单里出现 Children 时整份降级为原生（打一条日志）
	//   · **不支持快捷键列**（Accel 被忽略）
	// 非 Windows 或本构建未含玻璃引擎时降级为 StyleNative。自绘实现经过三十轮截图
	// 验收打磨，本仓只维护不重写。
	StyleGlass
)

// DotKind 状态行左侧圆点的语义色（见契约 C-13）。
// 具体 RGB 由实现按亮/暗主题映射：暗色用亮色才跳得出来，亮色用深色才压得住白板。
type DotKind int

const (
	DotNone DotKind = iota // 不画点（普通菜单项）
	DotRun                 // 运行中：绿
	DotWarn                // 端口被占等异常：琥珀
	DotIdle                // 已停止：灰
)

// MenuItem 一条菜单项。ID 由调用方分配并保证**整棵树唯一**（0 留给「无选择」，见契约 C-4）。
type MenuItem struct {
	ID        int
	Label     string
	Separator bool
	Disabled  bool
	Checked   bool

	// Children 非空时本项变成子菜单（此时 ID 被忽略）。
	// 子菜单项同样走 TPM_RETURNCMD 返回 ID，所以整棵树里的 ID 都要唯一 ——
	// 调用方最好给不同层级分配独立区间（一级 1-99、节点 200+、地区 300+）。
	// 空子菜单不得生成：Windows 上会显示成一条点了没反应的灰条（契约 C-12）。
	Children []MenuItem

	// Dot 只在 StyleGlass 下生效（状态行左侧画语义色圆点）。
	// 原生菜单没有任何装饰能力，StyleNative 下忽略此字段。
	//
	// ★与 Checked 互斥：Dot 用于**状态行**（通常配 Disabled，表示「现在是什么状态」），
	// Checked 用于**选项**（表示「选中了哪个」）。同时设置会出现 ✓ 与 ● 叠在一起。
	// ValidateMenu 会拦，引擎也会兜底（有勾选就不画点）。
	Dot DotKind

	// Accel 快捷键提示（如 "Ctrl+Alt+S"），原生菜单按 \t 分段右对齐显示。
	//
	// ★不要在 Label 里手拼 "\t"：拼序、对齐、以及「哪些项该显示快捷键」由库统一处理，
	// 项目各拼一套就会出现同一份菜单里两种对齐。
	// 菜单项图标暂不支持，见契约 §2.4。
	Accel string
}

// Options 托盘行为。回调都在消息泵线程上执行 —— 别在里面做长阻塞操作，
// 否则菜单会卡住（启停服务那种耗时动作请自己起 goroutine，并立刻更新 tooltip，见契约 C-3）。
type Options struct {
	// Icon 托盘图标字节（.ico，含 16/32/48 多档）。
	// 由**调用方**提供：内核不内置图标，也不 embed 任何资源（契约 §2.3）。
	// 实现会按 SM_CXSMICON 选最贴近系统小图标尺寸的那一档（契约 C-8）。
	Icon []byte

	// Tooltip 悬停提示，上限 127 个 UTF-16 码元（szTip 为 128 WCHAR 含结尾），超长截断。
	Tooltip string

	// Style 菜单渲染风格。零值 = StyleNative。
	Style Style

	// V0Compat 退回 V0 回调语义（放弃键盘选中，换老式tooltip行为）。
	//
	// 默认 false = V4（键盘选中 / 中键 + 标准 tooltip 兼得）。V4 下标准 tooltip
	// 靠 NIF_SHOWTIP 开启（MSDN 明定），且 NOTIFYICONDATAW 必须是完整 976 字节、
	// NIM_ADD 时 uVersion 就要写 4 —— 这三点都满足时 Win11 可兼顾。
	// 只有目标系统上 V4+tooltip 仍不弹时，才设 true 退回 V0 作逃生舱。
	V0Compat bool

	// Theme 菜单明暗策略。零值 = ThemeAuto（跟随系统）。
	// 注意：这是**进程级**设置（Win32 没有「只让这个菜单变深」的口子），
	// 会让本进程的所有原生菜单/对话框都跟随 —— 托盘进程通常只有这一个菜单，无副作用。
	Theme Theme

	// Menu 每次右键弹出时**重新求值**（契约 C-1），因此可以按实时状态生成菜单。
	// ⚠️ 里面禁止 IO / 子进程 / 网络（契约 C-2）：每次右键都会调用它，一次往返就卡成假死。
	Menu func() []MenuItem

	// OnMenu 菜单项被点击（ID 来自 Menu 返回的项）。
	OnMenu func(id int)

	// OnOpen 左键单击或双击图标。
	OnOpen func()

	// OnReady 图标成功挂上通知区时回调一次（用于落日志、弹引导通知）。
	// 注意：首轮注册失败转退避重试时，它会在**重试成功的那一刻**才被调用。
	OnReady func()

	// OnBalloonClick 用户点击气泡通知时回调。
	//
	// 这是通知最有价值的用法：点「已在托盘」那条通知直接打开管理台，
	// 而不是让用户自己去 ^ 溢出区里找图标。平台侧依赖 V4 回调语义。
	OnBalloonClick func()
}

// Theme 菜单的明暗策略。
//
// ★为什么需要这个：Win32 原生菜单**默认永远是浅色**，即使系统是深色模式 ——
// 深色任务栏配白色菜单，观感很突兀。要让 `TrackPopupMenu` 变深，必须调 uxtheme.dll 的
// 未文档化导出 `SetPreferredAppMode`（只按序号导出，见 tray_windows.go 的 applyTheme）。
type Theme int

const (
	// ThemeAuto 跟随系统「应用模式」（读 AppsUseLightTheme）：深色系统用深色菜单。
	// 这是默认值，符合用户预期。
	ThemeAuto Theme = iota

	// ThemeLight 恒用浅色菜单（不调任何 API，即 Win32 默认行为）。
	ThemeLight

	// ThemeDark 恒用深色菜单，不管系统设置。
	ThemeDark
)

// BalloonKind 通知的级别（决定系统通知的图标与强调色）。
type BalloonKind int

const (
	BalloonNone  BalloonKind = iota // 无图标
	BalloonInfo                     // 信息（默认）
	BalloonWarn                     // 警告：端口被占、配置异常
	BalloonError                    // 错误：启动失败这类必须让人看见的
)

// Logf 排障日志钩子（默认丢弃）。托盘常由计划任务隐身启动，没有 stdout ——
// 「图标没显示 / 菜单点了没反应」这类问题，不落盘就是零证据。
var Logf = func(string, ...any) {}

// ValidateMenu 按契约的硬约束检查一棵菜单树，返回第一条问题。
//
// 检查项（都是肉眼看不出来、只有点下去才知道的那种错）：
//   - 非分隔项必须有 Label
//   - 分隔项不得带 Label / ID / Children
//   - ID 非 0，且**整棵树唯一**（0 留给「没选中」，子菜单与父层共享命名空间）
//   - 子菜单层级 ≤ 4（契约建议 ≤ 2 层：Linux DBusMenu 深层嵌套体验差）
//
// 建议在项目自己的守卫测试里跑一次，别等到线上右键才发现点不动。
func ValidateMenu(items []MenuItem) error {
	seen := map[int]string{}
	return validateMenu(items, 0, seen)
}

func validateMenu(items []MenuItem, depth int, seen map[int]string) error {
	if depth > 4 {
		return errors.New("菜单层级超过 4 层（契约建议 ≤ 2 层）")
	}
	for i, it := range items {
		where := fmt.Sprintf("第 %d 层第 %d 项", depth+1, i+1)
		if it.Separator {
			if it.Label != "" || it.ID != 0 || len(it.Children) > 0 {
				return fmt.Errorf("%s：分隔线不得带 Label / ID / Children", where)
			}
			continue
		}
		if it.Label == "" {
			return fmt.Errorf("%s：Label 为空", where)
		}
		if strings.Contains(it.Label, "\t") {
			return fmt.Errorf("%s：Label 里不要拼 \\t，快捷键请用 Accel 字段", where)
		}
		if strings.Contains(it.Accel, "\t") {
			return fmt.Errorf("%s：Accel 里不要拼 \\t", where)
		}
		if it.ID != 0 {
			if prev, ok := seen[it.ID]; ok {
				return fmt.Errorf("%s：ID %d 与%s 重复（整棵树必须唯一）", where, it.ID, prev)
			}
			seen[it.ID] = where
		} else if len(it.Children) == 0 {
			return fmt.Errorf("%s：可点击项缺少 ID（0 留给「没选中」）", where)
		}
		if err := validateMenu(it.Children, depth+1, seen); err != nil {
			return err
		}
	}
	return nil
}

// Throttle 按最小间隔放行一次（对应契约 C-9）。
//
// 为什么放进库：托盘由计划任务保活，进程被拉起一次就 OnReady 一次；各项目自己写一遍
// 「24 小时内只弹一次」，迟早写出四种行为。时间戳**落盘**而不是记内存 —— 托盘重启后
// 内存归零，只有落盘才是真的「一天一次」。
//
// 时间戳在未来（时钟回拨 / 跨机拷了 .local）或文件是垃圾内容，一律按「没弹过」处理：
// 宁可多弹一次，不要永久静默。
type Throttle struct {
	path string
	min  time.Duration
	now  func() time.Time // 可注入，便于单测
}

// NewThrottle 建一个节流器。stampPath 用来持久化上次放行时间（父目录会自动创建）。
func NewThrottle(stampPath string, min time.Duration) *Throttle {
	return &Throttle{path: stampPath, min: min, now: time.Now}
}

// Allow 报告这次是否可以放行；放行时顺手写入时间戳。
// nil 接收者返回 true —— 调用方不必为「没配节流」写分支。
func (t *Throttle) Allow() bool {
	if t == nil {
		return true
	}
	now := t.now()
	if last, ok := readStamp(t.path); ok && !last.After(now) && now.Sub(last) < t.min {
		return false
	}
	_ = os.MkdirAll(filepath.Dir(t.path), 0o755)
	_ = os.WriteFile(t.path, []byte(strconv.FormatInt(now.Unix(), 10)), 0o644)
	return true
}

func readStamp(p string) (time.Time, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

// Supported 当前平台是否支持系统托盘（Windows 为 true）。
//
// 比「调 Run 再判 ErrUnsupported」早一步：调用方常在启动流程里就要决定走托盘还是走无头 CLI。
func Supported() bool { return supported }

// Run 在当前 goroutine 跑托盘消息泵，直到 Quit 或窗口被销毁。
//
// 图标注册失败**不会**让 Run 立刻返回：资源管理器可能还没起来（开机自启场景），
// 此时转入退避重试（契约 C-10），消息泵先跑起来。
func Run(opts Options) error { return run(opts) }

// Quit 请求 Run 退出（可从任意 goroutine 调用，幂等）。
func Quit() { quit() }
