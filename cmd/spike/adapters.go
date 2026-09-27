package main

// adapters.go 鈥斺€?engine.Cloud / engine.Local 鐨勭湡瀹炵幇锛坮clone RC + cfapi锛夈€?
//
// 寮曟搸涓庡钩鍙拌В鑰︼細engine 鍖呭彧绠″喅绛栦笌闃熷垪锛屾墍鏈夊壇浣滅敤缁忚繖涓や釜鎺ュ彛钀藉湴銆?

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	"onerclone/internal/cfapi"
	"onerclone/internal/engine"
	"onerclone/internal/rclone"
)

// ---------- Cloud锛歳clone RC ----------

// cloudRC 鎶?rclone RC 鍖呰鎴?engine.Cloud銆?
// client 鐢?atomic.Pointer 鎸佹湁锛歳cd 鏂嚎閲嶅惎鍚庣儹鏇挎崲锛團R5锛夛紝寮曟搸骞跺彂璇诲畨鍏ㄣ€?
type cloudRC struct {
	rc    atomic.Pointer[rclone.Client]
	nm    *nameMap // Poll 时登记云端字面名（namemap 碰撞判定），nil 安全
	srcFs string // 鏈湴鍚屾鏍癸紙rclone 浠?local 鍚庣璇诲畠锛?
	dstFs string // 浜戠 fs锛圥0=鏈湴鏇胯韩鐩綍锛汸1=quark remote锛?
}

// SetClient 鐑浛鎹?RC 瀹㈡埛绔紙rcd 鑷姩閲嶅惎鍚庤皟鐢級銆?
func (c *cloudRC) SetClient(cl *rclone.Client) { c.rc.Store(cl) }

func (c *cloudRC) client() *rclone.Client { return c.rc.Load() }

func (c *cloudRC) List() ([]engine.CloudEntry, error) {
	entries, err := c.client().ListRecursive(c.dstFs, "")
	if err != nil {
		return nil, err
	}
	out := make([]engine.CloudEntry, 0, len(entries))
	for _, e := range entries {
		c.nm.observeCloud(e.Path) // 每次 Poll 刷新云端字面名（namemap 碰撞判定）
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
	e, err := c.client().Stat(c.dstFs, rel)
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
	// 本地源路径要过 namemap（云端名可能含 Windows 非法字符），云端目标用原名
	return c.client().CopyFile(c.srcFs, c.nm.localPath(rel), c.dstFs, rel)
}

func (c *cloudRC) Download(rel string) error {
	return c.client().CopyFile(c.dstFs, rel, c.srcFs, c.nm.localPath(rel))
}

func (c *cloudRC) DownloadTo(src, dst string) error {
	// dst = 本地冲突副本路径（父目录可能被映射过）
	return c.client().CopyFile(c.dstFs, src, c.srcFs, c.nm.localPath(dst))
}

func (c *cloudRC) Delete(rel string) error {
	return c.client().DeleteFile(c.dstFs, rel)
}

func (c *cloudRC) Mkdir(rel string) error {
	return c.client().Mkdir(c.dstFs, rel)
}

// ---------- Local锛氭枃浠剁郴缁?+ cfapi 鍗犱綅绗?----------

// localFS 鎶婂悓姝ユ牴 + cfapi 鍖呰鎴?engine.Local銆?
type localFS struct {
	nm *nameMap // 浜戠绔为目标：云端名↔本地名 映射（DR4 方案 A），nil=不映射
	root string // 鍚屾鏍圭粷瀵硅矾寰?
}

// Scan 鍏ㄩ噺鎵弿鍚屾鏍癸紙鍚崰浣嶇鈥斺€攐s.Stat 瀵瑰崰浣嶇杩斿洖姝ｇ‘鐨?size/mtime锛?
// P0 宸查獙璇侊紱ModeIrregular 涓嶅奖鍝嶆湰鐢ㄩ€旓級銆?
func (l *localFS) Scan() ([]engine.CloudEntry, error) {
	var out []engine.CloudEntry
	root := l.root
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			// 鍗曚釜鏂囦欢琚崰鐢ㄧ瓑鐬€侀敊璇笉闃绘柇鎵弿锛堜笅娆¤疆璇㈣嚜鎰堬級
			return nil
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		// 本地名 → 云端原名（映射表反向还原）：engine 的键永远是云端名，
		// 这样云端 `来自:分享` 与本地 `来自：分享` 在快照/队列里是同一条
		rel = l.nm.cloudPath(filepath.ToSlash(rel))
		out = append(out, engine.CloudEntry{
			Path:  rel,
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

// Stat 鍗曟潯鐩姸鎬侊紙exec 鍓嶅鏌ョ敤锛夛紝涓嶅瓨鍦ㄨ繑鍥?(nil, nil)銆?
func (l *localFS) Stat(rel string) (*engine.CloudEntry, error) {
	abs := l.path(rel)
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

// ApplyDownload 璁╂湰鍦板弽鏄犱簯绔潯鐩細
//   - 鐩綍 鈫?MkdirAll
//   - 鏂囦欢 鈫?鍒犳棫锛堣嫢鏈夛級+ 寤哄崰浣嶇锛坰ize/mtime 鐢ㄤ簯绔厓鏁版嵁锛夈€?
//     鍗犱綅绗︽暟鎹噿姘村悎锛氫笅娆¤鏃剁粡 FETCH_DATA 鎷夊彇锛堣鍒扮殑蹇呮槸鏈€鏂板唴瀹癸級銆?
func (l *localFS) ApplyDownload(rel string, e engine.CloudEntry) error {
	abs := l.path(rel)
	if e.IsDir {
		return os.MkdirAll(abs, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	// 宸插瓨鍦?鈫?鍏堝垹锛堟湰鍦版湭淇敼鏄紩鎿庝繚璇佺殑鍓嶆彁锛屽垹闄ゆ棤鏁版嵁鎹熷け锛?
	if _, err := os.Lstat(abs); err == nil {
		if err := os.Remove(abs); err != nil {
			return fmt.Errorf("鍒犻櫎鏃ф枃浠?%s: %w", rel, err)
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
		return fmt.Errorf("寤哄崰浣嶇 %s: %w", rel, err)
	}
	for _, hr := range results {
		if hr != 0 && uint32(hr) != hrAlreadyExists {
			return fmt.Errorf("寤哄崰浣嶇 %s: HRESULT 0x%08X", rel, uint32(hr))
		}
	}
	return nil
}

// Remove 鍒犻櫎鏈湴鏂囦欢/鐩綍锛堝紩鎿庝繚璇佹湭淇敼锛夈€?
func (l *localFS) Remove(rel string) error {
	abs := l.path(rel)
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		// 闈炵┖鐩綍鍏滃簳
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

// FinalizeUpload 涓婁紶鎴愬姛鍚庯細鏅€氭枃浠惰浆鍗犱綅绗?+ in-sync锛堜簯鏈?缁垮嬀鍥炬爣锛夈€?
// 宸叉槸鍗犱綅绗﹀垯鍙爣璁?in-sync銆?
func (l *localFS) FinalizeUpload(rel string) error {
	abs := l.path(rel)
	if err := cfapi.SetInSync(abs); err == nil {
		return nil
	}
	// 0x80070178 = 涓嶆槸浜戞枃浠讹紙鏅€氭枃浠讹級鈫?杞崲
	return cfapi.ConvertToPlaceholder(abs, cfapi.ConvertFlagMarkInSync)
}

// WasHydrated 鍒ゆ柇姝ゅ墠鏄惁宸叉按鍚堬紙Q6锛夈€?
// P1 绠€鍖栵細鍙鏈湴鏇炬槸鍗犱綅绗︿笖 size>0 鍗宠涓哄彲鑳藉凡姘村悎鈥斺€?
// ApplyDownload 璧版噿姘村悎锛堣鏃舵媺鏈€鏂帮級锛屼富鍔ㄩ噸鎷夌暀寰?Q6 瀹屾暣瀹炵幇銆?
func (l *localFS) WasHydrated(rel string) bool {
	abs := l.path(rel)
	st, err := os.Lstat(abs)
	if err != nil {
		return false
	}
	return !st.IsDir() && st.Size() > 0
}

// path 云端原名（engine 的键）→ 同步根下的本地绝对路径：先经 namemap
// 把 Windows 非法字符换成全角，再拼根（DR4 方案 A）。
func (l *localFS) path(rel string) string {
	return filepath.Join(l.root, filepath.FromSlash(l.nm.localPath(rel)))
}

// Hydrate 主动把占位符的数据拉到本地（Q6）：读一遍文件会触发平台的
// FETCH_DATA 回调 → rclone RangeGet → 回填，读完再标 in-sync（云朵图标）。
// 离线/网络失败返回错误：文件仍是占位符，读时懒水合兑底（DR1：水合回填
// 不产生 watcher 事件，不会引发回环扫描）。
func (l *localFS) Hydrate(rel string) error {
	abs := l.path(rel)
	st, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return nil
	}
	f, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for {
		_, err := f.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("水合读取 %s: %w", rel, err)
		}
	}
	// 占位符 → 已就地可读，标 in-sync（只影响图标，失败不致命）
	_ = cfapi.SetInSync(abs)
	return nil
}
