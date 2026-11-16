//go:build !windows

package tray

// Supported 本平台不支持托盘。
const supported = false

// 非 Windows 全是桩：本包只在 Windows 上实现（契约 §4 说明了原因 —— macOS 的 NSMenu
// 与 Linux 的 DBusMenu 都由系统渲染，自绘复刻不了；而四个消费者被管理的服务本身
// 强绑 Windows）。
//
// 桩的意义是让 `GOOS=linux go vet ./...` / `GOOS=darwin go build` 通过，
// 调用方不必为平台写 build tag。

func run(Options) error { return ErrUnsupported }

func quit() {}

// SetTooltip 非 Windows 空实现。
func SetTooltip(string) {}

// SetIcon 非 Windows 返回 ErrUnsupported。
func SetIcon([]byte) error { return ErrUnsupported }

// Balloon 非 Windows 空实现。
func Balloon(string, string) {}

// ShowBalloon 非 Windows 空实现。
func ShowBalloon(string, string, BalloonKind) {}

// SingleInstance 非 Windows 返回 (true, noop)：放行，让调用方走到 Run 那里拿到
// ErrUnsupported —— 比在这里假装「已经有一份在跑」更诚实。
func SingleInstance(string) (bool, func()) { return true, func() {} }

// SetStyle 非 Windows 空实现。
func SetStyle(Style) {}
