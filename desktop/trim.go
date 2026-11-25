package main

import (
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procEmptyWorkingSet = windows.NewLazySystemDLL("psapi.dll").NewProc("EmptyWorkingSet")

// trimAfterHide 在窗口隐藏后把外壳与 WebView2 的工作集归还系统。
// 隐藏态下页面已停止渲染，这些页长期闲置；归还后常驻物理内存可从数百 MB
// 降到几十 MB（提交内存不变，再次打开窗口时按需换入）。
// server.exe 不参与回收，避免空闲后的首个 API 请求因换页而变慢。
func trimAfterHide() {
	time.Sleep(1200 * time.Millisecond)

	self := uint32(os.Getpid())
	parents := make(map[uint32]uint32)
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(snap)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		parents[entry.ProcessID] = entry.ParentProcessID
	}

	// WebView2 的 browser 进程由外壳直接创建，gpu/renderer/utility 挂在它下面，
	// 因此需要递归收集全部后代。
	children := make(map[uint32][]uint32)
	for pid, ppid := range parents {
		children[ppid] = append(children[ppid], pid)
	}
	targets := []uint32{self}
	for i := 0; i < len(targets); i++ {
		targets = append(targets, children[targets[i]]...)
	}
	for _, pid := range targets {
		h, err := windows.OpenProcess(
			windows.PROCESS_SET_QUOTA|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			continue
		}
		if pid == self || isWebViewImage(h) {
			_, _, _ = procEmptyWorkingSet.Call(uintptr(h))
		}
		_ = windows.CloseHandle(h)
	}
}

// isWebViewImage 判断进程是否为 msedgewebview2.exe。
func isWebViewImage(h windows.Handle) bool {
	buf := make([]uint16, syscall.MAX_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(windows.UTF16ToString(buf[:n])), "msedgewebview2.exe")
}
