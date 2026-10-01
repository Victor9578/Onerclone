//go:build windows

package main

// console_windows.go —— 单 exe 双形态：
//   - 无参数 = GUI（-H windowsgui，无控制台窗口）
//   - 有子命令 = CLI：AttachConsole(ATTACH_PARENT_PROCESS) 挂回父终端
//     （cmd/PowerShell），重定向 stdout/stderr，输出正常可见。
// 从资源管理器双击跑 CLI 子命令（无父控制台）时输出无处可去，静默忽略。

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole  = kernel32.NewProc("AttachConsole")
	procGetStdHandle   = kernel32.NewProc("GetStdHandle")
	procSetStdHandle   = kernel32.NewProc("SetStdHandle")
	procCreateFileW    = kernel32.NewProc("CreateFileW")
	procFreeConsole    = kernel32.NewProc("FreeConsole")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
	procSetConsoleCP   = kernel32.NewProc("SetConsoleCP")
	procSetConsoleOut  = kernel32.NewProc("SetConsoleOutputCP")
)

const (
	attachParentProcess = ^uintptr(0) // ATTACH_PARENT_PROCESS
	stdOutputHandle     = uintptr(uint32(^uint32(11)))
	stdErrorHandle      = uintptr(uint32(^uint32(12)))
	genericWrite        = 0x40000000
	createAlways        = 2
	enableVT            = 0x0004
	cpUTF8              = 65001
)

// attachParentConsole 把 CLI 输出接回启动它的终端。返回 false = 没有
// 父控制台（双击启动），调用方应放弃控制台输出。
func attachParentConsole() bool {
	r1, _, _ := procAttachConsole.Call(attachParentProcess)
	if r1 == 0 {
		return false
	}
	// stdout/stderr → CONOUT$（AttachConsole 不自动重定向 Go 的 os.Stdout）
	for _, h := range []uintptr{stdOutputHandle, stdErrorHandle} {
		name, _ := syscall.UTF16PtrFromString("CONOUT$")
		ch, _, _ := procCreateFileW.Call(
			uintptr(unsafe.Pointer(name)), genericWrite, 1, 0, createAlways, 0, 0)
		if ch != ^uintptr(0) && ch != 0 {
			procSetStdHandle.Call(h, ch)
			if h == stdOutputHandle {
				os.Stdout = os.NewFile(ch, "|stdout")
			} else {
				os.Stderr = os.NewFile(ch, "|stderr")
			}
		}
	}
	// UTF-8 代码页 + VT 转义（二维码 ANSI 字符画依赖）
	procSetConsoleCP.Call(cpUTF8)
	procSetConsoleOut.Call(cpUTF8)
	if h, _, _ := procGetStdHandle.Call(stdOutputHandle); h != 0 && h != ^uintptr(0) {
		var mode uint32
		if ok, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); ok != 0 {
			procSetConsoleMode.Call(h, uintptr(mode)|enableVT)
		}
	}
	return true
}

// cliPrintf 是无控制台时的兜底输出（弹窗，仅致命错误用）。
func cliAlert(msg string) {
	fmt.Fprint(os.Stderr, msg)
}
