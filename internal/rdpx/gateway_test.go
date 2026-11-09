// W4：RDP 网关测试（契约 9.2 G21：fake guacd TCP listener 按
// select/args/size/connect 序列验证凭据注入——握手参数含解密密码、
// 日志/WS 帧/审计均不含密码；透传字节；guacd 断开时会话 closed；
// Open 错误哨兵；空闲/时长治理与 goleak）。
package rdpx

import (
	"bufio"
	"bytes"
	"context"
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

	"aiops/internal/audit"
	"aiops/internal/cryptox"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// 测试用 guacd stub：按契约 5.14 握手序列对话并记录原始指令
// ---------------------------------------------------------------------------

// fakeGuacd 是本地 TCP 假 guacd：校验 select/args/size/audio/video/image
// 序列，解析 connect 的 key:value 参数，随后进入 after 定制的行为
// （nil=静默保持连接直到对端断开）。
type fakeGuacd struct {
	t    *testing.T
	ln   net.Listener
	addr string

	mu           sync.Mutex
	instructions []string          // 收到的每条握手指令原文
	connect      map[string]string // connect 参数
	conns        map[net.Conn]struct{}
	wg           sync.WaitGroup
}

// newFakeGuacd 启动假 guacd；after 为 nil 时握手后仅排空对端数据。
func newFakeGuacd(t *testing.T, after func(conn net.Conn, r *bufio.Reader)) *fakeGuacd {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake guacd: %v", err)
	}
	g := &fakeGuacd{
		t:     t,
		ln:    ln,
		addr:  ln.Addr().String(),
		conns: make(map[net.Conn]struct{}),
	}
	t.Cleanup(g.close)
	g.wg.Add(1)
	go g.acceptLoop(after)
	return g
}

func (g *fakeGuacd) close() {
	_ = g.ln.Close()
	g.mu.Lock()
	for c := range g.conns {
		_ = c.Close()
	}
	g.mu.Unlock()
	g.wg.Wait()
}

func (g *fakeGuacd) acceptLoop(after func(net.Conn, *bufio.Reader)) {
	defer g.wg.Done()
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		g.mu.Lock()
		g.conns[conn] = struct{}{}
		g.mu.Unlock()
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			g.handle(conn, after)
			g.mu.Lock()
			delete(g.conns, conn)
			g.mu.Unlock()
			_ = conn.Close()
		}()
	}
}

func (g *fakeGuacd) record(raw []byte) {
	g.mu.Lock()
	g.instructions = append(g.instructions, string(raw)+";")
	g.mu.Unlock()
}

func (g *fakeGuacd) handle(conn net.Conn, after func(net.Conn, *bufio.Reader)) {
	r := bufio.NewReaderSize(conn, 64*1024)

	// 1. select → args（首参协议版本，其后参数名列表；契约 5.14）。
	raw, err := readInstruction(r)
	if err != nil {
		return
	}
	g.record(raw)
	if string(raw) != "6.select,3.rdp" {
		fmt.Fprintf(os.Stderr, "fake guacd: unexpected select: %q\n", raw)
		return
	}
	args := encodeInstruction("args", "1.5.0", "hostname", "port", "username",
		"password", "security", "ignore-cert", "width", "height", "dpi", "recording-name")
	if _, err := conn.Write(args); err != nil {
		return
	}

	// 2. size/audio/video/image 四连发。
	for i := 0; i < 4; i++ {
		raw, err = readInstruction(r)
		if err != nil {
			return
		}
		g.record(raw)
	}

	// 3. connect：解析 key:value 参数。
	raw, err = readInstruction(r)
	if err != nil {
		return
	}
	g.record(raw)
	opcode, elems, perr := parseInstruction(raw)
	if perr != nil || opcode != "connect" {
		fmt.Fprintf(os.Stderr, "fake guacd: bad connect: %q (%v)\n", raw, perr)
		return
	}
	params := make(map[string]string, len(elems))
	for _, e := range elems {
		if k, v, ok := strings.Cut(e, "="); ok {
			params[k] = v
		}
	}
	g.mu.Lock()
	g.connect = params
	g.mu.Unlock()

	if after != nil {
		after(conn, r)
		return
	}
	// 默认：静默保持连接，排空对端数据直到断开。
	_, _ = io.Copy(io.Discard, r)
}

func (g *fakeGuacd) getInstructions() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.instructions))
	copy(out, g.instructions)
	return out
}

func (g *fakeGuacd) getConnect() map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]string, len(g.connect))
	for k, v := range g.connect {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// 内存 store：rdp_sessions + servers + 审计
// ---------------------------------------------------------------------------

type fakeRdpStore struct {
	mu   sync.Mutex
	rows map[string]*store.RdpSession
}

func newFakeRdpStore() *fakeRdpStore {
	return &fakeRdpStore{rows: map[string]*store.RdpSession{}}
}

func (f *fakeRdpStore) Create(_ context.Context, p store.CreateRdpSessionParams) (store.RdpSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	s := store.RdpSession{
		ID:             uuid.NewString(),
		ServerID:       p.ServerID,
		ServerName:     "win-host",
		Width:          p.Width,
		Height:         p.Height,
		DPI:            p.DPI,
		Status:         store.SessionActive,
		StartedAt:      now,
		LastActivityAt: now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	f.rows[s.ID] = &s
	return s, nil
}

func (f *fakeRdpStore) Get(_ context.Context, id string) (store.RdpSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.rows[id]; ok {
		return *s, nil
	}
	return store.RdpSession{}, store.ErrNotFound
}

func (f *fakeRdpStore) List(_ context.Context, flt store.InteractiveSessionFilter) ([]store.RdpSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := make([]store.RdpSession, 0)
	for _, s := range f.rows {
		if flt.Status != "" && s.Status != flt.Status {
			continue
		}
		if flt.ServerID != "" && s.ServerID != flt.ServerID {
			continue
		}
		items = append(items, *s)
	}
	return items, nil
}

func (f *fakeRdpStore) TouchActivity(_ context.Context, id string, at time.Time, bytes int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.rows[id]; ok && s.Status == store.SessionActive {
		s.LastActivityAt = at
		s.BytesWritten = bytes
	}
	return nil
}

func (f *fakeRdpStore) Close(_ context.Context, p store.CloseInteractiveSessionParams) (store.RdpSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.rows[p.ID]
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
		s.RecordingFile = s.ID + guacFileSuffix
	}
	s.UpdatedAt = p.EndedAt
	return *s, nil
}

func (f *fakeRdpStore) CountActive(context.Context) (int64, error) {
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

func (f *fakeRdpStore) CountActiveByServer(_ context.Context, serverID string) (int64, error) {
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

func (f *fakeRdpStore) get(id string) (store.RdpSession, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.rows[id]
	return *s, ok
}

// fakeAuditStore 收集审计事件。
type fakeAuditStore struct {
	mu     sync.Mutex
	events []store.CreateAuditParams
}

func (f *fakeAuditStore) Append(_ context.Context, p store.CreateAuditParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, p)
	return nil
}

func (f *fakeAuditStore) List(context.Context, store.AuditFilter) ([]store.AuditLog, bool, error) {
	return nil, false, nil
}

func (f *fakeAuditStore) LastHash(context.Context) (string, error) {
	return strings.Repeat("0", 64), nil
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

func (f *fakeAuditStore) raw() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b bytes.Buffer
	for _, e := range f.events {
		b.WriteString(e.Action)
		b.WriteByte(' ')
		b.Write(e.Detail)
		b.WriteByte('\n')
	}
	return b.String()
}

// fakeServers 提供最小 ServerStore（仅 Get）。
type fakeServers struct {
	mu      sync.Mutex
	servers map[string]store.Server
}

func (f *fakeServers) Get(_ context.Context, id string) (store.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return store.Server{}, store.ErrNotFound
}

func (f *fakeServers) List(context.Context) ([]store.Server, error) { return nil, nil }

func (f *fakeServers) UpdateContainerRuntime(_ context.Context, _ string, _ store.ContainerRuntime, _ time.Time) (store.Server, error) {
	return store.Server{}, store.ErrNotFound
}
func (f *fakeServers) Create(context.Context, store.CreateServerParams) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}
func (f *fakeServers) Update(context.Context, string, store.UpdateServerParams) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}
func (f *fakeServers) Delete(context.Context, string) error { return errors.New("not implemented") }
func (f *fakeServers) UpdateHostKey(context.Context, string, string, string, time.Time) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}

// fakeStore 组装 Gateway 需要的最小 store 面（嵌入 nil：未实现的方法
// 一律不调用）。
type fakeStore struct {
	store.Store
	rdp     *fakeRdpStore
	servers *fakeServers
	auditL  *fakeAuditStore
}

func (f *fakeStore) RdpSessions() store.RdpSessionStore { return f.rdp }
func (f *fakeStore) Servers() store.ServerStore         { return f.servers }
func (f *fakeStore) AuditLogs() store.AuditStore        { return f.auditL }
func (f *fakeStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return fn(f)
}

// ---------------------------------------------------------------------------
// fake WS 连接：chan 双向缓冲 + 全量写出记录（凭据不外泄断言用）
// ---------------------------------------------------------------------------

type fakeWSConn struct {
	fromClient chan []byte // 客户端→服务端（Read 消费）
	toClient   chan []byte // 服务端→客户端（Write 投递）
	done       chan struct{}
	closeOnce  sync.Once

	mu      sync.Mutex
	written bytes.Buffer // 服务端写出的全部字节
}

func newFakeWSConn() *fakeWSConn {
	return &fakeWSConn{
		fromClient: make(chan []byte, 64),
		toClient:   make(chan []byte, 256),
		done:       make(chan struct{}),
	}
}

var errFakeWSClosed = errors.New("fake ws: closed")

func (f *fakeWSConn) Read(p []byte) (int, error) {
	var buf []byte
	select {
	case b := <-f.fromClient:
		buf = b
	case <-f.done:
		return 0, io.EOF
	}
	n := copy(p, buf)
	return n, nil
}

func (f *fakeWSConn) Write(p []byte) (int, error) {
	select {
	case <-f.done:
		return 0, errFakeWSClosed
	default:
	}
	f.mu.Lock()
	f.written.Write(p)
	f.mu.Unlock()
	cp := append([]byte(nil), p...)
	select {
	case f.toClient <- cp:
		return len(p), nil
	case <-f.done:
		return 0, errFakeWSClosed
	}
}

func (f *fakeWSConn) Close() error {
	f.closeOnce.Do(func() { close(f.done) })
	return nil
}

func (f *fakeWSConn) clientSend(t *testing.T, b []byte) {
	t.Helper()
	select {
	case f.fromClient <- b:
	case <-time.After(2 * time.Second):
		t.Fatal("client send timed out")
	}
}

// clientExpect 等待服务端写出累计内容包含 want。
func (f *fakeWSConn) clientExpect(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var got bytes.Buffer
	for {
		select {
		case b := <-f.toClient:
			got.Write(b)
			if bytes.Contains(got.Bytes(), []byte(want)) {
				return
			}
		case <-deadline:
			t.Fatalf("client did not receive %q; got %q", want, got.String())
		}
	}
}

func (f *fakeWSConn) writtenBytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.written.Bytes()...)
}

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

const (
	testPassword  = "s3cret-Pa55w0rd,with;specials"
	testSessionIP = "127.0.0.9"
)

type testFixture struct {
	gateway *Gateway
	st      *fakeStore
	rdp     *fakeRdpStore
	auditL  *fakeAuditStore
	auditW  *audit.Writer
	cipher  *cryptox.Cipher
	logBuf  *bytes.Buffer
	dir     string
	server  store.Server
	guacd   *fakeGuacd // 内部 stub；nil=调用方自定义 Addr
}

// newTestFixture 组装 Gateway + 假 guacd + 审计 writer。
//
// cfg.Addr 为空时内部启动 fakeGuacd 并回填地址；非空时沿用调用方自定义
// stub 地址（TestAttachHandshakeFailureClosesSession 的 error 服务端）。
// goleak 最先注册（Cleanup LIFO：最后执行），此时 stub close、Shutdown、
// writer Close 均已完成。
func newTestFixture(t *testing.T, cfg Config, after func(net.Conn, *bufio.Reader)) *testFixture {
	t.Helper()
	t.Cleanup(func() { goleak.VerifyNone(t) })
	var guacd *fakeGuacd
	if cfg.Addr == "" {
		guacd = newFakeGuacd(t, after)
		cfg.Addr = guacd.addr
	}
	cipher, err := cryptox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	enc, err := cipher.Encrypt(testPassword)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	rdp := newFakeRdpStore()
	auditL := &fakeAuditStore{}
	st := &fakeStore{
		rdp: rdp,
		servers: &fakeServers{servers: map[string]store.Server{
			"srv-1": {
				ID:                "srv-1",
				Name:              "win-host",
				OS:                store.ServerOSWindows,
				Host:              "127.0.0.1",
				Port:              3389,
				Username:          "Administrator",
				AuthMethod:        store.AuthMethodPassword,
				PasswordEncrypted: enc,
			},
		}},
		auditL: auditL,
	}
	writer := audit.NewWriter(st, slog.New(slog.DiscardHandler), nil)
	writer.Start(context.Background())
	cfg.RecordingsDir = t.TempDir()
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 10 * time.Minute
	}
	if cfg.MaxDuration == 0 {
		cfg.MaxDuration = time.Hour
	}
	logBuf := &bytes.Buffer{}
	g := NewGateway(cfg, st, writer, cipher, nil, slog.New(slog.NewTextHandler(logBuf, nil)))
	f := &testFixture{
		gateway: g,
		st:      st,
		rdp:     rdp,
		auditL:  auditL,
		auditW:  writer,
		cipher:  cipher,
		logBuf:  logBuf,
		dir:     cfg.RecordingsDir,
		server:  st.servers.servers["srv-1"],
		guacd:   guacd,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		g.Shutdown(ctx)
		_ = writer.Close(ctx)
	})
	return f
}

func (f *testFixture) openDefault(t *testing.T) store.RdpSession {
	t.Helper()
	s, err := f.gateway.Open(context.Background(), f.server, "user-1", 1280, 800, 96)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

// waitForRow 轮询会话行直至 closed 且 reason 匹配。
func waitForRow(t *testing.T, ts *fakeRdpStore, id, reason string) store.RdpSession {
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
	return store.RdpSession{}
}

// flushAudit 等待审计 writer 把已入队事件落库。
func (f *testFixture) flushAudit(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.auditW.Flush(ctx); err != nil {
		t.Fatalf("flush audit: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 指令编解码
// ---------------------------------------------------------------------------

func TestInstructionCodec(t *testing.T) {
	defer goleak.VerifyNone(t)

	// 基本形态。guacamole 指令全元素（含 opcode）带长度前缀——
	// 经真 guacd 1.5.0 实测：契约 5.14 的 "select,3.rdp;" 是文档式简写，
	// 线上格式为 "6.select,3.rdp;"（opcode select 长 6）。
	if got := string(encodeInstruction("select", "rdp")); got != "6.select,3.rdp;" {
		t.Fatalf("encode select = %q", got)
	}
	if got := string(encodeInstruction("size", "1280", "800", "96")); got != "4.size,4.1280,3.800,2.96;" {
		t.Fatalf("encode size = %q", got)
	}

	// 值含分隔符：长度前缀定界，无需转义（凭据注入依赖此性质）。
	tricky := "pw,with;semi.colon=and.dot"
	raw := encodeInstruction("connect", "key="+tricky)
	opcode, args, err := parseInstruction(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opcode != "connect" || len(args) != 1 || args[0] != "key="+tricky {
		t.Fatalf("round trip failed: opcode=%q args=%q", opcode, args)
	}

	// 解析拒绝：超长长度、非数字长度、空指令。
	if _, _, err := parseInstruction([]byte("connect,99.too-long")); err == nil {
		t.Fatal("expected error for overflowing length")
	}
	if _, _, err := parseInstruction([]byte("connect,x.abc")); err == nil {
		t.Fatal("expected error for malformed length")
	}
	if _, _, err := parseInstruction(nil); err == nil {
		t.Fatal("expected error for empty instruction")
	}
}

// ---------------------------------------------------------------------------
// Open 错误哨兵（G21：非 windows / 无凭据 / GuacdAddr 空 / 会话冲突）
// ---------------------------------------------------------------------------

func TestOpenValidations(t *testing.T) {
	rdp := newFakeRdpStore()
	st := &fakeStore{
		rdp:     rdp,
		servers: &fakeServers{servers: map[string]store.Server{}},
		auditL:  &fakeAuditStore{},
	}
	t.Cleanup(func() { goleak.VerifyNone(t) })
	g := NewGateway(Config{Addr: "127.0.0.1:14822"}, st, nil, nil, nil, slog.New(slog.DiscardHandler))
	ctx := context.Background()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		g.Shutdown(c)
	})

	// 非 windows 服务器。
	linux := store.Server{ID: "l1", OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"}
	if _, err := g.Open(ctx, linux, "u", 1280, 800, 96); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("linux open err = %v", err)
	}
	// windows 但 private_key 认证（无密码凭据）。
	keyAuth := store.Server{ID: "w1", OS: store.ServerOSWindows, AuthMethod: store.AuthMethodPrivateKey, PrivateKeyEncrypted: "k"}
	if _, err := g.Open(ctx, keyAuth, "u", 1280, 800, 96); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("private key open err = %v", err)
	}
	// windows password 但密文为空。
	noEnc := store.Server{ID: "w2", OS: store.ServerOSWindows, AuthMethod: store.AuthMethodPassword}
	if _, err := g.Open(ctx, noEnc, "u", 1280, 800, 96); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("empty enc open err = %v", err)
	}
	// 越界分辨率。
	ok := store.Server{ID: "w3", OS: store.ServerOSWindows, AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"}
	if _, err := g.Open(ctx, ok, "u", 100, 800, 96); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("bad width err = %v", err)
	}
	// 同服务器第二个活动会话。
	if _, err := g.Open(ctx, ok, "u", 1280, 800, 96); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := g.Open(ctx, ok, "u", 1280, 800, 96); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("second open err = %v", err)
	}
	// 全局上限（MaxSessions=2，另一台服务器）。
	other := store.Server{ID: "w4", OS: store.ServerOSWindows, AuthMethod: store.AuthMethodPasswordNTLM, PasswordEncrypted: "enc"}
	if _, err := g.Open(ctx, other, "u", 1280, 800, 96); err != nil {
		t.Fatalf("ntlm open: %v", err)
	}
	third := store.Server{ID: "w5", OS: store.ServerOSWindows, AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"}
	if _, err := g.Open(ctx, third, "u", 1280, 800, 96); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("limit open err = %v", err)
	}
}

func TestOpenGuacdAddrEmpty(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	rdp := newFakeRdpStore()
	st := &fakeStore{rdp: rdp, servers: &fakeServers{servers: map[string]store.Server{}}, auditL: &fakeAuditStore{}}
	g := NewGateway(Config{}, st, nil, nil, nil, slog.New(slog.DiscardHandler))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		g.Shutdown(ctx)
	})
	win := store.Server{ID: "w1", OS: store.ServerOSWindows, AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"}
	_, err := g.Open(context.Background(), win, "user-1", 1280, 800, 96)
	if !errors.Is(err, ErrGuacdUnavailable) {
		t.Fatalf("open with empty addr err = %v", err)
	}
}

func TestOpenWritesSessionStartAudit(t *testing.T) {
	f := newTestFixture(t, Config{}, nil)
	session := f.openDefault(t)
	f.flushAudit(t)

	e, ok := f.auditL.find("session.start")
	if !ok {
		t.Fatal("session.start audit missing")
	}
	if e.ResourceType != "rdp_session" || e.ResourceID != session.ID || e.ActorType != "user" {
		t.Fatalf("bad audit envelope: %+v", e)
	}
	var detail map[string]any
	if err := json.Unmarshal(e.Detail, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if detail["server_name"] != "win-host" || detail["width"].(float64) != 1280 {
		t.Fatalf("bad detail: %+v", detail)
	}
}

// ---------------------------------------------------------------------------
// Attach：握手序列、凭据注入与不外泄（G21 核心）
// ---------------------------------------------------------------------------

func TestAttachHandshakeInjectsCredentials(t *testing.T) {
	f := newTestFixture(t, Config{}, nil)
	session := f.openDefault(t)

	ws := newFakeWSConn()
	if err := f.gateway.Attach(ws, session.ID, "user-1", "req-1", testSessionIP); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// 握手序列：select → (args) → size/audio/video/image → connect，
	// 网关侧共发出 6 条指令（args 为 guacd 响应，不计入）。
	deadline := time.Now().Add(5 * time.Second)
	var instrs []string
	for time.Now().Before(deadline) {
		instrs = f.guacd.getInstructions()
		if len(instrs) >= 6 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(instrs) != 6 {
		t.Fatalf("handshake instructions = %v", instrs)
	}
	if instrs[0] != "6.select,3.rdp;" {
		t.Fatalf("select = %q", instrs[0])
	}
	if instrs[1] != "4.size,4.1280,3.800,2.96;" {
		t.Fatalf("size = %q", instrs[1])
	}
	if instrs[2] != "5.audio,1.0;" || instrs[3] != "5.video,1.0;" || instrs[4] != "5.image,1.0;" {
		t.Fatalf("audio/video/image = %q %q %q", instrs[2], instrs[3], instrs[4])
	}
	if !strings.HasPrefix(instrs[5], "7.connect,") {
		t.Fatalf("connect = %q", instrs[5])
	}

	// connect 参数逐项断言（契约 5.14 第 3 条）。
	for time.Now().Before(deadline) {
		if len(f.guacd.getConnect()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	params := f.guacd.getConnect()
	// 契约 5.14 参数 + 实测补充的 recording-path/create-recording-path
	//（guacd CLI 无 -r，录制目录是连接参数）。
	want := map[string]string{
		"hostname":              "127.0.0.1",
		"port":                  "3389",
		"username":              "Administrator",
		"password":              testPassword,
		"security":              "nego",
		"ignore-cert":           "true",
		"width":                 "1280",
		"height":                "800",
		"dpi":                   "96",
		"recording-path":        f.dir,
		"recording-name":        session.ID,
		"create-recording-path": "true",
	}
	if len(params) != len(want) {
		t.Fatalf("connect params = %v", params)
	}
	for k, v := range want {
		if params[k] != v {
			t.Fatalf("connect param %s = %q, want %q (all: %v)", k, params[k], v, params)
		}
	}

	// 关闭并等待收尾，随后断言凭据未泄漏到 WS 帧/日志/审计。
	ws.Close()
	waitForRow(t, f.rdp, session.ID, reasonClientClosed)
	f.flushAudit(t)

	if bytes.Contains(ws.writtenBytes(), []byte(testPassword)) {
		t.Fatal("password leaked into WS frames")
	}
	if strings.Contains(f.logBuf.String(), testPassword) {
		t.Fatal("password leaked into logs")
	}
	if strings.Contains(f.auditL.raw(), testPassword) {
		t.Fatal("password leaked into audit")
	}
}

func TestAttachRejectsWrongUserAndUnknownSession(t *testing.T) {
	f := newTestFixture(t, Config{}, nil)
	session := f.openDefault(t)

	ws := newFakeWSConn()
	defer ws.Close()
	if err := f.gateway.Attach(ws, session.ID, "someone-else", "req", "ip"); !errors.Is(err, ErrNotActive) {
		t.Fatalf("wrong user err = %v", err)
	}
	if err := f.gateway.Attach(ws, uuid.NewString(), "user-1", "req", "ip"); !errors.Is(err, ErrNotActive) {
		t.Fatalf("unknown session err = %v", err)
	}
}

// TestAttachPassthroughBothDirections 验证握手后的原始字节透传：
// guacd→WS 与 WS→guacd 双向（echo 服务端）。
func TestAttachPassthroughBothDirections(t *testing.T) {
	f := newTestFixture(t, Config{}, func(conn net.Conn, r *bufio.Reader) {
		// guacd 侧：把收到的字节原样回发（echo），并先主动发一帧。
		go func() {
			_, _ = conn.Write([]byte("4.sync,3.123;"))
		}()
		_, _ = io.Copy(conn, r)
	})
	session := f.openDefault(t)

	ws := newFakeWSConn()
	if err := f.gateway.Attach(ws, session.ID, "user-1", "req-2", testSessionIP); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// guacd → WS：sync 指令原样到达。
	ws.clientExpect(t, "4.sync,3.123;")

	// WS → guacd → （echo） → WS：键盘/鼠标指令往返。
	ws.clientSend(t, []byte("3.key,2.65,1.1;"))
	ws.clientExpect(t, "3.key,2.65,1.1;")

	// 双向字节均不含密码。
	if bytes.Contains(ws.writtenBytes(), []byte(testPassword)) {
		t.Fatal("password leaked into passthrough")
	}

	ws.Close()
	waitForRow(t, f.rdp, session.ID, reasonClientClosed)
}

// TestAttachGuacdDisconnectClosesSession：guacd 断开 → 会话 closed(error)，
// .guac stat 回填 bytes_written（契约 4.14/5.14）。
func TestAttachGuacdDisconnectClosesSession(t *testing.T) {
	f := newTestFixture(t, Config{}, func(conn net.Conn, r *bufio.Reader) {
		_, _ = io.Copy(io.Discard, r) // 排空后由 cleanup 断开
	})
	session := f.openDefault(t)

	ws := newFakeWSConn()
	if err := f.gateway.Attach(ws, session.ID, "user-1", "req-3", testSessionIP); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// 预置 .guac 录制产物（guacd 侧写共享卷的模拟）。
	guacPath := filepath.Join(f.dir, session.ID+guacFileSuffix)
	if err := os.WriteFile(guacPath, bytes.Repeat([]byte("x"), 4242), 0o600); err != nil {
		t.Fatalf("write .guac: %v", err)
	}

	// 断开 guacd（close 内部关闭全部连接）。
	f.guacd.close()
	row := waitForRow(t, f.rdp, session.ID, reasonError)
	if row.BytesWritten != 4242 {
		t.Fatalf("bytes_written = %d, want 4242 (stat of .guac)", row.BytesWritten)
	}
	if !row.HasRecording || row.RecordingFile != session.ID+guacFileSuffix {
		t.Fatalf("recording file not derived: %+v", row)
	}

	f.flushAudit(t)
	if _, ok := f.auditL.find("session.end"); !ok {
		t.Fatal("session.end audit missing")
	}
}

// TestAttachHandshakeFailureClosesSession：guacd 返回 error 指令 →
// Attach 报错 + 会话行 closed(error) + session.end 审计。
func TestAttachHandshakeFailureClosesSession(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write(encodeInstruction("error", "Connection refused to host"))
		_ = conn.Close()
	}()

	f := newTestFixture(t, Config{Addr: ln.Addr().String()}, nil)
	session := f.openDefault(t)

	ws := newFakeWSConn()
	defer ws.Close()
	err = f.gateway.Attach(ws, session.ID, "user-1", "req-4", testSessionIP)
	if err == nil || !strings.Contains(err.Error(), "Connection refused to host") {
		t.Fatalf("attach err = %v", err)
	}
	waitForRow(t, f.rdp, session.ID, reasonError)
	f.flushAudit(t)
	if _, ok := f.auditL.find("session.end"); !ok {
		t.Fatal("session.end audit missing after handshake failure")
	}
}

// ---------------------------------------------------------------------------
// 治理：空闲/最大时长/Shutdown/CloseByServer/孤儿回收
// ---------------------------------------------------------------------------

func TestGovernIdleTimeout(t *testing.T) {
	f := newTestFixture(t, Config{IdleTimeout: 300 * time.Millisecond}, func(conn net.Conn, r *bufio.Reader) {
		_, _ = io.Copy(io.Discard, r)
	})
	session := f.openDefault(t)

	ws := newFakeWSConn()
	if err := f.gateway.Attach(ws, session.ID, "user-1", "req-5", testSessionIP); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// 无任何字节流动（guacd stub 静默）→ idle_timeout。
	row := waitForRow(t, f.rdp, session.ID, reasonIdleTimeout)
	if row.EndedAt == nil {
		t.Fatal("ended_at not set")
	}
}

func TestGovernMaxDuration(t *testing.T) {
	f := newTestFixture(t, Config{MaxDuration: 300 * time.Millisecond}, func(conn net.Conn, r *bufio.Reader) {
		// 持续发字节保持活跃，规避空闲路径，只命中时长上限。
		for {
			if _, err := conn.Write(encodeInstruction("nop")); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	session := f.openDefault(t)

	ws := newFakeWSConn()
	if err := f.gateway.Attach(ws, session.ID, "user-1", "req-6", testSessionIP); err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitForRow(t, f.rdp, session.ID, reasonMaxDuration)
}

func TestShutdownClosesLiveSessions(t *testing.T) {
	f := newTestFixture(t, Config{}, func(conn net.Conn, r *bufio.Reader) {
		_, _ = io.Copy(io.Discard, r)
	})
	session := f.openDefault(t)

	ws := newFakeWSConn()
	if err := f.gateway.Attach(ws, session.ID, "user-1", "req-7", testSessionIP); err != nil {
		t.Fatalf("attach: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.gateway.Shutdown(ctx)
	waitForRow(t, f.rdp, session.ID, reasonServerShutdown)
}

func TestCloseByServer(t *testing.T) {
	f := newTestFixture(t, Config{}, func(conn net.Conn, r *bufio.Reader) {
		_, _ = io.Copy(io.Discard, r)
	})
	session := f.openDefault(t)

	ws := newFakeWSConn()
	if err := f.gateway.Attach(ws, session.ID, "user-1", "req-8", testSessionIP); err != nil {
		t.Fatalf("attach: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.gateway.CloseByServer(ctx, "srv-1", reasonError)
	waitForRow(t, f.rdp, session.ID, reasonError)
}

func TestOrphanReap(t *testing.T) {
	// 空闲超时极短，但孤儿行按行内 LastActivityAt 判定。
	f := newTestFixture(t, Config{IdleTimeout: 100 * time.Millisecond}, nil)

	// 直接落一行 active 孤儿（模拟进程重启残留：不在 pending/live）。
	now := time.Now().UTC().Add(-time.Hour)
	orphan := store.RdpSession{
		ID: uuid.NewString(), ServerID: "srv-1", Status: store.SessionActive,
		StartedAt: now, LastActivityAt: now, CreatedAt: now, UpdatedAt: now,
	}
	f.rdp.rows[orphan.ID] = &orphan

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if row, ok := f.rdp.get(orphan.ID); ok && row.Status == store.SessionClosed {
			if row.EndedReason != reasonIdleTimeout {
				t.Fatalf("orphan reason = %q", row.EndedReason)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("orphan row not reaped")
}

// 确认端口缺省：server.Port=0 时 connect 注入 3389（契约 5.14）。
func TestConnectPortDefaultsTo3389(t *testing.T) {
	f := newTestFixture(t, Config{}, nil)
	noPort := f.server
	noPort.Port = 0
	session, err := f.gateway.Open(context.Background(), noPort, "user-1", 1280, 800, 96)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ws := newFakeWSConn()
	defer ws.Close()
	if err := f.gateway.Attach(ws, session.ID, "user-1", "req-9", testSessionIP); err != nil {
		t.Fatalf("attach: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p := f.guacd.getConnect(); p != nil && p["port"] != "" {
			if p["port"] != "3389" {
				t.Fatalf("port = %q, want 3389", p["port"])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("connect params not observed")
}
