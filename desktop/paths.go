package main

import (
	"crypto/sha256"
	"embed"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// assets 内嵌构建产物：server.exe 由 build.ps1 在编译外壳前生成。
//
//go:embed assets/server.exe
var assets embed.FS

// appDataDir 返回桌面版数据目录 %LOCALAPPDATA%\WorkBuddy2API。
// 账号、配置、状态、日志都落在这里，与程序本体分离，升级 exe 不丢数据。
func appDataDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", err
		}
	}
	dir := filepath.Join(base, "WorkBuddy2API")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// auths 目录必须预先存在：server 启动时 LoadDir 读不到会直接退出。
	if err := os.MkdirAll(filepath.Join(dir, "auths"), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// releaseServer 把内嵌的 server.exe 释放到数据目录，返回可执行文件路径。
// 内容一致（SHA256 相同）时跳过写入，避免每次启动都动 22MB 文件。
func releaseServer(dir string) (string, error) {
	data, err := assets.ReadFile("assets/server.exe")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	target := filepath.Join(dir, "server.exe")

	if cur, err := os.ReadFile(target); err == nil && sha256.Sum256(cur) == sum {
		return target, nil
	}
	// 面板里的自动更新会替换数据目录下的 server.exe：替换后的文件比外壳本体新，
	// 此时保留更新结果，避免下次启动又被内嵌的旧版本覆盖回去。
	if st, err := os.Stat(target); err == nil {
		if self, err2 := os.Executable(); err2 == nil {
			if si, err3 := os.Stat(self); err3 == nil && st.ModTime().After(si.ModTime().Add(time.Minute)) {
				return target, nil
			}
		}
	}
	// 先写临时文件再替换：目标正被旧进程占用时 MoveFileEx 会失败，
	// 此时保留旧文件继续用（下次启动再换），不让外壳起不来。
	tmp := target + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return target, nil
	}
	if err := os.Rename(tmp, target); err != nil {
		// Windows 上目标被占用时 rename 失败：删掉临时文件，沿用现有版本。
		_ = os.Remove(tmp)
		if _, statErr := os.Stat(target); statErr != nil {
			return "", errors.Join(err, statErr)
		}
	}
	return target, nil
}

// appendLog 在数据目录 logs/<name> 追加一行日志；写失败静默（不影响主流程）。
func appendLog(dir, name, msg string) {
	f, err := os.OpenFile(filepath.Join(dir, "logs", name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = io.WriteString(f, msg+"\r\n")
}
