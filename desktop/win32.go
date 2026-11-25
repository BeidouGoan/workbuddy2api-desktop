package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var (
	shell32                   = syscall.NewLazyDLL("shell32.dll")
	procCallWindowProcW       = user32.NewProc("CallWindowProcW")
	procSetWindowLongPtrW     = user32.NewProc("SetWindowLongPtrW")
	procGetWindowLongPtrW     = user32.NewProc("GetWindowLongPtrW")
	procSendMessageW          = user32.NewProc("SendMessageW")
	procOpenClipboard         = user32.NewProc("OpenClipboard")
	procCloseClipboard        = user32.NewProc("CloseClipboard")
	procEmptyClipboard        = user32.NewProc("EmptyClipboard")
	procSetClipboardData      = user32.NewProc("SetClipboardData")
	procGlobalAlloc           = kernel32.NewProc("GlobalAlloc")
	procGlobalLock            = kernel32.NewProc("GlobalLock")
	procGlobalUnlock          = kernel32.NewProc("GlobalUnlock")
	procGlobalFree            = kernel32.NewProc("GlobalFree")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procShowWindow            = user32.NewProc("ShowWindow")
	procIsIconic              = user32.NewProc("IsIconic")
	procLoadIconW             = user32.NewProc("LoadIconW")
	procGetSystemMetrics      = user32.NewProc("GetSystemMetrics")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procGetCursorPos          = user32.NewProc("GetCursorPos")
	procCreatePopupMenu       = user32.NewProc("CreatePopupMenu")
	procAppendMenuW           = user32.NewProc("AppendMenuW")
	procTrackPopupMenu        = user32.NewProc("TrackPopupMenu")
	procDestroyMenu           = user32.NewProc("DestroyMenu")
	procRegisterClassExW      = user32.NewProc("RegisterClassExW")
	procCreateWindowExW       = user32.NewProc("CreateWindowExW")
	procDefWindowProcW        = user32.NewProc("DefWindowProcW")
	procGetMessageW           = user32.NewProc("GetMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessageW      = user32.NewProc("DispatchMessageW")
	procDestroyWindow         = user32.NewProc("DestroyWindow")
	procPostMessageW          = user32.NewProc("PostMessageW")
	procPostQuitMessage       = user32.NewProc("PostQuitMessage")
	procSetWindowPos          = user32.NewProc("SetWindowPos")
	procMonitorFromWindow     = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW       = user32.NewProc("GetMonitorInfoW")
	procShellNotifyIconW      = shell32.NewProc("Shell_NotifyIconW")
	procShellExecuteW         = shell32.NewProc("ShellExecuteW")
	procGetModuleHandleW      = kernel32.NewProc("GetModuleHandleW")
	procCreateSolidBrush      = gdi32.NewProc("CreateSolidBrush")
	procSetClassLongPtrW      = user32.NewProc("SetClassLongPtrW")
	procInvalidateRect        = user32.NewProc("InvalidateRect")
	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
)

var (
	gdi32  = syscall.NewLazyDLL("gdi32.dll")
	dwmapi = syscall.NewLazyDLL("dwmapi.dll")
)

// applyDarkChrome 让标题栏跟随深色界面，并把窗口类背景画刷换成同色，
// 避免 WebView2 首帧渲染前闪白。
func applyDarkChrome(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	on := int32(1)
	// DWMWA_USE_IMMERSIVE_DARK_MODE：20 是 Win10 20H1+，19 是更早版本。
	if r, _, _ := procDwmSetWindowAttribute.Call(hwnd, 20, uintptr(unsafe.Pointer(&on)), 4); r != 0 {
		procDwmSetWindowAttribute.Call(hwnd, 19, uintptr(unsafe.Pointer(&on)), 4)
	}
	// DWMWA_BORDER_COLOR(34)：Win11 下把窗口描边也换成深色，避免浅色细边割裂界面。
	border := uint32(0x000E0B08)
	procDwmSetWindowAttribute.Call(hwnd, 34, uintptr(unsafe.Pointer(&border)), 4)
	// COLORREF 0x00BBGGRR（对应 #080B0E）
	if brush, _, _ := procCreateSolidBrush.Call(0x000E0B08); brush != 0 {
		procSetClassLongPtrW.Call(hwnd, ^uintptr(9), brush) // GCLP_HBRBACKGROUND = -10
		procInvalidateRect.Call(hwnd, 0, 1)
	}
}

const (
	wmDestroy  = 0x0002
	wmClose    = 0x0010
	wmSize     = 0x0005
	wmCommand  = 0x0111
	wmSetIcon  = 0x0080
	wmApp      = 0x8000
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205
	wmNCLButtonDown = 0x00A0
	wmGetMinMaxInfo = 0x0024
	wmQueryEndSession = 0x0011

	sizeMinimized = 1

	swHide    = 0
	swRestore = 9
	swShow    = 5

	gwlpWndProc = ^uintptr(3) // GWLP_WNDPROC = -4

	iconSmall = 0
	iconBig   = 1

	cfUnicodeText = 13
	gmemMoveable  = 0x0042
)

// 主窗口子类化：拦截关闭与最小化，交给托盘接管。
var (
	oldWndProc uintptr
	forceClose atomic.Bool
)

func showWindow(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	procShowWindow.Call(hwnd, swRestore)
	procShowWindow.Call(hwnd, swShow)
	procSetForegroundWindow.Call(hwnd)
}

func hideWindow(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	procShowWindow.Call(hwnd, swHide)
}

func isWindowMinimized(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	r, _, _ := procIsIconic.Call(hwnd)
	return r != 0
}

// installMainWindowHooks 子类化主窗口，onHide 在用户点关闭/最小化时回调。
// 必须在创建窗口的线程（webview.Run 之前）调用。
func installMainWindowHooks(hwnd uintptr, logDir string, onHide func()) {
	if hwnd == 0 {
		return
	}
	// 只读取旧窗口过程，绝不能用 SetWindowLongPtr(..., 0) 去「查」——
	// 那会把 WndProc 真的置空，窗口随即失效。
	prev, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlpWndProc)
	oldWndProc = prev
	appendLog(logDir, "desktop.log", fmt.Sprintf("子类化窗口 hwnd=%#x oldWndProc=%#x", hwnd, prev))

	cb := syscall.NewCallback(func(h, msg, wp, lp uintptr) uintptr {
		switch msg {
		case wmClose:
			appendLog(logDir, "desktop.log", "收到 WM_CLOSE")
			if forceClose.Load() {
				break
			}
			onHide()
			return 0
		case wmSize:
			if wp == sizeMinimized && !forceClose.Load() {
				appendLog(logDir, "desktop.log", "收到 WM_SIZE(SIZE_MINIMIZED)")
				onHide()
				return 0
			}
		case wmDestroy:
			appendLog(logDir, "desktop.log", "收到 WM_DESTROY")
		case wmQueryEndSession:
			appendLog(logDir, "desktop.log", "收到 WM_QUERYENDSESSION")
		}
		r, _, _ := procCallWindowProcW.Call(oldWndProc, h, msg, wp, lp)
		return r
	})
	procSetWindowLongPtrW.Call(hwnd, gwlpWndProc, cb)
}

// destroyMainWindow 真正关闭窗口（退出流程用）。
func destroyMainWindow(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	forceClose.Store(true)
	procDestroyWindow.Call(hwnd)
}

// setClipboardText 把文本写进 Windows 剪贴板。
func setClipboardText(text string) error {
	utf16, err := syscall.UTF16FromString(text)
	if err != nil {
		return err
	}
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		return errors.New("打开剪贴板失败")
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()

	size := uintptr(len(utf16) * 2)
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return errors.New("分配剪贴板内存失败")
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		procGlobalFree.Call(h)
		return errors.New("锁定剪贴板内存失败")
	}
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), len(utf16)), utf16) //nolint:govet // GlobalLock 返回非 Go 指针，此处转换安全
	procGlobalUnlock.Call(h)
	if r, _, _ := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h)
		return errors.New("写入剪贴板失败")
	}
	return nil
}

// loadAppIcon 从 ICO 字节里挑最合适的尺寸创建 HICON。
// 失败时退回系统默认应用图标，保证托盘总有个能显示的图标。
func loadAppIcon(data []byte) uintptr {
	if len(data) < 22 {
		return defaultIcon(iconSmall)
	}
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	m, _, _ := procGetSystemMetrics.Call(iconSmall)
	want := int32(m)
	bestOff, bestSize, bestScore := -1, 0, 1<<30
	for i := 0; i < count; i++ {
		off := 6 + i*16
		if off+16 > len(data) {
			break
		}
		w := int32(data[off])
		if w == 0 {
			w = 256
		}
		size := int(binary.LittleEndian.Uint32(data[off+8 : off+12]))
		imgOff := int(binary.LittleEndian.Uint32(data[off+12 : off+16]))
		score := int(abs32(w - want))
		if score < bestScore && imgOff+size <= len(data) {
			bestScore, bestOff, bestSize = score, imgOff, size
		}
	}
	if bestOff < 0 {
		return defaultIcon(iconSmall)
	}
	h, _, _ := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&data[bestOff])), uintptr(bestSize), 1, 0x00030000, 0, 0, 0)
	if h == 0 {
		return defaultIcon(iconSmall)
	}
	return h
}

func defaultIcon(kind int32) uintptr {
	id := uintptr(32512) // IDI_APPLICATION
	if kind == iconBig {
		id = 32512
	}
	h, _, _ := procLoadIconW.Call(0, id)
	return h
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// openWithShell 用系统默认程序打开路径（数据目录等）。
func openWithShell(path string) error {
	verb, _ := syscall.UTF16PtrFromString("open")
	target, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	r, _, _ := procShellExecuteW.Call(0,
		uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(target)), 0, 0, 1)
	if r <= 32 {
		return errors.New("无法打开: " + path)
	}
	return nil
}
