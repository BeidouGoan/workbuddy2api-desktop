package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var procMessageBoxW = user32.NewProc("MessageBoxW")

// messageBox 简单的原生消息框（WebView2 缺失、启动失败等场景），返回按钮 ID。
func messageBox(title, text string, flags uintptr) uintptr {
	t, _ := syscall.UTF16PtrFromString(title)
	b, _ := syscall.UTF16PtrFromString(text)
	r, _, _ := procMessageBoxW.Call(0, uintptr(unsafe.Pointer(b)), uintptr(unsafe.Pointer(t)), flags)
	return r
}

// askYesNo 弹出「是/否」确认框。
func askYesNo(title, text string) bool {
	const mbYesNo, mbIconWarning, mbSetForeground, idYes = 0x04, 0x30, 0x10000, 6
	return messageBox(title, text, mbYesNo|mbIconWarning|mbSetForeground) == idYes
}

// appendFileWriter 把标准库 log 的输出落到桌面日志文件。
// windowsgui 程序没有 stderr，WebView2 初始化失败等致命信息否则会彻底丢失。
type appendFileWriter struct {
	dir  string
	name string
}

func (w appendFileWriter) Write(p []byte) (int, error) {
	appendLog(w.dir, w.name, strings.TrimRight(string(p), "\r\n"))
	return len(p), nil
}

func main() {
	// WebView2 的消息循环必须固定在创建窗口的 OS 线程上。
	runtime.LockOSThread()

	fromAutostart := false
	for _, a := range os.Args[1:] {
		if a == "--autostart" {
			fromAutostart = true
		}
	}

	dataDir, err := appDataDir()
	if err != nil {
		messageBox("WorkBuddy2API", "无法创建数据目录：\n"+err.Error(), 0x10)
		return
	}
	// windowsgui 没有 stderr：把库内 log（含 WebView2 初始化的致命错误）落到日志文件。
	log.SetOutput(appendFileWriter{dir: dataDir, name: "desktop.log"})

	mutex, already := acquireSingleInstance()
	if already {
		focusExistingWindow()
		return
	}
	defer syscall.CloseHandle(mutex)

	// 尽力而为：失败不影响功能，只是外壳异常退出时可能残留子进程。
	_ = setupKillOnCloseJob()

	exePath, err := releaseServer(dataDir)
	if err != nil {
		messageBox("WorkBuddy2API", "释放服务组件失败：\n"+err.Error(), 0x10)
		return
	}

	sup := NewSupervisor(dataDir, exePath, filepath.Join(dataDir, "config.json"))
	sup.SetNotifier(notifyBalloon)
	app := newApp(dataDir, sup)
	app.iconSmall = loadAppIcon(iconBytes())

	shell, err := newShellServer(app)
	if err != nil {
		messageBox("WorkBuddy2API", "初始化本地界面失败：\n"+err.Error(), 0x10)
		return
	}
	app.shell = shell
	go shell.run()

	// 先拉起服务并等首轮就绪：面板的 API Key 由服务首次运行生成，
	// 窗口要在拿到 key 之后再创建，才能自动带入免手填。
	sup.Start()
	waitReadyBriefly(sup, 10*time.Second)
	startupErr := ""
	if st := sup.Snapshot(); st.State == stateFailed {
		startupErr = st.LastError
	}

	// 先确认 WebView2 运行时存在：缺失时不再尝试建窗（组件内部失败会静默退出）。
	viewVersion, viewErr := webview2RuntimeVersion()
	hasWindow := false
	if viewErr != nil {
		app.webviewFail.Store(true)
		appendLog(dataDir, "desktop.log", "WebView2 检测失败: "+viewErr.Error())
	} else {
		appendLog(dataDir, "desktop.log", "WebView2 运行时 "+viewVersion)
		hasWindow = app.createWindow()
	}

	go runTray(trayActions{
		Show: func() {
			if app.view != nil {
				app.ShowConsole()
			} else {
				_ = openWithShell(shell.url)
			}
		},
		OpenPanel: func() {
			if app.view != nil {
				app.OpenPanel()
			} else {
				_ = openWithShell(sup.Snapshot().PanelURL)
			}
		},
		CopyBase:  app.copyBase,
		CopyKey:   app.copyKey,
		Restart:   sup.Restart,
		ToggleRun: func(v bool) { _ = setAutostart(v) },
		OpenData:  func() { _ = openWithShell(dataDir) },
		Quit:      func() { go app.Quit() },
		GetAutostart: isAutostartEnabled,
		GetTip:       app.trayTip,
		GetTrayIcon:  func() uintptr { return app.iconSmall },
	})

	serverLog := filepath.Join(dataDir, "logs", "server-console.log")
	if startupErr != "" {
		appendLog(dataDir, "desktop.log", "服务启动失败: "+startupErr)
		if fromAutostart {
			notifyBalloon("WorkBuddy2API 启动失败", startupErr)
		} else {
			messageBox("WorkBuddy2API 服务启动失败", startupErr+"\n\n服务日志："+serverLog, 0x30)
		}
	}

	if !hasWindow {
		if viewErr != nil {
			if fromAutostart {
				notifyBalloon("缺少 WebView2 运行时", "窗口不可用，可右键托盘图标用浏览器打开面板")
			} else {
				text := "未检测到 Microsoft Edge WebView2 运行时，桌面窗口无法显示。\n\n" +
					"服务已在后台运行，可右键托盘图标，用浏览器打开控制台与 Web 管理面板。\n\n" +
					"是否现在打开 WebView2 运行时下载页面？安装后重启本程序即可。"
				if askYesNo("缺少 WebView2 运行时", text) {
					_ = openWithShell(webview2DownloadURL)
				}
				_ = openWithShell(shell.url)
			}
		} else {
			messageBox("WorkBuddy2API", "窗口初始化失败，已改用浏览器打开控制台。\n\n日志："+filepath.Join(dataDir, "logs", "desktop.log"), 0x30)
			if !fromAutostart {
				_ = openWithShell(shell.url)
			}
		}
	}

	if fromAutostart && hasWindow {
		hideWindow(app.hwnd)
	}

	if hasWindow {
		appendLog(dataDir, "desktop.log", "进入窗口消息循环")
		app.view.Run()
		appendLog(dataDir, "desktop.log", "窗口消息循环已退出")
	} else {
		select {}
	}
	// Run 返回（窗口被真正关闭）：走统一退出流程。
	app.Quit()
}

// waitReadyBriefly 等待服务首次健康，最多等 timeout；超时也继续（tick 会持续探测）。
func waitReadyBriefly(sup *Supervisor, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snap := sup.Snapshot()
		if snap.State == stateRunning && snap.APIKey != "" {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}
