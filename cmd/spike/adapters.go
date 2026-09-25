package main

// adapters.go —— engine.Cloud / engine.Local 的真实现（rclone RC + cfapi）。
//
// 引擎与平台解耦：engine 包只管决策与队列，所有副作用经这两个接口落地。

import (
	"fmt"
	"os"
	"path/filepath"

	"onerclone/internal/cfapi"
	"onerclone/internal/engine"
	"onerclone/internal/rclone"
)

// ---------- Cloud：rclone RC ----------

// cloudRC 把 rclone RC 包装成 engine.Cloud。
type cloudRC struct {
	rc    *rclone.Client
	srcFs string // 本地同步根（rclone 以 local 后端读它）
	dstFs string // 云端 fs（P0=本地替身目录；P1=quark remote）
}

func (c *cloudRC) List() ([]engine.CloudEntry, error) {
	entries, err := c.rc.ListRecursive(c.dstFs, "")
	if err != nil {
		return nil, err
	}
	out := make([]engine.CloudEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, engine.CloudEntry{
			Path:  e.Path,
			Size:  e.Size,
			MTime: e.ModTime,
			IsDir: e.IsDir,
		})
	}
	return out, nil
}

func (c *cloudRC) Stat(rel string) (*engine.CloudEntry, error) {
	e, err := c.rc.Stat(c.dstFs, rel)
	if err != nil || e == nil {
		return nil, err
	}
	return &engine.CloudEntry{
		Path:  e.Path,
		Size:  e.Size,
		MTime: e.ModTime,
		IsDir: e.IsDir,
	}, nil
}

func (c *cloudRC) Upload(rel string) error {
	return c.rc.CopyFile(c.srcFs, rel, c.dstFs, rel)
}

func (c *cloudRC) Download(rel string) error {
	return c.rc.CopyFile(c.dstFs, rel, c.srcFs, rel)
}

func (c *cloudRC) DownloadTo(src, dst string) error {
	return c.rc.CopyFile(c.dstFs, src, c.srcFs, dst)
}

func (c *cloudRC) Delete(rel string) error {
	return c.rc.DeleteFile(c.dstFs, rel)
}

func (c *cloudRC) Mkdir(rel string) error {
	return c.rc.Mkdir(c.dstFs, rel)
}

// ---------- Local：文件系统 + cfapi 占位符 ----------

// localFS 把同步根 + cfapi 包装成 engine.Local。
type localFS struct {
	root string // 同步根绝对路径
}

// Scan 全量扫描同步根（含占位符——os.Stat 对占位符返回正确的 size/mtime，
// P0 已验证；ModeIrregular 不影响本用途）。
func (l *localFS) Scan() ([]engine.CloudEntry, error) {
	var out []engine.CloudEntry
	root := l.root
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			// 单个文件被占用等瞬态错误不阻断扫描（下次轮询自愈）
			return nil
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out = append(out, engine.CloudEntry{
			Path:  filepath.ToSlash(rel),
			Size:  info.Size(),
			MTime: info.ModTime(),
			IsDir: info.IsDir(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Stat 单条目状态（exec 前复查用），不存在返回 (nil, nil)。
func (l *localFS) Stat(rel string) (*engine.CloudEntry, error) {
	abs := filepath.Join(l.root, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &engine.CloudEntry{
		Path:  rel,
		Size:  info.Size(),
		MTime: info.ModTime(),
		IsDir: info.IsDir(),
	}, nil
}

// ApplyDownload 让本地反映云端条目：
//   - 目录 → MkdirAll
//   - 文件 → 删旧（若有）+ 建占位符（size/mtime 用云端元数据）。
//     占位符数据懒水合：下次读时经 FETCH_DATA 拉取（读到的必是最新内容）。
func (l *localFS) ApplyDownload(rel string, e engine.CloudEntry) error {
	abs := filepath.Join(l.root, filepath.FromSlash(rel))
	if e.IsDir {
		return os.MkdirAll(abs, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	// 已存在 → 先删（本地未修改是引擎保证的前提，删除无数据损失）
	if _, err := os.Lstat(abs); err == nil {
		if err := os.Remove(abs); err != nil {
			return fmt.Errorf("删除旧文件 %s: %w", rel, err)
		}
	}
	results, err := cfapi.CreatePlaceholders(filepath.Dir(abs), []cfapi.NewPlaceholder{{
		RelativeFileName: filepath.Base(abs),
		FileSize:          e.Size,
		ModTime:           e.MTime,
		Flags:             cfapi.PlaceholderCreateFlagMarkInSync,
		Identity:          []byte(rel),
	}})
	if err != nil {
		if code, _ := cfapi.AsHRESULT(err); code == hrAlreadyExists {
			return nil
		}
		return fmt.Errorf("建占位符 %s: %w", rel, err)
	}
	for _, hr := range results {
		if hr != 0 && uint32(hr) != hrAlreadyExists {
			return fmt.Errorf("建占位符 %s: HRESULT 0x%08X", rel, uint32(hr))
		}
	}
	return nil
}

// Remove 删除本地文件/目录（引擎保证未修改）。
func (l *localFS) Remove(rel string) error {
	abs := filepath.Join(l.root, filepath.FromSlash(rel))
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		// 非空目录兜底
		if os.IsExist(err) || isDir(abs) {
			return os.RemoveAll(abs)
		}
		return err
	}
	return nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// FinalizeUpload 上传成功后：普通文件转占位符 + in-sync（云朵/绿勾图标）。
// 已是占位符则只标记 in-sync。
func (l *localFS) FinalizeUpload(rel string) error {
	abs := filepath.Join(l.root, filepath.FromSlash(rel))
	if err := cfapi.SetInSync(abs); err == nil {
		return nil
	}
	// 0x80070178 = 不是云文件（普通文件）→ 转换
	return cfapi.ConvertToPlaceholder(abs, cfapi.ConvertFlagMarkInSync)
}

// WasHydrated 判断此前是否已水合（Q6）。
// P1 简化：只要本地曾是占位符且 size>0 即视为可能已水合——
// ApplyDownload 走懒水合（读时拉最新），主动重拉留待 Q6 完整实现。
func (l *localFS) WasHydrated(rel string) bool {
	abs := filepath.Join(l.root, filepath.FromSlash(rel))
	st, err := os.Lstat(abs)
	if err != nil {
		return false
	}
	return !st.IsDir() && st.Size() > 0
}
