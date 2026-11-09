// W4：RDP 会话/录制 REST 与 WS 升级校验的 handler 测试（契约 9.2 G21
// httpapi 部分：非 windows 400 RDP_UNSUPPORTED、无密码凭据 400
// RDP_NO_CREDENTIALS、GuacdAddr 空时 503 GUACD_UNAVAILABLE、WS 升级前
// Origin 403 / 未登录 401 / 随机会话 404、录制下发鉴权与 Content-Type、
// gorilla WS ↔ 假 guacd 端到端文本帧透传）。
//
// memRdpTable 为 rdp_sessions 内存实现；对裸 *memStore 的 RdpSessions()
// 补丁与 W3 的 TerminalSessions() 同款（包级注册，不改 fakes_test.go）。
package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
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
	"aiops/internal/rdpx"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// memRdpTable：rdp_sessions 内存表
// ---------------------------------------------------------------------------

type memRdpTable struct {
	mu   sync.Mutex
	rows map[string]*store.RdpSession
}

func (t *memRdpTable) Create(_ context.Context, p store.CreateRdpSessionParams) (store.RdpSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().UTC()
	s := store.RdpSession{
		ID:             uuid.NewString(),
		ServerID:       p.ServerID,
		ServerName:     "mem-server",
		Width:          p.Width,
		Height:         p.Height,
		DPI:            p.DPI,
		Status:         store.SessionActive,
		StartedAt:      now,
		LastActivityAt: now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	t.rows[s.ID] = &s
	return s, nil
}

func (t *memRdpTable) Get(_ context.Context, id string) (store.RdpSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.rows[id]; ok {
		return *s, nil
	}
	return store.RdpSession{}, store.ErrNotFound
}

func (t *memRdpTable) List(_ context.Context, f store.InteractiveSessionFilter) ([]store.RdpSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	items := make([]store.RdpSession, 0)
	for _, s := range t.rows {
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

func (t *memRdpTable) TouchActivity(_ context.Context, id string, at time.Time, bytes int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.rows[id]; ok && s.Status == store.SessionActive {
		s.LastActivityAt = at
		s.BytesWritten = bytes
	}
	return nil
}

func (t *memRdpTable) Close(_ context.Context, p store.CloseInteractiveSessionParams) (store.RdpSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.rows[p.ID]
	if !ok {
		return store.RdpSession{}, store.ErrNotFound
	}
	if s.Status != store.SessionActive {
		return store.RdpSession{}, store.ErrConflict
	}
	s.Status = store.SessionClosed
	s.EndedReason = p.Reason
	s.EndedAt = &p.EndedAt
	s.BytesWritten = p.BytesWritten
	s.HasRecording = p.BytesWritten > 0
	if s.HasRecording {
		s.RecordingFile = s.ID + ".guac"
	}
	s.UpdatedAt = p.EndedAt
	return *s, nil
}

func (t *memRdpTable) CountActive(context.Context) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var n int64
	for _, s := range t.rows {
		if s.Status == store.SessionActive {
			n++
		}
	}
	return n, nil
}

func (t *memRdpTable) CountActiveByServer(_ context.Context, serverID string) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var n int64
	for _, s := range t.rows {
		if s.Status == store.SessionActive && s.ServerID == serverID {
			n++
		}
	}
	return n, nil
}

func (t *memRdpTable) get(id string) (store.RdpSession, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.rows[id]
	return *s, ok
}

// memRdpTables 让裸 *memStore 也满足二期 Store 接口（W4 增量）。
var memRdpTables sync.Map // *memStore → *memRdpTable

func (m *memStore) RdpSessions() store.RdpSessionStore {
	v, _ := memRdpTables.LoadOrStore(m, &memRdpTable{rows: map[string]*store.RdpSession{}})
	return v.(*memRdpTable)
}

// ---------------------------------------------------------------------------
// 假 guacd（httpapi 端到端用）：select→args→size×4→connect→echo 透传
// ---------------------------------------------------------------------------

// startGuacdStub 启动本地 TCP 假 guacd；返回地址与 connect 参数信道。
func startGuacdStub(t *testing.T) (addr string, connectCh <-chan map[string]string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen guacd stub: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan map[string]string, 4)
	t.Cleanup(func() {
		for {
			select {
			case <-ch:
			default:
				return
			}
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleGuacdStubConn(conn, ch)
		}
	}()
	return ln.Addr().String(), ch
}

func handleGuacdStubConn(conn net.Conn, ch chan<- map[string]string) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 64*1024)
	// select → args
	if _, err := readGuacdInstruction(r); err != nil {
		return
	}
	if _, err := conn.Write([]byte("4.args,13.VERSION_1_5_0,8.hostname,4.port,8.username,8.password,8.security,11.ignore-cert,5.width,6.height,3.dpi,14.recording-name;")); err != nil {
		return
	}
	// size/audio/video/image
	for i := 0; i < 4; i++ {
		if _, err := readGuacdInstruction(r); err != nil {
			return
		}
	}
	// connect
	raw, err := readGuacdInstruction(r)
	if err != nil {
		return
	}
	params := map[string]string{}
	for _, e := range splitGuacdElements(raw) {
		if k, v, ok := strings.Cut(e, "="); ok {
			params[k] = v
		}
	}
	select {
	case ch <- params:
	default:
	}
	// 握手后：先发一帧，再回显（echo）透传。
	if _, err := conn.Write([]byte("4.sync,1.0;")); err != nil {
		return
	}
	buf := make([]byte, 64*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}

// readGuacdInstruction 按长度前缀读一条完整指令（同 rdpx 私有实现的语义）。
func readGuacdInstruction(r *bufio.Reader) ([]byte, error) {
	var b []byte
	for {
		var lenStr []byte
		for {
			c, err := r.ReadByte()
			if err != nil {
				if len(b) == 0 && len(lenStr) == 0 {
					return nil, err
				}
				return nil, io.ErrUnexpectedEOF
			}
			if c == '.' {
				break
			}
			if c < '0' || c > '9' {
				return nil, errors.New("bad length")
			}
			lenStr = append(lenStr, c)
		}
		n := 0
		for _, d := range lenStr {
			n = n*10 + int(d-'0')
		}
		if len(b) > 0 {
			b = append(b, ',')
		}
		b = append(b, lenStr...)
		b = append(b, '.')
		for i := 0; i < n; i++ {
			c, err := r.ReadByte()
			if err != nil {
				return nil, io.ErrUnexpectedEOF
			}
			b = append(b, c)
		}
		c, err := r.ReadByte()
		if err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		if c == ';' {
			return b, nil
		}
		if c != ',' {
			return nil, errors.New("bad separator")
		}
	}
}

// splitGuacdElements 解析指令元素（第一个为 opcode，其余为参数）。
func splitGuacdElements(raw []byte) []string {
	var out []string
	for i := 0; i < len(raw); {
		dot := -1
		for j := i; j < len(raw); j++ {
			if raw[j] == '.' {
				dot = j
				break
			}
		}
		if dot < 0 {
			return out
		}
		n := 0
		for _, d := range raw[i:dot] {
			n = n*10 + int(d-'0')
		}
		start, end := dot+1, dot+1+n
		if end > len(raw) {
			return out
		}
		out = append(out, string(raw[start:end]))
		i = end + 1
	}
	return out
}

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

type rdpTestEnv struct {
	t       *testing.T
	router  *gin.Engine
	st      *memStore
	cipher  *cryptox.Cipher
	cookie  string
	dir     string
	connect <-chan map[string]string
}

// newRdpTestEnv 组装路由。guacdAddr 为空表示 RDP 禁用（Gateway Addr=""）。
func newRdpTestEnv(t *testing.T, guacdAddr string) *rdpTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st := newMemStore()
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
	gw := rdpx.NewGateway(rdpx.Config{
		Addr:          guacdAddr,
		RecordingsDir: dir,
	}, st, nil, cipher, time.Now, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		gw.Shutdown(ctx)
	})
	cfg := &config.Config{
		AllowedOrigins:   []string{testOrigin},
		PublicOrigin:     testOrigin,
		WebDir:           "",
		RdpRecordingsDir: dir,
	}
	env := &rdpTestEnv{t: t, st: st, cipher: cipher, dir: dir}
	env.router = NewRouter(&Deps{
		Config: cfg,
		Auth:   authSvc,
		Store:  st,
		Cipher: cipher,
		RDP:    gw,
		Clock:  time.Now,
	})
	env.cookie = env.login()
	return env
}

func (e *rdpTestEnv) login() string {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"`+testPassword+`"}`))
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

func (e *rdpTestEnv) do(method, path, body string) *httptest.ResponseRecorder {
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

// createServer 注入一台服务器（密码用测试 cipher 真加密）。
func (e *rdpTestEnv) createServer(t *testing.T, osName string, authMethod store.AuthMethod, withPassword bool) string {
	t.Helper()
	id := uuid.NewString()
	srv := store.Server{
		ID:         id,
		Name:       "rdp-host-" + id[:6],
		OS:         store.ServerOS(osName),
		Host:       "127.0.0.1",
		Port:       3389,
		Username:   "Administrator",
		AuthMethod: authMethod,
	}
	if withPassword {
		enc, err := e.cipher.Encrypt("rdp-secret")
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		srv.PasswordEncrypted = enc
	}
	e.st.mu.Lock()
	e.st.servers[id] = srv
	e.st.mu.Unlock()
	return id
}

func (e *rdpTestEnv) createSession(t *testing.T, serverID, body string) (int, string) {
	t.Helper()
	rec := e.do(http.MethodPost, "/api/rdp/sessions", body)
	var out struct {
		Session store.RdpSession `json:"session"`
	}
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal session: %v", err)
		}
	}
	return rec.Code, out.Session.ID
}

// ---------------------------------------------------------------------------
// 创建/列表 REST（G21：错误码与状态码逐项匹配）
// ---------------------------------------------------------------------------

func TestRdpCreateValidation(t *testing.T) {
	addr, connect := startGuacdStub(t)
	env := newRdpTestEnv(t, addr)
	env.connect = connect

	// 非 windows 服务器 → 400 RDP_UNSUPPORTED。
	linuxID := env.createServer(t, "linux", store.AuthMethodPassword, true)
	rec := env.do(http.MethodPost, "/api/rdp/sessions", `{"server_id":"`+linuxID+`"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "RDP_UNSUPPORTED") {
		t.Fatalf("linux create: %d %s", rec.Code, rec.Body.String())
	}

	// windows 无密码凭据 → 400 RDP_NO_CREDENTIALS。
	winNoPw := env.createServer(t, "windows", store.AuthMethodPassword, false)
	rec = env.do(http.MethodPost, "/api/rdp/sessions", `{"server_id":"`+winNoPw+`"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "RDP_NO_CREDENTIALS") {
		t.Fatalf("no credential: %d %s", rec.Code, rec.Body.String())
	}

	// windows private_key 认证 → 400 RDP_NO_CREDENTIALS。
	winKey := env.createServer(t, "windows", store.AuthMethodPrivateKey, false)
	rec = env.do(http.MethodPost, "/api/rdp/sessions", `{"server_id":"`+winKey+`"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "RDP_NO_CREDENTIALS") {
		t.Fatalf("private key: %d %s", rec.Code, rec.Body.String())
	}

	// 不存在的服务器 → 404 SERVER_NOT_FOUND。
	rec = env.do(http.MethodPost, "/api/rdp/sessions", `{"server_id":"`+uuid.NewString()+`"}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "SERVER_NOT_FOUND") {
		t.Fatalf("missing server: %d %s", rec.Code, rec.Body.String())
	}

	// 非法 server_id → 400 VALIDATION_FAILED。
	rec = env.do(http.MethodPost, "/api/rdp/sessions", `{"server_id":"not-a-uuid"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "VALIDATION_FAILED") {
		t.Fatalf("bad uuid: %d %s", rec.Code, rec.Body.String())
	}

	// 越界分辨率 → 400。
	winID := env.createServer(t, "windows", store.AuthMethodPassword, true)
	rec = env.do(http.MethodPost, "/api/rdp/sessions", `{"server_id":"`+winID+`","width":100}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "VALIDATION_FAILED") {
		t.Fatalf("bad width: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodPost, "/api/rdp/sessions", `{"server_id":"`+winID+`","dpi":300}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad dpi: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRdpCreateGuacdUnavailable(t *testing.T) {
	// GuacdAddr 空 = RDP 禁用 → 503 GUACD_UNAVAILABLE（S19）。
	env := newRdpTestEnv(t, "")
	winID := env.createServer(t, "windows", store.AuthMethodPassword, true)
	rec := env.do(http.MethodPost, "/api/rdp/sessions", `{"server_id":"`+winID+`"}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "GUACD_UNAVAILABLE") {
		t.Fatalf("guacd unavailable: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRdpCreateSuccessAndConflict(t *testing.T) {
	addr, connect := startGuacdStub(t)
	env := newRdpTestEnv(t, addr)
	env.connect = connect
	winID := env.createServer(t, "windows", store.AuthMethodPasswordNTLM, true)

	rec := env.do(http.MethodPost, "/api/rdp/sessions", `{"server_id":"`+winID+`","width":1600,"height":900,"dpi":120}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Session store.RdpSession `json:"session"`
		WsURL   string           `json:"ws_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.WsURL != "/api/rdp/sessions/"+out.Session.ID+"/ws" {
		t.Fatalf("ws_url = %q", out.WsURL)
	}
	if out.Session.Width != 1600 || out.Session.Height != 900 || out.Session.DPI != 120 {
		t.Fatalf("resolution not persisted: %+v", out.Session)
	}
	if out.Session.Status != store.SessionActive {
		t.Fatalf("status = %q", out.Session.Status)
	}
	// 响应不含任何凭据。
	if strings.Contains(rec.Body.String(), "rdp-secret") {
		t.Fatal("credential leaked in create response")
	}

	// 同服务器第二个活动会话 → 409 RDP_SESSION_EXISTS。
	rec = env.do(http.MethodPost, "/api/rdp/sessions", `{"server_id":"`+winID+`"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "RDP_SESSION_EXISTS") {
		t.Fatalf("second create: %d %s", rec.Code, rec.Body.String())
	}

	// 列表 200 且含该会话。
	rec = env.do(http.MethodGet, "/api/rdp/sessions?status=active", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), out.Session.ID) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	// 非法 status → 400。
	rec = env.do(http.MethodGet, "/api/rdp/sessions?status=bogus", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad status filter: %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// 录制 REST
// ---------------------------------------------------------------------------

func TestRdpRecordings(t *testing.T) {
	addr, connect := startGuacdStub(t)
	env := newRdpTestEnv(t, addr)
	env.connect = connect

	// 空列表 200。
	rec := env.do(http.MethodGet, "/api/rdp/recordings", "")
	if rec.Code != http.StatusOK || rec.Body.String() == "" || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty recordings: %d %s", rec.Code, rec.Body.String())
	}

	// 无录制文件的会话下载 → 404 RECORDING_NOT_FOUND。
	winID := env.createServer(t, "windows", store.AuthMethodPassword, true)
	_, sessionID := env.createSession(t, winID, `{"server_id":"`+winID+`"}`)
	rec = env.do(http.MethodGet, "/api/rdp/recordings/"+sessionID, "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "RECORDING_NOT_FOUND") {
		t.Fatalf("no recording: %d %s", rec.Code, rec.Body.String())
	}

	// 造一行已结束且有录制的会话 + 落一个 .guac 文件 → 列表出现、下载 200。
	tbl, _ := memRdpTables.LoadOrStore(env.st, &memRdpTable{rows: map[string]*store.RdpSession{}})
	table := tbl.(*memRdpTable)
	ended := time.Now().UTC().Add(-time.Minute)
	done := store.RdpSession{
		ID: uuid.NewString(), ServerID: winID, ServerName: "mem-server",
		Width: 1280, Height: 800, DPI: 96, Status: store.SessionClosed,
		EndedReason: "client_closed", RecordingFile: "x", HasRecording: true,
		BytesWritten: 99, StartedAt: ended.Add(-time.Minute), EndedAt: &ended,
		CreatedAt: ended.Add(-time.Minute), UpdatedAt: ended,
	}
	table.rows[done.ID] = &done
	if err := os.WriteFile(filepath.Join(env.dir, done.ID+".guac"), []byte("GUAC-RECORDING-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec = env.do(http.MethodGet, "/api/rdp/recordings", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), done.ID) {
		t.Fatalf("recordings list: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/api/rdp/recordings/"+done.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("download: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	if rec.Body.String() != "GUAC-RECORDING-BYTES" {
		t.Fatalf("body = %q", rec.Body.String())
	}

	// 有录制行但文件缺失 → 502 RECORDING_UNAVAILABLE。
	table.rows[done.ID].RecordingFile = done.ID + ".guac"
	_ = os.Remove(filepath.Join(env.dir, done.ID+".guac"))
	rec = env.do(http.MethodGet, "/api/rdp/recordings/"+done.ID, "")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "RECORDING_UNAVAILABLE") {
		t.Fatalf("missing file: %d %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// WS 升级前校验顺序（契约 6.5：Origin → Cookie → 业务 → 升级）
// ---------------------------------------------------------------------------

func TestRdpWSUpgradeValidation(t *testing.T) {
	addr, connect := startGuacdStub(t)
	env := newRdpTestEnv(t, addr)
	env.connect = connect
	winID := env.createServer(t, "windows", store.AuthMethodPassword, true)
	_, sessionID := env.createSession(t, winID, `{"server_id":"`+winID+`"}`)
	path := "/api/rdp/sessions/" + sessionID + "/ws"

	// 未登录（无 Cookie）→ 401 AUTH_REQUIRED。
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "AUTH_REQUIRED") {
		t.Fatalf("no-cookie: %d %s", rec.Code, rec.Body.String())
	}

	// Origin 缺失/错误 → 403 ORIGIN_FORBIDDEN。
	req = httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Origin", "http://evil.example")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: env.cookie})
	rec = httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "ORIGIN_FORBIDDEN") {
		t.Fatalf("bad origin: %d %s", rec.Code, rec.Body.String())
	}

	// 随机会话 → 404 RDP_SESSION_NOT_FOUND。
	rec = env.do(http.MethodGet, "/api/rdp/sessions/"+uuid.NewString()+"/ws", "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "RDP_SESSION_NOT_FOUND") {
		t.Fatalf("random session: %d %s", rec.Code, rec.Body.String())
	}

	// 非法 id → 400。
	rec = env.do(http.MethodGet, "/api/rdp/sessions/not-uuid/ws", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

// TestRdpWSSubprotocolNegotiation 验证 guacamole 子协议协商（二期走查
// gap1）：guacamole-common-js 的 WebSocketTunnel 固定以子协议 "guacamole"
// 发起升级（dist 源码 new WebSocket(url, "guacamole")），upgrader 未声明
// Subprotocols 时 gorilla 不回显、浏览器以 400 拒绝握手（走查三重证据：
// 库源码 / curl 对比 / 真实浏览器 console）。声明后带子协议 Dial 应 101
// 且回显 "guacamole"；裸客户端（无子协议）不受影响仍可升级。
func TestRdpWSSubprotocolNegotiation(t *testing.T) {
	addr, connect := startGuacdStub(t)
	env := newRdpTestEnv(t, addr)
	env.connect = connect
	winID := env.createServer(t, "windows", store.AuthMethodPassword, true)
	_, sessionID := env.createSession(t, winID, `{"server_id":"`+winID+`"}`)

	server := httptest.NewServer(env.router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/rdp/sessions/" + sessionID + "/ws"

	// 带 guacamole 子协议（真实 WebSocketTunnel 行为）→ 101 + 回显。
	header := http.Header{}
	header.Set("Origin", testOrigin)
	header.Set("Cookie", sessionCookieName+"="+env.cookie)
	header.Set("Sec-WebSocket-Protocol", rdpWSSubprotocol)
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		body := ""
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
		}
		t.Fatalf("guacamole subprotocol dial: %v resp=%v body=%s", err, resp, body)
	}
	defer ws.Close()
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != rdpWSSubprotocol {
		t.Fatalf("negotiated subprotocol = %q, want %q", got, rdpWSSubprotocol)
	}
	// 协商后链路仍通：guacd stub 握手后主动发的 sync 指令原样到达，
	// 且为文本帧（guacamole-common-js 的 onmessage 按字符串解析，
	// 二进制帧在首帧即抛 TypeError——p2 sprint 3 修复点）。
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read sync: %v", err)
	}
	if mt != websocket.TextMessage || string(data) != "4.sync,1.0;" {
		t.Fatalf("sync frame: mt=%d data=%q", mt, data)
	}
	_ = ws.Close()

	// 裸客户端（无子协议，如 curl/既有测试）仍可升级（gorilla 只在
	// 客户端提供子协议列表时协商回显）——用不带子协议的全新 header。
	bareHeader := http.Header{}
	bareHeader.Set("Origin", testOrigin)
	bareHeader.Set("Cookie", sessionCookieName+"="+env.cookie)
	ws2, resp2, err := websocket.DefaultDialer.Dial(wsURL, bareHeader)
	if err != nil {
		t.Fatalf("bare dial: %v", err)
	}
	defer ws2.Close()
	if got := resp2.Header.Get("Sec-WebSocket-Protocol"); got != "" {
		t.Fatalf("bare dial negotiated subprotocol = %q, want none", got)
	}
}

// ---------------------------------------------------------------------------
// 端到端：gorilla WS 客户端 ↔ httpapi 适配器 ↔ 假 guacd（文本帧透传）
// ---------------------------------------------------------------------------

func TestRdpWSEndToEndPassthrough(t *testing.T) {
	addr, connect := startGuacdStub(t)
	env := newRdpTestEnv(t, addr)
	env.connect = connect
	winID := env.createServer(t, "windows", store.AuthMethodPassword, true)
	_, sessionID := env.createSession(t, winID, `{"server_id":"`+winID+`"}`)

	server := httptest.NewServer(env.router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/rdp/sessions/" + sessionID + "/ws"

	header := http.Header{}
	header.Set("Origin", testOrigin)
	header.Set("Cookie", sessionCookieName+"="+env.cookie)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))

	// guacd → WS：握手后 stub 主动发的 sync 指令原样到达（文本帧——
	// 官方客户端按 string 解析，二进制帧即 TypeError，p2 sprint 3 修复点）。
	mt, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read sync: %v", err)
	}
	if mt != websocket.TextMessage || string(data) != "4.sync,1.0;" {
		t.Fatalf("sync frame: mt=%d data=%q", mt, data)
	}

	// WS → guacd →（echo）→ WS：key 指令往返。上行也按真实客户端行为
	// 发文本帧（浏览器 WebSocket.send(string) 即文本帧；服务端 ReadMessage
	// 不区分帧类型）。
	if err := ws.WriteMessage(websocket.TextMessage, []byte("3.key,1.65,1.1;")); err != nil {
		t.Fatalf("write key: %v", err)
	}
	mt, data, err = ws.ReadMessage()
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if mt != websocket.TextMessage || string(data) != "3.key,1.65,1.1;" {
		t.Fatalf("echo frame: mt=%d data=%q", mt, data)
	}

	// 假 guacd 收到的 connect 参数含解密后的密码；WS 帧不含密码。
	select {
	case params := <-connect:
		if params["password"] != "rdp-secret" {
			t.Fatalf("connect password = %q", params["password"])
		}
		if params["hostname"] != "127.0.0.1" || params["recording-name"] != sessionID {
			t.Fatalf("connect params = %v", params)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guacd stub did not observe connect")
	}

	// 客户端断开 → 会话行 closed(client_closed)。
	_ = ws.Close()
	tbl, _ := memRdpTables.LoadOrStore(env.st, &memRdpTable{rows: map[string]*store.RdpSession{}})
	table := tbl.(*memRdpTable)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if row, ok := table.get(sessionID); ok && row.Status == store.SessionClosed && row.EndedReason == "client_closed" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, _ := table.get(sessionID)
	t.Fatalf("session not closed after client disconnect: %+v", row)
}
