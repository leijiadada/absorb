package desktop

import (
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"absorb-agent/internal/kind"
)

const (
	_WS_EX_TOOLWINDOW  = 0x00000080
	_SW_HIDE           = 0
	_SW_SHOWNOACTIVATE = 4 // 干净显示：无恢复动画、不尝试激活
	_SW_RESTORE        = 9 // 仅最小化态需要（还原窗口）

	_SWP_NOSIZE       = 0x0001
	_SWP_NOMOVE       = 0x0002
	_SWP_NOZORDER     = 0x0004
	_SWP_FRAMECHANGED = 0x0020
	_SWP_NOACTIVATE   = 0x0010

	// HWND_TOP：z 序抬到普通窗口带最顶（不进 topmost 带，避免双跳闪动）
	_hwndTop = uintptr(0)
)

// gwlExStyle GWL_EXSTYLE(-20)：按位取反 ^19 = uintptr 两补码形式的 -20
// （负数不能直接转 uintptr/uint32，编译期常量检查会拒绝）。
const gwlExStyle = ^uintptr(19)

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procEnumWindows         = user32.NewProc("EnumWindows")
	procGetWindowThreadProc = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procGetWindowLongPtrW   = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW   = user32.NewProc("SetWindowLongPtrW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procIsIconic            = user32.NewProc("IsIconic")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

// HideToTray 主窗口隐藏到托盘：SW_HIDE 前临时挂 WS_EX_TOOLWINDOW，任务栏按钮
// 随隐藏同步消失（wails 无边框窗口固定带 WS_EX_APPWINDOW，纯 SW_HIDE 会残留
// 一个点击无反应的死按钮）。原样式记入 savedExStyle，由 ShowMainWindow 还原。
func HideToTray() {
	hwnd := findMainHWND()
	if hwnd == 0 {
		return
	}
	styleMu.Lock()
	defer styleMu.Unlock()
	// 只在首次隐藏时记下原始样式。关窗按钮与 OnBeforeClose 都可能进这里，
	// 第二次再读到的已是带 TOOLWINDOW 的值，写回去后任务栏按钮不再出现。
	if !styleSaved {
		savedExStyle = getExStyle(hwnd)
		styleSaved = true
	}
	setExStyle(hwnd, savedExStyle|_WS_EX_TOOLWINDOW)
	showWindow(hwnd, _SW_HIDE)
}

// ShowMainWindow 恢复主窗口（托盘「显示主窗口」/ 单实例唤醒共用）。
// 次序关键：任务栏在窗口转为可见的瞬间评估按钮归属——必须先在隐藏态清掉
// TOOLWINDOW，再显示；显示后才改样式任务栏不会重新评估（按钮回不来）。
// frameChanged 通知框架刷新，兜底任务栏状态。
func ShowMainWindow() {
	hwnd := findMainHWND()
	if hwnd == 0 {
		return
	}
	styleMu.Lock()
	defer styleMu.Unlock()
	if styleSaved {
		setExStyle(hwnd, savedExStyle)
		styleSaved = false
	}
	// 隐藏态用无激活显示避免恢复动画；最小化态才需要走还原路径
	if isIconic(hwnd) == 0 {
		showWindow(hwnd, _SW_SHOWNOACTIVATE)
	} else {
		showWindow(hwnd, _SW_RESTORE)
	}
	frameChanged(hwnd)
	raiseToTop(hwnd)
	procSetForegroundWindow.Call(hwnd)
}

// savedExStyle / styleSaved：HideToTray 记下的原始扩展样式。
// styleSaved 与数值分开，避免把「样式本身为 0」和「还没隐藏过」混成一个哨兵。
// 托盘回调与 Wails 绑定不在同一 goroutine，读写都走 styleMu。
var (
	styleMu      sync.Mutex
	savedExStyle uintptr
	styleSaved   bool
)

// enumCB 只注册一次。syscall.NewCallback 的槽位进程内不释放，
// 每次查找都对新闭包注册，反复隐藏/显示会在约 2000 次后直接 throw。
var (
	enumMu    sync.Mutex
	enumPID   uint32
	enumFound uintptr
	enumCB    = syscall.NewCallback(onEnumWindow)
)

// frameChanged 通知窗口样式已变（任务栏按钮归属据此刷新）。
func frameChanged(hwnd uintptr) {
	procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0,
		uintptr(_SWP_NOMOVE|_SWP_NOSIZE|_SWP_NOZORDER|_SWP_FRAMECHANGED))
}

// raiseToTop 抬升 z 序。托盘点击时输入焦点在 shell，本进程没有前台权，
// Windows 前台锁只锁"激活"不锁 z 序——SWP_NOACTIVATE 的 HWND_TOP 单次调用
// 即可抬到普通窗口带最顶（不做 TOPMOST 双跳，避免闪动）。
// 末尾 SetForegroundWindow 补键盘焦点，被前台锁拒绝也无碍（窗口已可见可点）。
func raiseToTop(hwnd uintptr) {
	procSetWindowPos.Call(hwnd, _hwndTop, 0, 0, 0, 0,
		uintptr(_SWP_NOMOVE|_SWP_NOSIZE|_SWP_NOACTIVATE))
}

// findMainHWND 枚举本进程顶层窗口，按标题定位主窗口（句柄随窗口重建而变，每次现查）。
func findMainHWND() uintptr {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumFound = 0
	enumPID = uint32(os.Getpid())
	procEnumWindows.Call(enumCB, 0)
	return enumFound
}

// onEnumWindow 是 EnumWindows 的回调，由 findMainHWND 在持有 enumMu 时同步调用。
func onEnumWindow(hwnd, _ uintptr) uintptr {
	var wpid uint32
	procGetWindowThreadProc.Call(hwnd, uintptr(unsafe.Pointer(&wpid)))
	if wpid != enumPID {
		return 1
	}
	if windowTitle(hwnd) == kind.MainWindowTitle {
		enumFound = hwnd
		return 0
	}
	return 1
}

func windowTitle(hwnd uintptr) string {
	buf := make([]uint16, 128)
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return strings.TrimSpace(syscall.UTF16ToString(buf[:n]))
}

func getExStyle(hwnd uintptr) uintptr {
	v, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlExStyle)
	return v
}

func setExStyle(hwnd uintptr, style uintptr) {
	procSetWindowLongPtrW.Call(hwnd, gwlExStyle, style)
}

func showWindow(hwnd uintptr, cmd int) {
	procShowWindow.Call(hwnd, uintptr(cmd))
}

func isIconic(hwnd uintptr) int {
	ret, _, _ := procIsIconic.Call(hwnd)
	return int(ret)
}
