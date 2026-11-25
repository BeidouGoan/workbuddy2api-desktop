package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	webview2 "github.com/jchv/go-webview2"
)

const (
	desktopVersion = "1.0.0"
	consoleTitle   = "WorkBuddy2API · 控制台"
	panelTitle     = "WorkBuddy2API · 管理面板"
)

// App 把外壳各部件（窗口、托盘、服务守护、控制台数据通道）串起来。
type App struct {
	dataDir  string
	sup      *Supervisor
	view     webview2.WebView
	hwnd     uintptr
	shell    *shellServer
	iconBig  uintptr
	iconSmall uintptr

	quitting    atomic.Bool
	hiddenOnce  atomic.Bool
	webviewFail atomic.Bool
}

func newApp(dataDir string, sup *Supervisor) *App {
	return &App{dataDir: dataDir, sup: sup}
}

// statePayload 是控制台页面的数据源，也用于托盘提示文案。
type statePayload struct {
	Snapshot
	DesktopVersion string `json:"desktop_version"`
	WebViewOK      bool   `json:"webview_ok"`
	LogPath        string `json:"log_path"`
}

func (a *App) statePayload() statePayload {
	return statePayload{
		Snapshot:       a.sup.Snapshot(),
		DesktopVersion: desktopVersion,
		WebViewOK:      !a.webviewFail.Load(),
		LogPath:        a.dataDir + "\\logs",
	}
}

// doAction 处理控制台页面发来的动作。
func (a *App) doAction(req actionRequest) (string, error) {
	switch req.Action {
	case "open-panel":
		a.OpenPanel()
		return "已打开管理面板", nil
	case "open-console":
		a.ShowConsole()
		return "已返回控制台", nil
	case "restart":
		a.sup.Restart()
		return "正在重启服务", nil
	case "start":
		a.sup.StartService()
		return "正在启动服务", nil
	case "stop":
		a.sup.StopService()
		return "服务已停止", nil
	case "autostart":
		if req.Value == nil {
			return "", errors.New("缺少开关值")
		}
		if err := setAutostart(*req.Value); err != nil {
			return "", err
		}
		if *req.Value {
			return "已开启开机自启", nil
		}
		return "已关闭开机自启", nil
	case "copy":
		if req.Text == "" {
			return "", errors.New("没有可复制的内容")
		}
		if err := setClipboardText(req.Text); err != nil {
			return "", err
		}
		return "已复制到剪贴板", nil
	case "open-dir":
		return "", openWithShell(a.dataDir)
	case "open-logs":
		return "", openWithShell(a.dataDir + "\\logs")
	case "hide":
		a.Hide()
		return "已最小化到托盘", nil
	case "quit":
		go a.Quit()
		return "正在退出", nil
	}
	return "", fmt.Errorf("未知操作: %s", req.Action)
}

// createWindow 在主线程创建 WebView2 窗口并注入面板适配脚本。
// 返回 false 表示本机没有 WebView2 运行时（此时退化为托盘 + 默认浏览器）。
func (a *App) createWindow() bool {
	a.iconSmall = loadAppIcon(iconBytes())
	a.iconBig = a.iconSmall

	view := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  a.dataDir + "\\webview",
		WindowOptions: webview2.WindowOptions{
			Title:  consoleTitle,
			Width:  1100,
			Height: 780,
			Center: true,
		},
	})
	if view == nil {
		a.webviewFail.Store(true)
		return false
	}
	a.view = view
	a.hwnd = uintptr(view.Window())
	view.SetSize(920, 640, webview2.HintMin)
	applyDarkChrome(a.hwnd)

	if a.iconBig != 0 {
		procSendMessageW.Call(a.hwnd, wmSetIcon, 0, a.iconBig)
		procSendMessageW.Call(a.hwnd, wmSetIcon, 1, a.iconSmall)
	}

	installMainWindowHooks(a.hwnd, a.dataDir, func() { a.Hide() })
	view.Init(a.panelHelperScript())
	view.Navigate(a.shell.url)
	return true
}

// panelHelperScript 注入到每个新文档：
//   - 打开 Web 面板时自动写入 API Key（免去手动粘贴）
//   - 在面板顶部工具栏里加一个「← 控制台」按钮（跟随布局排布，不遮挡原有按钮）
func (a *App) panelHelperScript() string {
	key := a.sup.Snapshot().APIKey
	consoleURL := a.shell.url
	return fmt.Sprintf(`(function () {
  try { if (%q) localStorage.setItem('wb2api.key', %q); } catch (e) {}
  if (location.pathname.indexOf('/panel') !== 0) return;
  function mountBack() {
    if (document.getElementById('wb2a-back')) return;
    var bar = document.querySelector('.topbar');
    if (!bar) return;
    var b = document.createElement('button');
    b.id = 'wb2a-back';
    b.type = 'button';
    b.className = 'ghost';
    b.title = '返回桌面控制台';
    b.textContent = '← 控制台';
    b.style.cssText = 'flex:none;padding:7px 11px;font-size:12.5px;' +
      'display:inline-flex;align-items:center;gap:6px;';
    b.addEventListener('click', function () { location.href = %q; });
    var theme = document.getElementById('btnTheme');
    if (theme && theme.parentNode === bar) bar.insertBefore(b, theme);
    else bar.appendChild(b);
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', mountBack);
  else mountBack();
  window.addEventListener('hashchange', mountBack);
})();`, key, key, consoleURL)
}

// ShowConsole 显示窗口并切到控制台。
func (a *App) ShowConsole() {
	if a.view == nil {
		return
	}
	a.view.Dispatch(func() {
		a.view.SetTitle(consoleTitle)
		a.view.Navigate(a.shell.url)
		showWindow(a.hwnd)
	})
}

// OpenPanel 显示窗口并切到完整 Web 管理面板。
func (a *App) OpenPanel() {
	if a.view == nil {
		return
	}
	snap := a.sup.Snapshot()
	url := snap.PanelURL
	if url == "" {
		url = "http://127.0.0.1:7863/panel/"
	}
	a.view.Dispatch(func() {
		a.view.SetTitle(panelTitle)
		a.view.Navigate(url)
		showWindow(a.hwnd)
	})
}

// Hide 隐藏窗口到托盘。
func (a *App) Hide() {
	if a.view == nil {
		return
	}
	appendLog(a.dataDir, "desktop.log", "Hide: 隐藏窗口")
	a.view.Dispatch(func() { hideWindow(a.hwnd) })
	go trimAfterHide()
	if a.hiddenOnce.CompareAndSwap(false, true) {
		notifyBalloon("WorkBuddy2API 仍在后台运行", "单击托盘图标可恢复窗口，右键可退出")
	}
}

// Quit 退出程序：先停服务，再销毁窗口与托盘。
func (a *App) Quit() {
	if !a.quitting.CompareAndSwap(false, true) {
		return
	}
	appendLog(a.dataDir, "desktop.log", "Quit: 退出程序")
	if a.shell != nil {
		a.shell.close()
	}
	a.sup.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	a.sup.Wait(ctx)
	cancel()
	if a.view != nil {
		destroyMainWindow(a.hwnd)
		time.Sleep(200 * time.Millisecond)
	}
	if activeTray != nil {
		quitTray(activeTray.hwnd)
	}
	time.Sleep(200 * time.Millisecond)
	os.Exit(0)
}

// trayTip 生成托盘悬停文案。
func (a *App) trayTip() string {
	snap := a.sup.Snapshot()
	var state string
	switch snap.State {
	case stateRunning:
		state = "运行中"
	case stateStarting:
		state = "启动中"
	case stateRestarting:
		state = "重启中"
	case stateFailed:
		state = "已停止（异常）"
	default:
		state = "已停止"
	}
	tip := fmt.Sprintf("WorkBuddy2API · %s", state)
	if snap.State == stateRunning {
		tip += fmt.Sprintf(" · 账号 %d/%d 可用", snap.Accounts.Healthy, snap.Accounts.Total)
	}
	if len(tip) > 120 {
		tip = tip[:120]
	}
	return tip
}

// copyBase / copyKey 给托盘菜单用，失败时用气泡提示。
func (a *App) copyBase() {
	snap := a.sup.Snapshot()
	if err := setClipboardText(snap.BaseURL + "\n" + snap.APIKey); err != nil {
		notifyBalloon("复制失败", err.Error())
		return
	}
	notifyBalloon("已复制 API 地址与 Key", snap.BaseURL)
}

func (a *App) copyKey() {
	snap := a.sup.Snapshot()
	if snap.APIKey == "" {
		notifyBalloon("暂无 API Key", "服务尚未生成配置，请稍候再试")
		return
	}
	if err := setClipboardText(snap.APIKey); err != nil {
		notifyBalloon("复制失败", err.Error())
		return
	}
	notifyBalloon("已复制 API Key", maskKey(snap.APIKey))
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + "……" + k[len(k)-4:]
}
