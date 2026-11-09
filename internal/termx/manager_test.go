// W3：终端会话测试（契约 9.2：录制器单测 + x/crypto/ssh server 侧
// PTY 全链路回环 + goleak）。
package termx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/goleak"
	"golang.org/x/crypto/ssh"

	"aiops/internal/audit"
	"aiops/internal/sshx"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// 测试用 SSH 服务器（固定主机密钥、任意凭证接受）
// ---------------------------------------------------------------------------

// testSSHServer 是本地 SSH 服务：接受任意密码，对 session 通道实现
// pty-req / shell / window-change；shell 行为为回显输入（echo），
// resize 时回写提示标记，便于断言。
type testSSHServer struct {
	listener net.Listener
	config   *ssh.ServerConfig

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

func newTestSSHServer(t *testing.T) *testSSHServer {
	t.Helper()
	// 固定主机密钥：每次测试进程生成一次并保持不变（TOFU 首连接受）。
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	config := &ssh.ServerConfig{
		// 任意凭证接受（仅测试回环）。
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	config.AddHostKey(signer)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &testSSHServer{listener: l, config: config, conns: make(map[net.Conn]struct{})}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(s.close)
	return s
}

func (s *testSSHServer) addr() (host string, port int) {
	host, portStr, _ := net.SplitHostPort(s.listener.Addr().String())
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

func (s *testSSHServer) close() {
	_ = s.listener.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *testSSHServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
			_ = conn.Close()
		}()
	}
}

func (s *testSSHServer) handleConn(conn net.Conn) {
	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(ch, chReqs)
	}
}

// handleSession 实现最小 PTY 语义：
//   - pty-req / shell：Reply(true)，shell 后写 banner 并回显输入
//   - window-change：Reply(true) 并写 "[resized COLSxROWS]"
func (s *testSSHServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	shellStarted := false
	var echoWG sync.WaitGroup
	defer echoWG.Wait()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "shell":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			if !shellStarted {
				shellStarted = true
				_, _ = ch.Write([]byte("READY\r\n"))
				echoWG.Add(1)
				go func() {
					defer echoWG.Done()
					_, _ = io.Copy(ch, ch) // 回显
				}()
			}
		case "window-change":
			// payload: term width, term height, pixel width, pixel height。
			var winch struct {
				W, H, PW, PH uint32
			}
			if err := ssh.Unmarshal(req.Payload, &winch); err == nil {
				_, _ = fmt.Fprintf(ch, "[resized %d x %d]", winch.W, winch.H)
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeTerminalStore 是 TerminalSessionStore 的内存实现。
type fakeTerminalStore struct {
	mu   sync.Mutex
	rows map[string]*store.TerminalSession
}

func newFakeTerminalStore() *fakeTerminalStore {
	return &fakeTerminalStore{rows: make(map[string]*store.TerminalSession)}
}

func (f *fakeTerminalStore) Create(_ context.Context, p store.CreateTerminalSessionParams) (store.TerminalSession, error) {
	cols, rows := p.Cols, p.Rows
	if cols == 0 {
		cols = 120
	}
	if rows == 0 {
		rows = 40
	}
	now := p.StartedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s := &store.TerminalSession{
		ID:             uuid.NewString(),
		ServerID:       p.ServerID,
		ServerName:     "test-server",
		ServerOS:       store.ServerOSLinux,
		Username:       "admin",
		Status:         store.SessionActive,
		Cols:           cols,
		Rows:           rows,
		StartedAt:      now,
		LastActivityAt: now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	f.mu.Lock()
	f.rows[s.ID] = s
	f.mu.Unlock()
	return *s, nil
}

func (f *fakeTerminalStore) Get(_ context.Context, id string) (store.TerminalSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.rows[id]
	if !ok {
		return store.TerminalSession{}, store.ErrNotFound
	}
	return *s, nil
}

func (f *fakeTerminalStore) List(_ context.Context, flt store.InteractiveSessionFilter) ([]store.TerminalSession, error) {
	limit := flt.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	items := make([]store.TerminalSession, 0, len(f.rows))
	for _, s := range f.rows {
		if flt.Status != "" && s.Status != flt.Status {
			continue
		}
		if flt.ServerID != "" && s.ServerID != flt.ServerID {
			continue
		}
		items = append(items, *s)
	}
	// created_at DESC, id DESC 近似：倒序插入即可（测试规模小）。
	for i := len(items)/2 - 1; i >= 0; i-- {
		j := len(items) - 1 - i
		items[i], items[j] = items[j], items[i]
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (f *fakeTerminalStore) TouchActivity(_ context.Context, id string, at time.Time, bytes int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.rows[id]
	if !ok || s.Status != store.SessionActive {
		return nil
	}
	s.LastActivityAt = at
	s.BytesWritten = bytes
	s.UpdatedAt = at
	return nil
}

func (f *fakeTerminalStore) Close(_ context.Context, p store.CloseInteractiveSessionParams) (store.TerminalSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.rows[p.ID]
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

func (f *fakeTerminalStore) CountActive(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, s := range f.rows {
		if s.Status == store.SessionActive {
			n++
		}
	}
	return n, nil
}

func (f *fakeTerminalStore) CountActiveByServer(_ context.Context, serverID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, s := range f.rows {
		if s.Status == store.SessionActive && s.ServerID == serverID {
			n++
		}
	}
	return n, nil
}

func (f *fakeTerminalStore) get(id string) (store.TerminalSession, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.rows[id]
	return *s, ok
}

// fakeAuditStore 收集审计事件。
type fakeAuditStore struct {
	mu     sync.Mutex
	events []store.CreateAuditParams
	lastID int64
}

func newFakeAuditStore() *fakeAuditStore { return &fakeAuditStore{} }

func (f *fakeAuditStore) Append(_ context.Context, p store.CreateAuditParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastID++
	f.events = append(f.events, p)
	return nil
}

func (f *fakeAuditStore) List(context.Context, store.AuditFilter) ([]store.AuditLog, bool, error) {
	return nil, false, nil
}

func (f *fakeAuditStore) LastHash(context.Context) (string, error) {
	return strings.Repeat("0", 64), nil
}

func (f *fakeAuditStore) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.events))
	for _, e := range f.events {
		out = append(out, e.Action)
	}
	return out
}

func (f *fakeAuditStore) find(action string) (store.CreateAuditParams, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.Action == action {
			return e, true
		}
	}
	return store.CreateAuditParams{}, false
}

// detailOf 解出审计 detail（json.RawMessage → map）。
func detailOf(t *testing.T, p store.CreateAuditParams) map[string]any {
	t.Helper()
	m := make(map[string]any)
	if len(p.Detail) > 0 {
		if err := json.Unmarshal(p.Detail, &m); err != nil {
			t.Fatalf("unmarshal audit detail: %v", err)
		}
	}
	return m
}

// fakeStore 组装 Manager 需要的最小 store 面。
type fakeStore struct {
	store.Store // 嵌入 nil：未实现的方法一律不调用
	term        *fakeTerminalStore
	auditLog    *fakeAuditStore
}

func (f *fakeStore) TerminalSessions() store.TerminalSessionStore { return f.term }
func (f *fakeStore) AuditLogs() store.AuditStore                  { return f.auditLog }
func (f *fakeStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return fn(f)
}

// fakeConn 实现 termx.Conn：in/out 双向 JSON 帧通道。
type fakeConn struct {
	in        chan []byte // 客户端→服务端（ReadJSON 消费）
	out       chan []byte // 服务端→客户端（WriteJSON 投递）
	closeOnce sync.Once
	closed    chan struct{}
	readLimit int64
}

func newFakeConn() *fakeConn {
	return &fakeConn{
		in:     make(chan []byte, 16),
		out:    make(chan []byte, 256),
		closed: make(chan struct{}),
	}
}

func (f *fakeConn) ReadJSON(v any) error {
	select {
	case b := <-f.in:
		return json.Unmarshal(b, v)
	case <-f.closed:
		return errors.New("fakeConn: closed")
	}
}

func (f *fakeConn) WriteJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	select {
	case f.out <- b:
		return nil
	case <-f.closed:
		return errors.New("fakeConn: closed")
	}
}

func (f *fakeConn) SetReadLimit(n int64) { f.readLimit = n }

func (f *fakeConn) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

// sendInput 投递一条客户端 input 帧。
func (f *fakeConn) sendInput(t *testing.T, data []byte) {
	t.Helper()
	sendClientFrame(t, f, map[string]any{
		"type": "input",
		"data": base64.StdEncoding.EncodeToString(data),
	})
}

func sendClientFrame(t *testing.T, f *fakeConn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal client frame: %v", err)
	}
	select {
	case f.in <- b:
	case <-time.After(2 * time.Second):
		t.Fatal("send client frame timed out")
	}
}

// nextFrame 读取一条服务端帧（带超时）。
func (f *fakeConn) nextFrame(t *testing.T) map[string]any {
	t.Helper()
	select {
	case b := <-f.out:
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal server frame %q: %v", b, err)
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("waiting for server frame timed out")
		return nil
	}
}

// collectOutput 持续读取帧直到 cond 为真或超时；返回累计的 output data。
func (f *fakeConn) collectOutput(t *testing.T, cond func(string) bool) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var sb strings.Builder
	for {
		select {
		case b := <-f.out:
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("unmarshal frame %q: %v", b, err)
			}
			if m["type"] == "output" {
				data, _ := base64.StdEncoding.DecodeString(fmt.Sprint(m["data"]))
				sb.Write(data)
				if cond(sb.String()) {
					return sb.String()
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for output; got so far: %q", sb.String())
		}
	}
}

// waitForRow 轮询会话行直至 closed 且 reason 匹配。
func waitForRow(t *testing.T, ts *fakeTerminalStore, id, reason string) store.TerminalSession {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if row, ok := ts.get(id); ok && row.Status == store.SessionClosed && row.EndedReason == reason {
			return row
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, _ := ts.get(id)
	t.Fatalf("session row did not close with reason %q; got %+v", reason, row)
	return store.TerminalSession{}
}

// newTestManager 组装 SSH 池 + Manager + 审计 writer 的测试夹具。
type testFixture struct {
	manager *Manager
	store   *fakeStore
	term    *fakeTerminalStore
	auditW  *audit.Writer
	auditL  *fakeAuditStore
	pool    *sshx.Pool
	server  *testSSHServer
	dir     string
	host    string
	port    int
}

func newTestFixture(t *testing.T, cfg Config) *testFixture {
	t.Helper()
	// goleak 最先注册（Cleanup LIFO：最后执行），此时 fixture 的
	// Shutdown/Close 均已完成。
	t.Cleanup(func() { goleak.VerifyNone(t) })
	srv := newTestSSHServer(t)
	host, port := srv.addr()
	pool := sshx.New(sshx.Config{}, slog.New(slog.DiscardHandler))
	ts := newFakeTerminalStore()
	al := newFakeAuditStore()
	st := &fakeStore{term: ts, auditLog: al}
	writer := audit.NewWriter(st, slog.New(slog.DiscardHandler), nil)
	writer.Start(context.Background())
	cfg.RecordingsDir = t.TempDir()
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 10 * time.Minute
	}
	if cfg.MaxDuration == 0 {
		cfg.MaxDuration = time.Hour
	}
	m := NewManager(cfg, st, pool, writer, nil, slog.New(slog.DiscardHandler))
	f := &testFixture{
		manager: m,
		store:   st,
		term:    ts,
		auditW:  writer,
		auditL:  al,
		pool:    pool,
		server:  srv,
		dir:     cfg.RecordingsDir,
	}
	f.host, f.port = host, port
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Shutdown(ctx)
		_ = writer.Close(ctx)
		pool.Close()
	})
	return f
}

// ---------------------------------------------------------------------------
// 录制器单元测试
// ---------------------------------------------------------------------------

func TestRecorderWritesValidAsciinemaV2(t *testing.T) {
	defer goleak.VerifyNone(t)
	dir := t.TempDir()
	start := time.Now()

	rec, err := NewRecorder(dir, "0f0e0d0c-0b0a-4909-8807-060504030201", 132, 43, start)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	rec.Output(start.Add(100*time.Millisecond), []byte("hello"))
	rec.Resize(start.Add(250*time.Millisecond), 100, 30)
	rec.Output(start.Add(500*time.Millisecond), []byte(" world"))
	if !rec.ShouldFlush() {
		// 3 个事件未达 64 阈值，正常。
		t.Log("below flush threshold, ok")
	}
	bytes, err := rec.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if bytes <= 0 {
		t.Fatalf("bytes written must be positive, got %d", bytes)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "0f0e0d0c-0b0a-4909-8807-060504030201.cast"))
	if err != nil {
		t.Fatalf("read cast: %v", err)
	}
	if int64(len(raw)) != bytes {
		t.Fatalf("bytes(%d) != file size(%d)", bytes, len(raw))
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expect header + 3 events, got %d lines: %q", len(lines), raw)
	}

	// 首行 header：asciinema v2。
	var header struct {
		Version int `json:"version"`
		Width   int `json:"width"`
		Height  int `json:"height"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("header not valid JSON: %v", err)
	}
	if header.Version != 2 || header.Width != 132 || header.Height != 43 {
		t.Fatalf("unexpected header: %+v", header)
	}

	// 事件行：[elapsed, kind, data]；时间单调递增。
	prev := -1.0
	expect := []struct {
		kind string
		data string
	}{
		{"o", "hello"},
		{"r", "100x30"},
		{"o", " world"},
	}
	for i, want := range expect {
		var ev []any
		if err := json.Unmarshal([]byte(lines[i+1]), &ev); err != nil {
			t.Fatalf("event line %d not valid JSON: %v", i+1, err)
		}
		if len(ev) != 3 {
			t.Fatalf("event line %d must have 3 elements: %q", i+1, lines[i+1])
		}
		dt, ok := ev[0].(float64)
		if !ok || dt < 0 {
			t.Fatalf("event %d timestamp must be non-negative float: %q", i+1, lines[i+1])
		}
		if dt < prev {
			t.Fatalf("event %d timestamp %.6f < previous %.6f", i+1, dt, prev)
		}
		prev = dt
		if fmt.Sprint(ev[1]) != want.kind {
			t.Fatalf("event %d kind = %v, want %s", i+1, ev[1], want.kind)
		}
		if fmt.Sprint(ev[2]) != want.data {
			t.Fatalf("event %d data = %v, want %q", i+1, ev[2], want.data)
		}
	}
	// 100ms 的 o 事件时间戳应在 [0.09, 0.11] 区间（UnixNano 差值精度）。
	var first []any
	_ = json.Unmarshal([]byte(lines[1]), &first)
	if dt, _ := first[0].(float64); dt < 0.09 || dt > 0.11 {
		t.Fatalf("first event timestamp %.6f not ~0.1s", dt)
	}
}

func TestRecorderFailsClosedWhenDirMissing(t *testing.T) {
	defer goleak.VerifyNone(t)
	// 不可写路径：录制器创建失败（Manager 侧降级为不录制）。
	if _, err := NewRecorder("/proc/definitely-not-writable", "x", 80, 24, time.Now()); err == nil {
		t.Fatal("expected error for unwritable recordings dir")
	}
}

// ---------------------------------------------------------------------------
// 输入摘要单元测试
// ---------------------------------------------------------------------------

func TestInputSummaryLinesAndSanitization(t *testing.T) {
	defer goleak.VerifyNone(t)
	var s inputSummary
	s.add([]byte("ls -la\r\nrm -rf /\r"))
	lines, truncated, count := s.snapshot()
	if truncated {
		t.Fatal("should not be truncated")
	}
	if count != 2 || len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", count, lines)
	}
	if lines[0] != "ls -la" || lines[1] != "rm -rf /" {
		t.Fatalf("unexpected lines: %q", lines)
	}

	// 控制序列（ANSI 转义）整段替换为单个 \ufffd；BEL 单独替换；原文不落摘要。
	var s2 inputSummary
	s2.add([]byte("\x1b[2Jclear\x07"))
	lines2, _, _ := s2.snapshot()
	want := "\ufffdclear\ufffd"
	if lines2[0] != want {
		t.Fatalf("control chars not sanitized: %q", lines2[0])
	}

	// 非法 UTF-8 字节替换为 \ufffd。
	var s4 inputSummary
	s4.add([]byte{0xff, 0xfe, 'x'})
	lines4, _, _ := s4.snapshot()
	if lines4[0] != "\ufffd\ufffdx" {
		t.Fatalf("invalid utf8 not replaced: %q", lines4[0])
	}
}

func TestInputSummaryTruncation(t *testing.T) {
	defer goleak.VerifyNone(t)
	var s inputSummary
	// 1001 行 → truncated。
	for i := 0; i < 1001; i++ {
		s.add([]byte("line\r"))
	}
	lines, truncated, count := s.snapshot()
	if !truncated {
		t.Fatal("expected truncated")
	}
	if count != 1000 || len(lines) != 1000 {
		t.Fatalf("want 1000 lines, got %d", count)
	}
	// 单行 512 字节截断。
	var s3 inputSummary
	s3.add([]byte(strings.Repeat("a", 600)))
	lines3, _, _ := s3.snapshot()
	if len(lines3[0]) != 512 {
		t.Fatalf("line not truncated to 512 bytes: %d", len(lines3[0]))
	}
}

// TestInputSummaryPerKeystrokeFrames 回归逐字符成行：浏览器 xterm 每次
// 按键发一帧，同一条命令不得被拆成单字符行（真链路实测曾把
// "whoami /priv" 记成 12 个单字符行）。
func TestInputSummaryPerKeystrokeFrames(t *testing.T) {
	defer goleak.VerifyNone(t)
	var s inputSummary
	for _, c := range []byte("whoami /priv") {
		s.add([]byte{c})
	}
	lines, truncated, count := s.snapshot()
	if truncated || count != 1 || len(lines) != 1 {
		t.Fatalf("want 1 line not truncated, got count=%d truncated=%v lines=%q", count, truncated, lines)
	}
	if lines[0] != "whoami /priv" {
		t.Fatalf("lines[0] = %q, want %q", lines[0], "whoami /priv")
	}
	// 回车终结 pending：后续输入开新行，不并入上一条命令。
	s.add([]byte("\r"))
	s.add([]byte("id"))
	lines, _, count = s.snapshot()
	if count != 2 || len(lines) != 2 || lines[0] != "whoami /priv" || lines[1] != "id" {
		t.Fatalf("unexpected lines after enter: count=%d lines=%q", count, lines)
	}
	// 跨帧拼接后按回车切行：帧边界与行边界解耦。
	var s2 inputSummary
	s2.add([]byte("ec"))
	s2.add([]byte("ho hi"))
	s2.add([]byte("\r\nw"))
	s2.add([]byte("c\r"))
	lines2, _, count2 := s2.snapshot()
	if count2 != 2 || len(lines2) != 2 || lines2[0] != "echo hi" || lines2[1] != "wc" {
		t.Fatalf("unexpected lines across frames: count=%d lines=%q", count2, lines2)
	}
	// 超长无换行输入达 pending 上限即切分 flush，缓冲不随输入无界增长。
	var s3 inputSummary
	s3.add(bytes.Repeat([]byte("b"), summaryMaxPendingBytes+3))
	flushed, _, count3 := s3.snapshot()
	if count3 != 1 || len(flushed[0]) != summaryMaxLineBytes {
		t.Fatalf("pending flush at cap: count=%d first line len=%d", count3, len(flushed[0]))
	}
	// flush 后缓冲清空：继续输入从新行开始。
	s3.add([]byte("tail"))
	lines3, _, _ := s3.snapshot()
	if len(lines3) != 2 || lines3[1] != "tail" {
		t.Fatalf("after cap flush lines = %q, want second line tail", lines3)
	}
}

// ---------------------------------------------------------------------------
// Open 治理单元测试（无 PTY 拨号路径）
// ---------------------------------------------------------------------------

func TestOpenRejectsWinRMAuth(t *testing.T) {
	f := newTestFixture(t, Config{MaxSessions: 2})
	_, err := f.manager.Open(context.Background(), store.Server{
		ID:         uuid.NewString(),
		OS:         store.ServerOSWindows,
		AuthMethod: store.AuthMethodPasswordNTLM,
	}, "user-1", 120, 40)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestOpenServerSingleActiveSession(t *testing.T) {
	f := newTestFixture(t, Config{MaxSessions: 4})
	srv := store.Server{ID: uuid.NewString(), OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	if _, err := f.manager.Open(context.Background(), srv, "user-1", 0, 0); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := f.manager.Open(context.Background(), srv, "user-1", 0, 0); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("second open on same server: want ErrSessionExists, got %v", err)
	}
}

func TestOpenGlobalSessionLimit(t *testing.T) {
	f := newTestFixture(t, Config{MaxSessions: 1})
	s1 := store.Server{ID: uuid.NewString(), OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	s2 := store.Server{ID: uuid.NewString(), OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	if _, err := f.manager.Open(context.Background(), s1, "user-1", 0, 0); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := f.manager.Open(context.Background(), s2, "user-1", 0, 0); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("second open: want ErrSessionLimit, got %v", err)
	}
}

func TestOpenWritesSessionStartAudit(t *testing.T) {
	f := newTestFixture(t, Config{MaxSessions: 2})
	srv := store.Server{
		ID:         uuid.NewString(),
		Name:       "web-1",
		OS:         store.ServerOSLinux,
		AuthMethod: store.AuthMethodPassword,
	}
	s, err := f.manager.Open(context.Background(), srv, "user-9", 100, 30)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.auditW.Flush(ctx); err != nil {
		t.Fatalf("audit flush: %v", err)
	}
	ev, ok := f.auditL.find("session.start")
	if !ok {
		t.Fatalf("session.start not written; actions=%v", f.auditL.actions())
	}
	if ev.ActorType != "user" || ev.ActorID != "user-9" || ev.ResourceType != "terminal_session" || ev.ResourceID != s.ID {
		t.Fatalf("unexpected session.start event: %+v", ev)
	}
	detail := detailOf(t, ev)
	if detail["server_name"] != "web-1" || detail["os"] != "linux" {
		t.Fatalf("unexpected session.start detail: %v", detail)
	}
}

func TestAttachRejectsWrongUserAndUnknownSession(t *testing.T) {
	f := newTestFixture(t, Config{MaxSessions: 2})
	srv := store.Server{ID: uuid.NewString(), OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	s, err := f.manager.Open(context.Background(), srv, "user-1", 0, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn := newFakeConn()
	defer conn.Close()
	if err := f.manager.Attach(conn, s.ID, "user-2", "req-1", "127.0.0.1", f.sshTarget(srv)); !errors.Is(err, ErrNotActive) {
		t.Fatalf("attach as other user: want ErrNotActive, got %v", err)
	}
	if err := f.manager.Attach(conn, uuid.NewString(), "user-1", "req-1", "127.0.0.1", f.sshTarget(srv)); !errors.Is(err, ErrNotActive) {
		t.Fatalf("attach unknown session: want ErrNotActive, got %v", err)
	}
	// 重复 Attach 同一会话（首个成功后）：409 语义。
	if err := f.manager.Attach(conn, s.ID, "user-1", "req-1", "127.0.0.1", f.sshTarget(srv)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitReady(t, conn)
	if err := f.manager.Attach(newFakeConn(), s.ID, "user-1", "req-2", "127.0.0.1", f.sshTarget(srv)); !errors.Is(err, ErrNotActive) {
		t.Fatalf("second attach: want ErrNotActive, got %v", err)
	}
	conn.Close()
	waitForRow(t, f.term, s.ID, reasonClientClosed)
}

// sshTarget 构造指向测试 SSH server 的连接目标。
func (f *testFixture) sshTarget(srv store.Server) sshx.Target {
	return sshx.Target{
		ServerID:   srv.ID,
		Host:       f.host,
		Port:       f.port,
		Username:   "tester",
		AuthMethod: store.AuthMethodPassword,
		Password:   "any",
		// KnownHostKey 为空 → TOFU 首连接受（测试 server 固定主机密钥）。
	}
}

// waitOpen 等待 PTY 建立（测试 server 的 READY banner）。
func waitReady(t *testing.T, conn *fakeConn) {
	t.Helper()
	conn.collectOutput(t, func(s string) bool { return strings.Contains(s, "READY") })
}

// ---------------------------------------------------------------------------
// SSH PTY 全链路回环（连接、输入、输出、resize、断开、录制解析）
// ---------------------------------------------------------------------------

func TestTerminalEndToEnd(t *testing.T) {
	f := newTestFixture(t, Config{MaxSessions: 2})
	srv := store.Server{
		ID:         uuid.NewString(),
		Name:       "loop-host",
		OS:         store.ServerOSLinux,
		AuthMethod: store.AuthMethodPassword,
	}
	userID := "user-e2e"
	s, err := f.manager.Open(context.Background(), srv, userID, 100, 30)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if s.Cols != 100 || s.Rows != 30 {
		t.Fatalf("unexpected initial size: %dx%d", s.Cols, s.Rows)
	}

	conn := newFakeConn()
	if err := f.manager.Attach(conn, s.ID, userID, "req-e2e", "127.0.0.1", f.sshTarget(srv)); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// PTY 建立后 server 发 READY banner。
	waitReady(t, conn)

	// 输入 → PTY → 回显输出。
	conn.sendInput(t, []byte("echo ping\r"))
	out := conn.collectOutput(t, func(s string) bool { return strings.Contains(s, "echo ping") })
	if !strings.Contains(out, "echo ping") {
		t.Fatalf("echo not received: %q", out)
	}

	// resize → window-change 透传 + resized 确认帧。
	sendClientFrame(t, conn, map[string]any{"type": "resize", "cols": 200, "rows": 50})
	resizedSeen := false
	resizeMarkSeen := false
	deadline := time.After(5 * time.Second)
	for !(resizedSeen && resizeMarkSeen) {
		select {
		case b := <-conn.out:
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("unmarshal frame: %v", err)
			}
			switch m["type"] {
			case "resized":
				if m["cols"] != float64(200) || m["rows"] != float64(50) {
					t.Fatalf("unexpected resized frame: %v", m)
				}
				resizedSeen = true
			case "output":
				data, _ := base64.StdEncoding.DecodeString(fmt.Sprint(m["data"]))
				if strings.Contains(string(data), "[resized 200 x 50]") {
					resizeMarkSeen = true
				}
			}
		case <-deadline:
			t.Fatalf("timed out; resized=%v mark=%v", resizedSeen, resizeMarkSeen)
		}
	}

	// 等治理循环落库：TouchActivity 节流 1 次/5s，轮询等待其发生。
	touchDeadline := time.Now().Add(10 * time.Second)
	for {
		row, ok := f.term.get(s.ID)
		if !ok || row.Status != store.SessionActive {
			t.Fatalf("session should still be active: %+v", row)
		}
		if row.BytesWritten > 0 {
			break
		}
		if time.Now().After(touchDeadline) {
			t.Fatal("TouchActivity should have persisted bytes_written > 0")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 客户端断开 → client_closed + 会话行关闭 + 封盘 + 审计。
	conn.Close()
	closed := waitForRow(t, f.term, s.ID, reasonClientClosed)
	if closed.BytesWritten == 0 {
		t.Fatal("closed row must have bytes_written > 0")
	}
	if !closed.HasRecording {
		t.Fatal("closed row must have recording")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.auditW.Flush(ctx); err != nil {
		t.Fatalf("audit flush: %v", err)
	}
	for _, action := range []string{"session.end", "session.input_summary"} {
		if _, ok := f.auditL.find(action); !ok {
			t.Fatalf("%s not written; actions=%v", action, f.auditL.actions())
		}
	}
	end, _ := f.auditL.find("session.end")
	endDetail := detailOf(t, end)
	if endDetail["reason"] != reasonClientClosed {
		t.Fatalf("session.end reason: %v", endDetail)
	}
	if _, ok := endDetail["duration_ms"]; !ok {
		t.Fatal("session.end detail missing duration_ms")
	}
	if _, ok := endDetail["bytes_written"]; !ok {
		t.Fatal("session.end detail missing bytes_written")
	}
	sum, _ := f.auditL.find("session.input_summary")
	sumDetail := detailOf(t, sum)
	gotLines, ok := sumDetail["lines"].([]any)
	if !ok {
		t.Fatalf("input_summary detail lines not a list: %v", sumDetail)
	}
	// 单帧 "echo ping\r" → 恰一行完整命令（防逐字符成行回归）。
	if len(gotLines) != 1 || gotLines[0] != "echo ping" {
		t.Fatalf("input_summary lines = %v, want [echo ping]", gotLines)
	}

	// 录制文件可解析：header + o/r 事件齐全。
	raw, err := os.ReadFile(filepath.Join(f.dir, s.ID+".cast"))
	if err != nil {
		t.Fatalf("read cast: %v", err)
	}
	if int64(len(raw)) != closed.BytesWritten {
		t.Fatalf("bytes_written(%d) != cast size(%d)", closed.BytesWritten, len(raw))
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) < 4 {
		t.Fatalf("cast too short: %d lines", len(lines))
	}
	var header struct {
		Version int `json:"version"`
		Width   int `json:"width"`
		Height  int `json:"height"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil || header.Version != 2 || header.Width != 100 || header.Height != 30 {
		t.Fatalf("bad header: %q err=%v", lines[0], err)
	}
	kinds := map[string]bool{}
	for _, line := range lines[1:] {
		var ev []any
		if err := json.Unmarshal([]byte(line), &ev); err != nil || len(ev) != 3 {
			t.Fatalf("invalid event line: %q err=%v", line, err)
		}
		kinds[fmt.Sprint(ev[1])] = true
	}
	if !kinds["o"] || !kinds["r"] {
		t.Fatalf("cast missing o/r events: %v", kinds)
	}
}

func TestTerminalIdleTimeout(t *testing.T) {
	f := newTestFixture(t, Config{MaxSessions: 2, IdleTimeout: 300 * time.Millisecond})
	srv := store.Server{ID: uuid.NewString(), OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	s, err := f.manager.Open(context.Background(), srv, "user-idle", 0, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn := newFakeConn()
	defer conn.Close()
	if err := f.manager.Attach(conn, s.ID, "user-idle", "req-idle", "127.0.0.1", f.sshTarget(srv)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitReady(t, conn) // banner 刷新活动；此后静默
	// 空闲 300ms + 治理 tick 1s 内结束。
	waitForRow(t, f.term, s.ID, reasonIdleTimeout)
	// 收到 closed 帧后连接关闭。
	deadline := time.After(3 * time.Second)
	for {
		select {
		case b := <-conn.out:
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			if m["type"] == "closed" && m["reason"] == reasonIdleTimeout {
				return
			}
		case <-deadline:
			t.Fatal("closed frame with idle_timeout not received")
		}
	}
}

func TestTerminalMaxDuration(t *testing.T) {
	f := newTestFixture(t, Config{MaxSessions: 2, MaxDuration: 300 * time.Millisecond})
	srv := store.Server{ID: uuid.NewString(), OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	s, err := f.manager.Open(context.Background(), srv, "user-max", 0, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn := newFakeConn()
	defer conn.Close()
	if err := f.manager.Attach(conn, s.ID, "user-max", "req-max", "127.0.0.1", f.sshTarget(srv)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitReady(t, conn)
	waitForRow(t, f.term, s.ID, reasonMaxDuration)
}

func TestTerminalOversizedInputFrame(t *testing.T) {
	f := newTestFixture(t, Config{MaxSessions: 2})
	srv := store.Server{ID: uuid.NewString(), OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	s, err := f.manager.Open(context.Background(), srv, "user-big", 0, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn := newFakeConn()
	defer conn.Close()
	if err := f.manager.Attach(conn, s.ID, "user-big", "req-big", "127.0.0.1", f.sshTarget(srv)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitReady(t, conn)

	// 9KB 原始输入（>8KB 上限）：error 帧 COMMAND_TOO_LARGE + closed(error)。
	conn.sendInput(t, make([]byte, 9*1024))
	sawErr := false
	deadline := time.After(5 * time.Second)
	for !sawErr {
		select {
		case b := <-conn.out:
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if m["type"] == "error" && m["code"] == "COMMAND_TOO_LARGE" {
				sawErr = true
			}
		case <-deadline:
			t.Fatal("COMMAND_TOO_LARGE error frame not received")
		}
	}
	waitForRow(t, f.term, s.ID, reasonError)
}

func TestTerminalInvalidResizeKeepsSession(t *testing.T) {
	f := newTestFixture(t, Config{MaxSessions: 2})
	srv := store.Server{ID: uuid.NewString(), OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	s, err := f.manager.Open(context.Background(), srv, "user-rz", 0, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn := newFakeConn()
	defer conn.Close()
	if err := f.manager.Attach(conn, s.ID, "user-rz", "req-rz", "127.0.0.1", f.sshTarget(srv)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitReady(t, conn)

	// 越界 resize（cols=5）：VALIDATION_FAILED 错误帧，会话保持。
	sendClientFrame(t, conn, map[string]any{"type": "resize", "cols": 5, "rows": 30})
	sawErr := false
	deadline := time.After(5 * time.Second)
	for !sawErr {
		select {
		case b := <-conn.out:
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if m["type"] == "error" && m["code"] == "VALIDATION_FAILED" {
				sawErr = true
			}
		case <-deadline:
			t.Fatal("VALIDATION_FAILED error frame not received")
		}
	}
	time.Sleep(300 * time.Millisecond)
	if row, ok := f.term.get(s.ID); !ok || row.Status != store.SessionActive {
		t.Fatalf("session must stay active after invalid resize: %+v", row)
	}
	conn.Close()
	waitForRow(t, f.term, s.ID, reasonClientClosed)
}

func TestCloseByServerEndsSessions(t *testing.T) {
	f := newTestFixture(t, Config{MaxSessions: 2})
	srv := store.Server{ID: uuid.NewString(), OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	s, err := f.manager.Open(context.Background(), srv, "user-cbs", 0, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn := newFakeConn()
	defer conn.Close()
	if err := f.manager.Attach(conn, s.ID, "user-cbs", "req-cbs", "127.0.0.1", f.sshTarget(srv)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitReady(t, conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.manager.CloseByServer(ctx, srv.ID, reasonError)
	waitForRow(t, f.term, s.ID, reasonError)
}

func TestShutdownEndsSessions(t *testing.T) {
	f := newTestFixture(t, Config{MaxSessions: 2})
	srv := store.Server{ID: uuid.NewString(), OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword}
	s, err := f.manager.Open(context.Background(), srv, "user-sd", 0, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn := newFakeConn()
	defer conn.Close()
	if err := f.manager.Attach(conn, s.ID, "user-sd", "req-sd", "127.0.0.1", f.sshTarget(srv)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitReady(t, conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	f.manager.Shutdown(ctx)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("shutdown exceeded 5s budget: %v", elapsed)
	}
	waitForRow(t, f.term, s.ID, reasonServerShutdown)
}
