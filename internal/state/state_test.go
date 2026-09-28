package state

import (
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBaselineGate(t *testing.T) {
	s := openTest(t)
	if done, _ := s.BaselineDone(); done {
		t.Fatal("baseline should start false")
	}
	if err := s.SetBaselineDone(); err != nil {
		t.Fatal(err)
	}
	if done, _ := s.BaselineDone(); !done {
		t.Fatal("baseline should be true after set")
	}
}

func TestSnapshotUpsertAndTombstone(t *testing.T) {
	s := openTest(t)
	f := FileSnap{Path: "a/b.txt", Size: 10, MTime: time.Unix(100, 0), Present: true}
	if err := s.PutSnap("local_snap", f); err != nil {
		t.Fatal(err)
	}
	// upsert 修改
	f.Size = 20
	if err := s.PutSnap("local_snap", f); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetSnap("local_snap", "a/b.txt")
	if err != nil || !ok || got.Size != 20 {
		t.Fatalf("got=%+v ok=%v err=%v", got, ok, err)
	}
	// 墓碑
	if err := s.MarkSnapDeleted("local_snap", "a/b.txt"); err != nil {
		t.Fatal(err)
	}
	got, ok, _ = s.GetSnap("local_snap", "a/b.txt")
	if !ok || got.Present {
		t.Fatalf("tombstone failed: %+v", got)
	}
	all, _ := s.AllSnap("local_snap")
	if len(all) != 1 {
		t.Fatalf("all=%d", len(all))
	}
}

// TestResetLocalSnap 换同步根复位（踩坑 #27）：本地快照清空 + 基线复位
//（否则新根空扫描把旧条目判"本地已删"→ delete_cloud 传到云端），
// 云端快照保留（云端与根无关，保留避免整盘重新下载）。
func TestResetLocalSnap(t *testing.T) {
	s := openTest(t)
	if err := s.PutSnap("local_snap", FileSnap{Path: "old/a.txt", Size: 1, MTime: time.Unix(1, 0), Present: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSnap("cloud_snap", FileSnap{Path: "qmt/b.txt", Size: 2, MTime: time.Unix(2, 0), Present: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBaselineDone(); err != nil {
		t.Fatal(err)
	}

	if err := s.ResetLocalSnap(); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.AllSnap("local_snap"); len(all) != 0 {
		t.Fatalf("local_snap 应清空: %v", all)
	}
	if all, _ := s.AllSnap("cloud_snap"); len(all) != 1 {
		t.Fatalf("cloud_snap 应保留: %v", all)
	}
	if done, _ := s.BaselineDone(); done {
		t.Fatal("基线应复位")
	}
}

func TestEnqueueIdempotentAndInflightStays(t *testing.T) {
	s := openTest(t)
	// 两次入队合并为一条
	if ok, _ := s.Enqueue("x.txt", KindUpload, ClassNetwork); !ok {
		t.Fatal("first enqueue should return true")
	}
	if ok, _ := s.Enqueue("x.txt", KindUpload, ClassNetwork); !ok {
		t.Fatal("second enqueue (pending) should reset and return true")
	}
	// claim → inflight，再次入队不动 inflight
	acts, err := s.ClaimDue(10)
	if err != nil || len(acts) != 1 {
		t.Fatalf("claim: %v n=%d", err, len(acts))
	}
	if ok, _ := s.Enqueue("x.txt", KindUpload, ClassNetwork); ok {
		t.Fatal("enqueue on inflight should not report new")
	}
	// inflight 重入队后仍 inflight → ClaimDue 不会重复拿到
	if acts2, _ := s.ClaimDue(10); len(acts2) != 0 {
		t.Fatalf("inflight should not be claimed again: %d", len(acts2))
	}
	// 完成
	if err := s.Complete(acts[0].ID); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats()
	if st[StateDone] != 1 {
		t.Fatalf("stats=%v", st)
	}
}

func TestRetryBackoffClasses(t *testing.T) {
	s := openTest(t)
	s.Enqueue("n.txt", KindUpload, ClassNetwork)
	s.Enqueue("r.txt", KindUpload, ClassRateLimit)
	s.Enqueue("a.txt", KindUpload, ClassAuth)
	s.Enqueue("p.txt", KindUpload, ClassPermanent)
	acts, _ := s.ClaimDue(10)
	if len(acts) != 4 {
		t.Fatalf("claim=%d", len(acts))
	}
	byPath := map[string]Action{}
	for _, a := range acts {
		byPath[a.Path] = a
	}

	now := time.Now()
	// network → 退避在未来（2s±抖动），仍是 pending
	if err := s.Fail(byPath["n.txt"].ID, ClassNetwork, "conn refused"); err != nil {
		t.Fatal(err)
	}
	// rate_limit → 至少 60s
	if err := s.Fail(byPath["r.txt"].ID, ClassRateLimit, "429"); err != nil {
		t.Fatal(err)
	}
	// auth / permanent → failed
	_ = s.Fail(byPath["a.txt"].ID, ClassAuth, "cookie expired")
	_ = s.Fail(byPath["p.txt"].ID, ClassPermanent, "404")

	// 到点调度：只有 network 的还算 pending 但未到点 → ClaimDue(现在) 拿不到
	claims, _ := s.ClaimDue(10)
	if len(claims) != 0 {
		t.Fatalf("nothing should be due immediately, got %d", len(claims))
	}
	if n, _ := s.CountAuthFailed(); n != 1 {
		t.Fatalf("auth failed count=%d", n)
	}

	// network 退避必须在 (now, now+300s] 内
	var nNext int64
	var nState string
	_ = s.db.QueryRow(`SELECT next_try, state FROM queue WHERE path='n.txt'`).Scan(&nNext, &nState)
	if time.Unix(0, nNext).Before(now) || time.Unix(0, nNext).After(now.Add(310*time.Second)) {
		t.Fatalf("network backoff out of range: %v", time.Unix(0, nNext))
	}
	// rate_limit 退避 ≥ 60s
	var ra struct {
		next int64
		st   string
	}
	_ = s.db.QueryRow(`SELECT next_try, state FROM queue WHERE path='r.txt'`).Scan(&ra.next, &ra.st)
	if time.Unix(0, ra.next).Before(now.Add(55 * time.Second)) {
		t.Fatalf("rate_limit backoff too short: %v", time.Unix(0, ra.next))
	}
	// 无限重试：network 失败 100 次仍是 pending
	for i := 0; i < 100; i++ {
		var id int64
		var st string
		_ = s.db.QueryRow(`SELECT id, state FROM queue WHERE path='n.txt'`).Scan(&id, &st)
		if st != string(StatePending) {
			t.Fatalf("network must retry forever, state=%s", st)
		}
		// 直接改 next_try 到过去模拟到点，再失败
		_, _ = s.db.Exec(`UPDATE queue SET next_try=0 WHERE id=?`, id)
		claims, _ := s.ClaimDue(10)
		var target *Action
		for i := range claims {
			if claims[i].Path == "n.txt" {
				target = &claims[i]
			}
		}
		if target == nil {
			t.Fatalf("iteration %d: n.txt not claimable", i)
		}
		_ = s.Fail(target.ID, ClassNetwork, "err")
	}
}

func TestReapInflight(t *testing.T) {
	s := openTest(t)
	s.Enqueue("x", KindUpload, ClassNetwork)
	acts, _ := s.ClaimDue(1)
	// 手动把 updated 拨回 10 分钟前（模拟崩溃残留）
	_, _ = s.db.Exec(`UPDATE queue SET updated=? WHERE id=?`,
		time.Now().Add(-10*time.Minute).UnixNano(), acts[0].ID)
	n, err := s.ReapInflight(5 * time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("reap n=%d err=%v", n, err)
	}
	claims, _ := s.ClaimDue(10)
	if len(claims) != 1 {
		t.Fatalf("should be claimable after reap: %d", len(claims))
	}
}
