package main

import (
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// 托盘菜单命令 ID。
const (
	idShowConsole = 1 + iota
	idOpenPanel
	idCopyBase
	idCopyKey
	idRestart
	idToggleAutostart
	idOpenData
	idQuit
)

const (
	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	niifInfo = 0x00000001
	niifWarn = 0x00000002

	mfString    = 0x00000000
	mfChecked   = 0x00000008
	mfSeparator = 0x00000800
	mfGrayed    = 0x00000001

	tpmRightButton = 0x0002
	tpmNoNotify    = 0x0080
	tpmReturnCmd   = 0x0100

	csVRedraw = 0x0001
	csHRedraw = 0x0002
)

type point struct{ X, Y int32 }

type msgStruct struct {
	HWnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uint32
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         syscall.GUID
	HBalloonIcon     uintptr
}

// trayActions 托盘菜单动作集合，由外层注入。
type trayActions struct {
	Show        func()
	OpenPanel   func()
	CopyBase    func()
	CopyKey     func()
	Restart     func()
	ToggleRun   func(enabled bool)
	OpenData    func()
	Quit        func()
	GetAutostart func() bool
	GetTip      func() string
	GetTrayIcon func() uintptr
}

type tray struct {
	hwnd     uintptr
	icon     uintptr
	tip      [128]uint16
	actions  trayActions
	trayMsg  uint32
	balloonQ chan [2]string
}

var activeTray *tray

const trayWndClass = "WB2APITrayHost"

// runTray 在独立线程跑托盘消息循环（与 WebView2 主窗口循环互不干扰）。
func runTray(a trayActions) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	t := &tray{actions: a, trayMsg: wmApp + 1, balloonQ: make(chan [2]string, 8)}
	activeTray = t
	t.icon = a.GetTrayIcon()
	if t.icon == 0 {
		t.icon = defaultIcon(iconSmall)
	}

	hinst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString(trayWndClass)

	wc := wndClassEx{
		Style:         csHRedraw | csVRedraw,
		LpfnWndProc:   syscall.NewCallback(trayWndProc),
		HInstance:     hinst,
		LpszClassName: className,
	}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	hwnd, _, _ := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), 0, 0,
		0, 0, 0, 0, 0, 0, hinst, 0)
	if hwnd == 0 {
		return
	}
	t.hwnd = hwnd
	t.addIcon()

	go t.balloonLoop()
	go t.tipLoop()

	var msg msgStruct
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

// setUTF16 把字符串写进定长 UTF-16 缓冲，超长截断且保证以 0 结尾。
func setUTF16(dst []uint16, s string) {
	u := syscall.StringToUTF16(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}

func (t *tray) addIcon() {
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = t.hwnd
	nid.UID = 1
	nid.UFlags = nifMessage | nifIcon | nifTip
	nid.UCallbackMessage = t.trayMsg
	nid.HIcon = t.icon
	setUTF16(nid.SzTip[:], tipOf(t.actions))
	procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
}

func (t *tray) modifyTip(text string) {
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = t.hwnd
	nid.UID = 1
	nid.UFlags = nifTip
	setUTF16(nid.SzTip[:], text)
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

func (t *tray) balloon(title, text string, warn bool) {
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = t.hwnd
	nid.UID = 1
	nid.UFlags = nifInfo
	setUTF16(nid.SzInfoTitle[:], title)
	setUTF16(nid.SzInfo[:], text)
	nid.DwInfoFlags = niifInfo
	if warn {
		nid.DwInfoFlags = niifWarn
	}
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

// tipLoop 周期刷新悬停提示（运行状态与账号数）。
func (t *tray) tipLoop() {
	tick := time.NewTicker(2 * time.Second)
	for range tick.C {
		if t.actions.GetTip != nil {
			t.modifyTip(tipOf(t.actions))
		}
	}
}

func (t *tray) balloonLoop() {
	for msg := range t.balloonQ {
		t.balloon(msg[0], msg[1], false)
	}
}

func tipOf(a trayActions) string {
	if a.GetTip != nil {
		if s := a.GetTip(); s != "" {
			return s
		}
	}
	return "WorkBuddy2API"
}

// notifyBalloon 供外部推送气泡；托盘未就绪时丢弃。
func notifyBalloon(title, text string) {
	if activeTray == nil {
		return
	}
	select {
	case activeTray.balloonQ <- [2]string{title, text}:
	default:
	}
}

func trayWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	t := activeTray
	if t == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	}
	switch msg {
	case uintptr(t.trayMsg):
		switch uint32(lp) {
		case wmLButtonUp:
			if t.actions.Show != nil {
				t.actions.Show()
			}
		case wmRButtonUp:
			t.showMenu()
		}
		return 0
	case wmClose:
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

// showMenu 动态构建右键菜单：开机自启按当前注册表状态打勾。
func (t *tray) showMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	addItem := func(id int, text string, flags uintptr) {
		p, _ := syscall.UTF16PtrFromString(text)
		procAppendMenuW.Call(menu, flags, uintptr(id), uintptr(unsafe.Pointer(p)))
	}
	addSep := func() { procAppendMenuW.Call(menu, mfSeparator, 0, 0) }

	addItem(idShowConsole, "打开控制台", mfString)
	addItem(idOpenPanel, "打开 Web 管理面板", mfString)
	addSep()
	addItem(idCopyBase, "复制 API 地址", mfString)
	addItem(idCopyKey, "复制 API Key", mfString)
	addSep()
	addItem(idRestart, "重启服务", mfString)
	flags := uintptr(mfString)
	if t.actions.GetAutostart != nil && t.actions.GetAutostart() {
		flags |= mfChecked
	}
	addItem(idToggleAutostart, "开机自启", flags)
	addItem(idOpenData, "打开数据目录", mfString)
	addSep()
	addItem(idQuit, "退出", mfString)

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(t.hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmReturnCmd|tpmNoNotify,
		uintptr(pt.X), uintptr(pt.Y), 0, t.hwnd, 0)
	procDestroyMenu.Call(menu)
	// 让菜单在点击外部时正常消失。
	procPostMessageW.Call(t.hwnd, 0, 0, 0)

	switch int(cmd) {
	case idShowConsole:
		if t.actions.Show != nil {
			t.actions.Show()
		}
	case idOpenPanel:
		if t.actions.OpenPanel != nil {
			t.actions.OpenPanel()
		}
	case idCopyBase:
		if t.actions.CopyBase != nil {
			t.actions.CopyBase()
		}
	case idCopyKey:
		if t.actions.CopyKey != nil {
			t.actions.CopyKey()
		}
	case idRestart:
		if t.actions.Restart != nil {
			t.actions.Restart()
		}
	case idToggleAutostart:
		if t.actions.GetAutostart != nil && t.actions.ToggleRun != nil {
			t.actions.ToggleRun(!t.actions.GetAutostart())
		}
	case idOpenData:
		if t.actions.OpenData != nil {
			t.actions.OpenData()
		}
	case idQuit:
		if t.actions.Quit != nil {
			t.actions.Quit()
		}
	}
}

// quitTray 移除图标并结束托盘消息循环。
func quitTray(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UID = 1
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	procDestroyWindow.Call(hwnd)
	procPostQuitMessage.Call(0)
}
