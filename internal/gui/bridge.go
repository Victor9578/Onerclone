package gui

// bridge.go —— GUI ↔ 同步运行时的桥。前端通过 Wails 绑定调用这些方法。
//
// 生命周期：startup 时异步 StartRuntime；窗口关闭（OnShutdown）时收尾。
// 状态查询直接读 state.Store（并发读安全）与引擎，不复制状态。

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"onerclone/internal/app"
	"onerclone/internal/state"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// StartupContext 是 Wails 的 context（OnStartup/OnShutdown 传入）。
type StartupContext = context.Context

// Bridge 是绑定给前端的对象（Wails 要求导出方法）。
type Bridge struct {
	mu  sync.Mutex
	ctx StartupContext
	rt  *app.Runtime
	err error // 启动失败原因（前端可查询）
}

// NewBridge 创建桥实例。
func NewBridge() *Bridge { return &Bridge{} }

func (b *Bridge) startup(ctx StartupContext) {
	b.ctx = ctx
	// GUI 进程无控制台 stdin：Runtime.Wait 不等回车
	app.SetHeadless(true)

	// 日志：GUI 模式写文件 + 转发前端（app.RunCLI 的控制台双写不经过这里；
	// app 包的日志初始化在 RunCLI 里，GUI 需要自己落盘）
	logPath := "onerclone.log"
	if exe, err := os.Executable(); err == nil && exe != "" {
		logPath = filepath.Join(filepath.Dir(exe), "onerclone.log")
	}
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		log.SetOutput(f)
	}
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("onerclone GUI 启动")

	// 异步启动同步运行时（cfapi 连接、rcd 启动可能要几秒；不阻塞窗口渲染）
	go func() {
		cfg := app.LoadConfigForGUI()
		rt, err := app.StartRuntime(app.Options{
			Root:      cfg.SyncRoot,
			Fs:        cfg.Fs,
			Remote:    cfg.Remote,
			RcloneExe: cfg.Rclone,
			Offline:   cfg.Offline,
		})
		b.mu.Lock()
		defer b.mu.Unlock()
		b.rt, b.err = rt, err
		if err != nil {
			log.Printf("同步运行时启动失败: %v", err)
		}
	}()
}

func (b *Bridge) shutdown() {
	b.mu.Lock()
	rt := b.rt
	b.mu.Unlock()
	if rt != nil {
		rt.Shutdown()
	}
}

// ---------- 状态查询（前端轮询） ----------

// StatusDTO 是 /status 一次轮询的载荷。
type StatusDTO struct {
	Running   bool             `json:"running"`
	Error     string           `json:"error,omitempty"`
	Root      string           `json:"root"`
	Remote    string           `json:"remote"`
	Offline   bool             `json:"offline"`
	Stats     map[string]int   `json:"stats"`     // 队列状态计数
	AuthFail  int              `json:"authFailed"` // 认证停摆动作数
	Actions   []ActionDTO      `json:"actions"`   // 活动动作（pending/inflight）
	UpdatedAt string           `json:"updatedAt"`
}

// ActionDTO 是一条队列动作。
type ActionDTO struct {
	ID       int64  `json:"id"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Class    string `json:"class"`
	Attempts int    `json:"attempts"`
	LastErr  string `json:"lastErr"`
}

// Status 返回当前同步状态（前端每 2s 调一次）。
func (b *Bridge) Status() StatusDTO {
	b.mu.Lock()
	rt, err := b.rt, b.err
	b.mu.Unlock()

	st := StatusDTO{
		Root:      app.DefaultRootForGUI(),
		Remote:    "",
		UpdatedAt: time.Now().Format("15:04:05"),
		Stats:     map[string]int{},
	}
	if rt != nil {
		st.Running = true
		st.Root = rt.App.SyncRoot()
		st.Remote = rt.App.RemoteDesc()
		st.Offline = rt.App.Offline()
	}
	if err != nil {
		st.Error = err.Error()
		return st
	}
	if rt == nil {
		return st // 还在启动
	}

	store := rt.Store()
	if m, err := store.Stats(); err == nil {
		for k, v := range m {
			st.Stats[string(k)] = v
		}
	}
	if n, err := store.CountAuthFailed(); err == nil {
		st.AuthFail = n
	}
	if acts, err := store.ListActions([]state.ActionState{state.StatePending, state.StateInflight}, 50); err == nil {
		for _, a := range acts {
			st.Actions = append(st.Actions, ActionDTO{
				ID: a.ID, Path: a.Path, Kind: string(a.Kind), State: string(a.State),
				Class: string(a.Class), Attempts: a.Attempts, LastErr: a.LastErr,
			})
		}
	}
	return st
}

// ---------- 动作（OneDrive 式弹窗里的按钮） ----------

// RetryFailed 把所有 failed 动作重新入队。
func (b *Bridge) RetryFailed() (int, error) {
	rt := b.runtime()
	if rt == nil {
		return 0, fmt.Errorf("同步引擎未启动")
	}
	return app.RetryAllFailed(rt.Store())
}

// OpenRoot 在资源管理器中打开同步根。
func (b *Bridge) OpenRoot() error {
	rt := b.runtime()
	if rt == nil {
		return fmt.Errorf("同步引擎未启动")
	}
	runtime.BrowserOpenURL(b.ctx, "file:///"+filepath.ToSlash(rt.App.SyncRoot()))
	return nil
}

// OpenLog 用系统默认程序打开日志文件。
func (b *Bridge) OpenLog() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	runtime.BrowserOpenURL(b.ctx, "file:///"+filepath.ToSlash(filepath.Join(filepath.Dir(exe), "onerclone.log")))
	return nil
}

// Quit 退出整个应用（托盘 + 同步 + 窗口）。
func (b *Bridge) Quit() {
	b.shutdown()
	runtime.Quit(b.ctx)
}

func (b *Bridge) runtime() *app.Runtime {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rt
}
