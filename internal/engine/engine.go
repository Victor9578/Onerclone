// Package engine 是 P1 双向同步引擎（PROGRESS §8 Phase 1 核心）。
//
// 结构（抄 aliyunpan 三库模型 + Q18 冲突优先）：
//
//	Scan()  本地扫描 → 对比 local_snap  → 入队 upload / delete_cloud / conflict
//	Poll()  云端轮询 → 对比 cloud_snap  → 入队 download / delete_local / conflict
//	Worker  取队列动作执行（带上传前复查 mtime、DR2 基线门、分类重试）
//
// 关键约定：
//
//   - 快照即回声防护：引擎落地任何本地改动（下载/删除）时同步更新 local_snap，
//     后续 Scan 对比自然无动作——不需要 selfWrite 时间窗（P0 坑的结构性消除）
//   - 所有本地/云端操作走接口注入，本包不 import cfapi（纯逻辑可单测）
//   - 单写者：Scan/Poll/Worker 的状态读写全部持 e.mu（Q13 单写者扩展）
//   - Q18 冲突优先：conflict 是最高优先级动作，入队时吞掉同路径其他 pending，
//     其他动作入队前见 pending conflict 则让位（conflict 执行是它们的超集）
package engine

import (
	"fmt"
	"log"
	"path"
	"strings"
	"sync"
	"time"

	"onerclone/internal/state"
)

// ---------- 注入接口 ----------

// CloudEntry 是云端的一个条目（相对同步根的正斜杠路径）。
type CloudEntry struct {
	Path  string
	Size  int64
	MTime time.Time
	IsDir bool
}

// Cloud 是引擎所需的云端操作面（rclone RC 的子集，便于 fake）。
type Cloud interface {
	// List 全量递归列举云端。
	List() ([]CloudEntry, error)
	// ListDir 递归列举某目录下全部条目（不含目录自身；目录删除前整树复查用）。
	ListDir(rel string) ([]CloudEntry, error)
	// Stat 单条目状态，不存在返回 (nil, nil)。
	Stat(rel string) (*CloudEntry, error)
	// Upload 把本地文件推到云端 rel。
	Upload(rel string) error
	// Download 把云端 rel 拉到本地（覆盖同名）。
	Download(rel string) error
	// DownloadTo 把云端 src 拉到本地 dst（冲突副本用）。
	DownloadTo(src, dst string) error
	// Delete 删云端单文件。
	Delete(rel string) error
	// Purge 递归删云端目录（目录删除必须走它，deletefile 对目录报错）。
	Purge(rel string) error
	// Mkdir 建云端目录（幂等）。
	Mkdir(rel string) error
}

// Local 是引擎所需的本地操作面（占位符/文件系统，由 cmd 层用 cfapi 实现）。
type Local interface {
	// Scan 全量扫描同步根，返回相对路径条目。
	Scan() ([]CloudEntry, error)
	// Stat 单条目状态，不存在返回 (nil, nil)（exec 前复查用）。
	Stat(rel string) (*CloudEntry, error)
	// ApplyDownload 让本地反映云端条目（建/更新占位符或目录）。
	// 实现负责更新本地数据，但不写快照（引擎写）。
	ApplyDownload(rel string, e CloudEntry) error
	// Remove 删除本地未修改的文件/目录。
	Remove(rel string) error
	// FinalizeUpload 上传成功后收尾：普通文件转占位符 + in-sync（图标）。
	FinalizeUpload(rel string) error
	// MarkSyncing 把条目（含祖先目录）标为“未同步”——Explorer 显示同步中。
	// 入队时调用；实现应容忍路径不存在（云端新建等场景）。
	MarkSyncing(rel string)
	// MarkSynced 把条目标回 in-sync（绿勾）。动作结算后调用。
	MarkSynced(rel string)
	// WasHydrated 判断该路径此前是否已水合（Q6：曾水合的自动重新下载）。
	WasHydrated(rel string) bool
	// Hydrate 主动把占位符数据拉到本地（Q6）：曾水合的文件在云端变更后
	// 重新下载，使下载窗口结束即可离线读；实现负责不写快照。
	// 失败（离线/网络）应返回错误，由引擎降级为“读时懒水合”。
	Hydrate(rel string) error
}

// ---------- 引擎 ----------

type Engine struct {
	mu    sync.Mutex
	store *state.Store
	cloud Cloud
	local Local
	log   *log.Logger
	// 空列表护栏连续计数（Poll 内使用，e.mu 保护）
	emptyStreak int
}

// New 创建引擎。
func New(store *state.Store, cloud Cloud, local Local, logger *log.Logger) *Engine {
	if logger == nil {
		logger = log.Default()
	}
	return &Engine{store: store, cloud: cloud, local: local, log: logger}
}

// conflictName 生成冲突副本名（Q7 格式：`1 (冲突 20260924).doc`）。
func conflictName(rel string, now time.Time) string {
	ext := path.Ext(rel)
	stem := strings.TrimSuffix(rel, ext)
	return fmt.Sprintf("%s (冲突 %s)%s", stem, now.Format("20060102-150405"), ext)
}

// isTempPath 判断是否是临时文件（FR4：不参与同步）。
// `~$` 开头 = Office 锁文件；`.tmp` 结尾 = 常见编辑器临时文件。
// watcher 侧已过滤，引擎 Scan 侧必须同样过滤——否则 Office 打开文档的
// 锁文件会被入队上传，随后又被用户关闭删除 → 上传失败 + 无谓的删除传播
// （v0.3.0 用户实测：`~$21_设计标2 .docx` 上传 404 刷屏）。
func isTempPath(rel string) bool {
	base := path.Base(rel)
	if strings.HasPrefix(base, "~$") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(base), ".tmp")
}

func changed(snap state.FileSnap, e CloudEntry) bool {
	if snap.IsDir != e.IsDir {
		return true
	}
	if snap.IsDir {
		return false // 目录只比存在性
	}
	// Q 无 hash：size+mtime 比对（mtime 容忍 1s 传输误差）
	if snap.Size != e.Size {
		return true
	}
	d := snap.MTime.Sub(e.MTime)
	if d < 0 {
		d = -d
	}
	return d > time.Second
}

// snapChanged 比较两张快照（local_snap vs cloud_snap）。
func snapChanged(a, b state.FileSnap) bool {
	return changed(a, CloudEntry{
		Path: b.Path, Size: b.Size, MTime: b.MTime, IsDir: b.IsDir,
	})
}

// ---------- 本地扫描 ----------

// Scan 全量扫描本地并入队差异。返回入队动作数。
//
// 语义（三方对账的“本地侧”）：本地实际 vs local_snap 变了 → 一律 upload。
// **不在这里判 conflict**——Scan 只有单侧信息，“云端变没变”由 execUpload
// 的前复查（Stat 云端 vs cloud_snap）裁决；Poll 侧发现两侧都变时经
// conflict 抢占规则覆盖本入队。
func (e *Engine) Scan() (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, queued, err := e.scanLocked()
	return queued, err
}

// scanLocked 执行本地扫描；返回本轮本地“新增/修改”的路径集合（dirty，
// 供 Poll 判断双侧变化）与入队数。调用方必须持 e.mu。
func (e *Engine) scanLocked() (map[string]bool, int, error) {
	entries, err := e.local.Scan()
	if err != nil {
		return nil, 0, fmt.Errorf("local scan: %w", err)
	}
	snap, err := e.store.AllSnap("local_snap")
	if err != nil {
		return nil, 0, err
	}
	baseline, err := e.store.BaselineDone()
	if err != nil {
		return nil, 0, err
	}

	nowMap := map[string]CloudEntry{}
	for _, en := range entries {
		if isTempPath(en.Path) {
			continue // 临时文件（~$/.tmp）不进快照 → 永不触发上传/删除
		}
		nowMap[en.Path] = en
	}

	dirty := map[string]bool{}
	queued := 0
	// Engine 的 Scan/Poll/exec 共用 e.mu，因此这里看到的 inflight 只可能是
	// 上一次进程崩溃留下的残留；立即回收后才能按本地现状收敛旧动作。
	if n, err := e.store.ReapInflight(0); err != nil {
		return dirty, queued, err
	} else if n > 0 {
		e.log.Printf("♻ 回收 %d 个上次进程残留的 inflight 动作", n)
	}
	present := make(map[string]bool, len(nowMap))
	for p := range nowMap {
		present[p] = true
	}
	if n, err := e.store.CancelMissingLocal(present); err != nil {
		return dirty, queued, err
	} else if n > 0 {
		e.log.Printf("♻ 收敛 %d 个本地已消失的旧动作（upload/conflict/dehydrate）", n)
	}
	// 1) 新增 / 修改 → upload（conflict 由 exec 前复查或 Poll 抢占裁决）
	for _, en := range entries {
		if isTempPath(en.Path) {
			continue
		}
		old, ok := snap[en.Path]
		if ok && old.Present && !changed(old, en) {
			continue // 未变
		}
		dirty[en.Path] = true
		n, err := e.enqueue(state.KindUpload, en.Path)
		if err != nil {
			return dirty, queued, err
		}
		if n > 0 {
			queued++
			e.log.Printf("📥 本地 %s（新/改）→ upload", en.Path)
		}
	}

	// 2) 消失 → delete_cloud（DR2 基线门）
	for p, old := range snap {
		if !old.Present {
			continue
		}
		if _, ok := nowMap[p]; ok {
			continue
		}
		if !baseline {
			e.log.Printf("⛔ 基线未完成，跳过本地删除 %s（DR2）", p)
			continue
		}
		n, err := e.enqueue(state.KindDeleteCloud, p)
		if err != nil {
			return dirty, queued, err
		}
		if n > 0 {
			queued++
			e.log.Printf("🗑 本地删除 %s → delete_cloud", p)
		}
	}

	// 3) 落快照（同一锁内完成 → 回声防护的结构性保证）
	if err := e.writeSnap("local_snap", nowMap, snap); err != nil {
		return dirty, queued, err
	}
	return dirty, queued, nil
}

// writeSnap upsert 现存条目 + 给消失条目打墓碑。
func (e *Engine) writeSnap(table string, now map[string]CloudEntry, old map[string]state.FileSnap) error {
	for p, en := range now {
		if err := e.store.PutSnap(table, state.FileSnap{
			Path: p, Size: en.Size, MTime: en.MTime, IsDir: en.IsDir, Present: true,
		}); err != nil {
			return err
		}
	}
	for p, o := range old {
		if !o.Present {
			continue
		}
		if _, ok := now[p]; !ok {
			if err := e.store.MarkSnapDeleted(table, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------- 云端轮询 ----------

// Poll 全量轮询云端并入队差异。返回入队动作数。
//
// 语义（三方对账的“云端侧”）：先刷新本地快照拿 dirty（本轮本地是否变过），
// 再 diff 云端 vs cloud_snap：
//   - 云端变了 + 本地也变（dirty）→ conflict（抢占 upload）
//   - 云端变了 + 本地没变 → download
//   - 云端删 + 本地改 → Q18 复活（upload）；云端删 + 本地稳 → delete_local
func (e *Engine) Poll() (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 先刷新本地侧（dirty = 本轮新增/修改的本地路径）
	dirty, localQueued, err := e.scanLocked()
	if err != nil {
		return 0, fmt.Errorf("local scan in poll: %w", err)
	}
	queued := localQueued
	// 本地是否变过：本轮扫描 dirty ∪ 有 pending upload（= 本地有未同步变化）
	localChanged := func(p string) (bool, error) {
		if dirty[p] {
			return true, nil
		}
		return e.store.HasPending(p, state.KindUpload)
	}

	entries, err := e.cloud.List()
	if err != nil {
		return 0, fmt.Errorf("cloud list: %w", err)
	}
	// List 成功 = 认证已恢复 → 复活 auth 失败动作（DR4 解除）
	if rn, rerr := e.store.RetryAuthFailed(); rerr == nil && rn > 0 {
		e.log.Printf("🔑 认证恢复，复活 %d 个 auth 失败动作", rn)
	}
	cloudSnap, err := e.store.AllSnap("cloud_snap")
	if err != nil {
		return 0, err
	}
	localSnap, err := e.store.AllSnap("local_snap")
	if err != nil {
		return 0, err
	}
	baseline, err := e.store.BaselineDone()
	if err != nil {
		return 0, err
	}

	// 空列表护栏：基线已建立、快照有内容而 List 为空（后端故障/权限降级
	// 常返回空而非 error）→ 首轮只记疑不下发，连续两轮空才按真实删除处理
	//（真实清空只延迟一个周期；瞬时故障下轮恢复即自愈）。
	if len(entries) == 0 && baseline {
		present := 0
		for _, cs := range cloudSnap {
			if cs.Present {
				present++
			}
		}
		if present > 0 {
			if e.emptyStreak == 0 {
				e.emptyStreak = 1
				return 0, fmt.Errorf("cloud list 为空但快照有 %d 条（疑似后端故障），本轮中止防误删，下轮复核", present)
			}
			e.log.Printf("⚠ cloud list 连续两轮为空（快照 %d 条）→ 按真实清空处理", present)
		}
	}
	e.emptyStreak = 0

	nowMap := map[string]CloudEntry{}
	for _, en := range entries {
		nowMap[en.Path] = en
	}

	for _, en := range entries {
		cs, ok := cloudSnap[en.Path]
		if ok && cs.Present && !changed(cs, en) {
			continue // 云端未变
		}
		ls, lok := localSnap[en.Path]
		localPresent := lok && ls.Present

		switch {
		case !ok || !cs.Present:
			// 云端新条目
			if localPresent {
				if !changed(ls, en) {
					// 本地也有且内容一致 → 只补云端快照（循环外统一写）
					continue
				}
				ch, err := localChanged(en.Path)
				if err != nil {
					return queued, err
				}
				if ch {
					// 内容不同 + 本地本轮也新建/修改 → 双侧分歧 → conflict
					n, err := e.enqueue(state.KindConflict, en.Path)
					if err != nil {
						return queued, err
					}
					queued += n
					if n > 0 {
						e.log.Printf("⚔️ 云端新 %s（本地本轮也变）→ conflict", en.Path)
					}
					continue
				}
				// 内容不同但本地未变 → 云端新内容覆盖（下方 download）
			}
			n, err := e.enqueue(state.KindDownload, en.Path)
			if err != nil {
				return queued, err
			}
			queued += n
			if n > 0 {
				e.log.Printf("📤 云端新 %s → download", en.Path)
			}
		default:
			// 云端已存在条目发生变更
			if !localPresent {
				// 本地已删 + 云端改 → Q18 宁复活不删除 → download 复活本地
				n, err := e.enqueue(state.KindDownload, en.Path)
				if err != nil {
					return queued, err
				}
				queued += n
				if n > 0 {
					e.log.Printf("♻️ 云端改 %s（本地已删）→ download 复活（Q18）", en.Path)
				}
				continue
			}
			ch, err := localChanged(en.Path)
			if err != nil {
				return queued, err
			}
			if ch {
				// 本地也改了 → conflict（抢占 scan 入队的 upload）
				n, err := e.enqueue(state.KindConflict, en.Path)
				if err != nil {
					return queued, err
				}
				queued += n
				if n > 0 {
					e.log.Printf("⚔️ 双方都改 %s → conflict", en.Path)
				}
				continue
			}
			// 本地未变 → 下载覆盖
			n, err := e.enqueue(state.KindDownload, en.Path)
			if err != nil {
				return queued, err
			}
			queued += n
			if n > 0 {
				e.log.Printf("📤 云端改 %s → download", en.Path)
			}
		}
	}

	// 云端消失
	for p, cs := range cloudSnap {
		if !cs.Present {
			continue
		}
		if _, ok := nowMap[p]; ok {
			continue
		}
		ls, lok := localSnap[p]
		localPresent := lok && ls.Present
		if !localPresent {
			// 双方都删 → 只清快照
			_ = e.store.MarkSnapDeleted("cloud_snap", p)
			continue
		}
		ch, err := localChanged(p)
		if err != nil {
			return queued, err
		}
		if ch {
			// 本地本轮也改过 → 宁复活不删除（Q18）：重新上传
			//（upload 已由 scanLocked 入队，这里幂等入队兜底）
			n, err := e.enqueue(state.KindUpload, p)
			if err != nil {
				return queued, err
			}
			queued += n
			if n > 0 {
				e.log.Printf("♻️ 云端删 %s（本地已改）→ upload 复活（Q18）", p)
			}
			continue
		}
		if !baseline {
			e.log.Printf("⛔ 基线未完成，跳过云端删除 %s（DR2）", p)
			continue
		}
		n, err := e.enqueue(state.KindDeleteLocal, p)
		if err != nil {
			return queued, err
		}
		queued += n
		if n > 0 {
			e.log.Printf("🗑 云端删除 %s → delete_local", p)
		}
	}

	// 落云端快照
	if err := e.writeSnap("cloud_snap", nowMap, cloudSnap); err != nil {
		return queued, err
	}

	// 首轮 本地+云端快照均写完 → 基线成立（DR2 开闸）
	if !baseline {
		if err := e.store.SetBaselineDone(); err != nil {
			return queued, err
		}
		e.log.Print("✅ 基线建立完成（DR2 删除保护开闸）")
	}
	return queued, nil
}

// RestoreInSync 在启动/全量对账后把“本地与云端一致且无待办”的文件重新
// 标记 in-sync。Explorer 的绿勾是 Cloud Files 元数据，不是 Onerclone 进程
// 内存；重启后补标可以修复此前状态丢失/目录未转占位符导致的同步箭头。
func (e *Engine) RestoreInSync() (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	localSnap, err := e.store.AllSnap("local_snap")
	if err != nil {
		return 0, err
	}
	cloudSnap, err := e.store.AllSnap("cloud_snap")
	if err != nil {
		return 0, err
	}
	restored := 0
	for p, ls := range localSnap {
		if !ls.Present {
			continue
		}
		cs, ok := cloudSnap[p]
		if !ok || !cs.Present || snapChanged(ls, cs) {
			continue
		}
		pending, err := e.store.HasPendingPath(p)
		if err != nil {
			return restored, err
		}
		if pending {
			continue
		}
		if err := e.local.FinalizeUpload(p); err != nil {
			e.log.Printf("⚠ 恢复 in-sync 失败 %s: %v", p, err)
			continue
		}
		restored++
	}
	if restored > 0 {
		e.log.Printf("✅ 已恢复 %d 个文件的 in-sync 状态", restored)
	}
	return restored, nil
}

// enqueue 统一入队收口。冲突裁决规则（Q18 冲突优先）：
//
//  1. 互斥升级：upload pending 表示本地有未同步变化，download pending 表示
//     云端有未同步变化；两侧数据移动动作在同路径相遇时升级为 conflict。
//  2. conflict 最高优先：入队时取消同路径其他 pending（CancelOthers）。
//  3. 其他动作入队前见 pending conflict 则让位（conflict 执行是其超集）。
func (e *Engine) enqueue(kind state.ActionKind, p string) (int, error) {
	// 规则 1：互斥升级
	if kind == state.KindUpload || kind == state.KindDownload {
		counterpart := state.KindDownload
		if kind == state.KindDownload {
			counterpart = state.KindUpload
		}
		has, err := e.store.HasPending(p, counterpart)
		if err != nil {
			return 0, err
		}
		if has {
			e.log.Printf("⚔️ %s 撞上 pending %s（%s）→ 升级 conflict", p, counterpart, kind)
			kind = state.KindConflict
		}
	}

	// 规则 2：conflict 抢占
	if kind == state.KindConflict {
		if n, err := e.store.CancelOthers(p, state.KindConflict); err != nil {
			return 0, err
		} else if n > 0 {
			e.log.Printf("⚔️ conflict %s 抢占，取消 %d 个待执行动作", p, n)
		}
	} else {
		// 规则 3：让位 pending conflict
		has, err := e.store.HasPending(p, state.KindConflict)
		if err != nil {
			return 0, err
		}
		if has {
			return 0, nil
		}
	}
	n, err := e.store.Enqueue(p, kind, state.ClassNetwork)
	if err != nil {
		return 0, err
	}
	if n {
		// 入队即标“同步中”：Explorer 状态列与真实队列对齐，而不是靠
		// MARK_IN_SYNC 的静态猜测（v0.3.6 实测：目录预标绿勾会误导状态列）。
		e.local.MarkSyncing(p)
		return 1, nil
	}
	return 0, nil
}

// ---------- 动作执行 ----------

// RunWorker 阻塞运行执行循环（直到 stop 关闭）。
func (e *Engine) RunWorker(stop <-chan struct{}, batch int) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		// 崩溃残留回收（inflight 超 2 分钟视为死任务）。
		// claim 与 exec 同锁：防止 claim 后 Poll 抢锁刷新 cloud_snap，
		// 使 execUpload 的“云端已变”复查失效 → 静默覆盖他人改动。
		e.mu.Lock()
		if n, err := e.store.ReapInflight(2 * time.Minute); err == nil && n > 0 {
			e.log.Printf("♻️ 回收 %d 个滞留 inflight 动作", n)
		}
		acts, err := e.store.ClaimDue(batch)
		if err != nil {
			e.mu.Unlock()
			e.log.Printf("✗ 队列读取失败: %v", err)
			sleep(stop, 5*time.Second)
			continue
		}
		if len(acts) == 0 {
			e.mu.Unlock()
			sleep(stop, 1*time.Second)
			continue
		}
		for _, a := range acts {
			select {
			case <-stop:
				e.mu.Unlock()
				return
			default:
			}
			e.execLocked(a)
		}
		e.mu.Unlock()
	}
}

func sleep(stop <-chan struct{}, d time.Duration) {
	select {
	case <-stop:
	case <-time.After(d):
	}
}

// exec 执行单个动作并回写结果（自行加锁；测试直接调用）。
func (e *Engine) exec(a state.Action) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.execLocked(a)
}

// execLocked 同 exec，但调用方必须已持 e.mu（RunWorker 批量执行用）。
func (e *Engine) execLocked(a state.Action) {

	var err error
	class := state.ClassNetwork
	switch a.Kind {
	case state.KindUpload:
		err, class = e.execUpload(a.Path)
	case state.KindDownload:
		err, class = e.execDownload(a.Path)
	case state.KindDeleteCloud:
		err, class = e.execDeleteCloud(a.Path)
	case state.KindDeleteLocal:
		err, class = e.execDeleteLocal(a.Path)
	case state.KindConflict:
		err, class = e.execConflict(a.Path)
	case state.KindDehydrate:
		err, class = e.execDehydrate(a.Path)
	default:
		err = fmt.Errorf("未知动作类型 %s", a.Kind)
		class = state.ClassPermanent
	}

	if err != nil {
		e.log.Printf("✗ %s %s 失败(第%d次): %v", a.Kind, a.Path, a.Attempts+1, err)
		if ferr := e.store.Fail(a.ID, class, err.Error()); ferr != nil {
			e.log.Printf("✗ 回写失败状态: %v", ferr)
		}
		if class == state.ClassAuth {
			e.log.Print("🛑 认证失败（cookie 过期）：队列已停，需重新扫码登录（DR4）")
		}
		return
	}
	if err := e.store.Complete(a.ID); err != nil {
		e.log.Printf("✗ 回写完成状态: %v", err)
		return
	}
	// 结算后对账图标：该路径无任何未完成动作（含 failed）才恢复绿勾。
	// 失败/重试中的条目保持“同步中”，与队列真相一致。
	if pending, err := e.store.HasPendingPath(a.Path); err == nil && !pending {
		e.local.MarkSynced(a.Path)
		// 祖先目录恢复：整棵子树无待办才转绿（否则深层文件同步完，
		// 祖先目录永远“同步中”直到重启——v0.3.6 实测的乱显示另一半根因）
		e.restoreAncestors(a.Path)
	}
}

// restoreAncestors 沿祖先链向上恢复绿勾：遇到第一个还有待办的目录即停
//（其上必然也未收敛）。查询失败时保守不动。
func (e *Engine) restoreAncestors(rel string) {
	for p := path.Dir(rel); p != "." && p != "/" && p != ""; p = path.Dir(p) {
		pend, err := e.store.HasPendingTree(p)
		if err != nil || pend {
			return
		}
		e.local.MarkSynced(p)
	}
}

// classify 把底层错误映射到重试类别（Q16）。
func classify(err error) state.RetryClass {
	if err == nil {
		return state.ClassNetwork
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "rclone rc 401"), strings.Contains(s, "rclone rc 403"),
		strings.Contains(s, "unauthorized"), strings.Contains(s, "login"),
		strings.Contains(s, "cookie"), strings.Contains(s, "token expired"),
		strings.Contains(s, "please re-auth"):
		return state.ClassAuth
	case strings.Contains(s, "rclone rc 429"), strings.Contains(s, "rate limit"),
		strings.Contains(s, "too many"), strings.Contains(s, "risk"):
		return state.ClassRateLimit
	case strings.Contains(s, "rclone rc 404"), strings.Contains(s, "object not found"),
		strings.Contains(s, "directory not found"), strings.Contains(s, "no such file"),
		strings.Contains(s, "not a file"):
		return state.ClassPermanent
	case strings.Contains(s, "cloud file metadata is corrupt"):
		// 坏占位符（踩坑 #24）：cldflt 对该文件的一切访问（属性/删除/改名）
		// 都返回 ERROR_CLOUD_FILE_METADATA_CORRUPT，当轮重试不可能成功 →
		// 永久失败，面板可见原因（此前按 network 类每轮重试、日志刷屏）。
		// 重启/手动删除修复后：面板重试，或删除文件后由下轮扫描补收敛。
		return state.ClassPermanent
	default:
		return state.ClassNetwork
	}
}

// execUpload 执行上传。含 aliyunpan 借鉴的“上传前复查”：云端若在上次轮询后
// 又变了 → 转 conflict（Q18 冲突优先）。
func (e *Engine) execUpload(rel string) (error, state.RetryClass) {
	// 目录条目 → mkdir（copyfile 会报 "is a directory not a file"）
	if ls, ok, err := e.store.GetSnap("local_snap", rel); err == nil && ok && ls.IsDir {
		if err := e.cloud.Mkdir(rel); err != nil {
			return err, classify(err)
		}
		if err := e.local.FinalizeUpload(rel); err != nil {
			e.log.Printf("⚠ 云端目录已建但本地收尾失败 %s: %v", rel, err)
		}
		if err := e.store.PutSnap("cloud_snap", ls); err != nil {
			return err, state.ClassNetwork
		}
		e.log.Printf("📁 云端建目录 %s", rel)
		return nil, state.ClassNetwork
	}
	ce, err := e.cloud.Stat(rel)
	if err != nil {
		return err, classify(err)
	}
	if ce != nil {
		cs, _, _ := e.store.GetSnap("cloud_snap", rel)
		if !cs.Present || changed(cs, *ce) {
			// 云端有我们不知道的变化 → 冲突
			if _, err := e.enqueue(state.KindConflict, rel); err != nil {
				return err, state.ClassNetwork
			}
			e.log.Printf("🔁 上传前复查：云端 %s 已变 → 转 conflict", rel)
			return nil, state.ClassNetwork // 本动作视为完成（已转交）
		}
		// 云端 = 快照（自上次 poll 未变）→ 正常覆盖
	}
	if err := e.cloud.Upload(rel); err != nil {
		return err, classify(err)
	}
	if err := e.local.FinalizeUpload(rel); err != nil {
		e.log.Printf("⚠ 上传成功但本地收尾失败 %s: %v", rel, err)
	}
	if err := e.refreshCloudSnapFromLocal(rel); err != nil {
		return err, state.ClassNetwork
	}
	e.log.Printf("⬆ 上传完成 %s", rel)
	return nil, state.ClassNetwork
}

// refreshCloudSnapFromLocal 上传后把 cloud_snap 对齐到该文件的本地实际状态。
func (e *Engine) refreshCloudSnapFromLocal(rel string) error {
	ls, ok, err := e.store.GetSnap("local_snap", rel)
	if err != nil {
		return err
	}
	if !ok || !ls.Present {
		return nil // 本地快照还没写（首轮边角），留给下次 poll 对齐
	}
	return e.store.PutSnap("cloud_snap", ls)
}

// execDownload 执行下载（云端新/改落地本地）。
func (e *Engine) execDownload(rel string) (error, state.RetryClass) {
	return e.download(rel, true)
}

// download 落地云端条目：删旧（若有）+ 建占位符 + 对齐快照。
// allowQ6=false 表示**脱水**：只重建占位符，不触发 Q6 主动重拉
// （否则刚释放的空间立刻又被拉回来）。
func (e *Engine) download(rel string, allowQ6 bool) (error, state.RetryClass) {
	ls, lok, err := e.store.GetSnap("local_snap", rel)
	if err != nil {
		return err, state.ClassNetwork
	}
	// 下载前复查：本地是否在入队后又被改（改了 → 转 conflict，防覆盖用户数据）
	if lok && ls.Present {
		lnow, err := e.local.Stat(rel)
		if err != nil {
			return err, classify(err)
		}
		if lnow != nil && changed(ls, *lnow) {
			if _, err := e.enqueue(state.KindConflict, rel); err != nil {
				return err, state.ClassNetwork
			}
			e.log.Printf("🔁 下载前复查：本地 %s 已改 → 转 conflict", rel)
			return nil, state.ClassNetwork
		}
	}
	// 拉取云端最新状态（poll 后可能又变，下载要拿最新的）
	ce, err := e.cloud.Stat(rel)
	if err != nil {
		return err, classify(err)
	}
	if ce == nil {
		// 云端又没了（刚被删/改名）→ 放弃，等下次 poll
		e.log.Printf("⊘ 下载前复查：云端 %s 已消失，取消", rel)
		return nil, state.ClassNetwork
	}
	hydrate := lok && e.local.WasHydrated(rel)
	if err := e.local.ApplyDownload(rel, *ce); err != nil {
		return err, classify(err)
	}
	// 快照对齐（同一锁内 → 回声防护）。local_snap 以**本地实际 stat** 为准：
	// 占位符落地后的 mtime 可能与云端有偏差，写云端值会导致下轮 Scan 误判
	// "本地改了" → 触发多余回环上传（P1 集成测试 B 实测的 bug）。
	if ln, err := e.local.Stat(rel); err == nil && ln != nil {
		if err := e.store.PutSnap("local_snap", state.FileSnap{
			Path: ln.Path, Size: ln.Size, MTime: ln.MTime, IsDir: ln.IsDir, Present: true,
		}); err != nil {
			return err, state.ClassNetwork
		}
	}
	if err := e.store.PutSnap("cloud_snap", state.FileSnap{
		Path: ce.Path, Size: ce.Size, MTime: ce.MTime, IsDir: ce.IsDir, Present: true,
	}); err != nil {
		return err, state.ClassNetwork
	}
	// Q6：曾水合的文件在云端变更后**主动**重拉数据（而不是只重建占位符），
	// 下载窗口一结束本地就是最新内容，离线期可读。
	// 拉不动（离线/失败）不判动作失败：占位符还在，读时懒水合兜底。
	if hydrate && allowQ6 {
		if err := e.local.Hydrate(rel); err != nil {
			e.log.Printf("⚠ Q6 主动重拉失败 %s（保留占位符，读时懒水合兑底）: %v", rel, err)
		} else {
			e.log.Printf("🔄 Q6 主动重拉完成 %s（已就地可读）", rel)
		}
	}
	if !allowQ6 {
		e.log.Printf("🫙 已脱水 %s（本地数据释放，占位符就位）", rel)
	} else {
		e.log.Printf("⬇ 下载完成 %s（曾水合=%v，Q6 自动重拉=%v）", rel, hydrate, hydrate)
	}
	return nil, state.ClassNetwork
}

// execDehydrate 面板“脱水”：删本地数据 + 重建占位符（不触发 Q6 重拉）。
// download 内部会先做“本地已改 → 转 conflict”复查，不丢用户未同步的改动。
func (e *Engine) execDehydrate(rel string) (error, state.RetryClass) {
	return e.download(rel, false)
}

// execDeleteCloud 本地删除 → 删云端。含复活保护（云端若已变 → 改为下载复活）。
func (e *Engine) execDeleteCloud(rel string) (error, state.RetryClass) {
	baseline, err := e.store.BaselineDone()
	if err != nil {
		return err, state.ClassNetwork
	}
	if !baseline {
		e.log.Printf("⛔ 基线未完成，取消删除云端 %s（DR2）", rel)
		return nil, state.ClassNetwork
	}
	ce, err := e.cloud.Stat(rel)
	if err != nil {
		return err, classify(err)
	}
	if ce != nil {
		cs, _, _ := e.store.GetSnap("cloud_snap", rel)
		if !cs.Present || changed(cs, *ce) {
			// 云端在我们删除落地前又变过 → 宁复活不删除（Q18）：拉回来
			if _, err := e.enqueue(state.KindDownload, rel); err != nil {
				return err, state.ClassNetwork
			}
			e.log.Printf("♻️ 删除前复查：云端 %s 已变 → 复活为 download（Q18）", rel)
			return nil, state.ClassNetwork
		}
		// 目录必须 Purge（递归删）：deletefile 只删文件，对目录报
		// "is a directory not a file" → 永久失败无限重试
		//（v0.3.0 用户实测：删 `来自:分享` 目录卡死在重试）。
		// ⚠ 快照里该目录的子条目会由下一轮 Poll 的"云端消失"分支清理，
		// 不在这里逐条打墓碑（Delete 语义 = 整树删除）。
		var err error
		if ce.IsDir {
			// Purge 前整树复查：changed() 对目录只比存在性，看不见他人
			// 新塞进来的文件 → 会删掉从未见过的内容。云端现内容必须与
			// cloud_snap 已知内容完全一致，否则宁复活不删除（Q18）。
			kids, lerr := e.cloud.ListDir(rel)
			if lerr != nil {
				return lerr, classify(lerr)
			}
			for _, k := range kids {
				ks, _, _ := e.store.GetSnap("cloud_snap", k.Path)
				if !ks.Present || changed(ks, k) {
					if _, err := e.enqueue(state.KindDownload, rel); err != nil {
						return err, state.ClassNetwork
					}
					e.log.Printf("♻️ 目录删除前复查：%s 内云端已变（%s）→ 整目录复活（Q18）", rel, k.Path)
					return nil, state.ClassNetwork
				}
			}
			err = e.cloud.Purge(rel)
		} else {
			err = e.cloud.Delete(rel)
		}
		if err != nil {
			return err, classify(err)
		}
	}
	if err := e.store.MarkSnapDeleted("cloud_snap", rel); err != nil {
		return err, state.ClassNetwork
	}
	if err := e.store.MarkSnapDeleted("local_snap", rel); err != nil {
		return err, state.ClassNetwork
	}
	e.log.Printf("🗑 云端已删 %s", rel)
	return nil, state.ClassNetwork
}

// execDeleteLocal 云端删除 → 删本地。执行前复查本地是否又被改（改了 → 上传复活）。
func (e *Engine) execDeleteLocal(rel string) (error, state.RetryClass) {
	baseline, err := e.store.BaselineDone()
	if err != nil {
		return err, state.ClassNetwork
	}
	if !baseline {
		e.log.Printf("⛔ 基线未完成，取消删除本地 %s（DR2）", rel)
		return nil, state.ClassNetwork
	}
	// 复查本地实际：文件存在且相对 local_snap 变了 → 复活上传（Q18 宁复活不删除）。
	// 文件不存在 = 入队后被用户删了 → 双删，直接落墓碑。
	// 定点 Stat 而非全量 Scan：O(1) 且无 Scan 的改名/清理副作用。
	actual, err := e.local.Stat(rel)
	if err != nil {
		return err, classify(err)
	}
	if actual != nil {
		ls, lok, err := e.store.GetSnap("local_snap", rel)
		if err != nil {
			return err, state.ClassNetwork
		}
		if !lok || !ls.Present || changed(ls, *actual) {
			if _, err := e.enqueue(state.KindUpload, rel); err != nil {
				return err, state.ClassNetwork
			}
			e.log.Printf("♻️ 删除前复查：本地 %s 已变 → 复活为 upload（Q18）", rel)
			return nil, state.ClassNetwork
		}
		if err := e.local.Remove(rel); err != nil {
			return err, classify(err)
		}
	}
	_ = e.store.MarkSnapDeleted("local_snap", rel)
	_ = e.store.MarkSnapDeleted("cloud_snap", rel)
	e.log.Printf("🗑 本地已删 %s", rel)
	return nil, state.ClassNetwork
}

// execConflict 冲突双保留（Q7/Q18）：
//  1. 把云端版本下载为本地冲突副本 `stem (冲突 时间).ext`
//  2. 把本地版本覆盖上传到原名
//  3. 冲突副本随后由 Scan 发现并上传 → 云端也双保留
//
// 云端已删（Stat=nil）时退化为纯上传 = Q18 复活语义。
func (e *Engine) execConflict(rel string) (error, state.RetryClass) {
	// 目录冲突：mkdir 幂等合并（copyfile 对目录报 "is a directory not a
	// file" → permanent 卡死，永不收敛）。
	if ls, ok, err := e.store.GetSnap("local_snap", rel); err == nil && ok && ls.IsDir {
		if err := e.cloud.Mkdir(rel); err != nil {
			return err, classify(err)
		}
		if err := e.local.FinalizeUpload(rel); err != nil {
			e.log.Printf("⚠ 目录冲突 mkdir 后收尾失败 %s: %v", rel, err)
		}
		if err := e.store.PutSnap("cloud_snap", ls); err != nil {
			return err, state.ClassNetwork
		}
		e.log.Printf("📁 目录冲突 %s：两侧合并为同一目录", rel)
		return nil, state.ClassNetwork
	}
	ce, err := e.cloud.Stat(rel)
	if err != nil {
		return err, classify(err)
	}
	if ce != nil && !ce.IsDir {
		// 冲突副本名用本地快照 mtime（而非 time.Now）：重试时同名覆盖
		// 幂等；time.Now 每次重试都生成新副本 → 重复冲突副本刷屏。
		nameTime := time.Now()
		if ls, ok, err := e.store.GetSnap("local_snap", rel); err == nil && ok && ls.Present {
			nameTime = ls.MTime
		}
		cp := conflictName(rel, nameTime)
		if err := e.cloud.DownloadTo(rel, cp); err != nil {
			return err, classify(err)
		}
		e.log.Printf("⚔️ 冲突 %s：云端版本 → %s", rel, path.Base(cp))
	}
	// 本地版本上传原名（云端存在则覆盖——副本已保底）
	if err := e.cloud.Upload(rel); err != nil {
		return err, classify(err)
	}
	if err := e.local.FinalizeUpload(rel); err != nil {
		e.log.Printf("⚠ 冲突上传成功但收尾失败 %s: %v", rel, err)
	}
	if err := e.refreshCloudSnapFromLocal(rel); err != nil {
		return err, state.ClassNetwork
	}
	e.log.Printf("⚔️ 冲突解决 %s：本地版=原名已上传，云端版=冲突副本", rel)
	return nil, state.ClassNetwork
}

func (e *Engine) RequestDehydrate(path string) (bool, error) {
	if !e.local.WasHydrated(path) {
		return false, nil
	}
	_, err := e.enqueue(state.KindDehydrate, path)
	return err == nil, err
}

// IsHydrated 判断本地是否有真实数据（面板展示/按钮置灰用）。
func (e *Engine) IsHydrated(path string) bool { return e.local.WasHydrated(path) }

// RetryAction 把一条动作重置为待执行（面板“重试”）。
func (e *Engine) RetryAction(id int64) error { return e.store.Retry(id) }

// DropAction 移除一条动作（面板“放弃”）。
func (e *Engine) DropAction(id int64) error { return e.store.Drop(id) }

func (e *Engine) Stats() (map[state.ActionState]int, error) {
	return e.store.Stats()
}
