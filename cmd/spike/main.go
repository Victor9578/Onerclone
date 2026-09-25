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
	hrNotCloudFile  = 0x80070178 // ERROR_NOT_CLOUD_FILE → 文件还不是占位符

	// 本程序写入本地后的回声抑制窗口：期间该路径的 watcher 事件视为自写
	selfWriteWindow = 15 * time.Second
	// 写入防抖窗口（FR4）
	debounceWindow = 3 * time.Second
)

const (
	providerName    = "Onerclone"
	providerVersion = "0.0.1-spike"
	syncRootID      = "onerclone-spike-root-1"
	fileIdentityID  = "onerclone-spike-file-1"
)

type app struct {
	syncRoot string // 本地同步根（NTFS）
	fsRoot   string // "云端"替身：rclone 以 local 后端服务的目录（正斜杠）
	rc       *rclone.Client
	session  *cfapi.Session
	offline  bool

	// P1 引擎（双向同步：快照 diff + 动作队列）
	eng   *engine.Engine
	store *state.Store

	// watcher 防抖后踢 pollLoop 立即轮询（本地有活动 = 活跃期语义）
	pollKick chan struct{}

	// watcher 触发全量扫描的节流（引擎自身有 mu，这里防扫描风暴）
	scanMu      sync.Mutex
	scanPending *time.Timer

	// FETCH_PLACEHOLDERS 限流日志计数
	fetchPhMu    sync.Mutex
	fetchPhCount int
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

func main() {
	// 日志同时输出到控制台与 spike.log（排查用：控制台会被回调刷屏）
	logFile, err := os.OpenFile("spike.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		defer logFile.Close()
		log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	}
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "register":
		cmdRegister(os.Args[2:])
	case "unregister":
		cmdUnregister(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`onerclone spike —— Cloud Files API 全链路验证

用法:
  spike register   [-root DIR]              注册同步根（以当前权限尝试）
  spike unregister [-root DIR]              注销同步根
  spike run        [-root DIR] [-fs DIR] [-rclone EXE] [-offline]

测试脚本 (run 起来后):
  1. 资源管理器打开同步根 → 检查云朵图标
  2. 记事本打开 hello.txt → 应秒开并显示内容（水合链路）
  3. 修改 hello.txt 保存 → 终端 3s 后出现“⬆ 上传”日志 → 核对 fs 目录内容
  4. Ctrl+C 退出，带 -offline 重跑 → 打开 big.bin → 应立即报错（不卡死）`)
}

// ---------- register / unregister ----------

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
	root := fs.String("root", defaultRoot(), "同步根目录（必须在 NTFS 上）")
	_ = fs.Parse(args)

	if err := os.MkdirAll(*root, 0o755); err != nil {
		log.Fatalf("创建同步根失败: %v", err)
	}

	elev := isElevated()
	log.Printf("当前进程 elevated = %v", elev)

	reg := cfapi.NewRegistration(providerName, providerVersion, []byte(syncRootID), []byte(fileIdentityID))
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
	root := fs.String("root", defaultRoot(), "同步根目录")
	_ = fs.Parse(args)
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
	root := fs.String("root", defaultRoot(), "同步根目录")
	fsDir := fs.String("fs", defaultFs(), "云端替身目录（rclone local 后端）")
	rcloneExe := fs.String("rclone", `D:\Software\rclone\rclone.exe`, "rclone 可执行文件")
	offline := fs.Bool("offline", false, "离线模式：水合请求立即快速失败")
	_ = fs.Parse(args)

	a := &app{
		syncRoot:  *root,
		fsRoot:    filepath.ToSlash(*fsDir),
		offline:   *offline,
		pollKick:  make(chan struct{}, 1),
	}

	// 1) 验证平台可用
	if v, err := cfapi.GetPlatformVersion(); err != nil {
		log.Fatalf("CfGetPlatformInfo 失败（系统不支持 Cloud Files API？需 Win10 1709+）: %v", err)
	} else {
		log.Printf("Cloud Files 平台版本: build=%d rev=%d int=%d", v.BuildNumber, v.RevisionNumber, v.IntegrationNumber)
	}

	// 2) 准备数据与本地目录
	if err := ensureSample(a.fsRoot); err != nil {
		log.Fatalf("准备示例数据失败: %v", err)
	}
	if err := os.MkdirAll(a.syncRoot, 0o755); err != nil {
		log.Fatalf("创建同步根失败: %v", err)
	}

	// 3) 启动 rclone rcd 子进程
	daemon, err := rclone.StartRcd(*rcloneExe)
	if err != nil {
		log.Fatalf("启动 rclone rcd 失败: %v", err)
	}
	defer daemon.Stop()
	a.rc = daemon.Client
	log.Printf("rclone rcd 就绪: %s", daemon.Client.Base)
	// 监控 rcd 子进程意外退出（上传 connection refused 的根因排查）
	go func() {
		err := <-daemon.Exited
		log.Printf("✗ rclone rcd 子进程已退出: %v\n--- rcd 输出 ---\n%s", err, daemon.Output())
	}()

	// 4) 确保注册策略为最新（Population=ALWAYS_FULL 需重新注册才生效），
	// 然后连接同步根（只注册 FETCH_DATA —— ALWAYS_FULL 下平台不会问
	// FETCH_PLACEHOLDERS，与微软 CloudMirror 示例一致）
	if err := cfapi.RegisterSyncRoot(a.syncRoot,
		cfapi.NewRegistration(providerName, providerVersion, []byte(syncRootID), []byte(fileIdentityID)),
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
	defer a.session.Disconnect()
	log.Print("同步根已连接（FETCH_DATA 回调在线）")

	// 5) 打开 P1 状态库 + 建引擎（三库模型：local_snap/cloud_snap/queue）
	statePath := filepath.Join(filepath.Dir(a.syncRoot), "OnercloneSpike.state", "state.db")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		log.Fatalf("创建状态目录失败: %v", err)
	}
	a.store, err = state.Open(statePath)
	if err != nil {
		log.Fatalf("打开状态库失败: %v", err)
	}
	defer a.store.Close()
	log.Printf("状态库: %s", statePath)

	a.eng = engine.New(a.store,
		&cloudRC{rc: a.rc, srcFs: a.syncRoot, dstFs: a.fsRoot},
		&localFS{root: a.syncRoot}, log.Default())

	// 6) 首轮基线：本地扫描 + 云端轮询 → 建立三库快照（DR2 之前无删除）
	if n, err := a.eng.Scan(); err != nil {
		log.Printf("⚠ 首轮本地扫描失败: %v", err)
	} else {
		log.Printf("首轮本地扫描：入队 %d", n)
	}
	if n, err := a.eng.Poll(); err != nil {
		log.Fatalf("首轮云端轮询失败（基线无法建立）: %v", err)
	} else {
		log.Printf("首轮云端轮询：入队 %d", n)
	}

	// 7) 引擎执行器（动作队列 worker，崩溃残留自动回收）
	workerStop := make(chan struct{})
	defer close(workerStop)
	go a.eng.RunWorker(workerStop, 8)

	// 8) 分层轮询（Q15）：活跃 60s / 空闲 5min；每 12h 强制一次
	go a.pollLoop()

	// 9) 全量列举云端 → 创建占位符（首轮快速预热；之后由引擎 download 接管）
	start := time.Now()
	n, err := a.populate("", a.syncRoot)
	if err != nil {
		log.Printf("⚠ 占位符预热失败（不致命，引擎会补齐）: %v", err)
	} else {
		log.Printf("✅ 占位符就绪: %d 个文件，用时 %s", n, time.Since(start).Round(time.Millisecond))
	}

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
	}
	log.Print("退出中…")
}

// populate 递归列举远程目录并在本地创建占位符，返回文件数。
func (a *app) populate(remoteRel, localDir string) (int, error) {
	entries, err := a.rc.List(a.fsRoot, remoteRel)
	if err != nil {
		return 0, fmt.Errorf("list %q: %w", remoteRel, err)
	}
	total := 0
	var items []cfapi.NewPlaceholder
	for _, e := range entries {
		if e.IsDir {
			sub := filepath.Join(localDir, e.Name)
			if err := os.MkdirAll(sub, 0o755); err != nil {
				return total, err
			}
			n, err := a.populate(e.Path, sub)
			if err != nil {
				return total, err
			}
			total += n
			continue
		}
		items = append(items, cfapi.NewPlaceholder{
			RelativeFileName: e.Name,
			FileSize:         e.Size,
			ModTime:          e.ModTime,
			Flags:            cfapi.PlaceholderCreateFlagMarkInSync,
			Identity:         []byte(e.Path), // e.Path = 相对云端根的路径，回调时可反查
		})
	}
	if len(items) == 0 {
		return total, nil
	}
	results, err := cfapi.CreatePlaceholders(localDir, items)
	if err != nil {
		// 平台会把首个失败码（如"已存在"）作为整体返回值，但条目仍被继续处理
		if code, _ := cfapi.AsHRESULT(err); code != hrAlreadyExists {
			return total, fmt.Errorf("create placeholders in %q: %w", localDir, err)
		}
	}
	for i, hr := range results {
		if hr == 0 || uint32(hr) == hrAlreadyExists {
			total++
			continue
		}
		log.Printf("⚠ 占位符创建失败 %s: HRESULT 0x%08X", items[i].RelativeFileName, uint32(hr))
	}
	return total, nil
}

// handleFetchPlaceholders —— 目录人口化回调。Population=PARTIAL 时平台在
// 枚举/路径解析目录前会询问 provider 有哪些待落地条目；不响应 = 60s 超时
// （实测 60.8s 报"云操作超时"）。我们的模型是占位符全部预先创建，
// 故答复"没有更多条目"，枚举立即返回本地实体。
func (a *app) handleFetchPlaceholders(info *cfapi.CallbackInfo, params *cfapi.CallbackParameters) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("✗ FETCH_PLACEHOLDERS panic: %v", r)
		}
	}()
	vol := cfapi.StringFromUTF16Ptr(info.VolumeDosName)
	dir := vol + cfapi.StringFromUTF16Ptr(info.NormalizedPath)
	if err := a.session.TransferPlaceholders(info, 0); err != nil {
		log.Printf("✗ FETCH_PLACEHOLDERS 应答失败 %s: %v", dir, err)
		return
	}
	// 限流日志：单次枚举可能触发上百次，只记首次和每 100 次
	a.fetchPhMu.Lock()
	a.fetchPhCount++
	n := a.fetchPhCount
	a.fetchPhMu.Unlock()
	if n == 1 || n%100 == 0 {
		log.Printf("📋 FETCH_PLACEHOLDERS 已答复“无待人口化条目”（累计 %d 次）", n)
	}
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

	// —— 离线：立即快速失败（开放事实②验证点）——
	if a.offline {
		log.Printf("⊘ [offline] 读取 %s [%d,+%d) → 立即返回 NETWORK_UNAVAILABLE", base, off, reqLen)
		if err := a.session.FailTransfer(info, off, reqLen, cfapi.StatusCloudFileNetworkUnavailable); err != nil {
			log.Printf("✗ 快速失败响应出错: %v", err)
		}
		return
	}

	if off >= size {
		_ = a.session.FailTransfer(info, off, reqLen, cfapi.StatusEndOfFile)
		return
	}
	end := off + reqLen
	if end > size {
		end = size
	}

	rel, err := a.rel(path)
	if err != nil {
		log.Printf("✗ 路径越界 %s: %v", base, err)
		_ = a.session.FailTransfer(info, off, reqLen, cfapi.StatusCloudFileNetworkUnavailable)
		return
	}

	// DR1 实测：水合回填不产生 watcher 事件，无需 markSelfWrite——
	// 之前在这里标记 15s 自写窗口，反而把紧随其后的真实用户写入吞掉
	//（Add-Content 先触发水合再落盘 → WRITE 事件被误抑制 → 上传丢失）。

	t0 := time.Now()
	data, err := a.rc.RangeGet(a.fsRoot, rel, off, end)
	if err != nil {
		log.Printf("✗ 取数失败 %s: %v", base, err)
		_ = a.session.FailTransfer(info, off, reqLen, cfapi.StatusCloudFileNetworkUnavailable)
		return
	}
	if int64(len(data)) != end-off {
		log.Printf("✗ 取数长度不符 %s: want=%d got=%d", base, end-off, len(data))
		_ = a.session.FailTransfer(info, off, reqLen, cfapi.StatusCloudFileNetworkUnavailable)
		return
	}
	if err := a.session.TransferData(info, off, int64(len(data)), data); err != nil {
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
						_ = a.addTree(w, ev.Name)
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

		// 12h 强制全量对账（Q15 兜底）：额外补一轮本地扫描
		if time.Since(lastFull) >= fullRecon {
			if n, err := a.eng.Scan(); err == nil && n > 0 {
				log.Printf("🕒 12h 全量对账：本地扫描入队 %d", n)
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
	return filepath.ToSlash(r), nil
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
