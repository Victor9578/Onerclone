//go:build windows

// spike 是 Phase 0 技术验证程序，目标是打通并验证三条开放路径：
//
//  1. 非管理员进程能否 CfRegisterSyncRoot（开放事实①）
//  2. 离线打开未水合占位符是否"快速失败而非卡死"（开放事实②）
//  3. Go 全链路：占位符 → FETCH_DATA → rclone RC 取流 → TRANSFER_DATA 回填
//     → 本地改动防抖上传 → in-sync 标记（开放事实③ + 回声观察）
//
// 用法：
//
//	spike register   [-root DIR]     注册同步根（普通用户尝试）
//	spike unregister [-root DIR]     注销同步根
//	spike run        [-root DIR] [-fs DIR] [-rclone EXE] [-offline]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"onerclone/internal/cfapi"
	"onerclone/internal/engine"
	"onerclone/internal/rclone"
	"onerclone/internal/state"

	"github.com/fsnotify/fsnotify"
)

const (
	hrAlreadyExists = 0x800700B7 // ERROR_ALREADY_EXISTS → HRESULT
	hrAccessDenied  = 0x80070005 // ERROR_ACCESS_DENIED → HRESULT

	// 写入防抖窗口（FR4）
	debounceWindow = 3 * time.Second
)

const (
	providerName    = "Onerclone"
	syncRootID      = "onerclone-spike-root-1"
	fileIdentityID  = "onerclone-spike-file-1"
)

// providerVersion 用 ldflags 注入的版本号（dev 构建为 "vdev"）。
func providerVersion() string { return "v" + version }

type app struct {
	syncRoot string // 本地同步根（NTFS）
	fsRoot   string // "云端"替身：rclone 以 local 后端服务的目录（正斜杠）
	// RC 客户端（atomic 热替换：rcd 断线自动重启后换新，FR5）
	rc      atomic.Pointer[rclone.Client]
	session *cfapi.Session
	offline bool

	// P1 引擎（双向同步：快照 diff + 动作队列）
	eng   *engine.Engine
	store *state.Store

	// 云端名↔本地名 映射（DR4 方案 A：Windows 非法字符），见 namemap.go
	nm *nameMap

	// watcher 防抖后踢 pollLoop 立即轮询（本地有活动 = 活跃期语义）
	pollKick chan struct{}

	// watcher 触发全量扫描的节流（引擎自身有 mu，这里防扫描风暴）
}

func defaultRoot() string {
	return filepath.Join(os.Getenv("USERPROFILE"), "OnercloneSpike")
}

func defaultFs() string {
	abs, err := filepath.Abs(filepath.Join("spike-remote"))
	if err != nil {
		return "spike-remote"
	}
	return abs
}

// setConsoleUTF8 把控制台输入/输出代码页设为 UTF-8（65001）。
// Go 输出的是 UTF-8，传统 conhost（chcp 936）会把中文日志渲染成乱码；
// 幂等调用，无控制台（服务/重定向）时静默失败。
func setConsoleUTF8() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	const cpUTF8 = 65001
	_, _, _ = kernel32.NewProc("SetConsoleOutputCP").Call(cpUTF8)
	_, _, _ = kernel32.NewProc("SetConsoleCP").Call(cpUTF8)
}

// isInfoCmd 判断是否是纯信息类命令（不写日志、不读配置）。
func isInfoCmd(arg string) bool {
	switch arg {
	case "version", "-version", "--version", "help", "-h", "--help":
		return true
	}
	return false
}

// 版本信息：build.ps1 用 -ldflags "-X main.version=... -X main.buildDate=..." 注入。
var (
	version   = "dev"
	buildDate = "unknown"
)

// printVersion 输出版本号（`onerclone version`）。
func printVersion() {
	fmt.Printf("onerclone %s (build %s)\n", version, buildDate)
}

// trayQuit 由托盘“退出”菜单触发（与 Ctrl+C 等价）。
var trayQuit = make(chan struct{}, 1) // 缓冲 1：cmdRun 未就绪时托盘退出也不丢

// logFilePath 是当前日志路径（托盘“打开日志”用）。
var logFilePath atomic.Value

func main() {
	// 控制台代码页改 UTF-8：Go 输出的是 UTF-8，而传统 conhost（chcp 936）
	// 会把中文日志渲染成乱码。不改也能跑，只是显示问题（实测 help 输出字节合法）
	setConsoleUTF8()

	// version/help 是纯信息命令：不落日志（否则在发布目录跑一次版本号
	// 就会在 exe 旁留个 onerclone.log，污染打包产物）
	if len(os.Args) < 2 || !isInfoCmd(os.Args[1]) {
		// 日志同时输出到控制台与 onerclone.log（排查用：控制台会被回调刷屏）。
		// 路径固定在 **exe 同目录**（相对路径会跟着启动时的 CWD 跑偏）。
		logPath := "onerclone.log"
		if exe, err := os.Executable(); err == nil && exe != "" {
			logPath = filepath.Join(filepath.Dir(exe), "onerclone.log")
		}
		logFilePath.Store(logPath)
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			defer logFile.Close()
			log.SetOutput(io.MultiWriter(os.Stdout, logFile))
		}
		log.SetFlags(log.Ltime | log.Lmicroseconds)
		log.Printf("onerclone %s (build %s)", version, buildDate)
	}
	if len(os.Args) < 2 {
		// 成品默认行为：无参数 = 开始同步（配置见 exe 同目录 onerclone.json）
		cmdRun(nil)
		return
	}
	switch os.Args[1] {
	case "register":
		cmdRegister(os.Args[2:])
	case "unregister":
		cmdUnregister(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "quark-login":
		cmdQuarkLogin(os.Args[2:])
	case "login":
		cmdLogin(os.Args[2:])
	case "autostart":
		cmdAutostart(os.Args[2:])
	case "version", "-version", "--version":
		printVersion()
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Printf(`onerclone %s —— 夸克网盘 Windows 原生同步（Cloud Files 占位符）

用法:
  onerclone                    直接开始同步（读 exe 同目录 onerclone.json）
  onerclone run [flags]        同上，flag 可覆盖配置
  onerclone quark-login        扫码登录夸克（需真实控制台；-tries N 超时自动重出二维码）
  onerclone login [-type T]    通用登录任意 rclone 后端（dropbox/onedrive/…；省略 -type 列出全部）
  onerclone autostart          查询开机自启；-enable/-disable 开关
  onerclone register   [-root DIR]   注册同步根（内核 + Shell 图标层）
  onerclone unregister [-root DIR]   注销同步根
  onerclone version            版本信息
  onerclone help               本帮助

run 的 flag（均覆盖配置）:
  -root DIR      同步根（必须 NTFS）
  -remote ADDR   rclone remote（如 quark:）；空=读配置，配置空=用 -fs 本地替身
  -fs DIR        本地替身目录（仅 remote 为空时）
  -rclone EXE    rclone 可执行文件（默认：配置 > exe 同目录 > PATH）
  -offline       离线模式：水合请求立即快速失败

配置: exe 同目录 onerclone.json（首次运行自动生成模板）
日志: 同目录 onerclone.log（控制台 + 文件双写）
`, version)
}

// ---------- register / unregister ----------

// rootMarkerName 是放进同步根的身份标识文件（localFS.Scan 跳过它，不同步）。
const rootMarkerName = ".onerclone-root"

// ensureRootMarker 踩坑 #27 的另一半：标识文件在 = 这个根还是我们认识的
// 那块数据；缺失（根被新建/整删重建/清空过）→ 清空本地快照 + 重置基线
// （cloud_snap 保留：云端与根无关，保留可避免换根后的同名文件被误判冲突）。
// 幂等：复位成功后写入标识，后续启动不再触发。
func ensureRootMarker(root string, st *state.Store) error {
	p := filepath.Join(root, rootMarkerName)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if err := st.ResetLocalSnap(); err != nil {
		return fmt.Errorf("复位本地快照: %w", err)
	}
	log.Print("🧹 同步根标识缺失（根是新建/重建的）：已清空本地快照并重置基线（DR2 重新保护；云端快照保留）")
	if err := os.WriteFile(p, []byte("onerclone sync root marker\n"), 0o644); err != nil {
		return fmt.Errorf("写标识文件: %w", err)
	}
	return nil
}

func isElevated() bool {
	const tokenElevationClass = 20 // TokenElevation
	advapi := syscall.NewLazyDLL("advapi32.dll")
	proc := advapi.NewProc("GetTokenInformation")
	var te struct{ TokenIsElevated uint32 }
	var outLen uint32
	procHandle, _ := syscall.GetCurrentProcess()
	r1, _, _ := proc.Call(
		uintptr(procHandle),
		uintptr(tokenElevationClass),
		uintptr(unsafe.Pointer(&te)),
		uintptr(unsafe.Sizeof(te)),
		uintptr(unsafe.Pointer(&outLen)),
	)
	return r1 != 0 && te.TokenIsElevated != 0
}

func cmdRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	root := fs.String("root", "", "同步根目录（必须在 NTFS 上；默认读 onerclone.json）")
	_ = fs.Parse(args)
	*root = pick(*root, loadConfig().SyncRoot, defaultRoot())

	if err := os.MkdirAll(*root, 0o755); err != nil {
		log.Fatalf("创建同步根失败: %v", err)
	}

	elev := isElevated()
	log.Printf("当前进程 elevated = %v", elev)

	reg := cfapi.NewRegistration(providerName, providerVersion(), []byte(syncRootID), []byte(fileIdentityID))
	pol := cfapi.NewPolicies()

	err := cfapi.RegisterSyncRoot(*root, reg, pol, cfapi.RegisterFlagNone)
	if err != nil {
		code, _ := cfapi.AsHRESULT(err)
		if code == hrAlreadyExists {
			log.Print("同步根已存在，以 UPDATE 标志重新注册…")
			err = cfapi.RegisterSyncRoot(*root, reg, pol, cfapi.RegisterFlagUpdate)
		}
	}
	if err != nil {
		code, _ := cfapi.AsHRESULT(err)
		switch code {
		case hrAccessDenied:
			log.Printf("❌ 注册被拒绝 (0x%08X) = ACCESS_DENIED，elevated=%v", code, elev)
			log.Print("→ 结论：普通用户无法注册，需要 UAC 提权一次")
		default:
			log.Printf("❌ 注册失败: %v, elevated=%v", err, elev)
		}
		os.Exit(1)
	}
	log.Printf("✅ 注册成功，elevated=%v", elev)
	// Shell 集成：SyncRootManager 注册表项（Explorer 状态图标依赖此层，
	// CfRegisterSyncRoot 不写它）——不写 = 云朵/绿勾图标永远不显示
	if err := shellRegister(*root); err != nil {
		log.Printf("⚠ Shell 注册失败（图标将不显示）: %v", err)
	} else {
		log.Print("✅ Shell 注册成功（SyncRootManager ✓ 图标将显示）")
	}
	if !elev {
		log.Print("→ 开放事实①结论：普通用户即可注册，安装流程无需 UAC！")
	} else {
		log.Print("→ 注意：当前为提权进程，需再以普通用户验证一次才能下结论")
	}
	log.Printf("同步根: %s", *root)
}

func cmdUnregister(args []string) {
	fs := flag.NewFlagSet("unregister", flag.ExitOnError)
	root := fs.String("root", "", "同步根目录（默认读 onerclone.json）")
	_ = fs.Parse(args)
	*root = pick(*root, loadConfig().SyncRoot, defaultRoot())
	if err := cfapi.UnregisterSyncRoot(*root); err != nil {
		log.Fatalf("注销失败（若程序在跑请先退出）: %v", err)
	}
	if err := shellUnregister(); err != nil {
		log.Printf("⚠ Shell 注销失败（残留 SyncRootManager 键）: %v", err)
	}
	log.Print("✅ 已注销同步根（内核 + Shell）")
}

// ---------- run ----------

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	root := fs.String("root", "", "同步根目录（默认读 onerclone.json）")
	fsDir := fs.String("fs", "", "云端替身目录（rclone local 后端；默认读配置）")
	remote := fs.String("remote", "", "真实 rclone remote（如 quark:）；空=读配置，配置空=用 -fs")
	rcloneExe := fs.String("rclone", "", "rclone 可执行文件（默认：配置 > exe 同目录 > PATH）")
	offline := fs.Bool("offline", false, "离线模式：水合请求立即快速失败（覆盖配置）")
	_ = fs.Parse(args)

	// 优先级：flag > onerclone.json > 内置默认（成品化）
	cfg := loadConfig()
	*root = pick(*root, cfg.SyncRoot, defaultRoot())
	*fsDir = pick(*fsDir, cfg.Fs, defaultFs())
	if *remote == "" {
		*remote = cfg.Remote
	}
	*offline = *offline || cfg.Offline
	*rcloneExe = resolveRclone(*rcloneExe, cfg.Rclone)

	dstFs := filepath.ToSlash(*fsDir)
	if *remote != "" {
		dstFs = quarkRemoteFlag(*remote)
		log.Printf("☁️ 使用真实 remote: %s（需已 quark-login）", dstFs)
	}

	a := &app{
		syncRoot: *root,
		fsRoot:   dstFs,
		offline:  *offline,
		pollKick: make(chan struct{}, 1),
	}

	// 1) 验证平台可用
	if v, err := cfapi.GetPlatformVersion(); err != nil {
		log.Fatalf("CfGetPlatformInfo 失败（系统不支持 Cloud Files API？需 Win10 1709+）: %v", err)
	} else {
		log.Printf("Cloud Files 平台版本: build=%d rev=%d int=%d", v.BuildNumber, v.RevisionNumber, v.IntegrationNumber)
	}

	// 2) 准备数据与本地目录（本地替身模式才写示例；真实 remote 不动云端）
	if *remote == "" {
		if err := ensureSample(a.fsRoot); err != nil {
			log.Fatalf("准备示例数据失败: %v", err)
		}
	}
	if err := os.MkdirAll(a.syncRoot, 0o755); err != nil {
		log.Fatalf("创建同步根失败: %v", err)
	}

	// 3) 启动 rclone rcd 子进程（FR5：断线自动重启 + client 热替换）
	cloud := &cloudRC{srcFs: a.syncRoot, dstFs: a.fsRoot}
	daemon, err := rclone.StartRcd(*rcloneExe)
	if err != nil {
		log.Fatalf("启动 rclone rcd 失败: %v", err)
	}
	var curDaemon atomic.Pointer[rclone.Daemon]
	curDaemon.Store(daemon)
	rcdDone := make(chan struct{})
	defer close(rcdDone) // 先于下面的 Stop 执行（LIFO）：停掉监控免重启孤儿
	defer func() { // 闭包捕获 holder（直接 defer daemon.Stop 会绑定旧实例）
		if d := curDaemon.Load(); d != nil {
			d.Stop()
		}
	}()
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
				nd, err := rclone.StartRcd(*rcloneExe)
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
	// v0.2.0 反馈问题②：换根自动迁移——若配置的根不是本 provider 注册的
	// 根（用户改了 sync_root），自动注销旧根再注册新根，避免 Explorer 里
	// 旧目录残留云图标、新目录不生效。
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
		log.Fatalf("CfConnectSyncRoot 失败（同步根未注册？先跑 register）: %v", err)
	}
	if err := a.session.UpdateProviderStatus(cfapi.ProviderStatusIdle); err != nil {
		log.Printf("⚠ 上报 provider IDLE 失败（Explorer 可能显示同步中）: %v", err)
	}
	defer func() {
		_ = a.session.UpdateProviderStatus(cfapi.ProviderStatusDisconnected)
		_ = a.session.Disconnect()
	}()
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
	// v0.2.0 反馈问题②：state.db 固定放 exe 同目录（或用户数据目录），
	// 与同步根**解耦**——换根不丢队列/基线/namemap，旧根下不再残留状态库。
	statePath := statePath()
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		log.Fatalf("创建状态目录失败: %v", err)
	}
	a.store, err = state.Open(statePath)
	if err != nil {
		log.Fatalf("打开状态库失败: %v", err)
	}
	defer a.store.Close()
	log.Printf("状态库: %s", statePath)

	// remote 指纹（踩坑 #13）：cloud_snap 是"上次在哪个后端看到什么"的
	// 真相，换了 remote 旧快照里的条目在新后端不存在 → 会被判"云端已删"
	// → 误删本地文件。指纹变更即清空 cloud_snap + 重置基线（DR2 重新保护）。
	remoteFP := a.fsRoot
	if v, ok, err := a.store.GetMeta("remote_fingerprint"); err != nil {
		log.Printf("⚠ 读 remote 指纹失败: %v", err)
	} else if ok && v != remoteFP {
		if err := a.store.ResetCloudSnap(); err != nil {
			log.Fatalf("切换 remote 后重置云端快照失败: %v", err)
		}
		log.Printf("🔄 检测到 remote 变更（%s → %s）：已清空云端快照并重建基线", v, remoteFP)
	}
	if err := a.store.SetMeta("remote_fingerprint", remoteFP); err != nil {
		log.Printf("⚠ 写 remote 指纹失败: %v", err)
	}

	// 同步根标识（踩坑 #27）：路径相同 ≠ 同一块数据 —— 用户删掉整个根后
	// cmdRun 的 MkdirAll 会静默重建空目录，此时 local_snap 还是旧根的真相
	// → 空扫描把全部条目判"本地已删"→ delete_cloud 清空云端（今天 15:12
	// 换根已险些发生一次）。标识文件缺失（新建/重建/清空过根）就复位
	// 本地快照 + 基线，让 DR2 在重建基线期间挡住一切删除。
	if err := ensureRootMarker(a.syncRoot, a.store); err != nil {
		log.Printf("⚠ 同步根标识检查失败: %v", err)
	}

	// 云端名↔本地名 映射表（从 meta 恢复上次的改名记录）
	a.nm = newNameMap(a.store)
	cloud.nm = a.nm

	a.eng = engine.New(a.store,
		cloud,
		&localFS{root: a.syncRoot, nm: a.nm}, log.Default())

	// 托盘常驻（Phase 2）：网页面板从运行路径移除，登录走 CLI，状态走 Explorer。
	trayRoot.Store(a.syncRoot)
	if v, ok := logFilePath.Load().(string); ok {
		trayLog.Store(v)
	}
	startTray()
	log.Print("📌 托盘已启动：同步根 / 日志 / 退出（右键图标）")

	// 6) 首轮基线：本地扫描 + 云端轮询 → 建立三库快照（DR2 之前无删除）
	if n, err := a.eng.Scan(); err != nil {
		log.Printf("⚠ 首轮本地扫描失败: %v", err)
	} else {
		log.Printf("首轮本地扫描：入队 %d", n)
	}
	if n, err := a.eng.Poll(); err != nil {
		// 成品化：首轮失败不再直接退出——基线未建立时 DR2 会保护删除/覆盖，
		// 后续 pollLoop 会自动重试（最常见原因是夸克未登录/cookie 过期）
		log.Printf("⚠ 首轮云端轮询失败（基线未建立，DR2 保护仍生效，后续轮询自动补）: %v", err)
		if *remote != "" {
			log.Print("→ 若是登录问题（未扫码 / cookie 过期），请先运行: onerclone quark-login")
		}
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
	defer close(workerStop)
	go a.eng.RunWorker(workerStop, 8)

	// 8) 分层轮询（Q15）：活跃 60s / 空闲 5min；每 12h 强制一次
	go a.pollLoop()

	// （P0 遗留的 populate 占位符预热已删除：引擎 download 已覆盖其职责，
	// 两者并发建/删占位符会破坏 reparse 元数据 → "云文件元数据已损坏"，
	// Acrobat 打不开。v0.3.0 用户实测踩坑 #22）

	// 6) 监听本地改动（防抖 3s）
	w, err := a.startWatcher()
	if err != nil {
		log.Fatalf("启动监听失败: %v", err)
	}
	defer w.Close()

	mode := "在线"
	if a.offline {
		mode = "离线（-offline：水合立即失败）"
	}
	log.Printf("READY [%s] 用资源管理器打开 %s 开始验证；Enter/Ctrl+C 退出", mode, a.syncRoot)

	// 7) 等待退出
	quit := make(chan struct{})
	go func() {
		bufio.NewReader(os.Stdin).ReadString('\n')
		close(quit)
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	select {
	case <-quit:
	case <-sig:
	case <-trayQuit:
	}
	log.Print("退出中…")
}

// trimOutput 截断 rcd 输出日志（防止刷屏）。
func trimOutput(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return "..." + s[len(s)-max:]
}

// statePath 返回状态库路径（与同步根解耦，v0.2.0 反馈问题②）：
// exe 同目录 OnercloneSpike.state\state.db；exe 目录不可写（如 Program
// Files）时退回 %LocalAppData%\Onerclone。换同步根不再丢队列/基线。
func statePath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		p := filepath.Join(filepath.Dir(exe), "OnercloneSpike.state", "state.db")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
			return p
		}
	}
	if d, err := os.UserCacheDir(); err == nil && d != "" { // Windows: %LocalAppData%
		return filepath.Join(d, "Onerclone", "state.db")
	}
	return filepath.Join(filepath.Dir(defaultRoot()), "OnercloneSpike.state", "state.db")
}

// hrNotSyncRoot 是"路径不在同步根下"的错误码（CfGetSyncRootInfoByPath
// 对非同步根路径的返回；ERROR_CLOUD_FILE_NOT_UNDER_SYNC_ROOT）。
const hrNotSyncRoot = 0x80070186

// migrateSyncRoot 换根迁移（v0.2.0 反馈问题②）：配置的根若不是本
// provider 注册的同步根，自动注销旧根（内核 + Shell 层）再让调用方注册
// 新根。旧根不存在/已注销的报错一律忽略（幂等）。
func migrateSyncRoot(newRoot string) error {
	// 记住上次注册的根（state meta；state.db 已与同步根解耦，跨根可读）
	sp := statePath()
	st, err := state.Open(sp)
	if err != nil {
		return fmt.Errorf("打开状态库: %w", err)
	}
	defer st.Close()
	last, hadLast, err := st.GetMeta("registered_root")
	if err != nil {
		return err
	}
	if !hadLast || strings.EqualFold(last, newRoot) {
		_ = st.SetMeta("registered_root", newRoot)
		return nil
	}

	// 根变了：确认旧根确实是本 provider 的同步根才注销（防止误注销别人的）
	if info, err := cfapi.GetSyncRootInfoByPath(last); err == nil &&
		info.ProviderName == providerName {
		log.Printf("🔄 检测到同步根变更: %s → %s，自动注销旧根", last, newRoot)
		if err := cfapi.UnregisterSyncRoot(last); err != nil {
			log.Printf("⚠ 旧根注销失败（%v），继续注册新根", err)
		}
		if err := shellUnregister(); err != nil {
			log.Printf("⚠ 旧根 Shell 注销失败: %v", err)
		}
	} else {
		log.Printf("🔄 同步根变更: %s → %s（旧根已非本 provider 注册，跳过注销）", last, newRoot)
	}
	// 换根必须复位本地快照 + 基线（踩坑 #27）：local_snap 是"旧根磁盘上
	// 有什么"的真相 —— 不清则新根（还没拷入文件）的空扫描把旧条目判成
	// "本地已删"→ 经 delete_cloud 传播到云端。今天实测：换根首轮就入队了
	// delete_cloud 17_旬阳招投标，恰好云端已无此目录才没丢数据。
	// cloud_snap 保留：云端与根无关，保留避免整盘重新下载。
	if err := st.ResetLocalSnap(); err != nil {
		log.Printf("⚠ 换根复位本地快照失败: %v", err)
	} else {
		log.Printf("🧹 换根复位：已清空本地快照并重置基线（DR2 重新保护；云端快照保留）")
	}
	_ = st.SetMeta("registered_root", newRoot)
	return nil
}

// handleFetchData —— 水合核心：系统要读哪段，就从 rclone RC 取哪段回填。
func (a *app) handleFetchData(info *cfapi.CallbackInfo, params *cfapi.CallbackParameters) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("✗ FETCH_DATA panic: %v", r)
		}
	}()
	// NormalizedPath 是卷内路径（\Users\...），盘符需从 VolumeDosName 拼接，
	// 否则 filepath.Rel 无法计算相对路径（watcher 给的是带盘符全路径）
	vol := cfapi.StringFromUTF16Ptr(info.VolumeDosName)
	path := vol + cfapi.StringFromUTF16Ptr(info.NormalizedPath)
	base := filepath.Base(path)
	fd := params.FetchData
	off := fd.RequiredFileOffset
	reqLen := fd.RequiredLength
	size := info.FileSize

	// 用当前活跃会话而非 a.session：Connect 返回前回调就可能触发，
// a.session 尚未赋值（nil 解引用被 recover 吞掉 → I/O 挂 60s）。
	s := cfapi.ActiveSession()
	if s == nil {
		log.Printf("✗ FETCH_DATA 无活跃会话，丢弃（CF 会重试水合）")
		return
	}

	// —— 离线：立即快速失败（开放事实②验证点）——
	if a.offline {
		log.Printf("⊘ [offline] 读取 %s [%d,+%d) → 立即返回 NETWORK_UNAVAILABLE", base, off, reqLen)
		if err := s.FailTransfer(info, off, reqLen, cfapi.StatusCloudFileNetworkUnavailable); err != nil {
			log.Printf("✗ 快速失败响应出错: %v", err)
		}
		return
	}

	if off >= size {
		_ = s.FailTransfer(info, off, reqLen, cfapi.StatusEndOfFile)
		return
	}
	end := off + reqLen
	if end > size {
		end = size
	}

	rel, err := a.rel(path)
	if err != nil {
		log.Printf("✗ 路径越界 %s: %v", base, err)
		_ = s.FailTransfer(info, off, reqLen, cfapi.StatusCloudFileNetworkUnavailable)
		return
	}

	// DR1 实测：水合回填不产生 watcher 事件，无需 markSelfWrite——
	// 之前在这里标记 15s 自写窗口，反而把紧随其后的真实用户写入吞掉
	//（Add-Content 先触发水合再落盘 → WRITE 事件被误抑制 → 上传丢失）。

	t0 := time.Now()
	data, err := a.rc.Load().RangeGet(a.fsRoot, rel, off, end)
	if err != nil {
		log.Printf("✗ 取数失败 %s: %v", base, err)
		_ = s.FailTransfer(info, off, reqLen, cfapi.StatusCloudFileNetworkUnavailable)
		return
	}
	if int64(len(data)) != end-off {
		log.Printf("✗ 取数长度不符 %s: want=%d got=%d", base, end-off, len(data))
		_ = s.FailTransfer(info, off, reqLen, cfapi.StatusCloudFileNetworkUnavailable)
		return
	}
	if err := s.TransferData(info, off, int64(len(data)), data); err != nil {
		log.Printf("✗ 回填失败 %s: %v", base, err)
		return
	}
	log.Printf("💧 水合 %s [%d, +%d) 用时 %s", base, off, len(data), time.Since(t0).Round(time.Millisecond))
}

// ---------- 本地监听 + 防抖 ----------

func (a *app) startWatcher() (*fsnotify.Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := a.addTree(w, a.syncRoot); err != nil {
		w.Close()
		return nil, err
	}

	go func() {
		// 防抖：time.AfterFunc 实现（独立 goroutine 触发，规避 channel 定时器
		// 手术写法的未解卡死），mu 保护脏集合
		var mu sync.Mutex
		dirty := map[string]bool{}
		var pending *time.Timer // 当前防抖计时器

		flush := func() {
			mu.Lock()
			if len(dirty) == 0 {
				mu.Unlock()
				return
			}
			n := len(dirty)
			dirty = map[string]bool{}
			mu.Unlock()
			log.Printf("▶ 防抖结算（%d 项）→ 引擎全量扫描", n)
			// P1：不再逐路径手工上传——全量 Scan 由引擎做快照 diff，
			// 回声防护结构性内建（程序自写的落地已同步进快照）
			if cnt, err := a.eng.Scan(); err != nil {
				log.Printf("✗ 引擎扫描失败: %v", err)
			} else if cnt > 0 {
				log.Printf("✅ 扫描入队 %d 个动作", cnt)
			}
			// 本地有活动 → 踢 pollLoop 立即对账（Q15 活跃期）
			select {
			case a.pollKick <- struct{}{}:
			default:
			}
		}

		schedule := func(path string) {
			mu.Lock()
			dirty[path] = true
			n := len(dirty)
			// 每次都重置计时：3s 无新事件才结算（FR4 防抖语义）
			if pending != nil {
				pending.Stop()
			}
			pending = time.AfterFunc(debounceWindow, flush)
			mu.Unlock()
			log.Printf("· 已入队 %s（脏 %d 项），%s 后结算", filepath.Base(path), n, debounceWindow)
		}

		// 心跳：证明 watcher select 循环存活（30s 一次）
		heartbeat := time.NewTicker(30 * time.Second)
		defer heartbeat.Stop()

		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				base := filepath.Base(ev.Name)
				log.Printf("· raw: %v %s", ev.Op, base)
				if strings.HasPrefix(base, "~$") || strings.HasSuffix(strings.ToLower(base), ".tmp") {
					continue // 过滤临时文件（FR4）
				}
				if ev.Op&fsnotify.Chmod != 0 {
					continue
				}
				if ev.Op&fsnotify.Create != 0 {
					if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
						// 异步补挂子树：同步 Walk 大树会阻塞事件循环 →
						// 内核缓冲溢出丢事件（fsnotify.Add 并发安全）
						go func(name string) { _ = a.addTree(w, name) }(ev.Name)
					}
				}
				schedule(ev.Name)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("⚠ watcher: %v", err)
			case <-heartbeat.C:
				mu.Lock()
				n := len(dirty)
				mu.Unlock()
				log.Printf("💓 watcher 心跳（脏 %d 项）", n)
			}
		}
	}()
	return w, nil
}

func (a *app) addTree(w *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 走不到的子树跳过
		}
		if info.IsDir() {
			_ = w.Add(p)
		}
		return nil
	})
}

// pollLoop —— Q15 分层轮询：活跃期 60s / 空闲期 5min / 每 12h 强制对账。
// 队列有活 = 活跃（说明用户在操作）。
func (a *app) pollLoop() {
	const (
		active    = 60 * time.Second
		idle      = 5 * time.Minute
		fullRecon = 12 * time.Hour
	)
	lastFull := time.Now()
	for {
		// 决定本轮节奏（Q15 分层：队列有活 = 活跃期；本地事件踢醒 = 立即对账）
		wait := idle
		if pending, err := a.store.PendingCount(); err == nil && pending > 0 {
			wait = active
		}
		select {
		case <-a.pollKick:
			// 本地刚有活动：立即对账（对端可能同时有变更）
		case <-time.After(wait):
		}

		// 12h 强制全量对账（Q15 兜底）：补一轮本地扫描 + 图标恢复
		//（深层文件同步完但祖先目录未收敛时，平时无人再触发 MarkSynced，
		// 长跑进程图标会永久停在“同步中”直到重启——v0.3.6 实测现象）
		if time.Since(lastFull) >= fullRecon {
			if n, err := a.eng.Scan(); err == nil && n > 0 {
				log.Printf("🕒 12h 全量对账：本地扫描入队 %d", n)
			}
			if r, err := a.eng.RestoreInSync(); err == nil && r > 0 {
				log.Printf("🕒 12h 全量对账：恢复 %d 个绿钩", r)
			}
			if n, err := a.store.GCDone(7 * 24 * time.Hour); err == nil && n > 0 {
				log.Printf("🕒 12h 全量对账：清理 %d 条已完成动作", n)
			}
			lastFull = time.Now()
		}

		n, err := a.eng.Poll()
		if err != nil {
			log.Printf("⚠ 云端轮询失败（队列按分类退避自动补传）: %v", err)
			// 认证失败提醒（Q16/DR4）
			if cnt, _ := a.store.CountAuthFailed(); cnt > 0 {
				log.Printf("🛑 %d 个动作因认证失败停摆，需重新扫码", cnt)
			}
			continue
		}
		if n > 0 {
			log.Printf("☁️ 云端轮询入队 %d 个动作", n)
		}
	}
}

// ---------- 工具 ----------

func (a *app) rel(path string) (string, error) {
	r, err := filepath.Rel(a.syncRoot, path)
	if err != nil {
		return "", err
	}
	// 本地名 → 云端原名：水合回填要按云端名取数（本地名可能被改过）
	return a.nm.cloudPath(filepath.ToSlash(r)), nil
}

// ensureSample 在云端替身目录里准备验证数据。
func ensureSample(fsRoot string) error {
	marker := filepath.Join(fsRoot, "hello.txt")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(fsRoot, "sub"), 0o755); err != nil {
		return err
	}
	hello := "Hello from Onerclone spike!\r\n" +
		"这一行证明：占位符 -> FETCH_DATA -> rclone RC -> 回填 全链路打通。\r\n" +
		strings.Repeat("0123456789abcdef ", 8) + "\r\n"
	if err := os.WriteFile(marker, []byte(hello), 0o644); err != nil {
		return err
	}
	nested := "nested file: sub/nested.txt 水合成功。\r\n"
	if err := os.WriteFile(filepath.Join(fsRoot, "sub", "nested.txt"), []byte(nested), 0o644); err != nil {
		return err
	}
	// 8MB 大文件：验证分页水合
	big := make([]byte, 8<<20)
	for i := range big {
		big[i] = byte(i*31 + 7)
	}
	if err := os.WriteFile(filepath.Join(fsRoot, "big.bin"), big, 0o644); err != nil {
		return err
	}
	log.Printf("已在 %s 生成示例数据（hello.txt / sub/nested.txt / big.bin 8MB）", fsRoot)
	return nil
}
