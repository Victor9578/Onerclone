package app

// adapters.go 鈥斺€?engine.Cloud / engine.Local 鐨勭湡瀹炵幇锛坮clone RC + cfapi锛夈€?
//
// 寮曟搸涓庡钩鍙拌В鑰︼細engine 鍖呭彧绠″喅绛栦笌闃熷垪锛屾墍鏈夊壇浣滅敤缁忚繖涓や釜鎺ュ彛钀藉湴銆?

import (
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"onerclone/internal/cfapi"
	"onerclone/internal/engine"
	"onerclone/internal/rclone"
)

// convergeNames 把同步根下**物理名含 rclone 读不到字符**（全角 `：？＜＞` 等，
// 踩坑 #26）的文件/目录改名成引擎键（= 云端名，%XX 纯 ASCII），并登记
// identity 映射（markRenamed）。
//
// 为什么必须改名：copyfile 源侧只能给 rclone 一个路径，而这类名字对 rclone
// 恒为 object not found（它会先把全角改写成 ASCII 再打开，NTFS 上不存在
// ASCII 形），没有任何写法能绕过 —— 唯一出路是让磁盘名变成 rclone 读得到的
// 转义名，两侧字面一致（踩坑 #16/#23 的最终闭环）。
//
// 目录自顶向下：父目录先改名，子路径整体位移后递归处理子项自身的段。
// 改名失败（文件被占用/同名冲突）只告警：后续每轮 Scan 会自动重试，
// 期间该文件上传按 404 → 永久失败在面板可见。
func convergeNames(root string, nm *nameMap) {
	if nm == nil {
		return
	}
	var walk func(dir string)
	walk = func(dir string) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return // 被占用等瞬时错误：下轮扫描自愈
		}
		for _, e := range ents {
			physSeg := e.Name()
			// 临时文件不参与同步（engine.isTempPath 同款规则），也不改名
			if strings.HasPrefix(physSeg, "~$") ||
				strings.HasSuffix(strings.ToLower(physSeg), ".tmp") {
				continue
			}
			abs := filepath.Join(dir, physSeg)
			rel, err := filepath.Rel(root, abs)
			if err != nil {
				continue
			}
			slashed := filepath.ToSlash(rel)
			if slashed == rootMarkerName {
				continue // 同步根身份标识，不同步
			}
			// 引擎键（=云端名）：父段此时已收敛，engSeg 就是本段的映射结果
			eng := nm.ensureUploadable(slashed)
			engSeg := eng
			if i := strings.LastIndexByte(eng, '/'); i >= 0 {
				engSeg = eng[i+1:]
			}
			isDir := e.IsDir()
			if engSeg != physSeg && rcloneBlindSeg(physSeg) {
				target := filepath.Join(dir, filepath.FromSlash(engSeg))
				if _, err := os.Lstat(target); err == nil {
					log.Printf("⚠ 改名冲突：目标已存在，跳过（该文件上传将失败）: %s", target)
				} else if err := os.Rename(abs, target); err != nil {
					log.Printf("⚠ 改名失败（rclone 读不了全角名，稍后重试） %s → %s: %v", physSeg, engSeg, err)
				} else {
					log.Printf("🏷 本地改名 %s → %s（rclone 在 Windows 读不了全角冒号等字符）", physSeg, engSeg)
					nm.markRenamed(physSeg, engSeg)
					abs = target
					if st, err := os.Lstat(abs); err == nil {
						isDir = st.IsDir()
					}
				}
			}
			if isDir {
				walk(abs)
			}
		}
	}
	walk(root)
}

// ---------- Cloud锛歳clone RC ----------

// cloudRC 鎶?rclone RC 鍖呰鎴?engine.Cloud銆?
// client 鐢?atomic.Pointer 鎸佹湁锛歳cd 鏂嚎閲嶅惎鍚庣儹鏇挎崲锛團R5锛夛紝寮曟搸骞跺彂璇诲畨鍏ㄣ€?
type cloudRC struct {
	rc    atomic.Pointer[rclone.Client]
	nm    *nameMap // Poll 时登记云端字面名（namemap 碰撞判定），nil 安全
	srcFs string   // 鏈湴鍚屾鏍癸紙rclone 浠?local 鍚庣璇诲畠锛?
	dstFs string   // 浜戠 fs锛圥0=鏈湴鏇胯韩鐩綍锛汸1=quark remote锛?
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

// ListDir 递归列举 rel 目录下全部条目（不含自身；目录删除前整树复查用）。
func (c *cloudRC) ListDir(rel string) ([]engine.CloudEntry, error) {
	entries, err := c.client().ListRecursive(c.dstFs, rel)
	if err != nil {
		return nil, err
	}
	out := make([]engine.CloudEntry, 0, len(entries))
	for _, e := range entries {
		c.nm.observeCloud(e.Path)
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

// Purge 递归删除云端目录（引擎目录删除用；deletefile 对目录报
// "is a directory not a file"）。
func (c *cloudRC) Purge(rel string) error {
	return c.client().Purge(c.dstFs, rel)
}

func (c *cloudRC) Mkdir(rel string) error {
	return c.client().Mkdir(c.dstFs, rel)
}

// ---------- Local锛氭枃浠剁郴缁?+ cfapi 鍗犱綅绗?----------

// localFS 鎶婂悓姝ユ牴 + cfapi 鍖呰鎴?engine.Local銆?
type localFS struct {
	nm   *nameMap // 浜戠绔为目标：云端名↔本地名 映射（DR4 方案 A），nil=不映射
	root string   // 鍚屾鏍圭粷瀵硅矾寰?
}

// Scan 鍏ㄩ噺鎵弿鍚屾鏍癸紙鍚崰浣嶇鈥斺€攐s.Stat 瀵瑰崰浣嶇杩斿洖姝ｇ‘鐨?size/mtime锛?
// P0 宸查獙璇侊紱ModeIrregular 涓嶅奖鍝嶆湰鐢ㄩ€旓級銆?
// tmpSuffix 是 ApplyDownload 原子替换用的临时后缀（Scan 跳过并清理残留）。
const tmpSuffix = ".onerclone-tmp"

func (l *localFS) Scan() ([]engine.CloudEntry, error) {
	// 先收敛物理名：rclone 读不到的全角名（：？＜＞…）改名成引擎键（踩坑 #26），
	// 否则本轮 upload 源侧 copyfile 必 404。改名在 walk 之前自顶向下完成，
	// 本轮扫描看到的就是收敛后的最终路径（无“改名后子项漏扫一轮”问题）。
	convergeNames(l.root, l.nm)
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
		if filepath.ToSlash(rel) == rootMarkerName {
			return nil // 同步根身份标识文件，不参与同步
		}
		if strings.HasSuffix(p, tmpSuffix) {
			// ApplyDownload 原子替换的崩溃残留（半成品占位符），无数据，顺手清掉
			_ = os.Remove(p)
			return nil
		}
		// 本地名 → 云端原名（映射表反向还原）：engine 的键永远是云端名，
		// 这样云端 `来自:分享` 与本地 `来自：分享` 在快照/队列里是同一条。
		// 查不到映射 = 用户本地新建：名字若含 NFKC 不稳定字符（全角：等），
		// rclone 读不到 → 上传 404（踩坑 #23）。这里当场登记映射：
		// 云端名 = 转义名，本地保持用户字面名，两侧字面一致。
		rel = l.nm.ensureUploadable(filepath.ToSlash(rel))
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
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		results, err := cfapi.CreatePlaceholders(filepath.Dir(abs), []cfapi.NewPlaceholder{{
			RelativeFileName: filepath.Base(abs),
			FileSize:         0,
			ModTime:          e.MTime,
			Flags:            cfapi.PlaceholderCreateFlagMarkInSync,
			Identity:         []byte(rel),
			IsDir:            true,
		}})
		if err != nil {
			if code, _ := cfapi.AsHRESULT(err); code == hrNotSyncRoot {
				// 单测/未注册根的兜底：普通目录仍可同步，只是没有状态图标。
				return os.MkdirAll(abs, 0o755)
			}
			if code, _ := cfapi.AsHRESULT(err); code == hrAlreadyExists {
				return l.FinalizeUpload(rel)
			}
			return fmt.Errorf("建目录占位符 %s: %w", rel, err)
		}
		for _, hr := range results {
			if hr != 0 {
				if uint32(hr) == hrAlreadyExists {
					return l.FinalizeUpload(rel)
				}
				return fmt.Errorf("建目录占位符 %s: HRESULT 0x%08X", rel, uint32(hr))
			}
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	// 宸插瓨鍦?鈫?鍏堝垹锛堟湰鍦版湭淇敼鏄紩鎿庝繚璇佺殑鍓嶆彁锛屽垹闄ゆ棤鏁版嵁鎹熷け锛?
	// 已存在 → 先在旁边建好新占位符，再原子 rename 替换。
	// 旧做法先 os.Remove 后重建：删完、快照落地前崩溃 → 下轮 Scan 判
	// “本地已删” → 删云端 → 双侧全丢。同卷 rename 原子替换，无空窗。
	// ponytail: 崩溃残留的 *.onerclone-tmp 由 Scan 顺手清理。
	name := filepath.Base(abs)
	if _, err := os.Lstat(abs); err == nil {
		name += tmpSuffix
		_ = os.Remove(filepath.Join(filepath.Dir(abs), name))
	}
	results, err := cfapi.CreatePlaceholders(filepath.Dir(abs), []cfapi.NewPlaceholder{{
		RelativeFileName: name,
		FileSize:         e.Size,
		ModTime:          e.MTime,
		Flags:            cfapi.PlaceholderCreateFlagMarkInSync,
		Identity:         []byte(rel),
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
	if name != filepath.Base(abs) {
		if err := os.Rename(filepath.Join(filepath.Dir(abs), name), abs); err != nil {
			return fmt.Errorf("替换旧文件 %s: %w", rel, err)
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

// WasHydrated 判断此前是否已水合（Q6：曾水合的自动重新下载）。
// 占位符的 size 恒为云端大小（size>0 不能当水合证据——v0.3.6 用它把懒水合
// 击穿过：云端每改一个文件就全量重拉）。正确证据是占位符属性：
// FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS（0x400000）置位 = 脱水占位符。
func (l *localFS) WasHydrated(rel string) bool {
	fi, err := os.Lstat(l.path(rel))
	if err != nil || fi.IsDir() {
		return false
	}
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.FileAttributes&0x400000 == 0 // 无 RECALL 位 = 已水合/普通文件
	}
	return fi.Size() > 0
}
func (l *localFS) MarkSyncing(rel string) {
	for p := rel; p != "." && p != "/" && p != ""; p = path.Dir(p) {
		abs := l.path(p)
		if _, err := os.Lstat(abs); err != nil {
			continue
		}
		_ = cfapi.SetNotInSync(abs)
	}
}

// MarkSynced 把条目标回 in-sync（绿勾）。仅在该路径无未完成动作时由引擎
// 调用；祖先目录不在这里恢复——目录绿勾由 RestoreInSync/全量对账统一裁决，
// 避免目录先于子文件变绿（v0.3.6 实测的乱显示根因）。
func (l *localFS) MarkSynced(rel string) {
	abs := l.path(rel)
	if _, err := os.Lstat(abs); err != nil {
		return
	}
	_ = cfapi.SetInSync(abs)
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
