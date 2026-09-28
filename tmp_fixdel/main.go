//go:build windows

// 一次性修复工具 v2：占位符 reparse 元数据损坏后连 DeleteFileW 都被
// cldflt 拦截（ERROR_CLOUD_FILE_METADATA_CORRUPT）。最后一招：
// 用 FSCTL_DELETE_REPARSE_POINT 把云 reparse tag 从文件上剥掉
//（文件退化为普通文件），再正常删除。
// 用法: go run ./tmp_fixdel <文件绝对路径>
package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const fsctlDeleteReparsePoint = 0x000900AC

// REPARSE_GUID_DATA_BUFFER / REPARSE_DATA_BUFFER 的公共头（Tag+Length）
type reparseData struct {
	Tag      uint32
	Length   uint16
	Reserved uint16
	Guid     syscall.GUID
	DataBuf  [64]byte
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: tmp_fixdel <abs path>")
		os.Exit(2)
	}
	p := os.Args[1]

	k32 := syscall.NewLazyDLL("kernel32.dll")
	procOpen := k32.NewProc("CreateFileW")
	procDevIo := k32.NewProc("DeviceIoControl")
	procDel := k32.NewProc("DeleteFileW")
	procClose := k32.NewProc("CloseHandle")
	procMoveEx := k32.NewProc("MoveFileExW")

	up, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		fmt.Println("bad path:", err)
		os.Exit(1)
	}
	const (
		deleteAccess = 0x00010000
		shareAll     = 0x7
		openExisting = 3
	)
	h, _, e := procOpen.Call(
		uintptr(unsafe.Pointer(up)),
		uintptr(deleteAccess), // 只要 DELETE 权限（WRITE 会被 cldflt 校验元数据）
		uintptr(shareAll),
		0,
		uintptr(openExisting),
		0x00200000, // FILE_FLAG_OPEN_REPARSE_POINT —— 直接操作 reparse 数据本身
		0,
	)
	if h == 0 || h == 0xffffffffffffffff {
		fmt.Printf("CreateFileW failed: %v\n", e)
		// 兜底：MoveFileEx 改名（rename 路径不走 open 校验）
		dst, _ := syscall.UTF16PtrFromString(p + ".corrupt")
		r0, _, e0 := procMoveEx.Call(uintptr(unsafe.Pointer(up)), uintptr(unsafe.Pointer(dst)), 1)
		if r0 == 0 {
			fmt.Printf("MoveFileExW failed too: %v\n", e0)
			os.Exit(1)
		}
		fmt.Println("renamed to .corrupt（请手动删除）")
		os.Exit(0)
	}
	defer procClose.Call(h)

	// 先读现有 reparse tag（FSCTL_GET_REPARSE_POINT = 0x900A8）
	var rd reparseData
	var returned uint32
	r1, _, e2 := procDevIo.Call(
		h,
		0x000900A8, // FSCTL_GET_REPARSE_POINT
		0, 0,
		uintptr(unsafe.Pointer(&rd)),
		uintptr(unsafe.Sizeof(rd)),
		uintptr(unsafe.Pointer(&returned)),
		0,
	)
	if r1 == 0 {
		fmt.Printf("FSCTL_GET_REPARSE_POINT failed: %v\n", e2)
		os.Exit(1)
	}
	fmt.Printf("reparse tag = 0x%08X\n", rd.Tag)

	// 删除 reparse point：请求体 = 头（Tag 必须匹配现有 tag，Length = 0）
	var del reparseData
	del.Tag = rd.Tag
	var dummy uint32
	r2, _, e3 := procDevIo.Call(
		h,
		fsctlDeleteReparsePoint,
		uintptr(unsafe.Pointer(&del)),
		uintptr(unsafe.Sizeof(del)),
		0, 0,
		uintptr(unsafe.Pointer(&dummy)),
		0,
	)
	if r2 == 0 {
		fmt.Printf("FSCTL_DELETE_REPARSE_POINT failed: %v\n", e3)
		os.Exit(1)
	}
	fmt.Println("reparse point removed (file is now plain)")

	r3, _, _ := procDel.Call(uintptr(unsafe.Pointer(up)))
	if r3 == 0 {
		fmt.Println("DeleteFileW after strip: failed（文件已退化为普通文件，可手动删）")
		os.Exit(1)
	}
	fmt.Println("deleted ok")
}
