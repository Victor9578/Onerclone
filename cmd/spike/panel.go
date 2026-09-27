package main

// panel.go —— Phase 2（Q8/Q14）：本地 Web 面板，第 1 片（只读）。
//
// 形态：127.0.0.1 随机端口 · `go:embed` 单页 · **一次性 token**（`?t=` 换
// 会话 Cookie 后立即作废，日志里那行链接只能点一次，之后靠 Cookie）。
// 本片承载：同步根/云端 fs/在线状态/DR2 基线/队列统计/动作列表（含冲突与失败原因）。
// 后续片：扫码登录入口、脱水管理、冲突副本可视化。
//
// 安全边界：只绑 127.0.0.1、Cookie HttpOnly+SameSite=Strict、token 单次有效；
// 面板只读，不提供任何改变同步行为的接口（管理动作留到后续片）。

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"onerclone/internal/engine"
	"onerclone/internal/state"
)

//go:embed ui/index.html
var panelIndexHTML []byte

const panelCookieName = "spike_panel"

// panel 是本地 Web 面板的 HTTP 服务。
type panel struct {
	store    *state.Store
	eng      *engine.Engine
	syncRoot string
	fsRoot   string
	offline  bool
	started  time.Time

	mu       sync.Mutex
	token    string // 一次性访问令牌（换取 Cookie 后置空）
	session  string // 会话 Cookie 值
	quark    quarkStatus // 扫码登录会话（Phase 2）
	srv      *http.Server
	listener net.Listener
}

// quarkStatus 是扫码登录的前端状态。
type quarkStatus struct {
	Phase   string `json:"phase"` // idle | waiting | done | failed
	URL     string `json:"url"`
	Err     string `json:"err,omitempty"`
	Started string `json:"started,omitempty"`
	Done    string `json:"done,omitempty"`
}

// startPanel 在 127.0.0.1 随机端口启动面板（异步 Serve，立即返回）。
func startPanel(store *state.Store, eng *engine.Engine, syncRoot, fsRoot string, offline bool) (*panel, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &panel{
		store:    store,
		eng:      eng,
		syncRoot: syncRoot,
		fsRoot:   fsRoot,
		offline:  offline,
		started:  time.Now(),
		token:    randomToken(),
		session:  randomToken(),
		quark:    quarkStatus{Phase: "idle"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handleIndex)
	mux.HandleFunc("/api/state", p.handleState)
	mux.HandleFunc("/api/conflicts", p.handleConflicts)
	mux.HandleFunc("/api/action/retry", p.handleRetry)
	mux.HandleFunc("/api/action/drop", p.handleDrop)
	mux.HandleFunc("/api/dehydrate", p.handleDehydrate)
	mux.HandleFunc("/api/quark/start", p.handleQuarkStart)
	mux.HandleFunc("/api/quark/status", p.handleQuarkStatus)
	mux.HandleFunc("/api/quark/qr.png", p.handleQuarkQR)
	p.srv = &http.Server{Handler: mux}
	p.listener = ln
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// randomToken 生成 128bit 随机十六进制串。
func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属于系统级异常，退化为时间戳随机（面板仅本机只读）
		return hex.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	}
	return hex.EncodeToString(b)
}

// URL 返回带一次性 token 的访问地址。
func (p *panel) URL() string {
	return fmt.Sprintf("http://%s/?t=%s", p.listener.Addr().String(), p.token)
}

// Close 停止服务。
func (p *panel) Close() error { return p.srv.Close() }

// authorized 判断请求是否持有有效会话 Cookie。
func (p *panel) authorized(r *http.Request) bool {
	c, err := r.Cookie(panelCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return c.Value == p.session
}

// handleIndex 提供单页：一次性 token → 发 Cookie 并作废 token；否则校验 Cookie。
func (p *panel) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if t := r.URL.Query().Get("t"); t != "" {
		p.mu.Lock()
		ok := p.token != "" && t == p.token
		if ok {
			p.token = "" // 一次性：用过即废
		}
		p.mu.Unlock()
		if ok {
			http.SetCookie(w, &http.Cookie{
				Name:     panelCookieName,
				Value:    p.session,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}
	if !p.authorized(r) {
		http.Error(w, "403 令牌无效或已被使用（面板只允许本机访问；请用日志里的一次性链接，或重启后重取）",
			http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(panelIndexHTML)
}

// ---------- JSON 模型 ----------

type panelStatus struct {
	SyncRoot     string         `json:"sync_root"`
	FsRoot       string         `json:"fs_root"`
	Offline      bool           `json:"offline"`
	BaselineDone bool           `json:"baseline_done"`
	AuthFailed   int            `json:"auth_failed"`
	PendingDue   int            `json:"pending_due"`
	Stats        map[string]int `json:"stats"`
	UptimeSec    int64          `json:"uptime_sec"`
	ServerTime   string         `json:"server_time"`
}

type panelAction struct {
	ID       int64  `json:"id"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Class    string `json:"class"`
	Attempts int    `json:"attempts"`
	NextTry  string `json:"next_try"`
	LastErr  string `json:"last_err"`
	Updated  string `json:"updated"`
}

// handleState 输出状态 + 队列动作（只读）。
func (p *panel) handleState(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		http.Error(w, "403", http.StatusForbidden)
		return
	}
	statsRaw, err := p.store.Stats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stats := map[string]int{}
	for k, v := range statsRaw {
		stats[string(k)] = v
	}
	baseline, _ := p.store.BaselineDone()
	authFailed, _ := p.store.CountAuthFailed()
	due, _ := p.store.PendingCount()
	acts, err := p.store.ListActions(nil, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	actions := make([]panelAction, 0, len(acts))
	for _, a := range acts {
		actions = append(actions, panelAction{
			ID: a.ID, Path: a.Path, Kind: string(a.Kind), State: string(a.State),
			Class: string(a.Class), Attempts: a.Attempts,
			NextTry: a.NextTry.Format(time.RFC3339), LastErr: a.LastErr,
			Updated: a.UpdatedAt.Format(time.RFC3339),
		})
	}
	out := struct {
		Status  panelStatus   `json:"status"`
		Actions []panelAction `json:"actions"`
	}{
		Status: panelStatus{
			SyncRoot:     p.syncRoot,
			FsRoot:       p.fsRoot,
			Offline:      p.offline,
			BaselineDone: baseline,
			AuthFailed:   authFailed,
			PendingDue:   due,
			Stats:        stats,
			UptimeSec:    int64(time.Since(p.started).Seconds()),
			ServerTime:   time.Now().Format(time.RFC3339),
		},
		Actions: actions,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// ---------- 管理动作（Phase 2 第 2 片） ----------

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// readJSON 解析请求体（限 64KB）。
func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(v)
}

// requirePOST 鉴权 + 方法检查；返回 false 表示响应已写。
func (p *panel) requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if !p.authorized(r) {
		http.Error(w, "403", http.StatusForbidden)
		return false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "405 需要 POST", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// idReq 是动作类请求体。
type idReq struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
}

// handleRetry 重置一条动作为待执行。
func (p *panel) handleRetry(w http.ResponseWriter, r *http.Request) {
	if !p.requirePOST(w, r) {
		return
	}
	var req idReq
	if err := readJSON(r, &req); err != nil {
		http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := p.eng.RetryAction(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDrop 移除一条动作。
func (p *panel) handleDrop(w http.ResponseWriter, r *http.Request) {
	if !p.requirePOST(w, r) {
		return
	}
	var req idReq
	if err := readJSON(r, &req); err != nil {
		http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := p.eng.DropAction(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDehydrate 脱水：释放本地数据、只留占位符（未水合 → 409）。
func (p *panel) handleDehydrate(w http.ResponseWriter, r *http.Request) {
	if !p.requirePOST(w, r) {
		return
	}
	var req idReq
	if err := readJSON(r, &req); err != nil {
		http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		http.Error(w, "400 缺少 path", http.StatusBadRequest)
		return
	}
	queued, err := p.eng.RequestDehydrate(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !queued {
		writeJSON(w, http.StatusConflict,
			map[string]any{"ok": false, "reason": "not hydrated", "path": req.Path})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "queued": true, "path": req.Path})
}

// ---------- 冲突副本可视化 ----------

// conflictSide 是冲突某一侧的快照。
type conflictSide struct {
	Present bool   `json:"present"`
	Size    int64  `json:"size"`
	MTime   string `json:"mtime"`
}

// conflictRow 是一条冲突的两端 + 双侧副本。
type conflictRow struct {
	Path        string        `json:"path"`
	State       string        `json:"state"`
	LastErr     string        `json:"last_err"`
	Local       conflictSide  `json:"local"`
	Cloud       conflictSide  `json:"cloud"`
	LocalCopies []string      `json:"local_copies"`
	CloudCopies []string      `json:"cloud_copies"`
}

// handleConflicts 列出冲突动作及两端快照、冲突副本（Q7/Q18 双保留可视化）。
func (p *panel) handleConflicts(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		http.Error(w, "403", http.StatusForbidden)
		return
	}
	local, _ := p.store.AllSnap("local_snap")
	cloud, _ := p.store.AllSnap("cloud_snap")
	acts, err := p.store.ListActions(nil, 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	side := func(m map[string]state.FileSnap, k string) conflictSide {
		s, ok := m[k]
		if !ok {
			return conflictSide{}
		}
		return conflictSide{Present: s.Present, Size: s.Size, MTime: s.MTime.Format(time.RFC3339)}
	}
	out := []conflictRow{}
	for _, a := range acts {
		if a.Kind != state.KindConflict {
			continue
		}
		row := conflictRow{
			Path: a.Path, State: string(a.State), LastErr: a.LastErr,
			Local: side(local, a.Path), Cloud: side(cloud, a.Path),
		}
		// 冲突副本命名：`stem (冲突 时间).ext`（见 engine.conflictName）
		stem := strings.TrimSuffix(a.Path, path.Ext(a.Path))
		for k := range local {
			if k != a.Path && strings.HasPrefix(k, stem+" (冲突 ") {
				row.LocalCopies = append(row.LocalCopies, k)
			}
		}
		for k := range cloud {
			if k != a.Path && strings.HasPrefix(k, stem+" (冲突 ") {
				row.CloudCopies = append(row.CloudCopies, k)
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"conflicts": out})
}

// ---------- 扫码登录（浏览器出二维码） ----------

// handleQuarkStart 建扫码会话并在后台等待扫码确认（复用 CLI 的非交互协议）。
func (p *panel) handleQuarkStart(w http.ResponseWriter, r *http.Request) {
	if !p.requirePOST(w, r) {
		return
	}
	p.mu.Lock()
	cur := p.quark
	p.mu.Unlock()
	if cur.Phase == "waiting" { // 已有一个进行中的会话
		writeJSON(w, http.StatusOK, cur)
		return
	}

	exe := resolveRclone("", loadConfig().Rclone)
	q, restore, err := quarkStartQRSession(exe, "quark")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "err": err.Error()})
		return
	}
	st := quarkStatus{
		Phase:   "waiting",
		URL:     qrURLRe.FindString(q.Option.Help),
		Started: time.Now().Format(time.RFC3339),
	}
	p.mu.Lock()
	p.quark = st
	p.mu.Unlock()

	// 后台跑 --continue：rclone 轮询夸克直到扫码确认（约 3 分钟超时）
	go func() {
		out, err := runRcloneQuiet(exe, "config", "create", "quark", "quark",
			"--non-interactive", "--continue", "--state", q.State, "--result", "true")
		p.mu.Lock()
		defer p.mu.Unlock()
		if err != nil {
			p.quark.Phase = "failed"
			p.quark.Err = oneLine(out + " " + err.Error())
			restore() // 扫码失败：把旧 cookie 写回，登录态不丢
		} else {
			p.quark.Phase = "done"
			p.quark.Done = time.Now().Format(time.RFC3339)
		}
	}()
	writeJSON(w, http.StatusOK, st)
}

// handleQuarkStatus 返回扫码会话状态。
func (p *panel) handleQuarkStatus(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		http.Error(w, "403", http.StatusForbidden)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	writeJSON(w, http.StatusOK, p.quark)
}

// handleQuarkQR 输出当前扫码会话的二维码 PNG。
func (p *panel) handleQuarkQR(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		http.Error(w, "403", http.StatusForbidden)
		return
	}
	p.mu.Lock()
	u := p.quark.URL
	p.mu.Unlock()
	if u == "" {
		http.Error(w, "404 没有进行中的扫码会话", http.StatusNotFound)
		return
	}
	png, err := qrcode.Encode(u, qrcode.Medium, 320)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}
