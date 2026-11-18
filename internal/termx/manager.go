// W3：终端会话治理（CONTRACT.md 5.13/6.4）。
//
// Manager 负责会话上限、空闲/最大时长、asciinema v2 录制封盘、
// PTY↔WS 桥生命周期与审计接入。WS 连接抽象由 httpapi 注入（gorilla 适配），
// 本包不 import Gin。
package termx

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"aiops/internal/audit"
	"aiops/internal/logx"
	"aiops/internal/sshx"
	"aiops/internal/store"
)

// 契约默认值（CONTRACT.md 5.13 / config 5.12）。
const (
	defaultMaxSessions = 4
	defaultIdleTimeout = 10 * time.Minute
	defaultMaxDuration = 8 * time.Hour

	// 治理循环间隔与 TouchActivity 节流（契约 5.13：节流至 1 次/5s）。
	governInterval = time.Second
	touchThrottle  = 5 * time.Second

	// 孤儿行（Open 未 Attach/重启残留）扫描间隔；空闲判定 10 分钟级，
	// 无需每秒查库。
	orphanScanEvery = 5

	// 交互会话授权复检间隔（契约 12.3 第 13 条：WS 每 30 秒复检用户
	// enabled/权限仍在）。按 1s 治理 tick 计数。
	authRecheckEvery = 30

	// 收尾各阶段预算：Shutdown 同步封盘总预算 ≤5s（契约 2.1）。
	auditEnqueueTimeout = 2 * time.Second
	bridgeStopTimeout   = 2 * time.Second
	dbCloseTimeout      = 1 * time.Second

	ptyTerm = "xterm-256color"
)

// Config 是 Manager 配置（零值字段应用契约默认）。
type Config struct {
	MaxSessions   int           // 默认 4
	IdleTimeout   time.Duration // 默认 10m
	MaxDuration   time.Duration // 默认 8h
	RecordingsDir string        // .cast 落盘目录
	FramesPerTick int           // 固定 64：单次写盘 tick 聚合的事件上限
}

// Conn 是 WS 连接抽象（gorilla/websocket 适配由 httpapi 注入）。
type Conn interface {
	ReadJSON(v any) error
	WriteJSON(v any) error
	SetReadLimit(n int64)
	Close() error
}

// 错误哨兵（契约 5.13）。
var (
	ErrSessionExists = errors.New("termx: server already has an active session")
	ErrSessionLimit  = errors.New("termx: terminal session limit reached")
	ErrUnsupported   = errors.New("termx: auth method has no PTY support")
	ErrNotActive     = errors.New("termx: session is not active")
	ErrShuttingDown  = errors.New("termx: manager is shutting down")
	ErrNoPTYSupport  = errors.New("termx: ssh executor has no PTY support")
	// ErrConnectFailed 表示 WS 已升级、会话行已创建，但 PTY/SSH 拨号失败。
	// 会话已按 error 收尾（error/closed 帧已由 manager 写出并关闭连接）；
	// 调用方不得再写帧，只需回滚创建会话时消费的终端授权
	//（契约 12.3 第 12 条：消费后会话 Open 失败要保留 grant 可重试）。
	ErrConnectFailed = errors.New("termx: pty dial failed")
)

// pendingSession 是 Open 后、Attach 前的会话占位（归属校验用）。
type pendingSession struct {
	id        string
	serverID  string
	userID    string
	authID    string
	startedAt time.Time
}

// liveSession 是已 Attach 的活动会话。
type liveSession struct {
	id         string
	serverID   string
	userID     string
	authID     string
	serverName string
	os         store.ServerOS
	requestID  string
	ip         string
	cols, rows int
	startedAt  time.Time

	// PTY 会话（Attach 拨号完成后非 nil）。拨号发生在 m.mu 释放之后，
	// 治理循环/CloseByServer/CloseAll 可能并行 finish 读它——必须原子
	// 访问，否则构成数据竞争（rdpx 同构缺陷 -race 实测）。
	pty atomic.Value // sshx.PTYSession
	ws  Conn
	// Recorder（nil=录制降级，写入失败不中断会话）。Attach 在锁外完成
	// NewRecorder 后写入，治理循环 governOnce/finish 并发读——与 pty 同理
	// 必须原子访问（-race 实测 Attach 写 rec vs governOnce 读 rec 竞争）。
	rec atomic.Pointer[Recorder]
	sum inputSummary

	activity  atomic.Int64 // 最后活动 UnixNano（PTY 输出或 WS input/resize）
	lastTouch atomic.Int64 // 最后 TouchActivity 落库 UnixNano

	sendCh    chan Frame
	done      chan struct{}
	senderWG  sync.WaitGroup // runSender（单一 writer）
	ioWG      sync.WaitGroup // ptyReadLoop + wsReadLoop
	closeOnce sync.Once
}

// Manager 管理全部终端会话。
type Manager struct {
	cfg    Config
	st     store.Store
	ssh    sshx.Executor
	opener sshx.PTYOpener
	audit  *audit.Writer
	clock  func() time.Time
	log    *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingSession
	live    map[string]*liveSession
	servers map[string]string // serverID → sessionID（同服务器单会话）
	closed  bool
	// authorize 每 30 秒复检一次（用户 enabled/角色权限仍在）。由装配层
	// 注入，nil 表示不做周期复检（单测/骨架）。
	authorize func(ctx context.Context, userID string) error
	// sessionAuthorize 是带 auth session id 的复检（契约 12.3 第 13 条：
	// 每 30 秒用 Sessions.Get(sessionID) 复检会话仍存在且未过期）。设置后
	// 优先于 authorize；nil 时回落到 authorize。
	sessionAuthorize func(ctx context.Context, userID, authSessionID string) error

	govWG   sync.WaitGroup
	govStop chan struct{}
	govOnce sync.Once
}

// NewManager 构造 Manager 并启动治理循环。
func NewManager(cfg Config, st store.Store, ssh sshx.Executor, audit *audit.Writer, clock func() time.Time, log *slog.Logger) *Manager {
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultMaxSessions
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	if cfg.MaxDuration <= 0 {
		cfg.MaxDuration = defaultMaxDuration
	}
	if cfg.FramesPerTick <= 0 {
		cfg.FramesPerTick = defaultFramesPerTick
	}
	if clock == nil {
		clock = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		cfg:     cfg,
		st:      st,
		ssh:     ssh,
		audit:   audit,
		clock:   clock,
		log:     log,
		pending: make(map[string]*pendingSession),
		live:    make(map[string]*liveSession),
		servers: make(map[string]string),
		govStop: make(chan struct{}),
	}
	if opener, ok := ssh.(sshx.PTYOpener); ok {
		m.opener = opener
	} else if ssh != nil {
		m.log.Warn("termx: ssh executor lacks PTY support; sessions will fail", "type", fmt.Sprintf("%T", ssh))
	}
	m.govWG.Add(1)
	go m.governLoop()
	return m
}

// Open 创建会话行并占用槽位（进程内串行 + DB 活动行约定）：
// 同服务器已有活动会话返回 ErrSessionExists；全局满返回 ErrSessionLimit；
// WinRM 认证返回 ErrUnsupported。成功写审计 session.start。
// 不建立 WS 连接；WS handler 随后调 Attach。
func (m *Manager) Open(ctx context.Context, server store.Server, userID string, cols, rows int) (store.TerminalSession, error) {
	return m.OpenForAuthSession(ctx, server, userID, "", "", cols, rows)
}

// OpenForAuthSession 与 Open 相同，额外记录发起本次会话的 auth session id
// （契约 12.3 第 13 条：live session 按 auth_session_id 建键，logout 可精确
// 关闭本次登录建立的 shell）。空 authSessionID 表示不追踪（单测/骨架）。
// grantID 是本次会话消费的 terminal grant，持久化到会话行，WS 拨号失败时
// 供 httpapi 回滚 consumed_at（空串=无来源授权）。
func (m *Manager) OpenForAuthSession(ctx context.Context, server store.Server, userID, authSessionID, grantID string, cols, rows int) (store.TerminalSession, error) {
	switch server.AuthMethod {
	case store.AuthMethodPasswordNTLM, store.AuthMethodPasswordBasic:
		return store.TerminalSession{}, ErrUnsupported
	}
	if cols == 0 {
		cols = 120
	}
	if rows == 0 {
		rows = 40
	}
	if !validWindowSize(cols, rows) {
		return store.TerminalSession{}, fmt.Errorf("termx: invalid window %dx%d", cols, rows)
	}
	if m.opener == nil {
		return store.TerminalSession{}, ErrNoPTYSupport
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return store.TerminalSession{}, ErrShuttingDown
	}
	// DB 是跨重启的权威：活动行（含重启残留孤儿）同样占位。
	byServer, err := m.st.TerminalSessions().CountActiveByServer(ctx, server.ID)
	if err != nil {
		return store.TerminalSession{}, fmt.Errorf("termx: count active by server: %w", err)
	}
	if byServer > 0 {
		return store.TerminalSession{}, ErrSessionExists
	}
	total, err := m.st.TerminalSessions().CountActive(ctx)
	if err != nil {
		return store.TerminalSession{}, fmt.Errorf("termx: count active: %w", err)
	}
	if total >= int64(m.cfg.MaxSessions) {
		return store.TerminalSession{}, ErrSessionLimit
	}

	session, err := m.st.TerminalSessions().Create(ctx, store.CreateTerminalSessionParams{
		ServerID:  server.ID,
		UserID:    userID,
		GrantID:   grantID,
		Cols:      cols,
		Rows:      rows,
		StartedAt: m.clock().UTC(),
	})
	if err != nil {
		return store.TerminalSession{}, fmt.Errorf("termx: create session row: %w", err)
	}
	m.pending[session.ID] = &pendingSession{
		id:        session.ID,
		serverID:  server.ID,
		userID:    userID,
		authID:    authSessionID,
		startedAt: m.clock(),
	}
	m.enqueueAudit(session.ID, userID, server.ID, "session.start", map[string]any{
		"os":          string(server.OS),
		"cols":        cols,
		"rows":        rows,
		"server_name": server.Name,
	})
	return session, nil
}

// Attach 把已升级的 WS 连接与会话绑定，拨 PTY、启动双向桥、录制器与
// 治理定时器；返回后桥在独立 goroutine 运行直至 Close。
// 会话非 active（不存在/已结束/不属于该用户/已被绑定）返回 ErrNotActive。
//
// target 携带调用方（httpapi）解密后的 SSH 目标：凭据仅在进程内传递，
// 不落日志、不进审计、不进 WS。attachErr 非空时连接由本方法收尾关闭。
func (m *Manager) Attach(ws Conn, sessionID, userID, requestID, ip string, target sshx.Target) error {
	return m.AttachForAuthSession(ws, sessionID, userID, "", requestID, ip, target)
}

// AttachForAuthSession 与 Attach 相同，额外绑定 auth session id（logout 按
// auth session 精确关闭；空串表示不追踪，单测/骨架路径）。
func (m *Manager) AttachForAuthSession(ws Conn, sessionID, userID, authSessionID, requestID, ip string, target sshx.Target) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrShuttingDown
	}
	p, ok := m.pending[sessionID]
	if !ok {
		m.mu.Unlock()
		return ErrNotActive
	}
	if p.userID != userID {
		m.mu.Unlock()
		return ErrNotActive
	}
	delete(m.pending, sessionID)
	row, err := m.st.TerminalSessions().Get(context.Background(), sessionID)
	if err != nil || row.Status != store.SessionActive {
		m.mu.Unlock()
		return ErrNotActive
	}
	s := &liveSession{
		id:         sessionID,
		serverID:   p.serverID,
		userID:     userID,
		authID:     authSessionID,
		serverName: row.ServerName,
		os:         row.ServerOS,
		requestID:  requestID,
		ip:         ip,
		cols:       row.Cols,
		rows:       row.Rows,
		startedAt:  row.StartedAt,
		ws:         ws,
		sendCh:     make(chan Frame, 128),
		done:       make(chan struct{}),
	}
	now := m.clock()
	s.activity.Store(now.UnixNano())
	s.lastTouch.Store(now.UnixNano())
	// WaitGroup 计数必须在注册进 live（此后 CloseByServer/治理循环可
	// 触达 finish→Wait）之前累加完毕：sync 语义要求正增量 Add 先于 Wait，
	// 否则 Add 与 Wait 并发读写内部 state 构成数据竞争（-race 实测：
	// Attach 锁外 Add vs CloseByServer→finish→senderWG.Wait）。
	// PTY 拨号失败路径用哑 goroutine 冲销 ioWG，全程无 Add 负增量。
	s.senderWG.Add(1)
	s.ioWG.Add(2)
	m.live[sessionID] = s
	m.servers[p.serverID] = sessionID
	m.mu.Unlock()

	// 拨 PTY；失败（含执行器无 PTY 能力）走完整收尾（error 帧 + closed 帧
	// + 会话行 + 审计）。
	var pty sshx.PTYSession
	if m.opener == nil {
		err = ErrNoPTYSupport
	} else {
		pty, err = m.opener.Session(context.Background(), target, sshx.SessionRequest{
			Cols: s.cols,
			Rows: s.rows,
			Term: ptyTerm,
		})
	}
	if err != nil {
		m.log.Error("termx: pty dial failed", "session_id", sessionID, "error", err)
		go func() { defer s.senderWG.Done(); s.runSender() }()
		// 拨号失败不启动两个 reader：哑 goroutine 立即冲销预加的 ioWG
		// 计数（不用 Add 负数——可能与并发 finish 的 Wait 竞争）。
		go func() { s.ioWG.Done() }()
		go func() { s.ioWG.Done() }()
		// 走查 6：按底层错误分类生成中文短语（认证失败/不可达/超时），
		// 不再写死英文技术串，用户可区分密码错与网络问题。
		s.trySend(errorFrame("TERM_CONNECT_FAILED", DescribeDialError(err)))
		m.finish(s, reasonError, pty)
		// 会话已按 error 收尾并关闭 WS；把「拨号失败」作为哨兵返回，让
		// httpapi 回滚创建会话时消费的 grant（不再重复写帧）。
		return ErrConnectFailed
	}
	s.setPTY(pty)

	if m.cfg.RecordingsDir != "" {
		rec, recErr := NewRecorder(m.cfg.RecordingsDir, sessionID, s.cols, s.rows, m.clock())
		if recErr != nil {
			m.log.Error("termx: recorder init failed; session continues without recording",
				"session_id", sessionID, "error", recErr)
		} else {
			rec.frames = m.cfg.FramesPerTick
			s.rec.Store(rec)
		}
	}

	ws.SetReadLimit(16 * 1024) // base64 后的 JSON 帧上限（契约 6.4）

	go func() { defer s.senderWG.Done(); s.runSender() }()
	go func() { defer s.ioWG.Done(); m.runPTYReader(s) }()
	go func() { defer s.ioWG.Done(); m.runWSReader(s) }()
	return nil
}

// runSender 是单一 WS writer goroutine（契约 6.4：写并发串行化）。
// 收到 closed 哨兵帧：写出后退出；done 触发时先 drain 队列（closed 帧
// 在 close(done) 之前入队，drain 保证最后一帧不被截断）。
func (s *liveSession) runSender() {
	for {
		select {
		case f := <-s.sendCh:
			if err := s.ws.WriteJSON(f); err != nil {
				return
			}
			if f.Type == frameTypeClosed {
				return
			}
		case <-s.done:
			for {
				select {
				case f := <-s.sendCh:
					if err := s.ws.WriteJSON(f); err != nil {
						return
					}
					if f.Type == frameTypeClosed {
						return
					}
				default:
					return
				}
			}
		}
	}
}

// trySend 尽力投递一帧（会话结束时缓冲满则丢弃，避免阻塞收尾）。
func (s *liveSession) trySend(f Frame) {
	select {
	case s.sendCh <- f:
	default:
	}
}

// runPTYReader 把 PTY 输出转发到 WS 并录制 o 事件。
func (m *Manager) runPTYReader(s *liveSession) {
	buf := make([]byte, 32*1024)
	// 循环外取一次原子快照即可：本 goroutine 由 Attach 在拨号完成后启动，
	// 之后 pty 引用不再变化；finish 的 Close 由 finish 参数/原子读负责。
	pty := s.currentPTY()
	if pty == nil {
		return
	}
	for {
		n, err := pty.Read(buf)
		if n > 0 {
			now := m.clock()
			s.activity.Store(now.UnixNano())
			if rec := s.recorder(); rec != nil {
				rec.Output(now, buf[:n])
			}
			select {
			case s.sendCh <- outputFrame(buf[:n]):
			case <-s.done:
				return
			}
		}
		if err != nil {
			// io.EOF = 远端 shell 正常退出（用户输入 exit 等）→ client_closed；
			// 其他错误视为通道异常 → error。
			reason := reasonError
			if errors.Is(err, io.EOF) {
				reason = reasonClientClosed
			}
			m.finish(s, reason, s.currentPTY())
			return
		}
	}
}

// runWSReader 消费客户端帧：input → PTY stdin + 摘要；resize → PTY resize。
func (m *Manager) runWSReader(s *liveSession) {
	for {
		var f clientFrame
		if err := s.ws.ReadJSON(&f); err != nil {
			m.finish(s, reasonClientClosed, s.currentPTY())
			return
		}
		now := m.clock()
		switch f.Type {
		case frameTypeInput:
			data, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil || len(data) > maxInputBytes {
				// 解码失败或超限：error 帧后断开（reason=error）。
				s.trySend(errorFrame("COMMAND_TOO_LARGE", "input frame exceeds 8KB"))
				m.finish(s, reasonError, s.currentPTY())
				return
			}
			s.activity.Store(now.UnixNano())
			s.sum.add(data)
			if _, err := s.currentPTY().Write(data); err != nil {
				m.finish(s, reasonError, s.currentPTY())
				return
			}
		case frameTypeResize:
			s.activity.Store(now.UnixNano())
			if !validWindowSize(f.Cols, f.Rows) {
				// 越界 resize：400 帧不断开（契约 5.13）。
				s.trySend(errorFrame("VALIDATION_FAILED", "resize out of range"))
				continue
			}
			if err := s.currentPTY().Resize(f.Cols, f.Rows); err != nil {
				// resize 失败仅记日志不终止会话（契约 5.2）。
				m.log.Warn("termx: pty resize failed", "session_id", s.id, "error", err)
				continue
			}
			s.cols, s.rows = f.Cols, f.Rows
			if rec := s.recorder(); rec != nil {
				rec.Resize(now, f.Cols, f.Rows)
			}
			select {
			case s.sendCh <- resizedFrame(f.Cols, f.Rows):
			case <-s.done:
				return
			}
		default:
			// 未知帧类型：忽略（不回应、不断开）。
		}
	}
}

// finish 幂等收尾一个会话（契约 6.4 生命周期）：
// closed 帧 → 关 PTY → 封盘 .cast → 会话行 closed → 审计 end + input_summary。
// setPTY 记录拨号完成的 PTY（Attach 路径，锁外）。
func (s *liveSession) setPTY(p sshx.PTYSession) {
	if p != nil {
		s.pty.Store(p)
	}
}

// recorder 原子读取 Recorder；未启用录制（RecordingsDir 为空或初始化
// 失败降级）时为 nil。
func (s *liveSession) recorder() *Recorder {
	return s.rec.Load()
}

// currentPTY 原子读取 PTY 会话；未完成拨号时为 nil。
func (s *liveSession) currentPTY() sshx.PTYSession {
	if v := s.pty.Load(); v != nil {
		return v.(sshx.PTYSession)
	}
	return nil
}

func (m *Manager) finish(s *liveSession, reason string, pty sshx.PTYSession) {
	s.closeOnce.Do(func() {
		// 先投递 closed 哨兵帧再放行 done，sender drain 队列后写出并退出。
		s.trySend(closedFrame(reason))
		close(s.done)

		if pty != nil {
			_ = pty.Close() // 解除 PTY reader 阻塞
		}
		// 等 sender 写完最后一帧（closed）再关 ws，避免截断；
		// ws.Close() 随后解除 WS reader 的 ReadJSON 阻塞。
		senderDone := make(chan struct{})
		go func() { s.senderWG.Wait(); close(senderDone) }()
		select {
		case <-senderDone:
		case <-time.After(1 * time.Second):
			m.log.Warn("termx: sender did not drain in time", "session_id", s.id)
		}
		_ = s.ws.Close() // 解除 WS reader 阻塞

		ioDone := make(chan struct{})
		go func() { s.ioWG.Wait(); close(ioDone) }()
		select {
		case <-ioDone:
		case <-time.After(bridgeStopTimeout):
			m.log.Warn("termx: bridge goroutines did not stop in time", "session_id", s.id)
		}

		m.mu.Lock()
		delete(m.live, s.id)
		if cur, ok := m.servers[s.serverID]; ok && cur == s.id {
			delete(m.servers, s.serverID)
		}
		m.mu.Unlock()

		bytes := int64(0)
		if rec := s.recorder(); rec != nil {
			var err error
			bytes, err = rec.Close()
			if err != nil {
				m.log.Error("termx: recording close failed", "session_id", s.id, "error", err)
				bytes = 0 // 封盘失败按无录制处理
			}
		}

		endAt := m.clock().UTC()
		closeCtx, cancel := context.WithTimeout(context.Background(), dbCloseTimeout)
		defer cancel()
		if _, err := m.st.TerminalSessions().Close(closeCtx, store.CloseInteractiveSessionParams{
			ID:           s.id,
			Reason:       reason,
			EndedAt:      endAt,
			BytesWritten: bytes,
		}); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				// 行关闭失败（DB 抖动等）仍补发 session.end：会话事实上已
				// 结束，配对审计不能因落库失败而静默丢失（评审发现）。
				m.log.Error("termx: close session row failed", "session_id", s.id, "error", err)
				m.emitEndAudit(s, reason, endAt, bytes, "close_failed")
			}
			// 并发双断开时先到者已生效；ErrConflict 不重复审计。
			return
		}

		lines, truncated, count := s.sum.snapshot()
		m.enqueueAudit(s.id, s.userID, s.serverID, "session.input_summary", logx.Redact(map[string]any{
			"lines":      lines,
			"truncated":  truncated,
			"line_count": count,
		}))
		m.emitEndAudit(s, reason, endAt, bytes, "")
	})
}

// emitEndAudit 写 session.end；note 非 ” 时补充说明（如 close_failed
// 表示会话行未能落库但会话事实上已结束）。
func (m *Manager) emitEndAudit(s *liveSession, reason string, endAt time.Time, bytes int64, note string) {
	detail := map[string]any{
		"reason":        reason,
		"duration_ms":   endAt.Sub(s.startedAt).Milliseconds(),
		"bytes_written": bytes,
	}
	if note != "" {
		detail["note"] = note
	}
	m.enqueueAudit(s.id, s.userID, s.serverID, "session.end", detail)
}

// SetAuthorizer 注入周期授权复检（装配期调用，之后只读）。
func (m *Manager) SetAuthorizer(fn func(ctx context.Context, userID string) error) {
	m.mu.Lock()
	m.authorize = fn
	m.mu.Unlock()
}

// SetSessionAuthorizer 注入带 auth session id 的周期复检（logout/TTL 后
// 既有 WS 必须断开；nil 回落 SetAuthorizer 的仅用户复检）。
func (m *Manager) SetSessionAuthorizer(fn func(ctx context.Context, userID, authSessionID string) error) {
	m.mu.Lock()
	m.sessionAuthorize = fn
	m.mu.Unlock()
}

// CloseByAuthSession 结束某 auth session 名下的全部交互会话（登出）。
// pending（Open 未 Attach）与 live 都关闭；live 走完整封盘与 session.end 审计。
func (m *Manager) CloseByAuthSession(ctx context.Context, authSessionID, reason string) {
	if authSessionID == "" {
		return
	}
	m.mu.Lock()
	pendingTargets := make([]*pendingSession, 0, 1)
	for id, p := range m.pending {
		if p.authID == authSessionID {
			pendingTargets = append(pendingTargets, p)
			delete(m.pending, id)
		}
	}
	targets := make([]*liveSession, 0, 1)
	for _, s := range m.live {
		if s.authID == authSessionID {
			targets = append(targets, s)
		}
	}
	m.mu.Unlock()

	now := m.clock().UTC()
	for _, p := range pendingTargets {
		closeCtx, cancel := context.WithTimeout(ctx, dbCloseTimeout)
		_, _ = m.st.TerminalSessions().Close(closeCtx, store.CloseInteractiveSessionParams{
			ID: p.id, Reason: reason, EndedAt: now,
		})
		cancel()
	}
	m.finishAll(ctx, targets, reason)
}

// finishAll 并行收尾一组 live 会话，等待完成或超时（≤5s）。
func (m *Manager) finishAll(ctx context.Context, targets []*liveSession, reason string) {
	if len(targets) == 0 {
		return
	}
	done := make(chan struct{}, len(targets))
	for _, s := range targets {
		go func(s *liveSession) {
			m.finish(s, reason, s.currentPTY())
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

// CloseByUser 结束某用户的全部交互会话（停用/改角色/吊销会话/重置密码时
// 调用）。pending（已 Open 未 Attach）与 live（已建立 WS）都关闭；live 走
// 完整封盘与 session.end 审计。
func (m *Manager) CloseByUser(ctx context.Context, userID, reason string) {
	m.mu.Lock()
	pendingTargets := make([]*pendingSession, 0, 1)
	for id, p := range m.pending {
		if p.userID == userID {
			pendingTargets = append(pendingTargets, p)
			delete(m.pending, id)
		}
	}
	targets := make([]*liveSession, 0, 1)
	for _, s := range m.live {
		if s.userID == userID {
			targets = append(targets, s)
		}
	}
	m.mu.Unlock()

	now := m.clock().UTC()
	for _, p := range pendingTargets {
		closeCtx, cancel := context.WithTimeout(ctx, dbCloseTimeout)
		_, _ = m.st.TerminalSessions().Close(closeCtx, store.CloseInteractiveSessionParams{
			ID: p.id, Reason: reason, EndedAt: now,
		})
		cancel()
	}
	m.finishAll(ctx, targets, reason)
}

// CloseByServer 结束某服务器的全部活动会话（服务器删除前调用）。
func (m *Manager) CloseByServer(ctx context.Context, serverID, reason string) {
	m.mu.Lock()
	targets := make([]*liveSession, 0, 1)
	for _, s := range m.live {
		if s.serverID == serverID {
			targets = append(targets, s)
		}
	}
	m.mu.Unlock()

	done := make(chan struct{}, len(targets))
	for _, s := range targets {
		go func(s *liveSession) {
			m.finish(s, reason, s.currentPTY())
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
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	targets := make([]*liveSession, 0, len(m.live))
	for _, s := range m.live {
		targets = append(targets, s)
	}
	m.mu.Unlock()

	m.govOnce.Do(func() { close(m.govStop) })

	done := make(chan struct{}, len(targets))
	for _, s := range targets {
		go func(s *liveSession) {
			m.finish(s, reasonServerShutdown, s.currentPTY())
			done <- struct{}{}
		}(s)
	}
	for range targets {
		select {
		case <-done:
		case <-ctx.Done():
			m.govWG.Wait()
			return
		}
	}
	m.govWG.Wait()
}

// governLoop 每秒扫描：空闲/最大时长/录制 flush/TouchActivity 节流；
// 每 orphanScanEvery 个 tick 扫一次 DB 孤儿行（Open 未 Attach、进程重启
// 残留）按 idle_timeout 收尾。
func (m *Manager) governLoop() {
	defer m.govWG.Done()
	ticker := time.NewTicker(governInterval)
	defer ticker.Stop()
	tick := 0
	for {
		select {
		case <-m.govStop:
			return
		case now := <-ticker.C:
			tick++
			m.governOnce(now, tick%orphanScanEvery == 0, tick%authRecheckEvery == 0)
		}
	}
}

func (m *Manager) governOnce(now time.Time, scanOrphans, recheckAuth bool) {
	m.mu.Lock()
	authorize := m.authorize
	sessionAuthorize := m.sessionAuthorize
	live := make([]*liveSession, 0, len(m.live))
	for _, s := range m.live {
		live = append(live, s)
	}
	pending := make([]*pendingSession, 0, len(m.pending))
	for _, p := range m.pending {
		pending = append(pending, p)
	}
	m.mu.Unlock()

	// 契约 12.3 第 13 条：每 30 秒按用户复检 enabled/角色权限；失败即结束
	// 会话（停用/降级的用户在既有交互 shell 里不得继续执行命令）。
	check := func(userID, authID string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if sessionAuthorize != nil {
			return sessionAuthorize(ctx, userID, authID)
		}
		if authorize != nil {
			return authorize(ctx, userID)
		}
		return nil
	}
	if recheckAuth && (authorize != nil || sessionAuthorize != nil) {
		for _, p := range pending {
			err := check(p.userID, p.authID)
			if err == nil {
				continue
			}
			m.mu.Lock()
			delete(m.pending, p.id)
			m.mu.Unlock()
			closeCtx, closeCancel := context.WithTimeout(context.Background(), dbCloseTimeout)
			_, _ = m.st.TerminalSessions().Close(closeCtx, store.CloseInteractiveSessionParams{
				ID: p.id, Reason: reasonError, EndedAt: now.UTC(),
			})
			closeCancel()
		}
		for _, s := range live {
			if err := check(s.userID, s.authID); err != nil {
				m.finish(s, reasonError, s.currentPTY())
			}
		}
	}

	for _, s := range live {
		activity := time.Unix(0, s.activity.Load())
		idle := now.Sub(activity)
		if idle >= m.cfg.IdleTimeout {
			m.finish(s, reasonIdleTimeout, s.currentPTY())
			continue
		}
		if now.Sub(s.startedAt) >= m.cfg.MaxDuration {
			m.finish(s, reasonMaxDuration, s.currentPTY())
			continue
		}
		if rec := s.recorder(); rec != nil {
			if rec.ShouldFlush() {
				rec.Flush()
			}
		}
		lastTouch := time.Unix(0, s.lastTouch.Load())
		if s.activity.Load() > lastTouch.UnixNano() && now.Sub(lastTouch) >= touchThrottle {
			s.lastTouch.Store(now.UnixNano())
			bytes := int64(0)
			if rec := s.recorder(); rec != nil {
				bytes = rec.Bytes()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := m.st.TerminalSessions().TouchActivity(ctx, s.id, now.UTC(), bytes); err != nil {
				m.log.Warn("termx: touch activity failed", "session_id", s.id, "error", err)
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
		rows, err := m.st.TerminalSessions().List(ctx, store.InteractiveSessionFilter{
			Status: store.SessionActive,
			Limit:  200,
		})
		cancel()
		if err != nil {
			m.log.Warn("termx: orphan scan failed", "error", err)
			return
		}
		for _, row := range rows {
			m.mu.Lock()
			_, isLive := m.live[row.ID]
			_, isPending := m.pending[row.ID]
			m.mu.Unlock()
			if isLive || isPending {
				continue
			}
			if now.Sub(row.LastActivityAt) >= m.cfg.IdleTimeout {
				closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, err := m.st.TerminalSessions().Close(closeCtx, store.CloseInteractiveSessionParams{
					ID:           row.ID,
					Reason:       reasonIdleTimeout,
					EndedAt:      now.UTC(),
					BytesWritten: row.BytesWritten,
				})
				if err != nil && !errors.Is(err, store.ErrConflict) {
					m.log.Warn("termx: orphan close failed", "session_id", row.ID, "error", err)
				}
				closeCancel()
				// 孤儿行（重启残留/弃单）同样补发 session.end：Open 已写
				// session.start，收尾缺失会使审计配对断裂（评审发现）。
				if err == nil || !errors.Is(err, store.ErrConflict) {
					m.enqueueAudit(row.ID, row.Username, row.ServerID, "session.end", map[string]any{
						"reason":        reasonIdleTimeout,
						"duration_ms":   now.Sub(row.StartedAt).Milliseconds(),
						"bytes_written": row.BytesWritten,
						"note":          "orphan_reaped",
					})
				}
				m.log.Info("termx: orphan session reaped", "session_id", row.ID)
			}
		}
	}
}

// enqueueAudit 写一条 session 事件审计（actor_type=user）。
func (m *Manager) enqueueAudit(sessionID, userID, serverID, action string, detail map[string]any) {
	if m.audit == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), auditEnqueueTimeout)
	defer cancel()
	serverIDPtr := serverID
	err := m.audit.Enqueue(ctx, audit.Event{
		ActorType:    "user",
		ActorID:      userID,
		Action:       action,
		ResourceType: "terminal_session",
		ResourceID:   sessionID,
		ServerID:     &serverIDPtr,
		Detail:       detail,
	})
	if err != nil {
		m.log.Error("termx: audit enqueue failed", "action", action, "session_id", sessionID, "error", err)
	}
}
