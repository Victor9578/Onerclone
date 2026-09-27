package main

// namemap.go —— 云端文件名 ↔ Windows 本地名 的段级映射（DR4 剩余项·方案 A）。
//
// 背景：quark 允许名字含 `:` 等 Windows 非法字符（实测目录 `来自:分享`），
// 直接落地 → localFS.ApplyDownload 的 MkdirAll 报 "The directory name is
// invalid"，引擎把它归为 network 类 → 无限退避重试刷屏。
//
// 策略：
//  1. 段级转义成 **纯 ASCII**（`%XX`，逐字节）：Windows 非法字符
//     `<>:"|?*`、控制字符、结尾空格/点（Windows 会吞）、保留设备名
//     （CON/PRN/…/LPT9 → `%5F` 前缀），**以及任何 NFKC 会改动的字符**
//     （全角字母数字/全角标点/表意空格等，如 `：` `Ａ`）
//     ⚠ 不要用全角替换（`:`→`：`）：rclone 在 Windows 上会对文件名做
//     Unicode 归一化 + 非法字符↔全宽互转，实测结果是 **Go 只认全宽、
//     rclone 只认 ASCII**，copyfile 报 object not found（踩坑 #16）。
//     纯 ASCII 转义两边看到的字面完���一致，互不干扰。
//  2. 映射表持久化到 state.db meta（key=local_name_map_v2），Scan 时反向还原 ——
//     engine 的 path 键**永远是云端原名**（云端是唯一真相，快照/队列不受影响）
//  3. 转义是单射（含 `%` 的段必被转义），理论上不会撞名；仍保留
//     `~<hash6>` 兜底，并在 Poll 时登记云端字面名做二次防护
//  4. 表里查不到的本地段 → 原样返回（用户本地新建的文件名不动）
//
// 已知边界（Phase 2 再收）：meta 表损坏后重建的映射结果依赖云端列举顺序；
// 用户本地自建的 NFKC 不稳定名（如 `Ａ.txt`）不在映射表内，rclone 侧
// 会看到折形后的名字 → 上传失败，需在上传路径补一道转义。

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"golang.org/x/text/unicode/norm"
)

// nameMapMetaKey 是映射表在 state.db meta 表里的键。
// v2：转义方案从“全角替换”换成“ASCII %XX”（rclone 归一化不兼容，踩坑 #16），
// 换键让��映射表自动失效，避免旧全宽本地名被继续使用。
const nameMapMetaKey = "local_name_map_v2"

// nameMapStore = state.Store 的最小依赖（单测用假实现注入）。
type nameMapStore interface {
	GetMeta(k string) (string, bool, error)
	SetMeta(k, v string) error
}

// nameMap 云端段 ↔ 本地段 的双向映射。
// 并发：Poll（列举云端）、Scan（读本地）、worker（落地/回传）都会碰它。
type nameMap struct {
	mu    sync.RWMutex
	toLoc map[string]string // 云端段 → 本地段（只存改名项 + 被迫改名的字面项）
	toCld map[string]string // toLoc 的反向
	cloud map[string]bool   // 云端已知的**字面**段（内存态，随 Poll 刷新，碰撞判定用）
	store nameMapStore      // nil = 只在内存里（单测/无状态库场景）
	dirty bool
}

// newNameMap 建映射表；store 非 nil 时从 meta 恢复上次的映射。
func newNameMap(store nameMapStore) *nameMap {
	m := &nameMap{
		toLoc: map[string]string{},
		toCld: map[string]string{},
		cloud: map[string]bool{},
		store: store,
	}
	if store == nil {
		return m
	}
	v, ok, err := store.GetMeta(nameMapMetaKey)
	if err != nil {
		log.Printf("⚠ 读取名映射表失败（按空表处理）: %v", err)
		return m
	}
	if !ok || v == "" {
		return m
	}
	if err := json.Unmarshal([]byte(v), &m.toLoc); err != nil {
		log.Printf("⚠ 名映射表损坏（按空表重建）: %v", err)
		m.toLoc = map[string]string{}
		return m
	}
	for c, l := range m.toLoc {
		m.toCld[l] = c
	}
	return m
}

// observeCloud 登记云端出现过的**原始段名**，供碰撞判定
// （云端字面 `a：b` 与改名产物 `a：b` 相撞时，字面名优先保留）。
func (m *nameMap) observeCloud(cloudPath string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, seg := range strings.Split(cloudPath, "/") {
		if seg != "" {
			m.cloud[seg] = true
		}
	}
}

// localPath 云端相对路径 → 本地相对路径（正斜杠）。
func (m *nameMap) localPath(cloudRel string) string {
	if m == nil {
		return cloudRel
	}
	segs := strings.Split(cloudRel, "/")
	for i, s := range segs {
		segs[i] = m.localSeg(s)
	}
	return strings.Join(segs, "/")
}

// cloudPath 本地相对路径 → 云端相对路径（正斜杠）。
// 查不到映射的段原样返回：本地新建的名字就是云端名字。
func (m *nameMap) cloudPath(localRel string) string {
	if m == nil {
		return localRel
	}
	segs := strings.Split(localRel, "/")
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i, s := range segs {
		if c, ok := m.toCld[s]; ok {
			segs[i] = c
		}
	}
	return strings.Join(segs, "/")
}

// localSeg 单段映射（懒登记）。
func (m *nameMap) localSeg(cloudSeg string) string {
	m.mu.RLock()
	if s, ok := m.toLoc[cloudSeg]; ok {
		m.mu.RUnlock()
		return s
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.toLoc[cloudSeg]; ok { // 双检
		return s
	}

	cand := sanitizeSeg(cloudSeg)
	// 碰撞：候选名已被别的云端段占用（登记过的改名项，或云端字面同名段）
	if owner, taken := m.toCld[cand]; taken && owner != cloudSeg {
		cand = m.freeName(cloudSeg)
	} else if cand != cloudSeg && m.cloud[cand] {
		cand = m.freeName(cloudSeg)
	}
	if cand == cloudSeg {
		return cloudSeg // 干净名且无冲突：不登记，表不膨胀
	}

	m.toLoc[cloudSeg] = cand
	m.toCld[cand] = cloudSeg
	m.dirty = true
	m.saveLocked()
	return cand
}

// freeName 生成不冲突的本地名：`<改名>~<hash6>`，再撞就加序号（持锁调用）。
func (m *nameMap) freeName(cloudSeg string) string {
	base := sanitizeSeg(cloudSeg)
	sum := sha1.Sum([]byte(cloudSeg))
	cand := base + "~" + hex.EncodeToString(sum[:3])
	for i := 2; ; i++ {
		owner, taken := m.toCld[cand]
		if !taken || owner == cloudSeg {
			return cand
		}
		cand = fmt.Sprintf("%s~%s-%d", base, hex.EncodeToString(sum[:3]), i)
	}
}

// saveLocked 把映射表写回 meta（持锁调用；失败只告警，下次再存）。
func (m *nameMap) saveLocked() {
	if m.store == nil || !m.dirty {
		return
	}
	b, err := json.Marshal(m.toLoc)
	if err != nil {
		log.Printf("⚠ 序列化名映射表失败: %v", err)
		return
	}
	if err := m.store.SetMeta(nameMapMetaKey, string(b)); err != nil {
		log.Printf("⚠ 名映射表持久化失败: %v", err)
		return
	}
	m.dirty = false
}

// ---------- 段级净化 ----------

// winIllegal Windows 文件名非法字符（出现即必须转义）。
const winIllegal = `<>:"|?*`

// winReserved Windows 保留设备名（不区分大小写、不看扩展名）。
var winReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitizeSeg 把云端段名转成**同时被 Windows 和 rclone 接受**的本地名。
// 干净的段**原样返回**（identity，调用方据此决定"要不要登记"）。
func sanitizeSeg(s string) string {
	if s == "" || isCleanSeg(s) {
		return s
	}
	// 需要转义：逐字节 %XX（纯 ASCII，两边字面一致）
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if mustEscape(r, i == len(runes)-1) {
			for _, c := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if isReserved(out) {
		// 保留设备名（CON、CON.txt、LPT9.dat…）：加转义下划线前缀。
		// 干净段不含 `%`，所以带 `%` 开头的必是转义产物，无歧义。
		out = "%5F" + out
	}
	return out
}

// isCleanSeg 无需转义：不含 `%`、无 Windows 非法/控制字符、非保留名、
// 不以空格点结尾，且 NFKC 归一化后不变（否则 rclone 报的名字会与磁盘不同）。
func isCleanSeg(s string) bool {
	if strings.ContainsRune(s, '%') {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(winIllegal, r) {
			return false // Windows 非法/控制字符
		}
	}
	if norm.NFKC.String(s) != s {
		return false
	}
	if trailingCut(s) != len(s) {
		return false
	}
	return !isReserved(s)
}

// mustEscape 判断单个字符是否必须转义（last = 是否是段内最后一个字符）。
func mustEscape(r rune, last bool) bool {
	if r == '%' || r < 0x20 || r == 0x7f {
		return true
	}
	if strings.ContainsRune(winIllegal, r) {
		return true
	}
	if last && (r == ' ' || r == '.') {
		return true
	}
	return norm.NFKC.String(string(r)) != string(r)
}

// isReserved 判断段名（可带扩展名）的**基名**是否是 Windows 保留设备名：
// CON、CON.txt、LPT9.dat… —— 调用方可能传整段名，这里自己去扩展名，
// 否则 `lpt9.dat` 会因为查 `LPT9.DAT` 而漏判（实测踩过）。
func isReserved(name string) bool {
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return winReserved[strings.ToUpper(base)]
}

// trailingCut 返回字符串中尾部空格/点的起始下标。
func trailingCut(s string) int {
	i := len(s)
	for i > 0 && (s[i-1] == ' ' || s[i-1] == '.') {
		i--
	}
	return i
}
