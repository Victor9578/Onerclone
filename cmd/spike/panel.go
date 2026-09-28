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
	login    loginSession // 通用登录状态机（v0.2.0 反馈问题③）
	loginRestore func()   // 通用登录失败时恢复原 remote 参数
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
	mux.HandleFunc("/api/settings", p.handleSettings)
	mux.HandleFunc("/api/remotes", p.handleRemotes)
	mux.HandleFunc("/api/providers", p.handleProviders)
	mux.HandleFunc("/api/login/start", p.handleLoginStart)
	mux.HandleFunc("/api/login/answer", p.handleLoginAnswer)
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

// ---------- 设置区（v0.2.0 反馈问题②/③） ----------

// settingsReq 是设置写回请求体（零值字段 = 不改）。
type settingsReq struct {
	SyncRoot string `json:"sync_root"`
	Remote   string `json:"remote"`
}

// handleSettings GET 读当前配置；POST 写回（改 sync_root/remote）。
// 写回只动 onerclone.json，**重启后生效**（运行中的引擎不热切换——
// cfapi 连接、watcher、队列都绑定启动时的根）。
func (p *panel) handleSettings(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		http.Error(w, "403", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg := loadConfig()
		writeJSON(w, http.StatusOK, map[string]any{
			"sync_root":      cfg.SyncRoot,
			"remote":         cfg.Remote,
			"fs":             cfg.Fs,
			"rclone":         cfg.Rclone,
			"offline":        cfg.Offline,
			"restart_needed": false, // 写回成功后前端自行提示
		})
	case http.MethodPost:
		var req settingsReq
		if err := readJSON(r, &req); err != nil {
			http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.SyncRoot == "" && req.Remote == "" {
			http.Error(w, "400 缺少 sync_root / remote", http.StatusBadRequest)
			return
		}
		if req.SyncRoot != "" {
			// 提前校验：根必须在 NTFS 卷上（cfapi 硬约束）
			if err := checkNTFS(req.SyncRoot); err != nil {
				http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		path, err := saveConfig(config{SyncRoot: req.SyncRoot, Remote: req.Remote})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "path": path,
			"restart_needed": true,
			"hint":           "已写入配置，重启 onerclone 后生效（换根/换 remote 会自动迁移注册并重建云端基线）",
		})
	default:
		http.Error(w, "405", http.StatusMethodNotAllowed)
	}
}

// handleRemotes 列出 rclone 已配置的 remotes（设置区 remote 下拉用）。
func (p *panel) handleRemotes(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		http.Error(w, "403", http.StatusForbidden)
		return
	}
	exe := resolveRclone("", loadConfig().Rclone)
	out, err := rcloneOutput(exe, "listremotes")
	if err != nil {
		http.Error(w, "rclone listremotes 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	remotes := []string{}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			remotes = append(remotes, line)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"remotes": remotes})
}

// ---------- 通用登录（v0.2.0 反馈问题③：任意 rclone 后端） ----------

// loginSession 是面板驱动的通用登录状态机（同一时刻一个会话）。
type loginSession struct {
	Phase    string         `json:"phase"` // idle | question | done | failed
	Name     string         `json:"name,omitempty"`
	Type     string         `json:"type,omitempty"`
	Question *loginQuestion `json:"question,omitempty"` // 当前题（nil = 无进行中）
	Err      string         `json:"err,omitempty"`
	// oauthBrowser 为 true 时前端提示"浏览器已打开，完成授权后点继续"
	OAuthBrowser bool `json:"oauth_browser,omitempty"`
}

// loginStartReq 是 /api/login/start 请求体。
type loginStartReq struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// loginAnswerReq 是 /api/login/answer 请求体。
type loginAnswerReq struct {
	Result string `json:"result"`
}

// handleProviders 列出可登录的后端类型。
func (p *panel) handleProviders(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		http.Error(w, "403", http.StatusForbidden)
		return
	}
	exe := resolveRclone("", loadConfig().Rclone)
	prov, err := listProviders(exe)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": prov})
}

// handleLoginStart 启动通用登录状态机（或返回进行中的会话）。
func (p *panel) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if !p.requirePOST(w, r) {
		return
	}
	var req loginStartReq
	if err := readJSON(r, &req); err != nil {
		http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Type == "" {
		http.Error(w, "400 缺少 type", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		req.Name = req.Type
	}
	p.mu.Lock()
	cur := p.login
	p.mu.Unlock()
	if cur.Phase == "question" { // 已有进行中的会话：直接返回
		writeJSON(w, http.StatusOK, cur)
		return
	}

	exe := resolveRclone("", loadConfig().Rclone)
	q, restore, err := loginStart(exe, req.Name, req.Type)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, loginSession{Phase: "failed", Err: err.Error()})
		return
	}

	sess := loginSession{Phase: "done", Name: req.Name, Type: req.Type}
	if q != nil && q.State != "" {
		sess.Phase = "question"
		sess.Question = q
		// OAuth islocal 题：自动答 true（rclone 自己开浏览器），把下一题
		//（等回调/授权）直接带给前端
		if q.State == "*oauth-islocal" {
			nq, err := loginAnswer(exe, req.Name, q.State, "true")
			if err != nil {
				restore()
				writeJSON(w, http.StatusInternalServerError, loginSession{Phase: "failed", Err: err.Error()})
				return
			}
			if nq == nil || nq.State == "" {
				sess.Phase = "done"
				sess.Question = nil
			} else {
				sess.Question = nq
				sess.OAuthBrowser = true // 前端提示浏览器授权
			}
		}
		// quark 扫码：复用现有二维码渲染（qr_start → 前端轮询 /api/quark/*）
		if q.State == "qr_start" || q.State == "qr_poll" {
			if u := qrURLRe.FindString(q.Option.Help); u != "" {
				p.mu.Lock()
				p.quark = quarkStatus{Phase: "waiting", URL: u, Started: time.Now().Format(time.RFC3339)}
				p.mu.Unlock()
				sess.Question = nil
				sess.Phase = "question"
				// 前端看到 type=quark 就去渲染二维码并轮询 quark/status
			}
		}
	}
	p.mu.Lock()
	p.login = sess
	p.loginRestore = restore
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, sess)
}

// handleLoginAnswer 推进通用登录状态机。
func (p *panel) handleLoginAnswer(w http.ResponseWriter, r *http.Request) {
	if !p.requirePOST(w, r) {
		return
	}
	var req loginAnswerReq
	if err := readJSON(r, &req); err != nil {
		http.Error(w, "400 "+err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	cur := p.login
	restore := p.loginRestore
	p.mu.Unlock()
	if cur.Phase != "question" || cur.Question == nil {
		http.Error(w, "409 没有进行中的登录会话", http.StatusConflict)
		return
	}
	exe := resolveRclone("", loadConfig().Rclone)
	nq, err := loginAnswer(exe, cur.Name, cur.Question.State, req.Result)
	if err != nil {
		restore()
		p.mu.Lock()
		p.login = loginSession{Phase: "failed", Name: cur.Name, Type: cur.Type, Err: err.Error()}
		p.mu.Unlock()
		writeJSON(w, http.StatusOK, p.login)
		return
	}
	sess := loginSession{Phase: "done", Name: cur.Name, Type: cur.Type}
	if nq != nil && nq.State != "" {
		sess.Phase = "question"
		sess.Question = nq
	}
	p.mu.Lock()
	p.login = sess
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, sess)
}
