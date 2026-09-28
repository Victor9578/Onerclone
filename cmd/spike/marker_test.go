package main

// marker_test.go —— 踩坑 #27：同步根身份标识。根被整删重建（同一路径）时
// 标识缺失 → 复位本地快照 + 基线，防止空扫描把云端刚上传的文件全清掉。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"onerclone/internal/state"
)

func openMarkerState(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestEnsureRootMarkerResetsOnRecreatedRoot(t *testing.T) {
	st := openMarkerState(t)
	// 旧根的真相：本地快照有条目、云端快照有条目、基线已建立
	if err := st.PutSnap("local_snap", state.FileSnap{Path: "17_旬阳招投标/a.docx", Size: 1, MTime: time.Now(), Present: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnap("cloud_snap", state.FileSnap{Path: "17_旬阳招投标/a.docx", Size: 1, MTime: time.Now(), Present: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBaselineDone(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir() // 无标识 = 根是新建/重建的（cmdRun 的 MkdirAll 会静默重建）
	if err := ensureRootMarker(root, st); err != nil {
		t.Fatal(err)
	}
	if all, _ := st.AllSnap("local_snap"); len(all) != 0 {
		t.Fatalf("本地快照应复位: %v", all)
	}
	if all, _ := st.AllSnap("cloud_snap"); len(all) != 1 {
		t.Fatalf("云端快照应保留: %v", all)
	}
	if done, _ := st.BaselineDone(); done {
		t.Fatal("基线应复位（DR2 重新保护）")
	}
	if _, err := os.Stat(filepath.Join(root, rootMarkerName)); err != nil {
		t.Fatalf("标识文件应写出: %v", err)
	}

	// 幂等：标识已在 → 即便基线已重建也不再复位
	if err := st.SetBaselineDone(); err != nil {
		t.Fatal(err)
	}
	if err := ensureRootMarker(root, st); err != nil {
		t.Fatal(err)
	}
	if done, _ := st.BaselineDone(); !done {
		t.Fatal("标识存在时不应再复位")
	}
}

func TestScanSkipsRootMarker(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, rootMarkerName), []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &localFS{root: root, nm: newNameMap(nil)}
	entries, err := l.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "f.txt" {
		t.Fatalf("标识文件不应进同步（也不该入队上传）: %+v", entries)
	}
}
