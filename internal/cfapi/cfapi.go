//go:build windows

package cfapi

import (
	"fmt"
	"log"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var (
	dll = syscall.NewLazyDLL("CldApi.dll")

	procGetPlatformInfo    = dll.NewProc("CfGetPlatformInfo")
	procRegisterSyncRoot   = dll.NewProc("CfRegisterSyncRoot")
	procUnregisterSyncRoot = dll.NewProc("CfUnregisterSyncRoot")
	procGetSyncRootInfoByPath = dll.NewProc("CfGetSyncRootInfoByPath")
	procConnectSyncRoot    = dll.NewProc("CfConnectSyncRoot")
	procDisconnectSyncRoot = dll.NewProc("CfDisconnectSyncRoot")
	procExecute            = dll.NewProc("CfExecute")
	procCreatePlaceholders = dll.NewProc("CfCreatePlaceholders")
	procSetInSyncState     = dll.NewProc("CfSetInSyncState")
	procConvertToPlaceholder = dll.NewProc("CfConvertToPlaceholder")
)

// HresultError 表示一次 cfapi 调用返回的失败 HRESULT。
type HresultError struct {
	Code uint32
}

func (e *HresultError) Error() string {
	return fmt.Sprintf("cfapi: HRESULT 0x%08X", e.Code)
}

// AsHRESULT 便于调用方按错误码分支处理。
func AsHRESULT(err error) (uint32, bool) {
	if h, ok := err.(*HresultError); ok {
		return h.Code, true
	}
	return 0, false
}

func hr(r1 uintptr) error {
	if int32(r1) < 0 {
		return &HresultError{Code: uint32(r1)}
	}
	return nil
}

func utf16ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		// 含 NUL 等非法输入时降级为占位串，避免 panic
		p, _ = syscall.UTF16PtrFromString("")
	}
	return p
}

func bytesToPointer(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(&b[0])
}

// StringFromUTF16Ptr 将 NUL 结尾的 UTF-16 指针转为 Go 字符串。
func StringFromUTF16Ptr(p *uint16) string {
	if p == nil {
		return ""
	}
	var s []uint16
	for i := 0; ; i++ {
		v := *(*uint16)(unsafe.Add(unsafe.Pointer(p), i*2))
		if v == 0 {
			break
		}
		s = append(s, v)
	}
	return syscall.UTF16ToString(s)
}

// ---------- 同步根注册 ----------

// RegisterSyncRoot 注册同步根（CfRegisterSyncRoot）。会自动填充 StructSize。
func RegisterSyncRoot(path string, reg *SyncRegistration, pol *SyncPolicies, flags uint32) error {
	p := utf16ptr(path)
	reg.StructSize = uint32(unsafe.Sizeof(*reg))
	pol.StructSize = uint32(unsafe.Sizeof(*pol))
	r1, _, _ := procRegisterSyncRoot.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(reg)),
		uintptr(unsafe.Pointer(pol)),
		uintptr(flags),
	)
	runtime.KeepAlive(reg)
	runtime.KeepAlive(pol)
	runtime.KeepAlive(p)
	return hr(r1)
}

// UnregisterSyncRoot 注销同步根。必须在 Disconnect 之后调用。
func UnregisterSyncRoot(path string) error {
	p := utf16ptr(path)
	r1, _, _ := procUnregisterSyncRoot.Call(uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
	return hr(r1)
}

// SyncRootStandardInfo 对应 CF_SYNC_ROOT_STANDARD_INFO（可变长：ProviderName
// 255+1、ProviderVersion 255+1 WCHAR，尾部 SyncRootIdentity[1]）。这里只
// 取固定前缀 + 用大缓冲整体读，字段偏移按头文件 x64 布局：
//   FileId@0(8) Hydration@8(4) Population@12(4) InSync@16(4) HardLink@20(4)
//   ProviderStatus@24(4) pad@28 ProviderName@32(512) ProviderVersion@544(512)
//   IdentityLen@1056(4) Identity@1060
type SyncRootStandardInfo struct {
	SyncRootFileId    int64
	Hydration         uint16
	HydrationModifier uint16
	Population        uint16
	PopulationModifier uint16
	InSync            uint32
	HardLink          uint32
	ProviderStatus    uint32
	ProviderName      string
	ProviderVersion   string
	SyncRootIdentity  []byte
}

// GetSyncRootInfoByPath 查询路径所属同步根的信息（CfGetSyncRootInfoByPath，
// InfoClass=STANDARD）。路径不在任何同步根下 → HRESULT 0x80070186
//（ERROR_CLOUD_FILE_NOT_UNDER_SYNC_ROOT 语义，实测该 API 对非同步根路径
// 返回此码）。
func GetSyncRootInfoByPath(path string) (*SyncRootStandardInfo, error) {
	const (
		maxNameW = 255 + 1
		bufLen   = 8 + 4 + 4 + 4 + 4 + 4 + 4 + maxNameW*2 + maxNameW*2 + 4 + 256
	)
	buf := make([]byte, bufLen)
	p := utf16ptr(path)
	var returned uint32
	r1, _, _ := procGetSyncRootInfoByPath.Call(
		uintptr(unsafe.Pointer(p)),
		0, // CF_SYNC_ROOT_INFO_STANDARD
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(bufLen),
		uintptr(unsafe.Pointer(&returned)),
	)
	runtime.KeepAlive(p)
	if err := hr(r1); err != nil {
		return nil, err
	}
	// 解析固定前缀
	u16 := func(off, count int) string {
		s := (*[1 << 20]uint16)(unsafe.Pointer(&buf[off]))[:count:count]
		return syscall.UTF16ToString(s)
	}
	u32 := func(off int) uint32 {
		return *(*uint32)(unsafe.Pointer(&buf[off]))
	}
	u16v := func(off int) uint16 {
		return *(*uint16)(unsafe.Pointer(&buf[off]))
	}
	info := &SyncRootStandardInfo{
		SyncRootFileId:     *(*int64)(unsafe.Pointer(&buf[0])),
		Hydration:          u16v(8),
		HydrationModifier:  u16v(10),
		Population:          u16v(12),
		PopulationModifier: u16v(14),
		InSync:              u32(16),
		HardLink:            u32(20),
		ProviderStatus:      u32(24),
		ProviderName:        u16(32, maxNameW),
		ProviderVersion:     u16(32+maxNameW*2, maxNameW),
	}
	idLen := int(u32(32 + maxNameW*4))
	if idLen > 0 && 1060+idLen <= len(buf) {
		info.SyncRootIdentity = append([]byte{}, buf[1060:1060+idLen]...)
	}
	return info, nil
}

// ---------- 回调会话 ----------

// CallbackFunc 是回调处理函数；info/params 指向的内存仅在调用期间有效，
// 不得保留引用（需要跨调用保存的字段必须拷贝出来）。
type CallbackFunc func(info *CallbackInfo, params *CallbackParameters)

// Session 表示一次 CfConnectSyncRoot 连接。
type Session struct {
	key      uint64
	regs     []CallbackRegistration // 防止 GC 回收桩
	handlers map[CallbackType]CallbackFunc
}

var (
	sessionMu sync.RWMutex
	active    *Session
)

// invoke 由 syscall.NewCallback 桩调用；每个回调类型一个独立桩以区分类型。
func invoke(t CallbackType, infoPtr, paramsPtr uintptr) {
	sessionMu.RLock()
	s := active
	sessionMu.RUnlock()
	if s == nil {
		return
	}
	h := s.handlers[t]
	if h == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("cfapi: callback %d panic: %v", t, r)
		}
	}()
	h((*CallbackInfo)(bitCast(infoPtr)), (*CallbackParameters)(bitCast(paramsPtr)))
}

// bitCast 把回调桩收到的地址位模式转为 unsafe.Pointer。
// 指针仅在回调期间有效（C 栈内存），满足 unsafe 规则的"由系统保证有效"豁免；
// 写成位重解释形式以避免 vet unsafeptr 的 uintptr→Pointer 模式告警。
func bitCast(u uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&u))
}

// Connect 连接同步根并注册回调表。handlers 会在连接期间被调用。
func Connect(path string, flags uint32, handlers map[CallbackType]CallbackFunc) (*Session, error) {
	if len(handlers) == 0 {
		return nil, fmt.Errorf("cfapi: no handlers")
	}
	s := &Session{handlers: handlers}
	// 固定顺序遍历（回调类型数值升序），保证注册表确定性
	for t := CallbackTypeFetchData; t <= CallbackTypeNotifyRenameCompletion; t++ {
		if _, ok := handlers[t]; !ok {
			continue
		}
		tt := t
		// 每个类型独立桩：桩本身不携带类型信息，只能靠闭包区分
		stub := syscall.NewCallback(func(infoPtr, paramsPtr uintptr) uintptr {
			invoke(tt, infoPtr, paramsPtr)
			return 0
		})
		s.regs = append(s.regs, CallbackRegistration{Type: uint32(tt), Callback: stub})
	}
	s.regs = append(s.regs, CallbackRegistration{Type: uint32(CallbackTypeNone), Callback: 0})

	p := utf16ptr(path)
	sessionMu.Lock()
	active = s
	sessionMu.Unlock()
	r1, _, _ := procConnectSyncRoot.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&s.regs[0])),
		0, // CallbackContext —— 未使用，统一走全局会话
		uintptr(flags),
		uintptr(unsafe.Pointer(&s.key)),
	)
	runtime.KeepAlive(s)
	runtime.KeepAlive(p)
	if err := hr(r1); err != nil {
		sessionMu.Lock()
		active = nil
		sessionMu.Unlock()
		return nil, err
	}
	return s, nil
}

// Disconnect 断开同步根连接（不注销注册表）。
func (s *Session) Disconnect() error {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	if active != s {
		return nil
	}
	r1, _, _ := procDisconnectSyncRoot.Call(uintptr(s.key))
	active = nil
	return hr(r1)
}

// TransferData 以 CF_OPERATION_TYPE_TRANSFER_DATA 响应 FETCH_DATA 回调，
// 把 buf 中的数据回填到占位符。buf 长度必须等于 length。
func (s *Session) TransferData(info *CallbackInfo, offset, length int64, buf []byte) error {
	opInfo := OperationInfo{
		StructSize:    uint32(unsafe.Sizeof(OperationInfo{})),
		Type:          OpTypeTransferData,
		ConnectionKey: info.ConnectionKey,
		TransferKey:   info.TransferKey,
	}
	params := OperationParameters{
		ParamSize: uint32(unsafe.Sizeof(OperationParameters{})),
		Offset:    offset,
		Length:    length,
	}
	if length > 0 {
		if int64(len(buf)) < length {
			return fmt.Errorf("cfapi: buffer shorter than length")
		}
		params.Buffer = uintptr(unsafe.Pointer(&buf[0]))
	}
	r1, _, _ := procExecute.Call(
		uintptr(unsafe.Pointer(&opInfo)),
		uintptr(unsafe.Pointer(&params)),
	)
	runtime.KeepAlive(buf)
	runtime.KeepAlive(&params)
	runtime.KeepAlive(&opInfo)
	return hr(r1)
}

// fetchdata 子结构在 OperationParameters 中的布局见 types.go。

// FailTransfer 以指定 NTSTATUS 失败当前 FETCH_DATA 请求；
// 挂起的用户 I/O 会立即以该状态失败（不会等 60s 超时）。
func (s *Session) FailTransfer(info *CallbackInfo, offset, length int64, status int32) error {
	opInfo := OperationInfo{
		StructSize:    uint32(unsafe.Sizeof(OperationInfo{})),
		Type:          OpTypeTransferData,
		ConnectionKey: info.ConnectionKey,
		TransferKey:   info.TransferKey,
	}
	params := OperationParameters{
		ParamSize:        uint32(unsafe.Sizeof(OperationParameters{})),
		CompletionStatus: status,
		Offset:           offset,
		Length:           length,
	}
	r1, _, _ := procExecute.Call(
		uintptr(unsafe.Pointer(&opInfo)),
		uintptr(unsafe.Pointer(&params)),
	)
	runtime.KeepAlive(&params)
	runtime.KeepAlive(&opInfo)
	return hr(r1)
}

// ---------- 占位符创建 ----------

// CreatePlaceholders 在 baseDir 下批量创建占位符，返回每个条目的 HRESULT。
// 重跑时已存在的条目返回 0x800700B7（ERROR_ALREADY_EXISTS）。
func CreatePlaceholders(baseDir string, items []NewPlaceholder) ([]int32, error) {
	if len(items) == 0 {
		return nil, nil
	}
	p := utf16ptr(baseDir)
	infos := make([]PlaceholderCreateInfo, len(items))
	names := make([][]uint16, len(items)) // KeepAlive：持有 UTF-16 缓冲
	for i, it := range items {
		u, err := syscall.UTF16FromString(it.RelativeFileName)
		if err != nil {
			return nil, fmt.Errorf("cfapi: bad name %q: %w", it.RelativeFileName, err)
		}
		names[i] = u
		flags := it.Flags
		if flags == 0 {
			flags = PlaceholderCreateFlagMarkInSync
		}
		infos[i] = PlaceholderCreateInfo{
			RelativeFileName: &names[i][0],
			FsMetadata: FsMetadata{
				BasicInfo: FileBasicInfo{
					CreationTime:   toFiletime(it.ModTime),
					LastAccessTime: toFiletime(it.ModTime),
					LastWriteTime:  toFiletime(it.ModTime),
					ChangeTime:     toFiletime(it.ModTime),
					FileAttributes: FileAttributeNormal,
				},
				FileSize: it.FileSize,
			},
			// 官方要求：文件的 FileIdentity 必填（≤4KB）
			FileIdentity:       bytesToPointer(it.Identity),
			FileIdentityLength: uint32(len(it.Identity)),
			Flags:              flags,
		}
	}
	var processed uint32
	r1, _, _ := procCreatePlaceholders.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&infos[0])),
		uintptr(len(infos)),
		uintptr(CreateFlagNone),
		uintptr(unsafe.Pointer(&processed)),
	)
	runtime.KeepAlive(infos)
	runtime.KeepAlive(names)
	runtime.KeepAlive(p)
	results := make([]int32, len(infos))
	for i := range infos {
		results[i] = infos[i].Result
	}
	// 整体失败（如根路径无效）时返回错误；条目级错误在 results 中
	if err := hr(r1); err != nil {
		return results, err
	}
	return results, nil
}

// ---------- in-sync 标记 ----------

const (
	fileAccessAttributes = 0x00000080 | 0x00000100 // FILE_READ_ATTRIBUTES | FILE_WRITE_ATTRIBUTES
	fileShareAll         = 0x00000001 | 0x00000002 | 0x00000004
	openExisting         = 3
)

// SetInSync 以属性级访问打开文件并标记 in-sync（云朵图标条件之一）。
// 属性级打开不读取数据，不会触发水合。
func SetInSync(path string) error {
	p := utf16ptr(path)
	h, err := syscall.CreateFile(p, fileAccessAttributes, fileShareAll, nil, openExisting, 0, 0)
	if err != nil {
		return fmt.Errorf("cfapi: open for in-sync: %w", err)
	}
	defer syscall.CloseHandle(h)
	r1, _, _ := procSetInSyncState.Call(
		uintptr(h),
		uintptr(InSyncStateInSync),
		uintptr(SetInSyncFlagNone),
		0, // InSyncUsn = NULL：不校验 USN
	)
	runtime.KeepAlive(p)
	return hr(r1)
}

// ConvertToPlaceholder 把用户在同步根里新建的普通文件转换为占位符。
// handleFetchData 之外的场景：本地新建文件上传成功后调用，一步完成
// 转换 + in-sync 标记（CF_CONVERT_FLAG_MARK_IN_SYNC）。
// FileIdentity 可选，故传 nil/0（后续 CfUpdatePlaceholder 可再补）。
//
// 只读属性文件（用户从只读源拷贝，v0.3.2 实测 3/18 这样）：Windows 对
// FILE_ATTRIBUTE_READONLY 的文件拒绝 GENERIC_WRITE 打开（ERROR_ACCESS_DENIED
// = "open for convert: Access is denied"）。这里临时清掉 ReadOnly → 转换 →
// 无论成败都恢复用户原有的属性（不静默改变用户文件属性）。
func ConvertToPlaceholder(path string, flags uint32) error {
	const attrReadOnly uint32 = 0x1
	p := utf16ptr(path)

	origAttr, attrErr := syscall.GetFileAttributes(p)
	readonly := attrErr == nil && origAttr&attrReadOnly != 0
	if readonly {
		if err := syscall.SetFileAttributes(p, origAttr&^attrReadOnly); err != nil {
			return fmt.Errorf("cfapi: clear readonly for convert: %w", err)
		}
	}
	// 转换需要通用写权限（与 CreateFile 的属性级访问不同）
	h, err := syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		fileShareAll, nil, openExisting, 0, 0)
	if err != nil {
		if readonly {
			_ = syscall.SetFileAttributes(p, origAttr) // 打开失败也要把只读还回去
		}
		return fmt.Errorf("cfapi: open for convert: %w", err)
	}
	defer syscall.CloseHandle(h)
	r1, _, _ := procConvertToPlaceholder.Call(
		uintptr(h),
		0, // FileIdentity = NULL
		0, // FileIdentityLength = 0
		uintptr(flags),
		0, // ConvertUsn = NULL
		0, // Overlapped = NULL
	)
	if readonly {
		_ = syscall.SetFileAttributes(p, origAttr) // 恢复只读（占位符上同样有效）
	}
	runtime.KeepAlive(p)
	return hr(r1)
}

// PlatformVersion 返回系统 Cloud Files 平台版本。
type PlatformVersion struct {
	BuildNumber      uint32
	RevisionNumber   uint32
	IntegrationNumber uint32
}

// GetPlatformVersion 查询平台版本（顺带验证 CldApi.dll 可用）。
func GetPlatformVersion() (*PlatformVersion, error) {
	v := &PlatformVersion{}
	r1, _, _ := procGetPlatformInfo.Call(uintptr(unsafe.Pointer(v)))
	if err := hr(r1); err != nil {
		return nil, err
	}
	return v, nil
}
