//go:build !windows

package app

// enableVTProcessing 非 Windows 平台默认就支持 ANSI 转义，无需额外设置。
func enableVTProcessing() {}
