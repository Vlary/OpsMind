// W3：终端 WS 升级校验与会话/录制 REST 的 handler 测试（契约 9.2 G20 的
// httpapi 部分：WS 升级前 Origin 403 / 未登录 401 / 子协议 400、会话 404/409、
// 录制下发鉴权与 Content-Type）。
//
// termMemStore 嵌入 *memStore 并补 TerminalSessions()（Store 接口 W3 增量），
// 不改动 fakes_test.go（BE1 维护）。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"aiops/internal/auth"
	"aiops/internal/config"
	"aiops/internal/cryptox"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/termx"
)

// ---------------------------------------------------------------------------
// termMemStore：memStore + TerminalSessionStore
// ---------------------------------------------------------------------------

// memTerminalTable 是 TerminalSessionStore 的内存实现（独立类型：
// 其 Close 方法不得覆盖嵌入 Store 的 Close()）。
type memTerminalTable struct {
	mu    sync.Mutex
	terms map[string]*store.TerminalSession
}

func (t *memTerminalTable) Create(_ context.Context, p store.CreateTerminalSessionParams) (store.TerminalSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().UTC()
	s := store.TerminalSession{
		ID:             uuid.NewString(),
		ServerID:       p.ServerID,
		ServerName:     "mem-server",
		ServerOS:       store.ServerOSLinux,
		Username:       "admin",
		Status:         store.SessionActive,
		Cols:           p.Cols,
		Rows:           p.Rows,
		StartedAt:      now,
		LastActivityAt: now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	t.terms[s.ID] = &s
	return s, nil
}

func (t *memTerminalTable) Get(_ context.Context, id string) (store.TerminalSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.terms[id]; ok {
		return *s, nil
	}
	return store.TerminalSession{}, store.ErrNotFound
}

func (t *memTerminalTable) List(_ context.Context, f store.InteractiveSessionFilter) ([]store.TerminalSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	items := make([]store.TerminalSession, 0)
	for _, s := range t.terms {
		if f.Status != "" && s.Status != f.Status {
			continue
		}
		if f.ServerID != "" && s.ServerID != f.ServerID {
			continue
		}
		items = append(items, *s)
	}
	return items, nil
}

func (t *memTerminalTable) TouchActivity(_ context.Context, id string, at time.Time, bytes int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.terms[id]; ok && s.Status == store.SessionActive {
		s.LastActivityAt = at
		s.BytesWritten = bytes
	}
	return nil
}

func (t *memTerminalTable) Close(_ context.Context, p store.CloseInteractiveSessionParams) (store.TerminalSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.terms[p.ID]
	if !ok {
		return store.TerminalSession{}, store.ErrNotFound
	}
	if s.Status != store.SessionActive {
		return store.TerminalSession{}, store.ErrConflict
	}
	s.Status = store.SessionClosed
	s.EndedReason = p.Reason
	s.EndedAt = &p.EndedAt
	s.BytesWritten = p.BytesWritten
	s.HasRecording = p.BytesWritten > 0
	if s.HasRecording {
		s.RecordingFile = s.ID + ".cast"
	}
	s.UpdatedAt = p.EndedAt
	return *s, nil
}

func (t *memTerminalTable) CountActive(context.Context) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var n int64
	for _, s := range t.terms {
		if s.Status == store.SessionActive {
			n++
		}
	}
	return n, nil
}

func (t *memTerminalTable) CountActiveByServer(_ context.Context, serverID string) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var n int64
	for _, s := range t.terms {
		if s.Status == store.SessionActive && s.ServerID == serverID {
			n++
		}
	}
	return n, nil
}

// termMemStore 组合 memStore 与 terminal_sessions 表，作为 Store 传给
// term 路由的测试环境。
type termMemStore struct {
	*memStore
	table *memTerminalTable
}

func (t *termMemStore) TerminalSessions() store.TerminalSessionStore { return t.table }

// memTerminalTables 让裸 *memStore 也满足二期 Store 接口：每个实例对应
// 一张独立的 terminal_sessions 表（包级注册，测试进程生命周期内有效）。
var memTerminalTables sync.Map // *memStore → *memTerminalTable

func (m *memStore) TerminalSessions() store.TerminalSessionStore {
	v, _ := memTerminalTables.LoadOrStore(m, &memTerminalTable{terms: map[string]*store.TerminalSession{}})
	return v.(*memTerminalTable)
}

// fakePTYPool 满足 sshx.Executor（嵌入 nil：测试不触发 Test/Execute）并
// 提供 PTY 能力断言所需的 Session 方法；Session 恒失败——升级成功路径
// 因此走 TERM_CONNECT_FAILED → closed(error) 收尾。
type fakePTYPool struct {
	sshx.Executor
}

func (f *fakePTYPool) Session(context.Context, sshx.Target, sshx.SessionRequest) (sshx.PTYSession, error) {
	return nil, errors.New("fake: no pty")
}

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

type termTestEnv struct {
	t      *testing.T
	router *gin.Engine
	st     *termMemStore
	cipher *cryptox.Cipher
	cookie string
	dir    string
}

func newTermTestEnv(t *testing.T) *termTestEnv {
	t.Helper()
	return newTermTestEnvWithSSH(t, &fakePTYPool{})
}

// newTermTestEnvWithSSH 允许注入可拨号的 SSH/PTY fake（默认 fakePTYPool
// 的 Session 恒失败，无法形成 live 会话）。
func newTermTestEnvWithSSH(t *testing.T, ssh sshx.Executor) *termTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st := &termMemStore{memStore: newMemStore(), table: &memTerminalTable{terms: map[string]*store.TerminalSession{}}}
	st.users["admin"] = store.User{
		ID: "user-1", Username: "admin", PasswordHash: getAdminHash(),
		CreatedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
	}
	cipher, err := cryptox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	authSvc := auth.NewService(st, auth.Config{SessionTTL: time.Hour})
	dir := t.TempDir()
	termMgr := termx.NewManager(termx.Config{RecordingsDir: dir}, st, ssh, nil, time.Now, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		termMgr.Shutdown(ctx)
	})
	cfg := &config.Config{
		AllowedOrigins: []string{testOrigin},
		PublicOrigin:   testOrigin,
		WebDir:         "",
		RecordingsDir:  dir,
	}
	env := &termTestEnv{t: t, st: st, cipher: cipher, dir: dir}
	env.router = NewRouter(&Deps{
		Config: cfg,
		Auth:   authSvc,
		Store:  st,
		Cipher: cipher,
		Term:   termMgr,
		Clock:  time.Now,
	})
	env.cookie = env.login()
	return env
}

func (e *termTestEnv) login() string {
	e.t.Helper()
	body := `{"password":"` + testPassword + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("login status = %d body = %s", rec.Code, rec.Body.String())
	}
	raw := rec.Header().Get("Set-Cookie")
	parts := strings.SplitN(raw, ";", 2)
	return strings.TrimPrefix(parts[0], sessionCookieName+"=")
}

// do 发起带会话与 Origin 的请求。
func (e *termTestEnv) do(method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: e.cookie})
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// createServer 直接注入一台服务器（memStore 的自增 ID 非 UUID；server
// 创建本身的 HTTP 行为由 BE2 的测试覆盖）。密码用测试 cipher 真加密，
// 使 WS 路径能通过 sshTarget 解密进入 Attach。
func (e *termTestEnv) createServer(t *testing.T, os string, auth store.AuthMethod) string {
	t.Helper()
	enc, err := e.cipher.Encrypt("secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	id := uuid.NewString()
	e.st.mu.Lock()
	e.st.servers[id] = store.Server{
		ID:                id,
		Name:              "term-host-" + id[:6],
		OS:                store.ServerOS(os),
		Host:              "127.0.0.1",
		Port:              22,
		Username:          "root",
		AuthMethod:        auth,
		PasswordEncrypted: enc,
	}
	e.st.mu.Unlock()
	return id
}

func (e *termTestEnv) createSession(t *testing.T, serverID string) store.TerminalSession {
	t.Helper()
	rec := e.do(http.MethodPost, "/api/term/sessions", `{"server_id":"`+serverID+`","cols":120,"rows":40}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session status = %d body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Session store.TerminalSession `json:"session"`
		WsURL   string                `json:"ws_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}
	if out.WsURL != "/api/term/sessions/"+out.Session.ID+"/ws" {
		t.Fatalf("ws_url = %q", out.WsURL)
	}
	return out.Session
}

// ---------------------------------------------------------------------------
// WS 升级前校验顺序（契约 6.4：Origin → Cookie → 业务 → 子协议 → 升级）
// ---------------------------------------------------------------------------

func TestTermWSUpgradeValidation(t *testing.T) {
	env := newTermTestEnv(t)
	serverID := env.createServer(t, "linux", store.AuthMethodPassword)
	session := env.createSession(t, serverID)
	path := "/api/term/sessions/" + session.ID + "/ws"

	// 未登录（无 Cookie）→ 401 AUTH_REQUIRED。
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "AUTH_REQUIRED") {
		t.Fatalf("no-cookie: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Origin 错误 → 403 ORIGIN_FORBIDDEN。
	req = httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Origin", "http://evil.example")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: env.cookie})
	rec = httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "ORIGIN_FORBIDDEN") {
		t.Fatalf("bad-origin: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// 子协议缺失 → 400 TERM_WS_PROTOCOL。
	rec = env.do(http.MethodGet, path, "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "TERM_WS_PROTOCOL") {
		t.Fatalf("no-subprotocol: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// 子协议不匹配 → 400 TERM_WS_PROTOCOL。
	req = httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Sec-WebSocket-Protocol", "other-v9")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: env.cookie})
	rec = httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "TERM_WS_PROTOCOL") {
		t.Fatalf("wrong-subprotocol: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// 会话不存在 → 404 TERM_SESSION_NOT_FOUND。
	missing := "/api/term/sessions/" + uuid.NewString() + "/ws"
	req = httptest.NewRequest(http.MethodGet, missing, nil)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Sec-WebSocket-Protocol", termWSSubprotocol)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: env.cookie})
	rec = httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "TERM_SESSION_NOT_FOUND") {
		t.Fatalf("missing-session: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// 非 active 会话 → 409 CONFLICT。
	if _, err := env.st.TerminalSessions().Close(context.Background(), store.CloseInteractiveSessionParams{
		ID: session.ID, Reason: "client_closed", EndedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("close session: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Sec-WebSocket-Protocol", termWSSubprotocol)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: env.cookie})
	rec = httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("closed-session: status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// TestTermWSUpgradeSucceeds 验证校验全通过时升级 101 + 子协议协商；
// Manager 无 SSH 执行器，随后会话以 error 收尾（error/closed 帧）。
func TestTermWSUpgradeSucceeds(t *testing.T) {
	env := newTermTestEnv(t)
	serverID := env.createServer(t, "linux", store.AuthMethodPassword)
	session := env.createSession(t, serverID)

	httpSrv := httptest.NewServer(env.router)
	t.Cleanup(httpSrv.Close)
	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/api/term/sessions/" + session.ID + "/ws"

	header := http.Header{}
	header.Set("Origin", testOrigin)
	header.Set("Cookie", sessionCookieName+"="+env.cookie)
	header.Set("Sec-WebSocket-Protocol", termWSSubprotocol)
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		body := ""
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
		}
		t.Fatalf("dial: %v resp=%v body=%s", err, resp, body)
	}
	defer conn.Close()
	if resp.Header.Get("Sec-WebSocket-Protocol") != termWSSubprotocol {
		t.Fatalf("negotiated subprotocol = %q", resp.Header.Get("Sec-WebSocket-Protocol"))
	}
	// 无 PTY 支持：error 帧（TERM_CONNECT_FAILED）后 closed(error)。
	sawError, sawClosed := false, false
	deadline := time.Now().Add(5 * time.Second)
	for (!sawError || !sawClosed) && time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("read frame: %v (error=%v closed=%v)", err, sawError, sawClosed)
		}
		switch m["type"] {
		case "error":
			if m["code"] == "TERM_CONNECT_FAILED" {
				sawError = true
			}
		case "closed":
			if m["reason"] == "error" {
				sawClosed = true
			}
		}
	}
	if !sawError || !sawClosed {
		t.Fatalf("expected error+closed frames; error=%v closed=%v", sawError, sawClosed)
	}
	// closed 帧先于收尾落库到达；轮询等待会话行 closed(error)。
	rowDeadline := time.Now().Add(5 * time.Second)
	for {
		row, err := env.st.TerminalSessions().Get(context.Background(), session.ID)
		if err == nil && row.Status == store.SessionClosed && row.EndedReason == "error" {
			return
		}
		if time.Now().After(rowDeadline) {
			t.Fatalf("session row after failed pty: %+v err=%v", row, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// 会话/录制 REST
// ---------------------------------------------------------------------------

func TestTermSessionCreateAndList(t *testing.T) {
	env := newTermTestEnv(t)
	serverID := env.createServer(t, "linux", store.AuthMethodPassword)
	session := env.createSession(t, serverID)
	if session.Cols != 120 || session.Rows != 40 || session.Status != store.SessionActive {
		t.Fatalf("unexpected session: %+v", session)
	}

	// 同服务器第二个会话 → 409 TERM_SESSION_EXISTS（G20）。
	rec := env.do(http.MethodPost, "/api/term/sessions", `{"server_id":"`+serverID+`"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "TERM_SESSION_EXISTS") {
		t.Fatalf("second session: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// 非法 cols → 400。
	rec = env.do(http.MethodPost, "/api/term/sessions", `{"server_id":"`+serverID+`","cols":5}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cols: status = %d", rec.Code)
	}

	// 列表（status 过滤）。
	rec = env.do(http.MethodGet, "/api/term/sessions?status=active", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status = %d", rec.Code)
	}
	var list struct {
		Items []store.TerminalSession `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list items = %d err = %v", len(list.Items), err)
	}

	// WinRM 认证的服务器 → 400 TERM_UNSUPPORTED（契约 2.7）。
	winID := env.createServer(t, "windows", store.AuthMethodPasswordNTLM)
	rec = env.do(http.MethodPost, "/api/term/sessions", `{"server_id":"`+winID+`"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "TERM_UNSUPPORTED") {
		t.Fatalf("winrm term: status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestTermRecordingDownload(t *testing.T) {
	env := newTermTestEnv(t)
	serverID := env.createServer(t, "linux", store.AuthMethodPassword)
	session := env.createSession(t, serverID)

	// 未登录 → 401。
	req := httptest.NewRequest(http.MethodGet, "/api/term/recordings/"+session.ID, nil)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated download: status = %d", rec.Code)
	}

	// 无录制 → 404 RECORDING_NOT_FOUND。
	rec = env.do(http.MethodGet, "/api/term/recordings/"+session.ID, "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "RECORDING_NOT_FOUND") {
		t.Fatalf("no recording: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// 关闭会话（有录制）→ 列表出现 → 下发 Content-Type 固定。
	cast := `{"version":2,"width":120,"height":40}` + "\n" + `[0.5,"o","hi"]` + "\n"
	path := filepath.Join(env.dir, session.ID+".cast")
	if err := os.WriteFile(path, []byte(cast), 0o600); err != nil {
		t.Fatalf("write cast: %v", err)
	}
	if _, err := env.st.TerminalSessions().Close(context.Background(), store.CloseInteractiveSessionParams{
		ID: session.ID, Reason: "client_closed", EndedAt: time.Now().UTC(), BytesWritten: int64(len(cast)),
	}); err != nil {
		t.Fatalf("close: %v", err)
	}

	rec = env.do(http.MethodGet, "/api/term/recordings", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), session.ID) {
		t.Fatalf("recordings list: status = %d body = %s", rec.Code, rec.Body.String())
	}

	rec = env.do(http.MethodGet, "/api/term/recordings/"+session.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("download: status = %d body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != asciicastContentType {
		t.Fatalf("content type = %q", ct)
	}
	if rec.Body.String() != cast {
		t.Fatalf("body = %q", rec.Body.String())
	}

	// Range 请求由 ServeContent 处理（bytes=0-10）。
	req = httptest.NewRequest(http.MethodGet, "/api/term/recordings/"+session.ID, nil)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Range", "bytes=0-10")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: env.cookie})
	rec = httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || len(rec.Body.String()) != 11 {
		t.Fatalf("range download: status = %d len = %d", rec.Code, len(rec.Body.String()))
	}
}
