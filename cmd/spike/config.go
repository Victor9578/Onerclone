package main

// config.go —— 成品化：配置文件（onerclone.json）+ rclone 自动发现。
//
// 配置优先级：**命令行 flag > 配置文件 > 内置默认**。
// 配置文件放在 exe 同目录（首次运行自动生成模板；exe 目录不可写时退回
// 用户配置目录），全部字段可选。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// config 是 onerclone.json 的结构。
type config struct {
	// SyncRoot 本地同步根（必须 NTFS）
	SyncRoot string `json:"sync_root,omitempty"`
	// Remote 真实 rclone remote（如 quark:）；留空 = 本地替身模式（用 Fs）
	Remote string `json:"remote,omitempty"`
	// Fs 本地替身目录（仅 Remote 为空时使用，含示例数据）
	Fs string `json:"fs,omitempty"`
	// Rclone rclone 可执行文件；留空 = 自动发现（exe 同目录 → PATH → 默认）
	Rclone string `json:"rclone,omitempty"`
	// Offline 离线模式：水合请求立即快速失败
	Offline bool `json:"offline,omitempty"`
}

// defaultConfig 内置默认：同步根 = 用户目录，云端 = 真实 quark remote。
func defaultConfig() config {
	return config{
		SyncRoot: defaultRoot(),
		Remote:   "quark:",
	}
}

// applyDefaults 补齐缺省字段（配置文件里留空的）。
func (c *config) applyDefaults() {
	if c.SyncRoot == "" {
		c.SyncRoot = defaultRoot()
	}
	if c.Fs == "" {
		c.Fs = defaultFs()
	}
}

// configPaths 返回配置文件候选路径（exe 同目录优先，其次用户配置目录）。
func configPaths() []string {
	var out []string
	if exe, err := os.Executable(); err == nil && exe != "" {
		out = append(out, filepath.Join(filepath.Dir(exe), "onerclone.json"))
	}
	if d, err := os.UserConfigDir(); err == nil && d != "" {
		out = append(out, filepath.Join(d, "Onerclone", "onerclone.json"))
	}
	if len(out) == 0 {
		if p, err := filepath.Abs("onerclone.json"); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// loadConfig 读配置；不存在则用内置默认并生成模板（首次运行友好）。
// writeTemplate=false 时只读不写（login/quark-login 等纯配置命令用，
// 避免在 exe 旁生成模板污染发布目录）。
func loadConfig() config { return loadConfigOpt(true) }

func loadConfigOpt(writeTemplate bool) config {
	paths := configPaths()
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c config
		// 记事本/PS 保存的 JSON 常带 UTF-8 BOM，Go 的 json 不认，先剥掉
		b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
		if err := json.Unmarshal(b, &c); err != nil {
			log.Printf("⚠ 配置 %s 解析失败（改用内置默认）: %v", p, err)
			c = defaultConfig()
			c.applyDefaults()
			return c
		}
		c.applyDefaults()
		log.Printf("📄 已加载配置: %s", p)
		return c
	}

	// 首次运行：写出模板
	c := defaultConfig()
	c.applyDefaults()
	if !writeTemplate {
		return c
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err == nil {
		for _, p := range paths {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				continue
			}
			if err := os.WriteFile(p, append(data, '\n'), 0o644); err != nil {
				continue
			}
			log.Printf("📝 已生成默认配置: %s（可编辑 sync_root / remote / rclone）", p)
			break
		}
	}
	return c
}

// pick 三级取值：flag → 配置 → 默认。
func pick(flagVal, cfgVal, def string) string {
	if flagVal != "" {
		return flagVal
	}
	if cfgVal != "" {
		return cfgVal
	}
	return def
}

// configWritePath 返回配置写回目标：已存在的配置文件优先（保持用户
// 手改的字段），否则 exe 同目录，再否则用户配置目录。
func configWritePath() string {
	paths := configPaths()
	for _, p := range paths {
		if fileExists(p) {
			return p
		}
	}
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
			return p
		}
	}
	return ""
}

// saveConfig 把配置写回 onerclone.json（面板设置区用）。只覆盖给出的
// 字段：零值字段保留磁盘上已有配置的值，避免把用户没动的字段抹掉。
func saveConfig(patch config) (string, error) {
	p := configWritePath()
	if p == "" {
		return "", fmt.Errorf("找不到可写的配置目录")
	}
	cur := defaultConfig()
	if b, err := os.ReadFile(p); err == nil {
		b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
		_ = json.Unmarshal(b, &cur) // 解析失败则从默认开始
	}
	if patch.SyncRoot != "" {
		cur.SyncRoot = patch.SyncRoot
	}
	if patch.Remote != "" {
		cur.Remote = patch.Remote
	}
	if patch.Fs != "" {
		cur.Fs = patch.Fs
	}
	if patch.Rclone != "" {
		cur.Rclone = patch.Rclone
	}
	cur.Offline = patch.Offline || cur.Offline
	data, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// resolveRclone 找 rclone 可执行文件：flag → 配置 → exe 同目录 → PATH → 兜底。
func resolveRclone(flagVal, cfgVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if cfgVal != "" {
		if fileExists(cfgVal) {
			return cfgVal
		}
		log.Printf("⚠ 配置里的 rclone 不存在: %s（继续自动发现）", cfgVal)
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		if p := filepath.Join(filepath.Dir(exe), "rclone.exe"); fileExists(p) {
			return p
		}
	}
	if p, err := exec.LookPath("rclone.exe"); err == nil {
		return p
	}
	if p, err := exec.LookPath("rclone"); err == nil {
		return p
	}
	// 兜底候选（按本机常见安装位置依次找；找不到时由后续报错暴露）
	for _, p := range []string{
		`D:\Tools\rclone\rclone.exe`,
		`D:\Software\rclone\rclone.exe`,
		`D:\Tools\onerclone\rclone.exe`,
	} {
		if fileExists(p) {
			return p
		}
	}
	return `D:\Tools\rclone\rclone.exe`
}

// fileExists 是普通文件存在性判断。
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
