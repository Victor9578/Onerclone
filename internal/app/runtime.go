package app

// runtime.go —— 同步运行时的可复用封装（GUI 与 CLI 共用）。
//
// StartRuntime 启动完整同步链路（rclone rcd → cfapi 连接 → 状态库 →
// 引擎 → 托盘 → watcher/轮询），返回 *Runtime。Wait 阻塞到退出信号；
// Shutdown 幂等收尾。GUI 进程（Wails）与 CLI（onerclone run）走同一条路，
// 保证行为一致、bug 集中在一处复现。

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"onerclone/internal/cfapi"
	"onerclone/internal/engine"
	"onerclone/internal/rclone"
	"onerclone/internal/state"
)

// Options 是 StartRuntime 的启动参数（flag/config 已在调用方解析合并）。
type Options struct {
	Root      string // 同步根（NTFS，绝对路径）
	Fs        string // 本地替身目录（仅 Remote 为空时使用）
	Remote    string // 真实 rclone remote（如 quark:）；空 = 替身模式
	RcloneExe string // rclone 可执行文件
	Offline   bool   // 离线模式：水合立即失败
	// Headless 为 true 时 Wait 不监听 stdin（GUI 进程无控制台）。
	Headless bool
}

// Runtime 是一个运行中的同步实例。
type Runtime struct {
	App  *app
	Eng  *engine.Engine
	Stop func() // 幂等收尾（等价旧 cmdRun 的 defer 链）

	quit     chan struct{} // Shutdown 关闭它 → Wait 返回
	quitOnce sync.Once
}

// StartRuntime 启动同步运行时。失败时已启动的部分会被收尾。
func StartRuntime(opts Options) (*Runtime, error) {
	dstFs := filepath.ToSlash(opts.Fs)
	if opts.Remote != "" {
		dstFs = quarkRemoteFlag(opts.Remote)
		log.Printf("☁️ 使用真实 remote: %s（需已登录）", dstFs)
	}

	a := &app{
		syncRoot: opts.Root,
		fsRoot:   dstFs,
		offline:  opts.Offline,
		pollKick: make(chan struct{}, 1),
	}

	// 收尾链（LIFO，与旧 cmdRun 的 defer 顺序一致）
	var cleanups []func()
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	fail := func(err error) (*Runtime, error) {
		cleanup()
		return nil, err
	}

	// 1) 验证平台可用
	if v, err := cfapi.GetPlatformVersion(); err != nil {
		return fail(fmt.Errorf("CfGetPlatformInfo 失败（系统不支持 Cloud Files API？需 Win10 1709+）: %w", err))
	} else {
		log.Printf("Cloud Files 平台版本: build=%d rev=%d int=%d", v.BuildNumber, v.RevisionNumber, v.IntegrationNumber)
	}

	// 2) 准备数据与本地目录（本地替身模式才写示例；真实 remote 不动云端）
	if opts.Remote == "" {
		if err := ensureSample(a.fsRoot); err != nil {
			return fail(fmt.Errorf("准备示例数据失败: %w", err))
		}
	}
	if err := os.MkdirAll(a.syncRoot, 0o755); err != nil {
		return fail(fmt.Errorf("创建同步根失败: %w", err))
	}

	// 3) 启动 rclone rcd 子进程（FR5：断线自动重启 + client 热替换）
	cloud := &cloudRC{srcFs: a.syncRoot, dstFs: a.fsRoot}
	daemon, err := rclone.StartRcd(opts.RcloneExe)
	if err != nil {
		return fail(fmt.Errorf("启动 rclone rcd 失败: %w", err))
	}
	var curDaemon atomic.Pointer[rclone.Daemon]
	curDaemon.Store(daemon)
	rcdDone := make(chan struct{})
	cleanups = append(cleanups, func() { close(rcdDone) }) // 先停监控，再停 rcd
	cleanups = append(cleanups, func() {
		if d := curDaemon.Load(); d != nil {
			d.Stop()
		}
	})
	a.rc.Store(daemon.Client)
	cloud.SetClient(daemon.Client)
	log.Printf("rclone rcd 就绪: %s", daemon.Client.Base)
	// 监控 rcd 退出 → 指数退避自动重启（5s→10s→20s→30s 封顶，无限重试），
	// 重启成功后热替换 client：引擎/水合回调无感知，队列在退避期间自动挂起恢复
	go func() {
		for {
			d := curDaemon.Load()
			var err error
			select {
			case <-rcdDone:
				return // 应用退出：不再重启，避免孤儿 rcd
			case err = <-d.Exited:
			}
			log.Printf("✗ rclone rcd 退出: %v，5s 后自动重启\n--- 输出 ---\n%s",
				err, trimOutput(d.Output()))
			backoff := 5 * time.Second
			for {
				select {
				case <-rcdDone:
					return
				case <-time.After(backoff):
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
				nd, err := rclone.StartRcd(opts.RcloneExe)
				if err != nil {
					log.Printf("⚠ rcd 重启失败（%v），退避后重试", err)
					continue
				}
				// 重启成功瞬间应用退出：立即停掉新进程，不留孤儿
				select {
				case <-rcdDone:
					nd.Stop()
					return
				default:
				}
				curDaemon.Store(nd)
				a.rc.Store(nd.Client)
				cloud.SetClient(nd.Client)
				log.Printf("✅ rclone rcd 已重启: %s", nd.Client.Base)
				break
			}
		}
	}()

	// 4) 确保注册策略为最新（Population=ALWAYS_FULL 需重新注册才生效），
	// 然后连接同步根（只注册 FETCH_DATA —— ALWAYS_FULL 下平台不会问
	// FETCH_PLACEHOLDERS，与微软 CloudMirror 示例一致）
	if err := migrateSyncRoot(a.syncRoot); err != nil {
		log.Printf("⚠ 同步根迁移检查失败（继续尝试注册当前根）: %v", err)
	}
	if err := cfapi.RegisterSyncRoot(a.syncRoot,
		cfapi.NewRegistration(providerName, providerVersion(), []byte(syncRootID), []byte(fileIdentityID)),
		cfapi.NewPolicies(), cfapi.RegisterFlagUpdate); err != nil {
		log.Printf("⚠ 同步根策略更新失败（继续用现有注册）: %v", err)
	} else {
		log.Print("同步根策略已更新: Population=ALWAYS_FULL, Hydration=FULL")
	}
	a.session, err = cfapi.Connect(a.syncRoot, cfapi.ConnectFlagRequireFullFilePath, map[cfapi.CallbackType]cfapi.CallbackFunc{
		cfapi.CallbackTypeFetchData: a.handleFetchData,
	})
	if err != nil {
		return fail(fmt.Errorf("CfConnectSyncRoot 失败（同步根未注册？先跑 register）: %w", err))
	}
	cleanups = append(cleanups, func() {
		_ = a.session.UpdateProviderStatus(cfapi.ProviderStatusDisconnected)
		_ = a.session.Disconnect()
	})
	if err := a.session.UpdateProviderStatus(cfapi.ProviderStatusIdle); err != nil {
		log.Printf("⚠ 上报 provider IDLE 失败（Explorer 可能显示同步中）: %v", err)
	}
	log.Print("同步根已连接（FETCH_DATA 回调在线）")

	// 4.5) Shell 集成自动补注册（v0.2.0 用户反馈问题①）：CfRegisterSyncRoot
	// 只做内核层注册，不写 Explorer 依赖的 SyncRootManager 注册表项 →
	// 双击 exe 直接跑的成品用户永远看不到云朵/绿勾图标。这里每次启动
	// 自动补写（幂等），失败只记日志不影响同步。
	if err := shellRegister(a.syncRoot); err != nil {
		log.Printf("⚠ Shell 注册失败（状态图标将不显示，同步不受影响）: %v", err)
	} else {
		log.Print("✅ Shell 注册成功（SyncRootManager ✓ 状态图标已启用）")
	}
	// 同步根本身也标记 in-sync，避免 Explorer 把根目录一直显示成“同步中”。
	if err := cfapi.SetInSync(a.syncRoot); err != nil {
		log.Printf("⚠ 标记同步根 in-sync 失败: %v", err)
	}

	// 5) 打开 P1 状态库 + 建引擎（三库模型：local_snap/cloud_snap/queue）
	statePath := statePath()
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return fail(fmt.Errorf("创建状态目录失败: %w", err))
	}
	a.store, err = state.Open(statePath)
	if err != nil {
		return fail(fmt.Errorf("打开状态库失败: %w", err))
	}
	cleanups = append(cleanups, func() { _ = a.store.Close() })
	log.Printf("状态库: %s", statePath)

	// remote 指纹（踩坑 #13）：换了 remote 旧 cloud_snap 会把条目判
	// “云端已删”→ 误删本地。指纹变更即清空 cloud_snap + 重置基线。
	remoteFP := a.fsRoot
	if v, ok, err := a.store.GetMeta("remote_fingerprint"); err != nil {
		log.Printf("⚠ 读 remote 指纹失败: %v", err)
	} else if ok && v != remoteFP {
		if err := a.store.ResetCloudSnap(); err != nil {
			return fail(fmt.Errorf("切换 remote 后重置云端快照失败: %w", err))
		}
		log.Printf("🔄 检测到 remote 变更（%s → %s）：已清空云端快照并重建基线", v, remoteFP)
	}
	if err := a.store.SetMeta("remote_fingerprint", remoteFP); err != nil {
		log.Printf("⚠ 写 remote 指纹失败: %v", err)
	}

	// 同步根标识（踩坑 #27）：根被重建/清空过就复位本地快照 + 基线。
	if err := ensureRootMarker(a.syncRoot, a.store); err != nil {
		log.Printf("⚠ 同步根标识检查失败: %v", err)
	}

	// 云端名↔本地名 映射表（从 meta 恢复上次的改名记录）
	a.nm = newNameMap(a.store)
	cloud.nm = a.nm

	a.eng = engine.New(a.store, cloud, &localFS{root: a.syncRoot, nm: a.nm}, log.Default())

	// 托盘常驻：登录走 CLI，状态走 Explorer。
	trayRoot.Store(a.syncRoot)
	if v, ok := logFilePath.Load().(string); ok {
		trayLog.Store(v)
	}
	startTray()
	log.Print("📌 托盘已启动：同步根 / 日志 / 退出（右键图标）")

	// 6) 首轮基线：本地扫描 + 云端轮询（DR2 之前无删除）
	if n, err := a.eng.Scan(); err != nil {
		log.Printf("⚠ 首轮本地扫描失败: %v", err)
	} else {
		log.Printf("首轮本地扫描：入队 %d", n)
	}
	if n, err := a.eng.Poll(); err != nil {
		// 基线未建立时 DR2 会保护删除/覆盖，后续 pollLoop 会自动重试
		log.Printf("⚠ 首轮云端轮询失败（基线未建立，DR2 保护仍生效，后续轮询自动补）: %v", err)
	} else {
		log.Printf("首轮云端轮询：入队 %d", n)
		if restored, err := a.eng.RestoreInSync(); err != nil {
			log.Printf("⚠ 恢复 in-sync 状态失败: %v", err)
		} else if restored > 0 {
			log.Printf("✅ 启动时恢复 %d 个文件的绿勾状态", restored)
		}
	}

	// 7) 引擎执行器（动作队列 worker，崩溃残留自动回收）
	workerStop := make(chan struct{})
	cleanups = append(cleanups, func() { close(workerStop) })
	go a.eng.RunWorker(workerStop, 8)

	// 8) 分层轮询（Q15）：活跃 60s / 空闲 5min；每 12h 强制一次
	go a.pollLoop()

	// 9) 监听本地改动（防抖 3s）
	w, err := a.startWatcher()
	if err != nil {
		return fail(fmt.Errorf("启动监听失败: %w", err))
	}
	cleanups = append(cleanups, func() { _ = w.Close() })

	rt := &Runtime{App: a, Eng: a.eng, quit: make(chan struct{})}
	var stopOnce sync.Once
	rt.Stop = func() {
		stopOnce.Do(func() {
			rt.quitOnce.Do(func() { close(rt.quit) })
			cleanup()
		})
	}

	mode := "在线"
	if a.offline {
		mode = "离线（水合立即失败）"
	}
	log.Printf("READY [%s] 同步根: %s", mode, a.syncRoot)
	return rt, nil
}

// Wait 阻塞直到退出：Shutdown()、托盘“退出”、Ctrl+C，或（非 Headless）
// 控制台回车。返回后调用方应调用 Stop()（幂等）完成收尾。
func (r *Runtime) Wait() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	stdinQuit := make(chan struct{})
	if !r.isHeadless() {
		go func() {
			bufio.NewReader(os.Stdin).ReadString('\n')
			close(stdinQuit)
		}()
	}

	select {
	case <-r.quit:
	case <-sig:
	case <-trayQuit:
	case <-stdinQuit:
	}
	log.Print("退出中…")
}

// Shutdown 请求退出并收尾（幂等；GUI 关窗时调用）。
func (r *Runtime) Shutdown() { r.Stop() }

var runtimeHeadless atomic.Bool

// SetHeadless 标记当前进程为 GUI（无控制台 stdin 可等）。
func SetHeadless(v bool) { runtimeHeadless.Store(v) }
func (r *Runtime) isHeadless() bool { return runtimeHeadless.Load() }

var _ = io.Discard // 保持 io 导入（日志双写在 main.go）
