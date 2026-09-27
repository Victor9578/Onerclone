package main

// adapters_test.go —— localFS 的本地名↔云端名 转换（DR4 方案 A 落地验证）。

import (
	"os"
	"path/filepath"
	"testing"

	"onerclone/internal/engine"
)

// TestLocalFSScanReverseMaps 本地磁盘上的 `来自：分享` 必须以云端原名
// `来自:分享` 汇报给 engine —— 否则下一轮 Scan 会把它当成"云端没有的
// 新文件"触发回传，凭空多出一套目录。
func TestLocalFSScanReverseMaps(t *testing.T) {
	root := t.TempDir()
	nm := newNameMap(nil)

	// 模拟下载落地：engine 键是云端原名，实际写盘的是转义后的名字
	const cloudRel = "来自:分享/内部.txt"
	localRel := nm.localPath(cloudRel)
	if localRel != "来自%3A分享/内部.txt" {
		t.Fatalf("localPath = %q", localRel)
	}
	abs := filepath.Join(root, filepath.FromSlash(localRel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := &localFS{root: root, nm: nm}
	entries, err := l.Scan()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Path == cloudRel {
			found = true
		}
		if e.Path == localRel {
			t.Fatalf("Scan 泄漏了本地名 %q，engine 键必须是云端原名", e.Path)
		}
	}
	if !found {
		var got []string
		for _, e := range entries {
			got = append(got, e.Path)
		}
		t.Fatalf("Scan 未还原云端原名 %q，实际: %v", cloudRel, got)
	}
}

// TestLocalFSPathMapping localFS 的写路径必须先过映射（mkdir/占位符/删除）。
func TestLocalFSPathMapping(t *testing.T) {
	root := t.TempDir()
	l := &localFS{root: root, nm: newNameMap(nil)}

	got := l.path("a:b/c.txt")
	want := filepath.Join(root, "a%3Ab", "c.txt")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}

	// ApplyDownload 建目录：非法名必须能真的建出来（且是 ASCII 转义名，rclone 可访问）
	if err := l.ApplyDownload("来自:分享", engine.CloudEntry{IsDir: true}); err != nil {
		t.Fatalf("ApplyDownload(来自:分享) 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "来自%3A分享")); err != nil {
		t.Fatalf("转义后的目录没建出来: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "来自:分享")); !os.IsNotExist(err) {
		t.Fatal("不应存在带冒号的原始目录")
	}
	if _, err := os.Stat(filepath.Join(root, "来自：分享")); !os.IsNotExist(err) {
		t.Fatal("不应存在全角冒号目录（rclone 会折形导致找不到）")
	}
}
