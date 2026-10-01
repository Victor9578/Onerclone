package main

// main.go —— onerclone 入口：无参数 = GUI 同步客户端（Wails）；
// 子命令 = CLI（run/register/login/…，与 GUI 共用 internal/app 运行时）。
//
// 单 exe 双形态：GUI 构建用 -H windowsgui（无控制台闪窗）；CLI 子命令
// 启动时 AttachConsole 挂回父终端（console_windows.go）。

import (
	"fmt"
	"os"

	"onerclone/internal/app"
	"onerclone/internal/gui"
)

func main() {
	if len(os.Args) >= 2 && !isGUIFlag(os.Args[1]) {
		// CLI 形态：接回父终端的 stdout/stderr（无父控制台则静默）
		attachParentConsole()
		app.RunCLI()
		return
	}
	if err := gui.Run(); err != nil {
		// GUI 失败时也尝试接回终端打印（开发调试用）
		attachParentConsole()
		fmt.Fprintf(os.Stderr, "GUI 启动失败: %v\n", err)
		os.Exit(1)
	}
}

// isGUIFlag 判断参数是否是 GUI 自身的 flag（如 -console）而非 CLI 子命令。
func isGUIFlag(arg string) bool {
	switch arg {
	case "-console", "--console":
		return true
	}
	return false
}
