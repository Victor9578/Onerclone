// Package state 是 P1 的 SQLite 状态库（Q13：modernc 纯 Go，WAL，单写者=Engine）。
//
// 三库模型（抄 aliyunpan，见 PROGRESS §5.4）：
//
//   - local_snap  本地快照：上次扫描时同步根内每个条目的 size/mtime/状态
//   - cloud_snap  云端快照：上次轮询时云端每个条目的 size/mtime
//   - queue       动作队列：待执行的上传/下载/删除/冲突动作，带分类重试元数据
//
// 设计要点：
//
//   - 幂等：queue 有 (path, kind) 唯一约束，重复入队合并（ON CONFLICT DO UPDATE）
//   - 频控：成功动作有冷却（1 分钟），失败按分类退避（Q16）
//   - 进行中去重：state=inflight 的同路径动作不会重复入队
//   - DR2 基线：meta 表存 baseline_done，基线建立前禁止一切删除/覆盖
package state

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，注册 database/sql 驱动名 "sqlite"
)

// ActionKind 是队列动作类型。
type ActionKind string

const (
	KindUpload      ActionKind = "upload"       // 本地新/改 → 推云端
	KindDownload    ActionKind = "download"     // 云端新/改 → 落本地（建占位符或更新）
	KindDeleteCloud ActionKind = "delete_cloud" // 本地删除 → 删云端（受 DR2 基线保护）
	KindDeleteLocal ActionKind = "delete_local" // 云端删除 → 删本地（曾水合且未改才允许）
	KindConflict    ActionKind = "conflict"     // 双方都改 → 双保留冲突副本（Q7/Q18）
	KindDehydrate   ActionKind = "dehydrate"    // 面板“脱水”：删本地数据重建占位符（Q6 不重拉）
)

// ActionState 是动作生命周期状态。
type ActionState string

const (
	StatePending  ActionState = "pending"  // 等待执行（含退避中）
	StateInflight ActionState = "inflight" // 执行中（崩溃后超时自动回 pending）
	StateDone     ActionState = "done"     // 成功（保留 7 天供诊断，不参与调度）
	StateFailed   ActionState = "failed"   // 永久失败（需人工/UI 干预）
)

// RetryClass 是失败分类（Q16 分类退避）。
type RetryClass string

const (
	ClassNetwork   RetryClass = "network"    // 网络类：指数退避无限重试（封顶 5min+抖动）
	ClassRateLimit RetryClass = "rate_limit" // 429/风控：大幅降速
	ClassAuth      RetryClass = "auth"       // cookie 过期：停队列 + 扫码提醒（不自动重试）
	ClassPermanent RetryClass = "permanent"  // 永久错误（如远端 404）：进 failed
)

// Action 是队列中的一条动作。
type Action struct {
	ID         int64
	Path       string // 相对同步根的正斜杠路径
	Kind       ActionKind
	State      ActionState
	Class      RetryClass
	Attempts   int
	NextTry    time.Time // 到点才执行（退避/冷却）
	LastErr    string
	EnqueuedAt time.Time
	UpdatedAt  time.Time
}

// FileSnap 是本地/云端快照的一行。
type FileSnap struct {
	Path    string // 正斜杠相对路径
	Size    int64
	MTime   time.Time
	IsDir   bool
	Present bool // false = 已删除的墓碑（对比基线用）
}

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS local_snap (
  path     TEXT PRIMARY KEY,
  size     INTEGER NOT NULL DEFAULT 0,
  mtime    INTEGER NOT NULL DEFAULT 0, -- unixnano
  is_dir   INTEGER NOT NULL DEFAULT 0,
  present  INTEGER NOT NULL DEFAULT 1,
  updated  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS cloud_snap (
  path     TEXT PRIMARY KEY,
  size     INTEGER NOT NULL DEFAULT 0,
  mtime    INTEGER NOT NULL DEFAULT 0,
  is_dir   INTEGER NOT NULL DEFAULT 0,
  present  INTEGER NOT NULL DEFAULT 1,
  updated  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS queue (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  path      TEXT NOT NULL,
  kind      TEXT NOT NULL,
  state     TEXT NOT NULL DEFAULT 'pending',
  class     TEXT NOT NULL DEFAULT 'network',
  attempts  INTEGER NOT NULL DEFAULT 0,
  next_try  INTEGER NOT NULL DEFAULT 0, -- unixnano
  last_err  TEXT NOT NULL DEFAULT '',
  created   INTEGER NOT NULL,
  updated   INTEGER NOT NULL,
  UNIQUE(path, kind)
);

-- 调度索引：pending 且到点的先出
CREATE INDEX IF NOT EXISTS idx_queue_due ON queue(state, next_try);

CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
`

// Store 是状态库句柄。并发约定：Engine 单写者，读可并发。
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）状态库。path 通常在同步根外的状态目录。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	// modernc/sqlite 单连接即可保证写串行；多连接在 WAL 下也安全，
	// 但限制为 1 可彻底规避 SQLITE_BUSY（单写者=Engine 的 Q13 约定）
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ---------- meta（DR2 基线等开关） ----------

// GetMeta 读元数据，不存在返回 ("", false, nil)。
func (s *Store) GetMeta(k string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetMeta 写元数据。
func (s *Store) SetMeta(k, v string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// BaselineDone 返回是否已完成首次全量基线（DR2：之前禁止删除与覆盖上传）。
func (s *Store) BaselineDone() (bool, error) {
	v, ok, err := s.GetMeta("baseline_done")
	return ok && v == "1", err
}

// SetBaselineDone 标记基线完成。
func (s *Store) SetBaselineDone() error { return s.SetMeta("baseline_done", "1") }

// ---------- 快照 ----------

// GetSnap 读一张快照行。
func (s *Store) GetSnap(table, path string) (FileSnap, bool, error) {
	var f FileSnap
	var mtime int64
	var isDir, present bool
	err := s.db.QueryRow(
		`SELECT path,size,mtime,is_dir,present FROM `+table+` WHERE path=?`, path).
		Scan(&f.Path, &f.Size, &mtime, &isDir, &present)
	if err == sql.ErrNoRows {
		return FileSnap{}, false, nil
	}
	if err != nil {
		return FileSnap{}, false, err
	}
	f.MTime = time.Unix(0, mtime)
	f.IsDir = isDir
	f.Present = present
	return f, true, nil
}

// PutSnap upsert 一行快照。
func (s *Store) PutSnap(table string, f FileSnap) error {
	_, err := s.db.Exec(
		`INSERT INTO `+table+`(path,size,mtime,is_dir,present,updated)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET
		   size=excluded.size, mtime=excluded.mtime, is_dir=excluded.is_dir,
		   present=excluded.present, updated=excluded.updated`,
		f.Path, f.Size, f.MTime.UnixNano(), boolToInt(f.IsDir), boolToInt(f.Present),
		time.Now().UnixNano())
	return err
}

// MarkSnapDeleted 打墓碑（不物理删除，基线对比需要“曾经存在”信息）。
func (s *Store) MarkSnapDeleted(table, path string) error {
	_, err := s.db.Exec(
		`UPDATE `+table+` SET present=0, updated=? WHERE path=?`,
		time.Now().UnixNano(), path)
	return err
}

// AllSnap 全量读一张快照（内存态，10 万文件约 20MB，可接受；Q3 规模）。
func (s *Store) AllSnap(table string) (map[string]FileSnap, error) {
	rows, err := s.db.Query(
		`SELECT path,size,mtime,is_dir,present FROM ` + table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FileSnap{}
	for rows.Next() {
		var f FileSnap
		var mtime int64
		var isDir, present int
		if err := rows.Scan(&f.Path, &f.Size, &mtime, &isDir, &present); err != nil {
			return nil, err
		}
		f.MTime = time.Unix(0, mtime)
		f.IsDir = isDir != 0
		f.Present = present != 0
		out[f.Path] = f
	}
	return out, rows.Err()
}

// ResetCloudSnap 清空云端快照并重置基线（换 remote 时调用，见踩坑 #13：
// cloud_snap 是"上次在哪个后端看到什么"的真相，旧快照里的条目在新后端
// 不存在 → 会被判"云端已删"→ 误删本地文件）。
func (s *Store) ResetCloudSnap() error {
	if _, err := s.db.Exec(`DELETE FROM cloud_snap`); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM meta WHERE k='baseline_done'`)
	return err
}

// ResetLocalSnap 清空本地快照并重置基线（**换同步根**时调用，踩坑 #27：
// local_snap 是"旧根磁盘上有什么"的真相 —— 换根后新根若还没拷入文件，
// 空扫描会把旧条目判成"本地已删"→ 经 delete_cloud 传播到云端；基线一并
// 复位让 DR2 重新建立保护。cloud_snap 保留：云端与根无关，保留可避免
// 整盘重新下载、避免与云端现状对不上）。
func (s *Store) ResetLocalSnap() error {
	if _, err := s.db.Exec(`DELETE FROM local_snap`); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM meta WHERE k='baseline_done'`)
	return err
}

// ---------- 队列 ----------

// Enqueue 入队（幂等：同 path+kind 合并重置为 pending；仅 inflight/failed 不动
// ——执行中不打断；failed 不复活：auth 类等认证恢复后由 RetryAuthFailed 统一复活
// （否则每轮 Poll 幂等重入队把 DR4“认证失败停队列”打破成无限重试循环），
// permanent 类等路径变更（删后重建）自然换新动作）。
// 返回是否真正新入队/重置（用于日志去重）。
func (s *Store) Enqueue(path string, kind ActionKind, class RetryClass) (bool, error) {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO queue(path,kind,state,class,attempts,next_try,last_err,created,updated)
		 VALUES(?,?, 'pending', ?, 0, 0, '', ?, ?)
		 ON CONFLICT(path,kind) DO UPDATE SET
		   state='pending', class=excluded.class, next_try=0, updated=excluded.updated
		   WHERE queue.state NOT IN ('inflight','failed')`,
		path, kind, string(class), now.UnixNano(), now.UnixNano())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClaimDue 取一批到点的 pending 动作并置为 inflight（单写者下原子完成）。
func (s *Store) ClaimDue(limit int) ([]Action, error) {
	now := time.Now().UnixNano()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(
		`SELECT id,path,kind,state,class,attempts,next_try,last_err,created,updated
		 FROM queue WHERE state='pending' AND next_try<=?
		 ORDER BY next_try ASC, id ASC LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	var out []Action
	var ids []int64
	for rows.Next() {
		var a Action
		var next, created, updated int64
		var kind, st, class string
		if err := rows.Scan(&a.ID, &a.Path, &kind, &st, &class,
			&a.Attempts, &next, &a.LastErr, &created, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		a.Kind = ActionKind(kind)
		a.State = ActionState(st)
		a.Class = RetryClass(class)
		a.NextTry = time.Unix(0, next)
		a.EnqueuedAt = time.Unix(0, created)
		a.UpdatedAt = time.Unix(0, updated)
		out = append(out, a)
		ids = append(ids, a.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(
			`UPDATE queue SET state='inflight', updated=? WHERE id=?`, now, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// ReapInflight 把超时的 inflight（进程崩溃残留）收回 pending。
func (s *Store) ReapInflight(timeout time.Duration) (int64, error) {
	deadline := time.Now().Add(-timeout).UnixNano()
	res, err := s.db.Exec(
		`UPDATE queue SET state='pending', updated=? WHERE state='inflight' AND updated<?`,
		time.Now().UnixNano(), deadline)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Complete 动作成功：置 done，记录成功冷却（Q16 幂等冷却 1 分钟）。
func (s *Store) Complete(id int64) error {
	now := time.Now()
	_, err := s.db.Exec(
		`UPDATE queue SET state='done', attempts=attempts+1, last_err='', next_try=?, updated=? WHERE id=?`,
		now.Add(time.Minute).UnixNano(), now.UnixNano(), id)
	return err
}

// Fail 动作失败：按分类决定退避与是否继续。
//
//	class=network   → 指数退避（2^n 秒，封顶 300s）+ 5~20% 抖动，无限重试
//	class=rate_limit→ 每次至少 60s×attempts（429 大幅降速）
//	class=auth      → state=failed（停队列，等扫码；UI 提醒）
//	class=permanent → state=failed
func (s *Store) Fail(id int64, class RetryClass, msg string) error {
	now := time.Now()
	var attempts int
	err := s.db.QueryRow(`SELECT attempts FROM queue WHERE id=?`, id).Scan(&attempts)
	if err != nil {
		return err
	}
	attempts++

	var next time.Time
	var state ActionState = StatePending
	switch class {
	case ClassNetwork:
		shift := attempts
		if shift > 8 {
			shift = 8 // 2^8=256s ≈ 封顶前
		}
		back := time.Duration(1<<shift) * time.Second
		if back > 300*time.Second {
			back = 300 * time.Second
		}
		// 抖动：5%~20%（避免多客户端同步重试惊群）
		jitter := time.Duration(float64(back) * (0.05 + 0.15*float64((attempts*37%100))/100))
		next = now.Add(back + jitter)
	case ClassRateLimit:
		back := time.Duration(attempts) * 60 * time.Second
		if back > 30*time.Minute {
			back = 30 * time.Minute
		}
		next = now.Add(back)
	case ClassAuth, ClassPermanent:
		state = StateFailed
		next = now
	}

	_, err = s.db.Exec(
		`UPDATE queue SET state=?, class=?, attempts=?, next_try=?, last_err=?, updated=? WHERE id=?`,
		state, string(class), attempts, next.UnixNano(), truncate(msg, 500),
		now.UnixNano(), id)
	return err
}

// PendingAuthFailed 返回因认证失败而卡住的动作数（停队列提醒用）。
func (s *Store) CountAuthFailed() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM queue WHERE state='failed' AND class='auth'`).Scan(&n)
	return n, err
}

// Stats 返回队列统计（UI/日志用）。
func (s *Store) Stats() (map[ActionState]int, error) {
	rows, err := s.db.Query(`SELECT state, COUNT(*) FROM queue GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[ActionState]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[ActionState(st)] = n
	}
	return out, rows.Err()
}

// Retry 把动作重置为立即可执行（面板“重试”）：清空尝试次数与错误，回到 pending。
func (s *Store) Retry(id int64) error {
	_, err := s.db.Exec(
		`UPDATE queue SET state='pending', class='network', attempts=0, next_try=0,
		 last_err='', updated=? WHERE id=?`, time.Now().UnixNano(), id)
	return err
}

// Drop 直接移除动作（面板“放弃”）。
func (s *Store) Drop(id int64) error {
	_, err := s.db.Exec(`DELETE FROM queue WHERE id=?`, id)
	return err
}

// CancelOthers 把同路径除 keep 外的 pending 动作置为 done（last_err 标记
// superseded）。用于 Q18 冲突优先：conflict 入队时吞掉同路径的其他待执行动作。
// inflight 不动（正在执行），done/failed 不动。
func (s *Store) CancelOthers(path string, keep ActionKind) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE queue SET state='done', last_err='superseded', updated=?
		 WHERE path=? AND kind<>? AND state='pending'`,
		time.Now().UnixNano(), path, string(keep))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListActions 列出队列动作（本地面板/诊断用，不改状态）。
// states 为空 → 除 done 外的全部；limit<=0 → 200。按 updated 倒序。
func (s *Store) ListActions(states []ActionState, limit int) ([]Action, error) {
	if limit <= 0 {
		limit = 200
	}
	var (
		where string
		args  []any
	)
	if len(states) == 0 {
		where = `state<>'done'`
	} else {
		ph := make([]string, len(states))
		for i, st := range states {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = `state IN (` + strings.Join(ph, ",") + `)`
	}
	args = append(args, limit)
	rows, err := s.db.Query(
		`SELECT id,path,kind,state,class,attempts,next_try,last_err,created,updated
		 FROM queue WHERE `+where+` ORDER BY updated DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		var a Action
		var next, created, updated int64
		var kind, st, class string
		if err := rows.Scan(&a.ID, &a.Path, &kind, &st, &class,
			&a.Attempts, &next, &a.LastErr, &created, &updated); err != nil {
			return nil, err
		}
		a.Kind = ActionKind(kind)
		a.State = ActionState(st)
		a.Class = RetryClass(class)
		a.NextTry = time.Unix(0, next)
		a.EnqueuedAt = time.Unix(0, created)
		a.UpdatedAt = time.Unix(0, updated)
		out = append(out, a)
	}
	return out, rows.Err()
}

// HasPendingConflict 返回该路径是否已有待执行的 conflict（其他动作让位用）。
func (s *Store) HasPendingConflict(path string) (bool, error) {
	return s.HasPending(path, KindConflict)
}

// HasPending 返回该路径是否已有指定 kind 的待执行动作（动作互斥/升级用）。
func (s *Store) HasPending(path string, kind ActionKind) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM queue WHERE path=? AND kind=? AND state='pending'`,
		path, string(kind)).Scan(&n)
	return n > 0, err
}

// HasPendingPath 判断该路径是否还有未完成动作（含 failed/inflight）。
func (s *Store) HasPendingPath(path string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM queue WHERE path=? AND state<>'done'`, path).Scan(&n)
	return n > 0, err
}

// RetryAuthFailed 把 auth 类 failed 动作复活为 pending（认证恢复后调用，
// 通常由 Poll 在 List 成功后触发——List 能成功即认证有效）。
func (s *Store) RetryAuthFailed() (int64, error) {
	res, err := s.db.Exec(
		`UPDATE queue SET state='pending', next_try=0, updated=? WHERE state='failed' AND class='auth'`,
		time.Now().UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GCDone 清理已完成动作（保留最近 keep 内的，防 done 行无限膨胀：
// 10 万文件 = 10 万行永久滞留）。
func (s *Store) GCDone(keep time.Duration) (int64, error) {
	cutoff := time.Now().Add(-keep).UnixNano()
	res, err := s.db.Exec(`DELETE FROM queue WHERE state='done' AND updated < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// HasPendingTree 判断 prefix 目录（含自身路径）下是否还有未完成动作
// （含 failed/inflight；祖先目录图标恢复用）。
func (s *Store) HasPendingTree(prefix string) (bool, error) {
	esc := strings.ReplaceAll(prefix, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `%`, `\%`)
	esc = strings.ReplaceAll(esc, `_`, `\_`)
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM queue
		 WHERE state IN ('pending','inflight','failed')
		   AND (path = ? OR path LIKE ? ESCAPE '\')`,
		prefix, esc+"/%").Scan(&n)
	return n > 0, err
}

// CancelMissingLocal 收敛本地已消失的旧动作：upload/conflict/dehydrate
// 依赖本地文件存在，本地没了就不能继续执行，否则会拿旧路径打 rclone 404。
// delete_cloud 是“本地删除 -> 删云端”的正当动作，必须保留。
func (s *Store) CancelMissingLocal(present map[string]bool) (int64, error) {
	rows, err := s.db.Query(
		`SELECT id,path,kind FROM queue WHERE state IN ('pending','failed')`)
	if err != nil {
		return 0, err
	}
	type stale struct {
		id   int64
		path string
		kind ActionKind
	}
	var staleActions []stale
	for rows.Next() {
		var a stale
		var kind string
		if err := rows.Scan(&a.id, &a.path, &kind); err != nil {
			rows.Close()
			return 0, err
		}
		a.kind = ActionKind(kind)
		if !present[a.path] {
			switch a.kind {
			case KindUpload, KindConflict, KindDehydrate:
				staleActions = append(staleActions, a)
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	now := time.Now().UnixNano()
	for _, a := range staleActions {
		if _, err := s.db.Exec(
			`UPDATE queue SET state='done', last_err='superseded: local missing',
			 updated=? WHERE id=? AND state IN ('pending','failed')`, now, a.id); err != nil {
			return 0, err
		}
	}
	return int64(len(staleActions)), nil
}

// PendingCount 返回到点待执行数。
func (s *Store) PendingCount() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM queue WHERE state='pending' AND next_try<=?`,
		time.Now().UnixNano()).Scan(&n)
	return n, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
