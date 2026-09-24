//go:build windows

// Package cfapi 是 Windows Cloud Files API（cfapi.h / CldApi.dll）的最小化
// Go 绑定。所有结构体按 Windows SDK 10.0.16299 头文件逐字段镜像，
// x64 布局已对照 reference/cfapi.h 核验（Go 与 MSVC x64 对齐规则一致）。
package cfapi

import (
	"time"
	"unsafe"
)

// ---------- 枚举与标志位 ----------

const (
	// CF_PLACEHOLDER_CREATE_FLAGS
	PlaceholderCreateFlagNone       uint32 = 0x00000000
	PlaceholderCreateFlagMarkInSync uint32 = 0x00000002

	// CF_REGISTER_FLAGS
	RegisterFlagNone   uint32 = 0x00000000
	RegisterFlagUpdate uint32 = 0x00000001

	// CF_CONNECT_FLAGS
	ConnectFlagNone             uint32 = 0x00000000
	ConnectFlagRequireProcessInfo uint32 = 0x00000002
	ConnectFlagRequireFullFilePath uint32 = 0x00000004

	// CF_CREATE_FLAGS
	CreateFlagNone       uint32 = 0x00000000
	CreateFlagStopOnError uint32 = 0x00000001

	// CF_HYDRATION_POLICY_PRIMARY
	HydrationPolicyPartial    uint16 = 0
	HydrationPolicyProgressive uint16 = 1
	HydrationPolicyFull       uint16 = 2
	HydrationPolicyAlwaysFull uint16 = 3

	// CF_POPULATION_POLICY_PRIMARY
	PopulationPolicyPartial    uint16 = 0
	PopulationPolicyFull       uint16 = 2
	PopulationPolicyAlwaysFull uint16 = 3

	// CF_INSYNC_POLICY（位标志）
	InSyncPolicyTrackFileAll uint32 = 0x0055550f
	InSyncPolicyTrackDirAll  uint32 = 0x00aaaaf0
	InSyncPolicyTrackAll     uint32 = 0x00ffffff

	// CF_HARDLINK_POLICY
	HardLinkPolicyNone     uint32 = 0x00000000
	HardLinkPolicyAllowed  uint32 = 0x00000001

	// CF_IN_SYNC_STATE
	InSyncStateNotInSync uint32 = 0
	InSyncStateInSync    uint32 = 1

	// CF_SET_IN_SYNC_FLAGS
	SetInSyncFlagNone uint32 = 0

	// 文件属性（winnt.h）
	FileAttributeNormal uint32 = 0x80

	// NTSTATUS —— 已在 ntstatus.h 核准（高位码以补码形式表示）
	StatusEndOfFile                   int32 = -1073741807 // 0xC0000011
	StatusCloudFileNetworkUnavailable int32 = -1073688815 // 0xC000CF11
)

// CF_OPERATION_TYPE
const (
	OpTypeTransferData         uint32 = 0
	OpTypeRetrieveData         uint32 = 1
	OpTypeAckData              uint32 = 2
	OpTypeRestartHydration     uint32 = 3
	OpTypeTransferPlaceholders uint32 = 4
	OpTypeAckDehydrate         uint32 = 5
	OpTypeAckDelete            uint32 = 6
	OpTypeAckRename            uint32 = 7
)

// CallbackType 对应 CF_CALLBACK_TYPE（顺序即枚举值，勿改动顺序）。
type CallbackType uint32

const (
	CallbackTypeFetchData CallbackType = iota // 0
	CallbackTypeValidateData                  // 1
	CallbackTypeCancelFetchData               // 2
	CallbackTypeFetchPlaceholders             // 3
	CallbackTypeCancelFetchPlaceholders       // 4
	CallbackTypeNotifyFileOpenCompletion      // 5
	CallbackTypeNotifyFileCloseCompletion     // 6
	CallbackTypeNotifyDehydrate               // 7
	CallbackTypeNotifyDehydrateCompletion     // 8
	CallbackTypeNotifyDelete                  // 9
	CallbackTypeNotifyDeleteCompletion        // 10
	CallbackTypeNotifyRename                  // 11
	CallbackTypeNotifyRenameCompletion        // 12
	CallbackTypeNone CallbackType = 0xffffffff
)

// ---------- 结构体（x64 布局，字段顺序不可调整） ----------

// FILE_BASIC_INFO（32 字节数据 + 4 字节属性 + 4 字节对齐填充 = 40）
type FileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

// CF_FS_METADATA = { FILE_BASIC_INFO; LARGE_INTEGER FileSize; } = 48 字节
type FsMetadata struct {
	BasicInfo FileBasicInfo
	FileSize  int64
}

// CF_PLACEHOLDER_CREATE_INFO（88 字节）
// 布局：ptr@0, FsMetadata@8, ptr@56, len@64, flags@68, result@72, usn@80
type PlaceholderCreateInfo struct {
	RelativeFileName   *uint16
	FsMetadata         FsMetadata
	FileIdentity       unsafe.Pointer
	FileIdentityLength uint32
	Flags              uint32
	Result             int32
	CreateUsn          int64
}

// CF_SYNC_REGISTRATION（56 字节）
type SyncRegistration struct {
	StructSize            uint32
	_                     uint32
	ProviderName          *uint16
	ProviderVersion       *uint16
	SyncRootIdentity      unsafe.Pointer // 必须用 unsafe.Pointer：GC 需追踪
	SyncRootIdentityLength uint32
	_                     uint32
	FileIdentity          unsafe.Pointer
	FileIdentityLength    uint32
	_                     uint32
}

// CF_SYNC_POLICIES（20 字节）
type SyncPolicies struct {
	StructSize uint32
	Hydration  struct {
		Primary  uint16
		Modifier uint16
	}
	Population struct {
		Primary  uint16
		Modifier uint16
	}
	InSync   uint32
	HardLink uint32
}

// CF_CALLBACK_REGISTRATION（16 字节）—— 表项，以 {NONE, NULL} 结尾。
type CallbackRegistration struct {
	Type     uint32
	Callback uintptr
}

// CF_CALLBACK_INFO（x64 共 144 字节）
// 布局核验：size@0, key@8, ctx@16, volGuid@24, volDos@32, serial@40,
// srFileId@48, srIdent@56, srIdentLen@64, fileId@72, fileSize@80,
// fileIdent@88, fileIdentLen@96, normPath@104, transferKey@112,
// prio@120, corrVec@128, procInfo@136
type CallbackInfo struct {
	StructSize            uint32
	_                     uint32
	ConnectionKey         uint64
	CallbackContext       uintptr
	VolumeGuidName        *uint16
	VolumeDosName         *uint16
	VolumeSerialNumber    uint32
	_                     uint32
	SyncRootFileId        int64
	SyncRootIdentity      uintptr
	SyncRootIdentityLength uint32
	_                     uint32
	FileId                int64
	FileSize              int64
	FileIdentity          uintptr
	FileIdentityLength    uint32
	_                     uint32
	NormalizedPath        *uint16
	TransferKey           int64
	PriorityHint          uint8
	_                     [7]uint8
	CorrelationVector     uintptr
	ProcessInfo           uintptr
}

// fetchdata 子结构（CF_CALLBACK_PARAMETERS 联合体的 FETCH_DATA 视图）
type FetchDataParams struct {
	Flags              uint32
	_                  uint32
	RequiredFileOffset int64
	RequiredLength     int64
	OptionalFileOffset int64
	OptionalLength     int64
}

// CF_CALLBACK_PARAMETERS（联合体按 FETCH_DATA 视图解释；ParamSize@0, 联合体@8）
type CallbackParameters struct {
	ParamSize uint32
	_         uint32
	FetchData FetchDataParams
}

// CF_OPERATION_INFO（32 字节）
type OperationInfo struct {
	StructSize        uint32
	Type              uint32
	ConnectionKey     uint64
	TransferKey       int64
	CorrelationVector uintptr
}

// CF_OPERATION_PARAMETERS（TransferData 视图，40 字节）
// 布局：ParamSize@0, pad@4, Flags@8, CompletionStatus@12,
// Buffer@16, Offset@24, Length@32
type OperationParameters struct {
	ParamSize        uint32
	_                uint32
	Flags            uint32
	CompletionStatus int32
	Buffer           uintptr
	Offset           int64
	Length           int64
}

// ---------- 便捷构造 ----------

// NewRegistration 构造注册结构；identities 会被结构体内的指针字段持有。
func NewRegistration(provider, version string, syncRootIdentity, fileIdentity []byte) *SyncRegistration {
	return &SyncRegistration{
		StructSize:             uint32(unsafe.Sizeof(SyncRegistration{})),
		ProviderName:           utf16ptr(provider),
		ProviderVersion:        utf16ptr(version),
		SyncRootIdentity:      bytesToPointer(syncRootIdentity),
		SyncRootIdentityLength: uint32(len(syncRootIdentity)),
		FileIdentity:          bytesToPointer(fileIdentity),
		FileIdentityLength:    uint32(len(fileIdentity)),
	}
}

// NewPolicies 构造默认策略：水合=FULL（首次访问拉全文件），
// 人口=ALWAYS_FULL（目录内容以本地为准，平台永不再问 FETCH_PLACEHOLDERS ——
// 与微软 CloudMirror 示例一致；PARTIAL 会导致每次路径解析都触发回调风暴
// 且未正确应答时目录枚举 60s 超时），in-sync 跟踪仅文件级。
func NewPolicies() *SyncPolicies {
	p := &SyncPolicies{
		StructSize: uint32(unsafe.Sizeof(SyncPolicies{})),
	}
	p.Hydration.Primary = HydrationPolicyFull
	p.Population.Primary = PopulationPolicyAlwaysFull
	p.InSync = InSyncPolicyTrackFileAll
	p.HardLink = HardLinkPolicyNone
	return p
}

// NewPlaceholder 描述一个待创建占位符（便捷输入）。
// Identity 会作为 FileIdentity 写入占位符（官方要求：文件必填，≤4KB），
// 并会在后续所有回调中回传给 provider。
type NewPlaceholder struct {
	RelativeFileName string
	FileSize          int64
	ModTime           time.Time
	Flags             uint32
	Identity          []byte
}

// FILETIME 转换：Unix 100ns → 自 1601-01-01 起的 100ns。
// 偏移 11644473600 秒 = 11644473600000000 个 100ns（先除后加避免溢出）。
func toFiletime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()/100 + 11644473600000000
}
