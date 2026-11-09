// W3：终端会话与录制的 HTTP/WS handler（CONTRACT.md 2.5/6.4）。
//
// WS 升级前校验顺序（写死）：Origin 精确匹配（403）→ opsmind_session
// Cookie 鉴权（401）→ 会话存在且 active 且属于当前用户（404/409）→
// 子协议 opsmind-term-v1（400）→ 升级 101。失败响应是普通 JSON 错误体。
package httpapi

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"aiops/internal/apierr"
	"aiops/internal/auth"
	"aiops/internal/store"
	"aiops/internal/termx"
)

// termWSSubprotocol 是终端 WS 的唯一子协议（契约 6.4）。
const termWSSubprotocol = "opsmind-term-v1"

// WS 心跳参数（契约 6.4）：30s ping、90s 无 pong 视为断开。
const (
	wsPingInterval   = 30 * time.Second
	wsPongTimeout    = 90 * time.Second
	wsReadBufferSize = 4096
)

// asciicastContentType 是 .cast 下发的 Content-Type（api/openapi.yaml）。
const asciicastContentType = "application/x-asciicast"

// RegisterTermRoutes 注册 W3 负责的端点。
//
// REST 端点挂 protected（requireAuth）；WS 端点挂裸 api 组——升级前的
// Origin→Cookie→业务→子协议校验由 handler 自行完成（契约 6.4 写死顺序，
// 不复用 GET 不覆盖的 originCheck，也不让 requireAuth 抢在 Origin 之前）。
func (d *Deps) RegisterTermRoutes(protected, api *gin.RouterGroup) {
	protected.POST("/term/sessions", d.handleCreateTermSession)
	protected.GET("/term/sessions", d.handleListTermSessions)
	protected.GET("/term/recordings", d.handleListTermRecordings)
	protected.GET("/term/recordings/:id", d.handleDownloadTermRecording)
	api.GET("/term/sessions/:id/ws", d.handleTermSessionWS)
}

// ---------------------------------------------------------------------------
// 会话 REST
// ---------------------------------------------------------------------------

type termSessionCreateRequest struct {
	ServerID string `json:"server_id"`
	Cols     *int   `json:"cols"`
	Rows     *int   `json:"rows"`
}

func (d *Deps) handleCreateTermSession(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req termSessionCreateRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	if _, err := uuid.Parse(req.ServerID); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "server_id must be a uuid")
		return
	}
	cols, rows := 120, 40
	if req.Cols != nil {
		cols = *req.Cols
	}
	if req.Rows != nil {
		rows = *req.Rows
	}
	if cols < 10 || cols > 500 {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "cols must be between 10 and 500")
		return
	}
	if rows < 4 || rows > 200 {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "rows must be between 4 and 200")
		return
	}
	user, ok := userFromContext(c)
	if !ok {
		writeError(c, apierr.AuthRequired, "authentication required")
		return
	}
	server, err := d.Store.Servers().Get(c.Request.Context(), req.ServerID)
	if err != nil {
		d.handleStoreError(c, err, apierr.ServerNotFound)
		return
	}
	// 三期（N1，契约 2.8.6）：仅 access_proto=ssh 的网络设备可开终端；
	// telnet 返回 400 TERM_UNSUPPORTED（v1 不做 telnet 终端桥）。
	// 请求语义错误（400）优先于服务可用性（500）判定。
	if server.DeviceKind == store.DeviceKindNetworkDevice &&
		server.AccessProto == store.AccessProtoTelnet {
		writeAPIError(c, http.StatusBadRequest, apierr.TermUnsupported,
			"telnet access has no terminal support")
		return
	}
	if d.Term == nil {
		writeError(c, apierr.Internal, "terminal manager unavailable")
		return
	}

	session, err := d.Term.Open(c.Request.Context(), server, user.ID, cols, rows)
	if err != nil {
		d.writeTermOpenError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"session": session,
		"ws_url":  "/api/term/sessions/" + session.ID + "/ws",
	})
}

// writeTermOpenError 映射 termx.Open 的哨兵错误（契约 9.6）。
func (d *Deps) writeTermOpenError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, termx.ErrUnsupported):
		writeAPIError(c, http.StatusBadRequest, apierr.TermUnsupported, "auth method has no pty support")
	case errors.Is(err, termx.ErrSessionExists):
		writeAPIError(c, http.StatusConflict, apierr.TermSessionExists, "server already has an active session")
	case errors.Is(err, termx.ErrSessionLimit):
		writeAPIError(c, http.StatusTooManyRequests, apierr.TermSessionLimit, "terminal session limit reached")
	case errors.Is(err, termx.ErrNoPTYSupport), errors.Is(err, termx.ErrShuttingDown):
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "terminal service unavailable")
	default:
		d.logHandlerError(c, "term open failed", err)
		writeError(c, apierr.Internal, "internal server error")
	}
}

func (d *Deps) handleListTermSessions(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	limit, ok := queryLimit(c)
	if !ok {
		return
	}
	filter := store.InteractiveSessionFilter{Limit: limit}
	if raw := c.Query("status"); raw != "" {
		status := store.InteractiveSessionStatus(raw)
		if status != store.SessionActive && status != store.SessionClosed {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "status must be active or closed")
			return
		}
		filter.Status = status
	}
	if raw := c.Query("server_id"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "server_id must be a uuid")
			return
		}
		filter.ServerID = raw
	}
	items, err := d.Store.TerminalSessions().List(c.Request.Context(), filter)
	if err != nil {
		d.handleStoreError(c, err, apierr.TermSessionNotFound)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// ---------------------------------------------------------------------------
// 录制 REST
// ---------------------------------------------------------------------------

// termRecordingItem 对应 OpenAPI TerminalRecording schema。
type termRecordingItem struct {
	ID           string     `json:"id"`
	ServerID     string     `json:"server_id"`
	ServerName   string     `json:"server_name"`
	OS           string     `json:"os"`
	Username     string     `json:"username"`
	BytesWritten int64      `json:"bytes_written"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

func (d *Deps) handleListTermRecordings(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	limit, ok := queryLimit(c)
	if !ok {
		return
	}
	// 有录制文件的已结束会话（openapi：status=closed 且 recording_file 非空）。
	items, err := d.Store.TerminalSessions().List(c.Request.Context(), store.InteractiveSessionFilter{
		Status: store.SessionClosed,
		Limit:  limit,
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.TermSessionNotFound)
		return
	}
	out := make([]termRecordingItem, 0, len(items))
	for _, s := range items {
		if !s.HasRecording {
			continue
		}
		out = append(out, termRecordingItem{
			ID:           s.ID,
			ServerID:     s.ServerID,
			ServerName:   s.ServerName,
			OS:           string(s.ServerOS),
			Username:     s.Username,
			BytesWritten: s.BytesWritten,
			StartedAt:    s.StartedAt,
			EndedAt:      s.EndedAt,
			CreatedAt:    s.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (d *Deps) handleDownloadTermRecording(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "id must be a uuid")
		return
	}
	session, err := d.Store.TerminalSessions().Get(c.Request.Context(), id)
	if err != nil {
		d.handleStoreError(c, err, apierr.TermSessionNotFound)
		return
	}
	if !session.HasRecording {
		writeAPIError(c, http.StatusNotFound, apierr.RecordingNotFound, "session has no recording")
		return
	}
	dir := d.recordingsDir()
	// 文件名即会话 UUID（DB 行校验过的 <uuid>.cast，无用户可控拼接）。
	path := filepath.Join(dir, session.ID+".cast")
	f, err := os.Open(path)
	if err != nil {
		d.logHandlerError(c, "recording file unavailable", err)
		writeAPIError(c, http.StatusBadGateway, apierr.RecordingUnavailable, "recording file unavailable")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		writeAPIError(c, http.StatusBadGateway, apierr.RecordingUnavailable, "recording file unavailable")
		return
	}
	// ServeContent 处理 Range/条件请求并防止越界读取；Content-Type 固定。
	c.Header("Content-Type", asciicastContentType)
	c.Header("X-Content-Type-Options", "nosniff")
	http.ServeContent(c.Writer, c.Request, session.ID+".cast", info.ModTime(), f)
}

// recordingsDir 返回录制目录（默认 <DataDir>/recordings，契约 4.13）。
func (d *Deps) recordingsDir() string {
	if d.Config != nil && d.Config.RecordingsDir != "" {
		return d.Config.RecordingsDir
	}
	return "./data/recordings"
}

// ---------------------------------------------------------------------------
// 终端 WebSocket
// ---------------------------------------------------------------------------

// termWSUpgrader 只接受 opsmind-term-v1 子协议；Origin 已在升级前精确校验。
var termWSUpgrader = websocket.Upgrader{
	Subprotocols:    []string{termWSSubprotocol},
	ReadBufferSize:  wsReadBufferSize,
	WriteBufferSize: wsReadBufferSize,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// termWSConn 把 gorilla/websocket 适配为 termx.Conn（契约 5.13），
// 并实现 30s ping / 90s pong deadline 心跳（契约 6.4）。
type termWSConn struct {
	conn *websocket.Conn
}

func (a *termWSConn) ReadJSON(v any) error  { return a.conn.ReadJSON(v) }
func (a *termWSConn) WriteJSON(v any) error { return a.conn.WriteJSON(v) }
func (a *termWSConn) SetReadLimit(n int64)  { a.conn.SetReadLimit(n) }
func (a *termWSConn) Close() error {
	// Close 帧与关闭提示 best effort；心跳 goroutine 随连接错误退出。
	_ = a.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	return a.conn.Close()
}

// startHeartbeat 启动 30s ping 循环与 90s pong 读超时；连接关闭后退出。
func (a *termWSConn) startHeartbeat() {
	_ = a.conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
	a.conn.SetPongHandler(func(string) error {
		return a.conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
	})
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := a.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		}
	}()
}

func (d *Deps) handleTermSessionWS(c *gin.Context) {
	// 1. Origin 精确匹配（originCheck 中间件不覆盖 GET，须自行校验）。
	origin := c.GetHeader("Origin")
	if d.Config == nil || !d.Config.OriginAllowed(origin) {
		writeError(c, apierr.OriginForbidden, "origin not allowed")
		return
	}
	// 2. opsmind_session Cookie 鉴权。
	if d.Auth == nil {
		writeError(c, apierr.Internal, "authentication unavailable")
		return
	}
	raw, err := c.Cookie(sessionCookieName)
	if err != nil || raw == "" {
		writeError(c, apierr.AuthRequired, "authentication required")
		return
	}
	user, err := d.Auth.Authenticate(c.Request.Context(), raw)
	if err != nil {
		if errors.Is(err, auth.ErrAuthRequired) {
			writeError(c, apierr.AuthRequired, "authentication required")
			return
		}
		d.logHandlerError(c, "authenticate failed", err)
		writeError(c, apierr.Internal, "internal server error")
		return
	}
	// 3. 业务校验：会话存在且 active 且属于当前用户。
	if d.Term == nil || !d.requireStore(c) {
		writeError(c, apierr.Internal, "terminal manager unavailable")
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "id must be a uuid")
		return
	}
	session, err := d.Store.TerminalSessions().Get(c.Request.Context(), id)
	if err != nil {
		d.handleStoreError(c, err, apierr.TermSessionNotFound)
		return
	}
	if session.Status != store.SessionActive {
		writeAPIError(c, http.StatusConflict, apierr.Conflict, "session is not active")
		return
	}
	if session.Username != user.Username {
		writeAPIError(c, http.StatusConflict, apierr.Conflict, "session belongs to another user")
		return
	}
	// 4. 子协议必须是 opsmind-term-v1。
	if !hasSubprotocol(c, termWSSubprotocol) {
		writeAPIError(c, http.StatusBadRequest, apierr.TermWSProtocol, "subprotocol opsmind-term-v1 is required")
		return
	}
	// 5. 升级 101。
	ws, err := termWSUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade 已写非 101 响应；此处只记日志。
		d.logHandlerError(c, "ws upgrade failed", err)
		return
	}
	conn := &termWSConn{conn: ws}
	conn.startHeartbeat()

	// 解密凭据并构造 SSH 目标；明文只在本次调用的栈内存在。
	server, err := d.Store.Servers().Get(c.Request.Context(), session.ServerID)
	if err != nil {
		_ = conn.WriteJSON(termx.Frame{Type: "error", Code: apierr.TermConnectFailed, Message: "server unavailable"})
		_ = conn.WriteJSON(termx.Frame{Type: "closed", Reason: "error"})
		_ = conn.Close()
		return
	}
	target, err := d.sshTarget(server)
	if err != nil {
		d.logHandlerError(c, "build ssh target failed", err)
		_ = conn.WriteJSON(termx.Frame{Type: "error", Code: apierr.TermConnectFailed, Message: termx.DescribeDialError(err)})
		_ = conn.WriteJSON(termx.Frame{Type: "closed", Reason: "error"})
		_ = conn.Close()
		return
	}
	// Attach 内部完成 PTY 拨号与完整生命周期收尾；连接由 termx 关闭。
	if err := d.Term.Attach(conn, session.ID, user.ID, requestIDOf(c), d.requestIP(c), target); err != nil {
		// 升级已完成的失败：错误帧 + closed 帧（契约 6.4 生命周期）。
		if errors.Is(err, termx.ErrNotActive) {
			_ = conn.WriteJSON(termx.Frame{Type: "error", Code: apierr.Conflict, Message: "session is not active"})
		} else {
			_ = conn.WriteJSON(termx.Frame{Type: "error", Code: apierr.TermConnectFailed, Message: termx.DescribeDialError(err)})
		}
		_ = conn.WriteJSON(termx.Frame{Type: "closed", Reason: "error"})
		_ = conn.Close()
	}
}

// hasSubprotocol 判断请求声明的子协议是否包含目标值。
func hasSubprotocol(c *gin.Context, want string) bool {
	for _, p := range websocket.Subprotocols(c.Request) {
		if p == want {
			return true
		}
	}
	return false
}
