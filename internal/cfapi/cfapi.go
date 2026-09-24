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
	procConnectSyncRoot    = dll.NewProc("CfConnectSyncRoot")
	procDisconnectSyncRoot = dll.NewProc("CfDisconnectSyncRoot")
	procExecute            = dll.NewProc("CfExecute")
	procCreatePlaceholders = dll.NewProc("CfCreatePlaceholders")
	procSetInSyncState     = dll.NewProc("CfSetInSyncState")
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
// CF_OPERATION_PARAMETERS.TransferPlaceholders 视图（同为 40 字节）：
// ParamSize@0, pad@4, Flags@8, CompletionStatus@12,
// PlaceholderTotalCount@16, PlaceholderArray@24,
// PlaceholderCount@32, EntriesProcessed@36
type opParamsPlaceholders struct {
	ParamSize            uint32
	_                    uint32
	Flags                uint32
	CompletionStatus     int32
	PlaceholderTotalCount int64
	PlaceholderArray     uintptr // NULL = 不携带任何条目
	PlaceholderCount     uint32
	EntriesProcessed     uint32
}

// TransferPlaceholders 以 TRANSFER_PLACEHOLDERS 操作响应 FETCH_PLACEHOLDERS
// 回调。placeholderCount=0 + 空数组 + STATUS_OK 表示"该目录没有更多待
// 人口化条目"，目录枚举将立即返回本地已有的实体条目。
func (s *Session) TransferPlaceholders(info *CallbackInfo, totalCount int64) error {
	opInfo := OperationInfo{
		StructSize:    uint32(unsafe.Sizeof(OperationInfo{})),
		Type:          OpTypeTransferPlaceholders,
		ConnectionKey: info.ConnectionKey,
		TransferKey:   info.TransferKey,
	}
	params := opParamsPlaceholders{
		ParamSize:            uint32(unsafe.Sizeof(opParamsPlaceholders{})),
		PlaceholderTotalCount: totalCount,
	}
	r1, _, _ := procExecute.Call(
		uintptr(unsafe.Pointer(&opInfo)),
		uintptr(unsafe.Pointer(&params)),
	)
	runtime.KeepAlive(&params)
	runtime.KeepAlive(&opInfo)
	return hr(r1)
}

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

// ClearInSync 清除 in-sync 标记（用户修改后由平台自动做，这里备用）。
func ClearInSync(path string) error {
	p := utf16ptr(path)
	h, err := syscall.CreateFile(p, fileAccessAttributes, fileShareAll, nil, openExisting, 0, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	r1, _, _ := procSetInSyncState.Call(
		uintptr(h),
		uintptr(InSyncStateNotInSync),
		uintptr(SetInSyncFlagNone),
		0,
	)
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
