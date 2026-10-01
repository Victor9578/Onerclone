package app

// gui.go —— internal/app 暴露给 GUI（internal/gui）的导出面。
// CLI 内部用小写（loadConfig/app），GUI 走这几个导出包装。

// ConfigDTO 是 GUI 读到的合并后配置。
type ConfigDTO struct {
	SyncRoot string
	Fs       string
	Remote   string
	Rclone   string
	Offline  bool
}

// LoadConfigForGUI 读 onerclone.json（含默认值合并），不写模板。
func LoadConfigForGUI() ConfigDTO {
	c := loadConfigOpt(false)
	return ConfigDTO{SyncRoot: c.SyncRoot, Fs: c.Fs, Remote: c.Remote, Rclone: c.Rclone, Offline: c.Offline}
}

// DefaultRootForGUI 返回默认同步根（未启动时的占位显示）。
func DefaultRootForGUI() string { return defaultRoot() }

// ResolveRcloneForGUI 是 rclone 自动发现（flag > 配置 > exe 同目录 > PATH）
// 的 GUI 包装。CLI 在 cmdRun 里调，GUI 必须同样调，否则空路径 →
// StartRcd 报 exec: no command。
func ResolveRcloneForGUI(cfgVal string) string { return resolveRclone("", cfgVal) }

// SyncRoot 返回当前同步根。
func (a *app) SyncRoot() string { return a.syncRoot }

// RemoteDesc 返回当前云端目标描述（remote 名或替身目录）。
func (a *app) RemoteDesc() string {
	if a.fsRoot != "" {
		return a.fsRoot
	}
	return "(未配置)"
}

// Offline 返回离线模式标记。
func (a *app) Offline() bool { return a.offline }

// Store 返回状态库的 GUI 只读面（并发读安全；Engine 是单写者）。
func (r *Runtime) Store() StoreView { return r.App.store }

// RetryAllFailed 把所有 failed 动作重置为 pending（GUI「重试」按钮）。
// 返回重置条数。
func RetryAllFailed(v StoreView) (int, error) { return retryAllFailed(v) }
