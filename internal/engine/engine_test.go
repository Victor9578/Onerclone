package engine

import (
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"onerclone/internal/state"
)

// ---------- fake 实现 ----------

type fakeCloud struct {
	mu    map[string]CloudEntry
	listE error
}

func newFakeCloud() *fakeCloud { return &fakeCloud{mu: map[string]CloudEntry{}} }

func (f *fakeCloud) List() ([]CloudEntry, error) {
	if f.listE != nil {
		return nil, f.listE
	}
	out := []CloudEntry{}
	for _, e := range f.mu {
		out = append(out, e)
	}
	return out, nil
}

func (f *fakeCloud) Stat(rel string) (*CloudEntry, error) {
	e, ok := f.mu[rel]
	if !ok {
		return nil, nil
	}
	return &e, nil
}

func (f *fakeCloud) Upload(rel string) error      { return nil }
func (f *fakeCloud) Download(rel string) error    { return nil }
func (f *fakeCloud) DownloadTo(a, b string) error { return nil }
func (f *fakeCloud) Delete(rel string) error {
	delete(f.mu, rel)
	return nil
}
func (f *fakeCloud) Purge(rel string) error {
	// 目录递归删：删掉所有以 rel/ 开头的子条目
	for p := range f.mu {
		if p == rel || strings.HasPrefix(p, rel+"/") {
			delete(f.mu, p)
		}
	}
	return nil
}
func (f *fakeCloud) Mkdir(rel string) error { return nil }

type fakeLocal struct {
	mu map[string]CloudEntry

	// Q6：这些路径“曾水合”（读过数据），云端变更后引擎应主动重拉
	hydrated     map[string]bool
	hydrateCalls []string
}

func newFakeLocal() *fakeLocal {
	return &fakeLocal{mu: map[string]CloudEntry{}, hydrated: map[string]bool{}}
}

func (f *fakeLocal) Scan() ([]CloudEntry, error) {
	out := []CloudEntry{}
	for _, e := range f.mu {
		out = append(out, e)
	}
	return out, nil
}
func (f *fakeLocal) Stat(rel string) (*CloudEntry, error) {
	e, ok := f.mu[rel]
	if !ok {
		return nil, nil
	}
	return &e, nil
}
func (f *fakeLocal) ApplyDownload(rel string, e CloudEntry) error {
	f.mu[rel] = e
	return nil
}
func (f *fakeLocal) Remove(rel string) error {
	delete(f.mu, rel)
	return nil
}
func (f *fakeLocal) FinalizeUpload(rel string) error { return nil }
func (f *fakeLocal) WasHydrated(rel string) bool     { return f.hydrated[rel] }
func (f *fakeLocal) Hydrate(rel string) error {
	f.hydrateCalls = append(f.hydrateCalls, rel)
	return nil
}

// ---------- 工具 ----------

func newTestEngine(t *testing.T) (*Engine, *state.Store, *fakeCloud, *fakeLocal) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fc, fl := newFakeCloud(), newFakeLocal()
	logger := log.New(io.Discard, "", 0)
	return New(st, fc, fl, logger), st, fc, fl
}

func e(path string, size int64, mt time.Time) CloudEntry {
	return CloudEntry{Path: path, Size: size, MTime: mt}
}

// drain 手动执行队列里所有到点动作（绕过 Worker 的睡眠循环）。
func drain(t *testing.T, eng *Engine) {
	t.Helper()
	for i := 0; i < 50; i++ {
		acts, err := eng.store.ClaimDue(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(acts) == 0 {
			return
		}
		for _, a := range acts {
			eng.exec(a)
		}
	}
}

// ---------- 测试 ----------

func TestScanNewLocalUpload(t *testing.T) {
	eng, st, _, fl := newTestEngine(t)
	mt := time.Now().Add(-time.Hour)
	fl.mu["a.txt"] = e("a.txt", 10, mt)

	if n, err := eng.Scan(); err != nil || n != 1 {
		t.Fatalf("scan n=%d err=%v", n, err)
	}
	// 基线首轮：云端无此文件 → upload 是允许的（基线只挡删除/覆盖）
	drain(t, eng)
	stats, _ := st.Stats()
	if stats[state.StateDone] != 1 {
		t.Fatalf("stats=%v", stats)
	}
}

func TestScanUnchangedNoAction(t *testing.T) {
	eng, st, _, fl := newTestEngine(t)
	mt := time.Now().Add(-time.Hour)
	fl.mu["a.txt"] = e("a.txt", 10, mt)
	_, _ = eng.Scan()
	drain(t, eng)
	// 第二次扫描：无变化 → 无新动作
	if n, err := eng.Scan(); err != nil || n != 0 {
		t.Fatalf("second scan n=%d err=%v", n, err)
	}
	stats, _ := st.Stats()
	if stats[state.StateDone] != 1 {
		t.Fatalf("stats=%v", stats)
	}
}

func TestPollNewCloudDownload(t *testing.T) {
	eng, st, fc, fl := newTestEngine(t)
	mt := time.Now().Add(-time.Hour)
	fc.mu["b.txt"] = e("b.txt", 20, mt)

	// 首轮 poll：基线建立（本地空 + 云端 b.txt）
	// 注意：b.txt 本地不存在 → download
	if n, err := eng.Poll(); err != nil || n != 1 {
		t.Fatalf("poll n=%d err=%v", n, err)
	}
	if done, _ := st.BaselineDone(); !done {
		t.Fatal("baseline should be done after first poll")
	}
	drain(t, eng)
	// download 执行后 local 应有 b.txt
	// （fakeLocal.ApplyDownload 写入）
	stats, _ := st.Stats()
	if stats[state.StateDone] != 1 {
		t.Fatalf("stats=%v", stats)
	}
	if len(fl.hydrateCalls) != 0 {
		t.Fatalf("首轮未水合过，不该重拉: %v", fl.hydrateCalls)
	}
}

// TestQ6RereadsPreviouslyHydratedFile —— Q6 完整语义：
// 曾水合的文件在云端变更后**主动**重拉数据（而非只重建占位符），
// 下载窗口结束即可离线读；从未水合过的新文件只建占位符（不白拉流量）。
func TestQ6RereadsPreviouslyHydratedFile(t *testing.T) {
	eng, _, fc, fl := newTestEngine(t)
	mt := time.Now().Add(-time.Hour)
	fc.mu["b.txt"] = e("b.txt", 20, mt)

	// 第一轮：新文件 → 只建占位符，不重拉
	if n, err := eng.Poll(); err != nil || n != 1 {
		t.Fatalf("首轮 poll n=%d err=%v", n, err)
	}
	drain(t, eng)
	if len(fl.hydrateCalls) != 0 {
		t.Fatalf("新文件不该主动重拉: %v", fl.hydrateCalls)
	}

	// 模拟用户读过 → 已水合
	fl.hydrated["b.txt"] = true

	// 云端变更 → 下载 → 必须主动重拉
	fc.mu["b.txt"] = e("b.txt", 30, mt.Add(time.Minute))
	if n, err := eng.Poll(); err != nil || n != 1 {
		t.Fatalf("二轮 poll n=%d err=%v", n, err)
	}
	drain(t, eng)

	if len(fl.hydrateCalls) != 1 || fl.hydrateCalls[0] != "b.txt" {
		t.Fatalf("Q6 应主动重拉 b.txt, got %v", fl.hydrateCalls)
	}
}

func TestDeleteLocalGatedByBaseline(t *testing.T) {
	eng, st, fc, fl := newTestEngine(t)
	mt := time.Now().Add(-time.Hour)
	// 本地有 a，云端有 a（先建立基线）
	fl.mu["a.txt"] = e("a.txt", 10, mt)
	fc.mu["a.txt"] = e("a.txt", 10, mt)
	if _, err := eng.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Poll(); err != nil {
		t.Fatal(err)
	}
	if done, _ := st.BaselineDone(); !done {
		t.Fatal("baseline should be done")
	}
	// 先执行完首轮动作（生产中 worker 会立刻跑；残留 pending upload 会
	// 让后续 delete_local 的 localChanged 判定走复活分支）
	drain(t, eng)
	// 云端删 a → poll 应入队 delete_local（基线已完成）
	delete(fc.mu, "a.txt")
	if n, err := eng.Poll(); err != nil || n != 1 {
		t.Fatalf("poll n=%d err=%v", n, err)
	}
	drain(t, eng)
	if _, ok := fl.mu["a.txt"]; ok {
		t.Fatal("local file should be deleted")
	}
}

func TestDeleteCloudGatedByBaseline(t *testing.T) {
	eng, st, fc, fl := newTestEngine(t)
	mt := time.Now().Add(-time.Hour)
	// 只有本地有 a（首轮 scan 云端空）→ 基线未建立（poll 还没跑）
	fl.mu["a.txt"] = e("a.txt", 10, mt)
	// 手动把 baseline 置 false 模拟首轮进行中
	if _, err := eng.Scan(); err != nil {
		t.Fatal(err)
	}
	// 删除本地 → scan 检测消失，但基线未完成（没跑过 Poll）→ 应被 DR2 挡住
	delete(fl.mu, "a.txt")
	if done, _ := st.BaselineDone(); done {
		t.Fatal("baseline should still be false")
	}
	if n, err := eng.Scan(); err != nil || n != 0 {
		t.Fatalf("delete should be gated, n=%d err=%v", n, err)
	}
	_ = st
	_ = fc
}

func TestConflictBothChanged(t *testing.T) {
	eng, st, fc, fl := newTestEngine(t)
	mt := time.Now().Add(-time.Hour)
	// 基线：双方一致
	fl.mu["c.txt"] = e("c.txt", 10, mt)
	fc.mu["c.txt"] = e("c.txt", 10, mt)
	_, _ = eng.Scan()
	_, _ = eng.Poll()
	drain(t, eng)

	// 双方都改（size 不同）
	fl.mu["c.txt"] = e("c.txt", 11, mt.Add(time.Minute))
	fc.mu["c.txt"] = e("c.txt", 12, mt.Add(2*time.Minute))

	// 先 poll：scanLocked 检测本地变→upload，云端变+本地变→conflict 升级抢占
	// （upload 被 CancelOthers 立即取消）。返回值含入队计数，最终态以队列为准。
	n, err := eng.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("poll should enqueue something, n=%d", n)
	}
	// 查队列：最终只应剩 conflict（Q18 冲突优先，upload 被抢占取消）
	acts, _ := st.ClaimDue(10)
	if len(acts) != 1 || acts[0].Kind != state.KindConflict {
		t.Fatalf("expected exactly 1 pending conflict, got %+v", acts)
	}
	// conflict 执行（fake 全 no-op，验证状态流转）
	eng.exec(acts[0])
	stats, _ := st.Stats()
	// done=2：基线轮 upload 1 个 + conflict 1 个
	if stats[state.StateDone] != 2 {
		t.Fatalf("after conflict exec, stats=%v", stats)
	}
	_ = fc
}

func TestUploadPreCheckTurnsToConflict(t *testing.T) {
	eng, st, fc, fl := newTestEngine(t)
	mt := time.Now().Add(-time.Hour)
	// 本地改了（无云端快照）→ scan 入队 upload
	fl.mu["d.txt"] = e("d.txt", 10, mt)
	_, _ = eng.Scan()
	// 但云端其实已有一个我们不知道的版本（cloud_snap 空、云端存在）
	fc.mu["d.txt"] = e("d.txt", 99, mt.Add(-time.Minute))

	acts, _ := st.ClaimDue(10)
	if len(acts) != 1 || acts[0].Kind != state.KindUpload {
		t.Fatalf("acts=%+v", acts)
	}
	eng.exec(acts[0])
	// upload 前复查应转 conflict
	acts2, _ := st.ClaimDue(10)
	if len(acts2) != 1 || acts2[0].Kind != state.KindConflict {
		t.Fatalf("expected conflict, got %+v", acts2)
	}
	stats, _ := st.Stats()
	_ = stats
}

func TestDeleteLocalRevivesWhenLocalChanged(t *testing.T) {
	// 云端删 + 本地也改 → Q18 宁复活不删除（Poll 应转 upload 而非 delete）
	eng, st, fc, fl := newTestEngine(t)
	mt := time.Now().Add(-time.Hour)
	fl.mu["e.txt"] = e("e.txt", 10, mt)
	fc.mu["e.txt"] = e("e.txt", 10, mt)
	_, _ = eng.Scan()
	_, _ = eng.Poll()
	drain(t, eng)

	// 云端删 + 本地改
	delete(fc.mu, "e.txt")
	fl.mu["e.txt"] = e("e.txt", 20, mt.Add(time.Minute))
	// Scan：本地变 → 恒 upload（conflict 由互斥升级/前复查裁决）
	nScan, err := eng.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if nScan != 1 {
		t.Fatalf("scan should enqueue upload, n=%d", nScan)
	}
	// Poll：云端消失 + 本地本轮变过 → localChanged=true → 走 Q18 复活分支，
	// upload 幂等重置（Enqueue 对已有 pending 重置也返回 true，n 可为 1），
	// 关键是**绝不能入队 delete_local**。
	if _, err := eng.Poll(); err != nil {
		t.Fatal(err)
	}
	acts, err := st.ClaimDue(10)
	if err != nil || len(acts) != 1 {
		t.Fatalf("acts=%+v err=%v", acts, err)
	}
	if acts[0].Kind != state.KindUpload {
		t.Fatalf("expected upload (no delete_local!), got %s", acts[0].Kind)
	}
	// execUpload：云端已删（Stat=nil）→ 直接上传 = Q18 复活
	eng.exec(acts[0])
	stats, _ := st.Stats()
	if stats[state.StateDone] == 0 {
		t.Fatalf("upload should complete, stats=%v", stats)
	}
}
// TestClassifyCorruptPlaceholder 损坏占位符（踩坑 #24）：cldflt 对一切访问
// 返回 "cloud file metadata is corrupt"，当轮重试无意义 → 必须归永久失败
//（此前归 network 类导致每轮重试、日志刷屏）。
func TestClassifyCorruptPlaceholder(t *testing.T) {
	err := errors.New(`unlinkat D:\sync\a.pdf: The cloud file metadata is corrupt and unreadable.`)
	if got := classify(err); got != state.ClassPermanent {
		t.Fatalf("classify(corrupt) = %s, want permanent", got)
	}
}