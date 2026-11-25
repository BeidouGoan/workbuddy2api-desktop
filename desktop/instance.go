package main

import (
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	user32           = syscall.NewLazyDLL("user32.dll")
	procCreateMutexW = kernel32.NewProc("CreateMutexW")
	procFindWindowW  = user32.NewProc("FindWindowW")
)

// ErrAlreadyRunning 表示已有实例持有单实例互斥体。
var ErrAlreadyRunning = errAlreadyRunning{}

type errAlreadyRunning struct{}

func (errAlreadyRunning) Error() string { return "another instance is already running" }

// acquireSingleInstance 创建命名互斥体；handled=true 表示互斥体创建成功，
// 调用方需在退出时 CloseHandle。
//
// 第二个实例拿到的是 ALREADY_EXISTS，此时唤起第一个实例的窗口并退出。
func acquireSingleInstance() (handle syscall.Handle, already bool) {
	name, _ := syscall.UTF16PtrFromString("WorkBuddy2API.Desktop.SingleInstance")
	h, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return 0, false
	}
	if errno, ok := err.(syscall.Errno); ok && errno == 183 { // ERROR_ALREADY_EXISTS
		return syscall.Handle(h), true
	}
	return syscall.Handle(h), false
}

// focusExistingWindow 唤起已运行实例：优先给托盘窗口发左键消息（等价用户点托盘，
// 会把窗口切回控制台），托盘不可用时回退到直接显示 WebView2 窗口。
func focusExistingWindow() {
	trayClass, _ := syscall.UTF16PtrFromString("WB2APITrayHost")
	if hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(trayClass)), 0); hwnd != 0 {
		procPostMessageW.Call(hwnd, 0x8000+1, 0, 0x0202) // WM_APP+1 / WM_LBUTTONUP
		return
	}
	viewClass, _ := syscall.UTF16PtrFromString("webview")
	hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(viewClass)), 0)
	if hwnd == 0 {
		return
	}
	const swRestore = 9
	procShowWindow.Call(hwnd, swRestore)
	procSetForegroundWindow.Call(hwnd)
}
