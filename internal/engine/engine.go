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
	// WasHydrated 判断该路径此前是否已水合（Q6：曾水合的自动重新下载）。
	WasHydrated(rel string) bool
}

// ---------- 引擎 ----------

type Engine struct {
	mu    sync.Mutex
	store *state.Store
	cloud Cloud
	local Local
	log   *log.Logger
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
		nowMap[en.Path] = en
	}

	dirty := map[string]bool{}
	queued := 0
	// 1) 新增 / 修改 → upload（conflict 由 exec 前复查或 Poll 抢占裁决）
	for _, en := range entries {
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
		// 崩溃残留回收（inflight 超 2 分钟视为死任务）
		if n, err := e.store.ReapInflight(2 * time.Minute); err == nil && n > 0 {
			e.log.Printf("♻️ 回收 %d 个滞留 inflight 动作", n)
		}
		acts, err := e.store.ClaimDue(batch)
		if err != nil {
			e.log.Printf("✗ 队列读取失败: %v", err)
			sleep(stop, 5*time.Second)
			continue
		}
		if len(acts) == 0 {
			sleep(stop, 1*time.Second)
			continue
		}
		for _, a := range acts {
			select {
			case <-stop:
				return
			default:
			}
			e.exec(a)
		}
	}
}

func sleep(stop <-chan struct{}, d time.Duration) {
	select {
	case <-stop:
	case <-time.After(d):
	}
}

// exec 执行单个动作并回写结果。
func (e *Engine) exec(a state.Action) {
	e.mu.Lock()
	defer e.mu.Unlock()

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
	}
}

// classify 把底层错误映射到重试类别（Q16）。
func classify(err error) state.RetryClass {
	if err == nil {
		return state.ClassNetwork
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "401"), strings.Contains(s, "403"),
		strings.Contains(s, "unauthorized"), strings.Contains(s, "login"),
		strings.Contains(s, "cookie"), strings.Contains(s, "token expired"),
		strings.Contains(s, "please re-auth"):
		return state.ClassAuth
	case strings.Contains(s, "429"), strings.Contains(s, "rate limit"),
		strings.Contains(s, "too many"), strings.Contains(s, "risk"):
		return state.ClassRateLimit
	case strings.Contains(s, "404"), strings.Contains(s, "object not found"),
		strings.Contains(s, "directory not found"), strings.Contains(s, "no such file"),
		strings.Contains(s, "not a file"):
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
	e.log.Printf("⬇ 下载完成 %s（曾水合=%v，Q6 自动重拉=%v）", rel, hydrate, hydrate)
	return nil, state.ClassNetwork
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
		if err := e.cloud.Delete(rel); err != nil {
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
	entries, err := e.local.Scan()
	if err != nil {
		return err, classify(err)
	}
	var actual *CloudEntry
	for i := range entries {
		if entries[i].Path == rel {
			actual = &entries[i]
			break
		}
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
	ce, err := e.cloud.Stat(rel)
	if err != nil {
		return err, classify(err)
	}
	if ce != nil && !ce.IsDir {
		cp := conflictName(rel, time.Now())
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

// Stats 返回队列统计（日志/UI）。
func (e *Engine) Stats() (map[state.ActionState]int, error) {
	return e.store.Stats()
}
