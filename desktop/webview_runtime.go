package main

import (
	"fmt"
	"strings"

	"github.com/jchv/go-webview2/webviewloader"
)

// webview2DownloadURL 微软官方的 WebView2 Evergreen 运行时下载页。
const webview2DownloadURL = "https://go.microsoft.com/fwlink/p/?LinkId=2124703"

// webview2RuntimeVersion 返回已安装的 WebView2 运行时版本。
// 未安装或 loader 不可用时返回错误，调用方据此给出安装提示而不是静默失败。
func webview2RuntimeVersion() (string, error) {
	ver, err := webviewloader.GetInstalledVersion()
	if err != nil {
		return "", fmt.Errorf("WebView2 运行时不可用: %w", err)
	}
	if strings.TrimSpace(ver) == "" {
		return "", fmt.Errorf("未检测到 WebView2 运行时")
	}
	return ver, nil
}
