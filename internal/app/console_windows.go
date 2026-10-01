//go:build windows

package app

import (
	"syscall"
	"unsafe"
)

// enableVTProcessing 打开 Windows 控制台输出句柄的虚拟终端处理（VT100/ANSI）。
//
// rclone 的夸克二维码是用 ANSI 背景色字符画出来的（ESC[107m/ESC[97;40m ▀），
// 如果控制台没开 ENABLE_VIRTUAL_TERMINAL_PROCESSING，这些转义序列会被直接
// 丢弃，屏幕上只剩一片空白 —— 表现就是"没有二维码"。
func enableVTProcessing() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getStdHandle := kernel32.NewProc("GetStdHandle")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")

	const stdOutputHandle = uintptr(uint32(^uint32(10))) // STD_OUTPUT_HANDLE = -11
	const enableVT = 0x0004                              // ENABLE_VIRTUAL_TERMINAL_PROCESSING

	h, _, _ := getStdHandle.Call(stdOutputHandle)
	if h == 0 || h == ^uintptr(0) {
		return // 没有控制台（重定向/服务），无需处理
	}
	var mode uint32
	ok, _, _ := getConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode)))
	if ok == 0 {
		return
	}
	setConsoleMode.Call(h, uintptr(mode)|enableVT)
}
