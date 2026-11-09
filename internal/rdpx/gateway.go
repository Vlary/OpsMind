// Package rdpx 实现堡垒机式 RDP 网关：guacamole 协议握手（select rdp +
// 凭据注入）与 WS↔guacd 字节流双向透传、会话治理与审计接入
// （CONTRACT.md 5.14/6.5）。
//
// 架构（调研结论）：浏览器(WS 文本帧) ↔ 本网关(纯字节透传) ↔
// guacd(TCP 4822, FreeRDP)。Go 侧不实现 guacamole 协议语义，只在握手
// 阶段注入凭据；凭据仅存在于握手调用栈内，不落日志、不进 WS、不进审计。
package rdpx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aiops/internal/audit"
	"aiops/internal/cryptox"
	"aiops/internal/store"
)

// 契约默认值（CONTRACT.md 5.14 / config 5.12）。
const (
	defaultMaxSessions = 2
	defaultIdleTimeout = 30 * time.Minute
	defaultMaxDuration = 8 * time.Hour
	defaultDialTimeout = 10 * time.Second

	// 治理循环间隔与 TouchActivity 节流（同 termx：节流至 1 次/5s）。
	governInterval = time.Second
	touchThrottle  = 5 * time.Second

	// 孤儿行（Open 未 Attach/重启残留）扫描间隔。
	orphanScanEvery = 5

	// 收尾各阶段预算：Shutdown 同步封盘总预算 ≤5s（契约 2.1）。
	auditEnqueueTimeout = 2 * time.Second
	bridgeStopTimeout   = 2 * time.Second
	dbCloseTimeout      = 1 * time.Second

	// 缓冲上限（契约 6.5）：guacd TCP 读缓冲 64KB；单条 guacamole 指令
	// 理论上限 1MB（WS 读限同值，由 httpapi 适配器设置）。
	guacdReadBuf = 64 * 1024
	maxHandshake = 1024 * 1024

	// RDP 端口缺省（契约 5.14：写死用 server.port，缺省 3389）。
	defaultRdpPort = 3389

	// 分辨率范围（OpenAPI RdpSessionCreate；RDP 不支持运行中 resize）。
	minWidth, maxWidth   = 640, 3840
	minHeight, maxHeight = 480, 2160
	minDPI, maxDPI       = 72, 120

	guacFileSuffix = ".guac"
)

// Config 是 Gateway 配置（零值字段应用契约默认）。
type Config struct {
	Addr          string        // cfg.GuacdAddr；""=RDP 禁用
	MaxSessions   int           // cfg.RdpMaxSessions（默认 2）
	IdleTimeout   time.Duration // cfg.RdpIdleTimeout（默认 30m）
	MaxDuration   time.Duration // cfg.RdpMaxDuration（默认 8h）
	RecordingsDir string        // cfg.RdpRecordingsDir（.guac 所在目录）
	DialTimeout   time.Duration // 10s
}

// WSConn 是 WS 连接抽象：双向透传 guacamole 指令字节流（gorilla/websocket 适配由
// httpapi 注入；rdpx 不 import Gin/websocket）。
type WSConn interface {
	io.ReadWriteCloser
}

// 错误哨兵（契约 5.14）。
var (
	ErrSessionExists    = errors.New("rdpx: server already has an active session")
	ErrSessionLimit     = errors.New("rdpx: rdp session limit reached")
	ErrUnsupported      = errors.New("rdpx: server os is not windows")
	ErrNoCredentials    = errors.New("rdpx: server has no password credential")
	ErrGuacdUnavailable = errors.New("rdpx: guacd is not configured")
	ErrNotActive        = errors.New("rdpx: session is not active")
)

// 会话结束原因（契约 4.14 ended_reason 枚举，同 terminal_sessions）。
const (
	reasonClientClosed   = "client_closed"
	reasonIdleTimeout    = "idle_timeout"
	reasonMaxDuration    = "max_duration"
	reasonServerShutdown = "server_shutdown"
	reasonError          = "error"
)

// pendingSession 是 Open 后、Attach 前的会话占位（归属校验用）。
type pendingSession struct {
	id        string
	serverID  string
	userID    string
	startedAt time.Time
}

// liveSession 是已 Attach 的活动会话。
type rdpLiveSession struct {
	id         string
	serverID   string
	userID     string
	serverName string
	requestID  string
	ip         string
	width      int
	height     int
	dpi        int
	startedAt  time.Time

	// guacd 连接（Attach 握手后非 nil）。拨号发生在 g.mu 释放之后，
	// 治理循环/CloseByServer 可能并行 finish 读该连接——必须原子访问，
	// 否则构成数据竞争（-race 实测：Attach 写 vs governLoop→finish 读）。
	guacd atomic.Value // net.Conn
	// guacd 读端（握手期间已缓冲的残余字节供透传消费）；仅在 Attach
	// 写入完成之后由 pumpGuacdToWS 消费（happens-before 由 goroutine
	// 启动顺序保证），治理路径不触碰。
	reader *bufio.Reader
	ws     WSConn

	activity  atomic.Int64 // 最后活动 UnixNano（任一方向有字节流动即刷新）
	lastTouch atomic.Int64 // 最后 TouchActivity 落库 UnixNano

	done      chan struct{}
	ioWG      sync.WaitGroup // 两个透传 goroutine
	closeOnce sync.Once
}

// Gateway 管理全部 RDP 会话。
type Gateway struct {
	cfg    Config
	st     store.Store
	audit  *audit.Writer
	cipher *cryptox.Cipher
	clock  func() time.Time
	log    *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingSession
	live    map[string]*rdpLiveSession
	servers map[string]string // serverID → sessionID（同服务器单会话）
	closed  bool

	govWG   sync.WaitGroup
	govStop chan struct{}
	govOnce sync.Once
}

// NewGateway 构造 Gateway 并启动治理循环。
func NewGateway(cfg Config, st store.Store, auditW *audit.Writer, cipher *cryptox.Cipher, clock func() time.Time, log *slog.Logger) *Gateway {
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultMaxSessions
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	if cfg.MaxDuration <= 0 {
		cfg.MaxDuration = defaultMaxDuration
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if clock == nil {
		clock = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	g := &Gateway{
		cfg:     cfg,
		st:      st,
		audit:   auditW,
		cipher:  cipher,
		clock:   clock,
		log:     log,
		pending: make(map[string]*pendingSession),
		live:    make(map[string]*rdpLiveSession),
		servers: make(map[string]string),
		govStop: make(chan struct{}),
	}
	g.govWG.Add(1)
	go g.governLoop()
	return g
}

// Open 创建 rdp_sessions 行：server.os 必须 windows（ErrUnsupported）、
// server 必须有密码凭据（ErrNoCredentials，password/basic 共用解密密码）、
// GuacdAddr 为空返回 ErrGuacdUnavailable；同服务器活动会话/全局上限同
// termx。成功写审计 session.start。不拨 guacd——WS Attach 时才拨。
func (g *Gateway) Open(ctx context.Context, server store.Server, userID string, width, height, dpi int) (store.RdpSession, error) {
	if server.OS != store.ServerOSWindows {
		return store.RdpSession{}, ErrUnsupported
	}
	if g.cfg.Addr == "" {
		return store.RdpSession{}, ErrGuacdUnavailable
	}
	// password/password_ntlm/password_basic 共用 password_enc（契约 4.3）；
	// private_key 认证无密码，RDP 不可用。
	if server.AuthMethod == store.AuthMethodPrivateKey || server.PasswordEncrypted == "" {
		return store.RdpSession{}, ErrNoCredentials
	}
	if width == 0 {
		width = 1280
	}
	if height == 0 {
		height = 800
	}
	if dpi == 0 {
		dpi = 96
	}
	if !validResolution(width, height, dpi) {
		return store.RdpSession{}, fmt.Errorf("rdpx: invalid resolution %dx%d@%d", width, height, dpi)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return store.RdpSession{}, ErrGuacdUnavailable
	}
	// DB 是跨重启的权威：活动行（含重启残留孤儿）同样占位。
	byServer, err := g.st.RdpSessions().CountActiveByServer(ctx, server.ID)
	if err != nil {
		return store.RdpSession{}, fmt.Errorf("rdpx: count active by server: %w", err)
	}
	if byServer > 0 {
		return store.RdpSession{}, ErrSessionExists
	}
	total, err := g.st.RdpSessions().CountActive(ctx)
	if err != nil {
		return store.RdpSession{}, fmt.Errorf("rdpx: count active: %w", err)
	}
	if total >= int64(g.cfg.MaxSessions) {
		return store.RdpSession{}, ErrSessionLimit
	}

	session, err := g.st.RdpSessions().Create(ctx, store.CreateRdpSessionParams{
		ServerID:  server.ID,
		UserID:    userID,
		Width:     width,
		Height:    height,
		DPI:       dpi,
		StartedAt: g.clock().UTC(),
	})
	if err != nil {
		return store.RdpSession{}, fmt.Errorf("rdpx: create session row: %w", err)
	}
	g.pending[session.ID] = &pendingSession{
		id:        session.ID,
		serverID:  server.ID,
		userID:    userID,
		startedAt: g.clock(),
	}
	g.enqueueAudit(session.ID, userID, server.ID, "session.start", map[string]any{
		"width":       width,
		"height":      height,
		"dpi":         dpi,
		"server_name": server.Name,
	})
	return session, nil
}

// Attach 拨 guacd（TCP）、select rdp、读 args、注入凭据并发起连接，
// 随后与 WSConn 双向透传原始字节（不解析 guacamole 指令）。
//
// 会话非 active（不存在/已结束/不属于该用户/已被绑定）返回 ErrNotActive；
// guacd 拨号或握手失败走完整收尾（Close 会话行 reason=error + 审计
// session.end）并返回包装错误。错误路径连接由本方法收尾关闭。
func (g *Gateway) Attach(ws WSConn, sessionID, userID, requestID, ip string) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return ErrGuacdUnavailable
	}
	p, ok := g.pending[sessionID]
	if !ok || p.userID != userID {
		g.mu.Unlock()
		return ErrNotActive
	}
	delete(g.pending, sessionID)
	row, err := g.st.RdpSessions().Get(context.Background(), sessionID)
	if err != nil || row.Status != store.SessionActive {
		g.mu.Unlock()
		return ErrNotActive
	}
	server, err := g.st.Servers().Get(context.Background(), row.ServerID)
	if err != nil {
		g.mu.Unlock()
		g.log.Error("rdpx: load server for attach failed", "session_id", sessionID, "error", err)
		return ErrNotActive
	}
	s := &rdpLiveSession{
		id:         sessionID,
		serverID:   p.serverID,
		userID:     userID,
		serverName: row.ServerName,
		requestID:  requestID,
		ip:         ip,
		width:      row.Width,
		height:     row.Height,
		dpi:        row.DPI,
		startedAt:  row.StartedAt,
		ws:         ws,
		done:       make(chan struct{}),
	}
	now := g.clock()
	s.activity.Store(now.UnixNano())
	s.lastTouch.Store(now.UnixNano())
	// WaitGroup 计数在注册进 live（此后 CloseByServer/治理循环可触达
	// finish→Wait）之前累加：正增量 Add 必须先于 Wait（sync 语义），
	// 否则 Add 与并发 Wait 构成数据竞争（termx 同构缺陷 -race 实测）。
	// 拨号/握手失败路径用哑 goroutine 冲销，全程无 Add 负增量。
	s.ioWG.Add(2)
	g.live[sessionID] = s
	g.servers[p.serverID] = sessionID
	g.mu.Unlock()

	// 解密凭据（明文仅存在于本次握手调用栈内）并拨 guacd 握手；
	// 失败走完整收尾：关 TCP → Close 会话行（error）→ 审计 session.end。
	releaseIO := func() {
		// 拨号/握手失败不启动两个透传 goroutine：哑 goroutine 立即冲销
		// 预加的 ioWG 计数（不用 Add 负数——可能与并发 finish 的 Wait 竞争）。
		go func() { s.ioWG.Done() }()
		go func() { s.ioWG.Done() }()
	}
	password, err := g.decryptPassword(server)
	if err != nil {
		g.log.Error("rdpx: decrypt credential failed", "session_id", sessionID, "error", err)
		releaseIO()
		g.finish(s, reasonError)
		return fmt.Errorf("rdpx: decrypt credential: %w", err)
	}
	conn, err := g.dialAndHandshake(s, server, password)
	clear(password) // 凭据用后立即清零（契约 5.14 第 5 条）
	if err != nil {
		g.log.Error("rdpx: guacd handshake failed", "session_id", sessionID,
			"server", server.Host, "error", err)
		releaseIO()
		g.finish(s, reasonError)
		return err
	}
	s.setGuacd(conn)

	go func() { defer s.ioWG.Done(); g.pumpGuacdToWS(s) }()
	go func() { defer s.ioWG.Done(); g.pumpWSToGuacd(s) }()
	return nil
}

// setGuacd 记录拨号完成的 guacd 连接（Attach 路径，锁外）。
func (s *rdpLiveSession) setGuacd(c net.Conn) { s.guacd.Store(c) }

// guacdConn 原子读取 guacd 连接；未完成拨号时为 nil。
func (s *rdpLiveSession) guacdConn() net.Conn {
	if v := s.guacd.Load(); v != nil {
		return v.(net.Conn)
	}
	return nil
}

// decryptPassword 解密服务器密码凭据；错误不含明文。
func (g *Gateway) decryptPassword(server store.Server) ([]byte, error) {
	if g.cipher == nil {
		return nil, errors.New("rdpx: cipher unavailable")
	}
	if server.AuthMethod == store.AuthMethodPrivateKey || server.PasswordEncrypted == "" {
		return nil, ErrNoCredentials
	}
	plain, err := g.cipher.Decrypt(server.PasswordEncrypted)
	if err != nil {
		return nil, fmt.Errorf("rdpx: decrypt: %w", err)
	}
	return []byte(plain), nil
}

// dialAndHandshake 按 guacamole 协议与 guacd 完成连接建立（契约 5.14）：
//
//  1. TCP 拨号（DialTimeout）
//  2. 发 select rdp → 读 args,...;（首参为协议版本 VERSION_1_5_0，其后
//     为参数名列表）
//  3. 发 size/audio/video/image
//  4. 发 connect + key:value 参数（hostname/port/username/password/
//     security/ignore-cert/width/height/dpi/recording-name）
//
// 指令编码：guacamole 指令全元素（含 opcode）带长度前缀——经真 guacd
// 1.5.0 实测（`6.select,3.rdp;` → `4.args,13.VERSION_1_5_0,...`；
// `select,3.rdp;` 无 opcode 前缀被 guacd 直接断开）。契约 5.14 的
// `select,3.rdp;` 为文档式简写。
//
// 返回的连接读端已包 bufio；握手期间 guacd 预发的指令字节（ready 等）
// 保留在缓冲内，由随后的透传循环消费转发。
func (g *Gateway) dialAndHandshake(s *rdpLiveSession, server store.Server, password []byte) (net.Conn, error) {
	if g.cfg.Addr == "" {
		return nil, ErrGuacdUnavailable
	}
	d := net.Dialer{Timeout: g.cfg.DialTimeout}
	conn, err := d.Dial("tcp", g.cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("rdpx: dial guacd: %w", err)
	}

	reader := bufio.NewReaderSize(conn, guacdReadBuf)
	s.reader = reader

	// select rdp → args
	if _, err := conn.Write(encodeInstruction("select", "rdp")); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rdpx: send select: %w", err)
	}
	raw, err := readInstruction(reader)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rdpx: read args: %w", err)
	}
	opcode, args, err := parseInstruction(raw)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rdpx: parse args: %w", err)
	}
	switch opcode {
	case "args":
		// 参数名列表（首参协议版本）；连接参数以 key:value 注入，
		// 不依赖位置顺序，列表内容无需逐项校验。
	case "error":
		_ = conn.Close()
		// guacd 的 error 参数是服务端状态描述（不含我们注入的凭据）。
		reason := "unknown error"
		if len(args) > 0 {
			reason = args[0]
		}
		return nil, fmt.Errorf("rdpx: guacd rejected rdp protocol: %s", reason)
	default:
		_ = conn.Close()
		return nil, fmt.Errorf("rdpx: unexpected guacd instruction %q", opcode)
	}

	// size / audio / video / image（契约 5.14 第 2 条）。
	var head []byte
	head = append(head, encodeInstruction("size",
		strconv.Itoa(s.width), strconv.Itoa(s.height), strconv.Itoa(s.dpi))...)
	head = append(head, encodeInstruction("audio", "0")...)
	head = append(head, encodeInstruction("video", "0")...)
	head = append(head, encodeInstruction("image", "0")...)
	if _, err := conn.Write(head); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rdpx: send size: %w", err)
	}

	// connect + 连接参数。port 写死用 server.port（缺省 3389）；
	// security=nego + ignore-cert=true（调研结论：兼容 NLA/非 NLA 与自签证书）。
	// 录制：guacd CLI 无 -r 选项（实测），录制目录经 recording-path 连接
	// 参数注入（契约 4.14：.guac 落 guacd 与 app 同路径挂载的共享卷），
	// create-recording-path 让 guacd 自动建目录（卷首启时）。
	port := server.Port
	if port <= 0 {
		port = defaultRdpPort
	}
	connectParams := []string{
		"hostname=" + server.Host,
		"port=" + strconv.Itoa(port),
		"username=" + server.Username,
		"password=" + string(password),
		"security=nego",
		"ignore-cert=true",
		"width=" + strconv.Itoa(s.width),
		"height=" + strconv.Itoa(s.height),
		"dpi=" + strconv.Itoa(s.dpi),
	}
	if g.cfg.RecordingsDir != "" {
		connectParams = append(connectParams,
			"recording-path="+g.cfg.RecordingsDir,
			"recording-name="+s.id,
			"create-recording-path=true",
		)
	}
	connect := encodeInstruction(append([]string{"connect"}, connectParams...)...)
	if _, err := conn.Write(connect); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rdpx: send connect: %w", err)
	}
	return conn, nil
}

// ---------------------------------------------------------------------------
// guacamole 指令编解码（长度前缀；凭据经同一编码函数注入，杜绝拼接遗漏）
// ---------------------------------------------------------------------------

// encodeInstruction 把指令元素序列编码为 guacamole 指令：
// "<len>.<e0>,<len>.<e1>,...;"。元素内容（含逗号/分号/等号）由长度前缀
// 定界，无需转义（契约 5.14：必须复用编码函数，不得字符串拼接）。
func encodeInstruction(elements ...string) []byte {
	var b strings.Builder
	for i, e := range elements {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(len(e)))
		b.WriteByte('.')
		b.WriteString(e)
	}
	b.WriteByte(';')
	return []byte(b.String())
}

// readInstruction 从 r 按长度前缀读取一条完整 guacamole 指令（至尾分号）。
//
// 元素内容可为任意字节（含 ',' ';' '.'——密码等凭据经同一编码传输，
// 逐字节扫 ';' 会把含分号的值截断）；只在元素边界识别分隔符。
func readInstruction(r *bufio.Reader) ([]byte, error) {
	var b []byte
	for {
		// 1. 读长度数字串（至 '.'）。
		var lenStr []byte
		for {
			c, err := r.ReadByte()
			if err != nil {
				if len(b) == 0 && len(lenStr) == 0 {
					return nil, err // 对端关闭且未开始新指令
				}
				return nil, io.ErrUnexpectedEOF
			}
			if c == '.' {
				break
			}
			if c < '0' || c > '9' {
				return nil, fmt.Errorf("rdpx: malformed length byte %q", c)
			}
			lenStr = append(lenStr, c)
			if len(lenStr) > 10 {
				return nil, errors.New("rdpx: length field too long")
			}
		}
		n, err := strconv.Atoi(string(lenStr))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("rdpx: bad length %q", lenStr)
		}
		if len(b)+n > maxHandshake {
			return nil, errors.New("rdpx: guacd instruction exceeds 1MB")
		}
		if len(b) > 0 {
			b = append(b, ',')
		}
		b = append(b, lenStr...)
		b = append(b, '.')
		// 2. 读 n 字节元素内容（原样保留）。
		for i := 0; i < n; i++ {
			c, rerr := r.ReadByte()
			if rerr != nil {
				return nil, io.ErrUnexpectedEOF
			}
			b = append(b, c)
		}
		// 3. 读分隔符：',' 继续下一元素；';' 指令结束。
		c, err := r.ReadByte()
		if err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		if c == ';' {
			return b, nil
		}
		if c != ',' {
			return nil, fmt.Errorf("rdpx: unexpected separator %q", c)
		}
	}
}

// parseInstruction 解析长度前缀指令，返回 opcode 与参数列表。
func parseInstruction(raw []byte) (string, []string, error) {
	if len(raw) == 0 {
		return "", nil, errors.New("rdpx: empty instruction")
	}
	var elements []string
	for i := 0; i < len(raw); {
		dot := indexByteFrom(raw, i, '.')
		if dot < 0 {
			return "", nil, fmt.Errorf("rdpx: malformed instruction at %d", i)
		}
		n, err := strconv.Atoi(string(raw[i:dot]))
		if err != nil || n < 0 {
			return "", nil, fmt.Errorf("rdpx: malformed length at %d", i)
		}
		start := dot + 1
		end := start + n
		if end > len(raw) {
			return "", nil, fmt.Errorf("rdpx: length %d overflows instruction", n)
		}
		elements = append(elements, string(raw[start:end]))
		i = end + 1 // 跳过分隔符 ','（最后一段之后无分隔符，i 越界即结束）
	}
	if len(elements) == 0 {
		return "", nil, errors.New("rdpx: no elements")
	}
	return elements[0], elements[1:], nil
}

func indexByteFrom(b []byte, from int, sep byte) int {
	for i := from; i < len(b); i++ {
		if b[i] == sep {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// 双向透传（契约 5.14 第 4 条：不解析、不改写、不缓存指令）
// ---------------------------------------------------------------------------

// pumpGuacdToWS 把 guacd 字节流原样转发到 WS（含 nop 心跳与 error 指令）。
func (g *Gateway) pumpGuacdToWS(s *rdpLiveSession) {
	buf := make([]byte, guacdReadBuf)
	for {
		n, err := s.reader.Read(buf)
		if n > 0 {
			s.activity.Store(g.clock().UnixNano())
			if _, werr := s.ws.Write(buf[:n]); werr != nil {
				g.finish(s, reasonError)
				return
			}
		}
		if err != nil {
			// WS 断（client_closed）与 guacd 断（error）都结束会话
			//（契约 5.14：断开即关对端）；本循环只服务 guacd 读端，
			// 读错误按 guacd 侧断开处理。
			g.finish(s, reasonError)
			return
		}
	}
}

// pumpWSToGuacd 把 WS 字节流原样转发到 guacd。
func (g *Gateway) pumpWSToGuacd(s *rdpLiveSession) {
	buf := make([]byte, guacdReadBuf)
	for {
		n, err := s.ws.Read(buf)
		if n > 0 {
			s.activity.Store(g.clock().UnixNano())
			// 原子取 guacd 连接：本循环虽在拨号完成后启动，但 finish
			// 可能并发关闭连接，统一走原子读避免数据竞争。
			c := s.guacdConn()
			if c == nil {
				g.finish(s, reasonError)
				return
			}
			if _, werr := c.Write(buf[:n]); werr != nil {
				g.finish(s, reasonError)
				return
			}
		}
		if err != nil {
			// 浏览器断开/读限超限 → client_closed。
			g.finish(s, reasonClientClosed)
			return
		}
	}
}

// finish 幂等收尾一个会话（契约 6.5 生命周期）：
// 关对端 → 会话行 closed（bytes 取 .guac stat 近似值）→ 审计 session.end。
func (g *Gateway) finish(s *rdpLiveSession, reason string) {
	s.closeOnce.Do(func() {
		close(s.done)

		if c := s.guacdConn(); c != nil {
			_ = c.Close() // 解除 guacd 读阻塞
		}
		_ = s.ws.Close() // 解除 WS 读阻塞

		ioDone := make(chan struct{})
		go func() { s.ioWG.Wait(); close(ioDone) }()
		select {
		case <-ioDone:
		case <-time.After(bridgeStopTimeout):
			g.log.Warn("rdpx: bridge goroutines did not stop in time", "session_id", s.id)
		}

		g.mu.Lock()
		delete(g.live, s.id)
		if cur, ok := g.servers[s.serverID]; ok && cur == s.id {
			delete(g.servers, s.serverID)
		}
		g.mu.Unlock()

		// .guac 由 guacd 写入共享卷；结束时 stat 一次取近似值
		//（契约 4.14；文件不存在不算错误，has_recording=false）。
		bytes := g.recordingBytes(s.id)

		endAt := g.clock().UTC()
		closeCtx, cancel := context.WithTimeout(context.Background(), dbCloseTimeout)
		defer cancel()
		if _, err := g.st.RdpSessions().Close(closeCtx, store.CloseInteractiveSessionParams{
			ID:           s.id,
			Reason:       reason,
			EndedAt:      endAt,
			BytesWritten: bytes,
		}); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				// 行关闭失败（DB 抖动等）仍补发 session.end：会话事实上已
				// 结束，配对审计不能因落库失败而静默丢失（评审发现）。
				g.log.Error("rdpx: close session row failed", "session_id", s.id, "error", err)
				g.emitEndAudit(s, reason, endAt, bytes, "close_failed")
			}
			// 并发双断开时先到者已生效；ErrConflict 不重复审计。
			return
		}

		g.emitEndAudit(s, reason, endAt, bytes, "")
	})
}

// emitEndAudit 写 session.end；note 非 ” 时补充说明（close_failed 表示
// 会话行未能落库但会话事实上已结束）。
func (g *Gateway) emitEndAudit(s *rdpLiveSession, reason string, endAt time.Time, bytes int64, note string) {
	detail := map[string]any{
		"reason":        reason,
		"duration_ms":   endAt.Sub(s.startedAt).Milliseconds(),
		"bytes_written": bytes,
	}
	if note != "" {
		detail["note"] = note
	}
	g.enqueueAudit(s.id, s.userID, s.serverID, "session.end", detail)
}

// recordingBytes stat <RecordingsDir>/<id>.guac 的当前大小；不存在返回 0。
func (g *Gateway) recordingBytes(sessionID string) int64 {
	if g.cfg.RecordingsDir == "" {
		return 0
	}
	info, err := os.Stat(filepath.Join(g.cfg.RecordingsDir, sessionID+guacFileSuffix))
	if err != nil || info.IsDir() {
		return 0
	}
	return info.Size()
}

// CloseByServer 结束某服务器的全部活动会话（服务器删除前调用）。
func (g *Gateway) CloseByServer(ctx context.Context, serverID, reason string) {
	g.mu.Lock()
	targets := make([]*rdpLiveSession, 0, 1)
	for _, s := range g.live {
		if s.serverID == serverID {
			targets = append(targets, s)
		}
	}
	g.mu.Unlock()

	done := make(chan struct{}, len(targets))
	for _, s := range targets {
		go func(s *rdpLiveSession) {
			g.finish(s, reason)
			done <- struct{}{}
		}(s)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for range targets {
		select {
		case <-done:
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		}
	}
}

// Shutdown 结束全部活动会话（ended_reason=server_shutdown），同步封盘（≤5s）。
func (g *Gateway) Shutdown(ctx context.Context) {
	g.mu.Lock()
	g.closed = true
	targets := make([]*rdpLiveSession, 0, len(g.live))
	for _, s := range g.live {
		targets = append(targets, s)
	}
	g.mu.Unlock()

	g.govOnce.Do(func() { close(g.govStop) })

	done := make(chan struct{}, len(targets))
	for _, s := range targets {
		go func(s *rdpLiveSession) {
			g.finish(s, reasonServerShutdown)
			done <- struct{}{}
		}(s)
	}
	for range targets {
		select {
		case <-done:
		case <-ctx.Done():
			g.govWG.Wait()
			return
		}
	}
	g.govWG.Wait()
}

// ---------------------------------------------------------------------------
// 治理循环（空闲/最大时长/TouchActivity 节流/孤儿行回收）
// ---------------------------------------------------------------------------

func (g *Gateway) governLoop() {
	defer g.govWG.Done()
	ticker := time.NewTicker(governInterval)
	defer ticker.Stop()
	tick := 0
	for {
		select {
		case <-g.govStop:
			return
		case now := <-ticker.C:
			tick++
			g.governOnce(now, tick%orphanScanEvery == 0)
		}
	}
}

func (g *Gateway) governOnce(now time.Time, scanOrphans bool) {
	g.mu.Lock()
	live := make([]*rdpLiveSession, 0, len(g.live))
	for _, s := range g.live {
		live = append(live, s)
	}
	g.mu.Unlock()

	for _, s := range live {
		activity := time.Unix(0, s.activity.Load())
		if now.Sub(activity) >= g.cfg.IdleTimeout {
			g.finish(s, reasonIdleTimeout)
			continue
		}
		if now.Sub(s.startedAt) >= g.cfg.MaxDuration {
			g.finish(s, reasonMaxDuration)
			continue
		}
		lastTouch := time.Unix(0, s.lastTouch.Load())
		if s.activity.Load() > lastTouch.UnixNano() && now.Sub(lastTouch) >= touchThrottle {
			s.lastTouch.Store(now.UnixNano())
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := g.st.RdpSessions().TouchActivity(ctx, s.id, now.UTC(), g.recordingBytes(s.id)); err != nil {
				g.log.Warn("rdpx: touch activity failed", "session_id", s.id, "error", err)
			}
			cancel()
		}
	}

	// 孤儿行：DB active 但既不在 live 也不在 pending（重启残留或 Open 后
	// 客户端弃单）。空闲超时后按 idle_timeout 收尾（无归属内存信息，不审计）。
	if !scanOrphans {
		return
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		rows, err := g.st.RdpSessions().List(ctx, store.InteractiveSessionFilter{
			Status: store.SessionActive,
			Limit:  200,
		})
		cancel()
		if err != nil {
			g.log.Warn("rdpx: orphan scan failed", "error", err)
			return
		}
		for _, row := range rows {
			g.mu.Lock()
			_, isLive := g.live[row.ID]
			_, isPending := g.pending[row.ID]
			g.mu.Unlock()
			if isLive || isPending {
				continue
			}
			if now.Sub(row.LastActivityAt) >= g.cfg.IdleTimeout {
				closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, err := g.st.RdpSessions().Close(closeCtx, store.CloseInteractiveSessionParams{
					ID:           row.ID,
					Reason:       reasonIdleTimeout,
					EndedAt:      now.UTC(),
					BytesWritten: g.recordingBytes(row.ID),
				})
				if err != nil && !errors.Is(err, store.ErrConflict) {
					g.log.Warn("rdpx: orphan close failed", "session_id", row.ID, "error", err)
				}
				closeCancel()
				// 孤儿行（重启残留/弃单）同样补发 session.end：Open 已写
				// session.start，收尾缺失会使审计配对断裂（评审发现）。
				if err == nil || !errors.Is(err, store.ErrConflict) {
					g.enqueueAudit(row.ID, "admin", row.ServerID, "session.end", map[string]any{
						"reason":        reasonIdleTimeout,
						"duration_ms":   now.Sub(row.StartedAt).Milliseconds(),
						"bytes_written": row.BytesWritten,
						"note":          "orphan_reaped",
					})
				}
				g.log.Info("rdpx: orphan session reaped", "session_id", row.ID)
			}
		}
	}
}

// enqueueAudit 写一条 session 事件审计（actor_type=user，resource_type=
// rdp_session）；detail 只含非敏感字段（契约 5.14：凭据不进审计）。
func (g *Gateway) enqueueAudit(sessionID, userID, serverID, action string, detail map[string]any) {
	if g.audit == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), auditEnqueueTimeout)
	defer cancel()
	serverIDPtr := serverID
	err := g.audit.Enqueue(ctx, audit.Event{
		ActorType:    "user",
		ActorID:      userID,
		Action:       action,
		ResourceType: "rdp_session",
		ResourceID:   sessionID,
		ServerID:     &serverIDPtr,
		Detail:       detail,
	})
	if err != nil {
		g.log.Error("rdpx: audit enqueue failed", "action", action, "session_id", sessionID, "error", err)
	}
}

// validResolution 校验分辨率范围（OpenAPI RdpSessionCreate）。
func validResolution(width, height, dpi int) bool {
	return width >= minWidth && width <= maxWidth &&
		height >= minHeight && height <= maxHeight &&
		dpi >= minDPI && dpi <= maxDPI
}
