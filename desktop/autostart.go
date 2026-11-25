package main

import (
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	autostartKey  = `Software\Microsoft\Windows\CurrentVersion\Run`
	autostartName = "WorkBuddy2API"
)

// autostartCommand 生成写入 Run 项的命令行：带引号的 exe 路径 + 标记参数，
// 便于启动时区分「开机自启」（此时默认不弹窗，只驻留托盘）。
func autostartCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return `"` + exe + `" --autostart`, nil
}

// isAutostartEnabled 报告注册表 Run 项是否指向当前 exe。
func isAutostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(autostartName)
	if err != nil {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(v), strings.ToLower(exe))
}

// setAutostart 写入/删除 Run 项。
func setAutostart(enabled bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !enabled {
		err := k.DeleteValue(autostartName)
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	cmd, err := autostartCommand()
	if err != nil {
		return err
	}
	return k.SetStringValue(autostartName, cmd)
}
