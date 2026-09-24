package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"onerclone/internal/cfapi"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	cld              = syscall.NewLazyDLL("CldApi.dll")
	procGetAttr      = kernel32.NewProc("GetFileAttributesW")
	procFindFirst    = kernel32.NewProc("FindFirstFileW")
	procFindClose    = kernel32.NewProc("FindClose")
	procStateFromFD  = cld.NewProc("CfGetPlaceholderStateFromFindData")
	procSRInfo       = cld.NewProc("CfGetSyncRootInfoByPath")
)

// WIN32_FIND_DATAW = 592 bytes; dwReserved0 (offset 36) = reparse tag
const findDataSize = 592

func stateName(st uint32) string {
	if st == 0xffffffff {
		return "INVALID"
	}
	s := ""
	add := func(b uint32, n string) { if st&b != 0 { s += n + " " } }
	add(0x1, "PLACEHOLDER")
	add(0x2, "SYNC_ROOT")
	add(0x4, "ESSENTIAL")
	add(0x8, "IN_SYNC")
	add(0x10, "PARTIAL")
	add(0x20, "PARTIALLY_ON_DISK")
	if s == "" { s = "NO_STATES" }
	return s
}

func probeState(path string) {
	p, _ := syscall.UTF16PtrFromString(path)
	ga, _, _ := procGetAttr.Call(uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
	// 子进程对照 1：PowerShell（.NET GetAttributes）
	psOut, psErr := exec.Command("powershell", "-NoProfile", "-Command",
		"[int][IO.File]::GetAttributes('"+path+"')").Output()
	// 子进程对照 2：attrib
	atOut, _ := exec.Command("cmd", "/c", "attrib", path).Output()
	li, lerr := os.Lstat(path)
	lmode := "ERR"
	if lerr == nil {
		lmode = li.Mode().String()
	}
	fmt.Printf("%-55s GoGA=0x%X PS子进程=0x%X attrib=%q lstat=%s psErr=%v\n",
		path, ga, uint32(mustAtoi(strings.TrimSpace(string(psOut)))),
		strings.TrimSpace(string(atOut)), lmode, psErr)
}

func mustAtoi(s string) int64 {
	var n int64
	fmt.Sscan(s, &n)
	return n
}

func probeProviderStatus() {
	p, _ := syscall.UTF16PtrFromString(`C:\Users\Jw\OnercloneSpike`)
	buf := make([]byte, 4096)
	var ret uint32
	r1, _, _ := procSRInfo.Call(uintptr(unsafe.Pointer(p)), 1,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&ret)))
	if hr := int32(r1); hr < 0 {
		fmt.Printf("sync root info FAILED hr=0x%08X\n", uint32(hr))
		return
	}
	status := *(*uint32)(unsafe.Pointer(&buf[24])) // after LARGE_INTEGER + 4 policy enums
	hydration := *(*uint32)(unsafe.Pointer(&buf[8]))
	population := *(*uint32)(unsafe.Pointer(&buf[12]))
	fmt.Printf("sync root: ProviderStatus=%d HydrationPolicy=%d PopulationPolicy=%d\n",
		status, hydration, population)
}

func ga(path string) uintptr {
	p, _ := syscall.UTF16PtrFromString(path)
	r, _, _ := procGetAttr.Call(uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
	return r
}

func main() {
	targets := []string{
		`C:\Users\Jw\OnercloneSpike\hello.txt`,
		`C:\Users\Jw\OnercloneSpike\big.bin`,
	}
	fmt.Println("== 连接 sync root 之前 ==")
	for _, t := range targets {
		fmt.Printf("  %-50s GA=0x%X\n", t, ga(t))
	}

	// 连接 sync root（空回调表），验证“连接后才可见占位符属性”假设
	sess, err := cfapi.Connect(`C:\Users\Jw\OnercloneSpike`,
		cfapi.ConnectFlagRequireFullFilePath, map[cfapi.CallbackType]cfapi.CallbackFunc{})
	if err != nil {
		fmt.Println("Connect 失败:", err)
	} else {
		fmt.Println("Connect 成功")
	}

	fmt.Println("== 连接 sync root 之后 ==")
	for _, t := range targets {
		fmt.Printf("  %-50s GA=0x%X\n", t, ga(t))
	}
	// 子进程对照（PS 未连接任何 sync root）
	for _, t := range targets {
		out, _ := exec.Command("powershell", "-NoProfile", "-Command",
			"[int][IO.File]::GetAttributes('"+t+"')").Output()
		fmt.Printf("  %-50s PS子进程=0x%X\n", t, mustAtoi(strings.TrimSpace(string(out))))
	}
	if sess != nil {
		sess.Disconnect()
	}
}
