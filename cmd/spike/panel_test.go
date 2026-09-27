package main

// panel_test.go —— 本地 Web 面板：一次性 token、Cookie 会话、只读 JSON。

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"onerclone/internal/engine"
	"onerclone/internal/state"
)

// stubCloud/stubLocal 是 engine.Cloud/Local 的最小实现（面板测试用）。
type stubCloud struct{}

func (stubCloud) List() ([]engine.CloudEntry, error)        { return nil, nil }
func (stubCloud) Stat(string) (*engine.CloudEntry, error)   { return nil, nil }
func (stubCloud) Upload(string) error                       { return nil }
func (stubCloud) Download(string) error                     { return nil }
func (stubCloud) DownloadTo(string, string) error           { return nil }
func (stubCloud) Delete(string) error                       { return nil }
func (stubCloud) Mkdir(string) error                        { return nil }

type stubLocal struct{ hydrated map[string]bool }

func (s *stubLocal) Scan() ([]engine.CloudEntry, error)                 { return nil, nil }
func (s *stubLocal) Stat(string) (*engine.CloudEntry, error)            { return nil, nil }
func (s *stubLocal) ApplyDownload(string, engine.CloudEntry) error      { return nil }
func (s *stubLocal) Remove(string) error                                { return nil }
func (s *stubLocal) FinalizeUpload(string) error                        { return nil }
func (s *stubLocal) WasHydrated(p string) bool                          { return s.hydrated[p] }
func (s *stubLocal) Hydrate(string) error                               { return nil }

// newTestPanel 起一个带引擎的测试面板（127.0.0.1 随机端口）。
func newTestPanel(t *testing.T) (*panel, *state.Store, *stubLocal) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fl := &stubLocal{hydrated: map[string]bool{}}
	eng := engine.New(st, stubCloud{}, fl, log.New(io.Discard, "", 0))
	p, err := startPanel(st, eng, `C:\sync`, `quark:`, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, st, fl
}

// get 发起请求（不自动跟随重定向），可携带 Cookie。
func get(t *testing.T, url, cookie string) (int, string, []*http.Cookie) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: panelCookieName, Value: cookie})
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Cookies()
}

func TestPanelOneTimeToken(t *testing.T) {
	p, _, _ := newTestPanel(t)

	// 无 token、无 Cookie → 403
	if code, _, _ := get(t, "http://"+p.listener.Addr().String()+"/", ""); code != http.StatusForbidden {
		t.Fatalf("裸访问应 403, got %d", code)
	}

	// 一次性链接 → 302 + Set-Cookie
	url := p.URL()
	code, _, cks := get(t, url, "")
	if code != http.StatusFound {
		t.Fatalf("带 token 访问应 302, got %d", code)
	}
	var session string
	for _, c := range cks {
		if c.Name == panelCookieName {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("没发会话 Cookie")
	}

	// 同一 token 再用一次 → 403（已作废）
	if code, _, _ := get(t, url, ""); code != http.StatusForbidden {
		t.Fatalf("token 应一次性, 二次使用 got %d", code)
	}

	// 凭 Cookie 可反复访问
	for i := 0; i < 2; i++ {
		code, body, _ := get(t, "http://"+p.listener.Addr().String()+"/", session)
		if code != http.StatusOK || !strings.Contains(body, "Onerclone") {
			t.Fatalf("Cookie 访问失败: code=%d body=%.60s", code, body)
		}
	}

	// 伪造 Cookie → 403
	if code, _, _ := get(t, "http://"+p.listener.Addr().String()+"/", "forged"); code != http.StatusForbidden {
		t.Fatalf("伪造 Cookie 应 403, got %d", code)
	}
}

func TestPanelStateJSON(t *testing.T) {
	p, st, _ := newTestPanel(t)
	// 取会话
	_, _, cks := get(t, p.URL(), "")
	session := ""
	for _, c := range cks {
		if c.Name == panelCookieName {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("没拿到会话")
	}

	// 未授权 → 403
	if code, _, _ := get(t, "http://"+p.listener.Addr().String()+"/api/state", ""); code != http.StatusForbidden {
		t.Fatalf("API 裸访问应 403, got %d", code)
	}

	// 造一条冲突动作
	if _, err := st.Enqueue("a/b.txt", state.KindConflict, state.ClassNetwork); err != nil {
		t.Fatal(err)
	}

	code, body, _ := get(t, "http://"+p.listener.Addr().String()+"/api/state", session)
	if code != http.StatusOK {
		t.Fatalf("API 应 200, got %d: %s", code, body)
	}
	var out struct {
		Status struct {
			SyncRoot     string         `json:"sync_root"`
			FsRoot       string         `json:"fs_root"`
			Offline      bool           `json:"offline"`
			BaselineDone bool           `json:"baseline_done"`
			Stats        map[string]int `json:"stats"`
		} `json:"status"`
		Actions []struct {
			Path  string `json:"path"`
			Kind  string `json:"kind"`
			State string `json:"state"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, body)
	}
	if out.Status.SyncRoot != `C:\sync` || out.Status.FsRoot != "quark:" || out.Status.Offline {
		t.Errorf("status 不对: %+v", out.Status)
	}
	if out.Status.BaselineDone {
		t.Error("测试库未建基线，baseline_done 应为 false")
	}
	if out.Status.Stats["pending"] != 1 {
		t.Errorf("pending 应为 1, got %v", out.Status.Stats)
	}
	if len(out.Actions) != 1 || out.Actions[0].Path != "a/b.txt" ||
		out.Actions[0].Kind != string(state.KindConflict) {
		t.Errorf("actions 不对: %+v", out.Actions)
	}
}

// sessionFor 用一次性链接换会话 Cookie。
func sessionFor(t *testing.T, p *panel) string {
	t.Helper()
	_, _, cks := get(t, p.URL(), "")
	for _, c := range cks {
		if c.Name == panelCookieName {
			return c.Value
		}
	}
	t.Fatal("没拿到会话 Cookie")
	return ""
}

// postJSON 发带 Cookie 的 POST。
func postJSON(t *testing.T, url, cookie, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: panelCookieName, Value: cookie})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestPanelActionEndpoints 覆盖重试/放弃/脱水三个管理动作。
func TestPanelActionEndpoints(t *testing.T) {
	p, st, fl := newTestPanel(t)
	sess := sessionFor(t, p)
	base := "http://" + p.listener.Addr().String()

	// 造一条动作并置为永久失败
	if _, err := st.Enqueue("a.txt", state.KindUpload, state.ClassNetwork); err != nil {
		t.Fatal(err)
	}
	acts, _ := st.ListActions(nil, 10)
	if len(acts) != 1 {
		t.Fatalf("准备动作失败: %d", len(acts))
	}
	id := acts[0].ID
	if err := st.Fail(id, state.ClassPermanent, "boom"); err != nil {
		t.Fatal(err)
	}

	// 未授权 → 403
	if code, _ := postJSON(t, base+"/api/action/retry", "", `{"id":1}`); code != http.StatusForbidden {
		t.Fatalf("裸 POST 应 403, got %d", code)
	}

	// 重试 → 回到 pending、清空错误
	if code, body := postJSON(t, base+"/api/action/retry", sess, fmt.Sprintf(`{"id":%d}`, id)); code != http.StatusOK {
		t.Fatalf("retry 应 200, got %d: %s", code, body)
	}
	acts, _ = st.ListActions(nil, 10)
	if len(acts) != 1 || acts[0].State != state.StatePending || acts[0].LastErr != "" || acts[0].Attempts != 0 {
		t.Fatalf("重试后应是干净的 pending: %+v", acts)
	}

	// 放弃 → 动作消失
	if code, body := postJSON(t, base+"/api/action/drop", sess, fmt.Sprintf(`{"id":%d}`, id)); code != http.StatusOK {
		t.Fatalf("drop 应 200, got %d: %s", code, body)
	}
	if acts, _ = st.ListActions(nil, 10); len(acts) != 0 {
		t.Fatalf("放弃后队列应空, got %+v", acts)
	}

	// 脱水：未水合 → 409
	if code, _ := postJSON(t, base+"/api/dehydrate", sess, `{"path":"x.bin"}`); code != http.StatusConflict {
		t.Fatalf("未水合脱水应 409, got %d", code)
	}
	// 水合后 → 200 且入队 dehydrate
	fl.hydrated["x.bin"] = true
	if code, body := postJSON(t, base+"/api/dehydrate", sess, `{"path":"x.bin"}`); code != http.StatusOK {
		t.Fatalf("脱水应 200, got %d: %s", code, body)
	}
	acts, _ = st.ListActions(nil, 10)
	if len(acts) != 1 || acts[0].Kind != state.KindDehydrate || acts[0].Path != "x.bin" {
		t.Fatalf("应入队 dehydrate: %+v", acts)
	}
}

// TestPanelConflicts 覆盖冲突可视化（两端快照 + 冲突副本）。
func TestPanelConflicts(t *testing.T) {
	p, st, _ := newTestPanel(t)
	sess := sessionFor(t, p)
	now := time.Now()

	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.PutSnap("local_snap", state.FileSnap{
		Path: "d/a.txt", Size: 10, MTime: now, Present: true}))
	must(st.PutSnap("local_snap", state.FileSnap{
		Path: "d/a (冲突 20260927-101112).txt", Size: 5, MTime: now, Present: true}))
	must(st.PutSnap("cloud_snap", state.FileSnap{
		Path: "d/a.txt", Size: 11, MTime: now.Add(time.Minute), Present: true}))
	if _, err := st.Enqueue("d/a.txt", state.KindConflict, state.ClassNetwork); err != nil {
		t.Fatal(err)
	}

	code, body, _ := get(t, "http://"+p.listener.Addr().String()+"/api/conflicts", sess)
	if code != http.StatusOK {
		t.Fatalf("conflicts 应 200, got %d: %s", code, body)
	}
	var out struct {
		Conflicts []conflictRow `json:"conflicts"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, body)
	}
	if len(out.Conflicts) != 1 {
		t.Fatalf("应 1 条冲突, got %d", len(out.Conflicts))
	}
	row := out.Conflicts[0]
	if row.Path != "d/a.txt" || !row.Local.Present || row.Cloud.Size != 11 {
		t.Errorf("两端快照不对: %+v", row)
	}
	if len(row.LocalCopies) != 1 || row.LocalCopies[0] != "d/a (冲突 20260927-101112).txt" {
		t.Errorf("本地冲突副本识别不对: %+v", row.LocalCopies)
	}
	if len(row.CloudCopies) != 0 {
		t.Errorf("云端应无副本: %+v", row.CloudCopies)
	}
}
