package main

// config_test.go —— 成品化：配置三级优先级、模板生成、rclone 自动发现。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPickPrecedence(t *testing.T) {
	if got := pick("flag", "cfg", "def"); got != "flag" {
		t.Errorf("flag 应优先, got %q", got)
	}
	if got := pick("", "cfg", "def"); got != "cfg" {
		t.Errorf("配置应次之, got %q", got)
	}
	if got := pick("", "", "def"); got != "def" {
		t.Errorf("应退回默认, got %q", got)
	}
}

func TestLoadConfigGeneratesTemplate(t *testing.T) {
	paths := configPaths()
	if len(paths) == 0 {
		t.Fatal("没有配置路径候选")
	}
	p := paths[0] // exe（测试二进制）同目录，在临时构建目录里，可安全写入
	_ = os.Remove(p)
	defer os.Remove(p)

	c := loadConfig()
	if c.SyncRoot == "" {
		t.Error("默认同步根不应为空")
	}
	if c.Remote != "quark:" {
		t.Errorf("默认 remote 应为 quark:, got %q", c.Remote)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("首次运行应生成模板: %v", err)
	}
	var back config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("模板应是合法 JSON: %v\n%s", err, b)
	}
	if back.Remote != "quark:" {
		t.Errorf("模板 remote 不对: %q", back.Remote)
	}
}

func TestLoadConfigReadsExisting(t *testing.T) {
	paths := configPaths()
	p := paths[0]
	want := config{SyncRoot: `D:\custom\root`, Remote: `other:`, Rclone: `E:\rclone.exe`, Offline: true}
	b, _ := json.Marshal(want)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Skipf("无法写临时配置: %v", err)
	}
	defer os.Remove(p)

	got := loadConfig()
	if got.SyncRoot != want.SyncRoot || got.Remote != want.Remote ||
		got.Rclone != want.Rclone || !got.Offline {
		t.Errorf("读回不对: %+v", got)
	}
	if got.Fs == "" {
		t.Error("Fs 空值应被补默认")
	}
}

func TestLoadConfigToleratesBOM(t *testing.T) {
	// Notepad / PS5.1 保存的 UTF-8 JSON 带 BOM，不能让解析挂掉
	paths := configPaths()
	p := paths[0]
	body := []byte{0xEF, 0xBB, 0xBF}
	body = append(body, []byte(`{"remote":"other:","sync_root":"D\\x"}`)...)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Skipf("无法写临时配置: %v", err)
	}
	defer os.Remove(p)

	c := loadConfig()
	// JSON 值 "D\\x" 解码后是 D\x
	if c.Remote != "other:" || c.SyncRoot != "D\\x" {
		t.Errorf("带 BOM 的配置应能正常读取, got %+v", c)
	}
}

func TestResolveRclonePrecedence(t *testing.T) {
	if got := resolveRclone(`X:\flag.exe`, `Y:\cfg.exe`); got != `X:\flag.exe` {
		t.Errorf("flag 应优先, got %q", got)
	}
	missing := filepath.Join(t.TempDir(), "nope.exe")
	got := resolveRclone("", missing)
	if got == "" {
		t.Fatal("应返回非空路径")
	}
	if got == missing {
		t.Error("不存在的配置路径不应被采用（继续自动发现）")
	}
	// 配置指向真实存在的文件 → 采用
	real := filepath.Join(t.TempDir(), "rclone.exe")
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveRclone("", real); got != real {
		t.Errorf("存在的配置路径应被采用, got %q", got)
	}
}
