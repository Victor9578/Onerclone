package app

// converge_test.go —— 踩坑 #26：rclone（Windows）读不到的全角名必须被
// 改名收敛成引擎键（%XX 纯 ASCII），其余全角字符（（）等）保持字面名。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRcloneBlindSeg(t *testing.T) {
	blind := []string{
		"：", "？", "＜", "＞", "｜", "＊", "＼", "＂", // rclone 实测改写集
		"附：件.txt", "a？b", "x＼y",
	}
	for _, s := range blind {
		if !rcloneBlindSeg(s) {
			t.Errorf("rcloneBlindSeg(%q) = false, 应为 true", s)
		}
	}
	readable := []string{
		"（）", "Ａ.txt", "a／b", // 实测：括号/全角字母/全角斜杠 rclone 照常可读
		"plain.txt", "中文名.pdf", "a%3Ab", "a:b", // ASCII 冒号不可能出现在磁盘，判否即可
	}
	for _, s := range readable {
		if rcloneBlindSeg(s) {
			t.Errorf("rcloneBlindSeg(%q) = true, 应为 false", s)
		}
	}
}

// TestConvergeRenamesBlindFile 含全角冒号的本地文件必须被改名成转义名：
// 磁盘名 = 引擎键 = 云端名，copyfile 源侧 rclone 才读得到（否则必 404）。
func TestConvergeRenamesBlindFile(t *testing.T) {
	root := t.TempDir()
	lit := filepath.Join(root, "附：件.txt")
	if err := os.WriteFile(lit, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := &localFS{root: root, nm: newNameMap(nil)}
	entries, err := l.Scan()
	if err != nil {
		t.Fatal(err)
	}
	const want = "附%EF%BC%9A件.txt"
	if len(entries) != 1 || entries[0].Path != want {
		t.Fatalf("engine 键应为 %q, got %+v", want, entries)
	}
	// 物理名已收敛：转义名存在、全角名消失
	if _, err := os.Stat(filepath.Join(root, want)); err != nil {
		t.Fatalf("转义名文件不存在: %v", err)
	}
	if _, err := os.Stat(lit); !os.IsNotExist(err) {
		t.Fatal("全角名文件应已被改名移除")
	}
	// localPath(引擎键) = 物理转义名（rclone 可读），不得解析回全角旧名
	if got := l.nm.localPath(want); got != want {
		t.Fatalf("localPath(%q) = %q，应 identity（改名后两侧字面一致）", want, got)
	}

	// 第二轮必须幂等：% 不得被二次转义成 %25，也不得重复改名
	entries2, err := l.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries2) != 1 || entries2[0].Path != want {
		t.Fatalf("第二轮 Scan 不幂等: %+v", entries2)
	}
	files, _ := os.ReadDir(root)
	if len(files) != 1 || files[0].Name() != want {
		var names []string
		for _, f := range files {
			names = append(names, f.Name())
		}
		t.Fatalf("磁盘残留异常: %v", names)
	}
}

// TestConvergeKeepsReadableFullwidth 全角括号 rclone 读得到：物理名保持
// 用户字面名（不改名），引擎键是转义名，localPath 解析回字面名供 copyfile 用。
func TestConvergeKeepsReadableFullwidth(t *testing.T) {
	root := t.TempDir()
	lit := filepath.Join(root, "c（d）.txt")
	if err := os.WriteFile(lit, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := &localFS{root: root, nm: newNameMap(nil)}
	entries, err := l.Scan()
	if err != nil {
		t.Fatal(err)
	}
	const key = "c%EF%BC%88d%EF%BC%89.txt"
	if len(entries) != 1 || entries[0].Path != key {
		t.Fatalf("engine 键应为 %q, got %+v", key, entries)
	}
	// 物理名不许被改名
	if _, err := os.Stat(lit); err != nil {
		t.Fatalf("括号文件不应被改名: %v", err)
	}
	// copyfile 源侧 = 字面全角名（rclone 实测可读，14:00:36 真机已验证）
	if got := l.nm.localPath(key); got != "c（d）.txt" {
		t.Fatalf("localPath(%q) = %q", key, got)
	}
}

// TestConvergeBlindDir 目录名含全角冒号：父目录先改名，子项在其下继续收敛。
func TestConvergeBlindDir(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "x：y")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "z.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := &localFS{root: root, nm: newNameMap(nil)}
	entries, err := l.Scan()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Path] = true
	}
	for _, want := range []string{"x%EF%BC%9Ay", "x%EF%BC%9Ay/z.txt"} {
		if !got[want] {
			t.Fatalf("缺少 %q, got %v", want, got)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "x%EF%BC%9Ay", "z.txt")); err != nil {
		t.Fatalf("收敛后的子文件不存在: %v", err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("全角名目录应已被改名")
	}
	// 幂等：二轮无 %25、无重复
	entries2, err := l.Scan()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries2 {
		if strings.Contains(e.Path, "%25") {
			t.Fatalf("出现二次转义: %q", e.Path)
		}
	}
	if len(entries2) != len(entries) {
		t.Fatalf("二轮条目数变化: %d → %d", len(entries), len(entries2))
	}
}
