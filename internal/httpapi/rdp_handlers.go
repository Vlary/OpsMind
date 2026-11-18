// W4：RDP 会话与录制的 HTTP/WS handler（CONTRACT.md 2.5/5.14/6.5）。
//
// WS 升级前校验顺序（写死，同 6.4）：Origin 精确匹配
// （403）→ opsmind_session Cookie 鉴权（401）→ 会话存在且 active
// （404/409）→ 升级 101。guacamole-common-js 的 WebSocketTunnel 以
// 子协议 "guacamole" 发起升级（dist 源码 new WebSocket(url, "guacamole")，
// 服务端不声明该子协议时浏览器直接 400 拒绝握手——二期走查实测），
// upgrader 声明后 gorilla 回显协商；无子协议客户端仍可升级。载荷为
// 文本帧承载的 guacamole 指令流（官方 Guacamole WebSocket tunnel servlet
// 以文本帧发送指令；guacamole-common-js 的 WebSocketTunnel.onmessage 对
// event.data 直接 .indexOf('.') 按字符串解析，二进制帧会在首帧抛
// TypeError——p2 sprint 3 走查实测），Go 侧纯字节透传。
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
	"aiops/internal/authz"
	"aiops/internal/rdpx"
	"aiops/internal/store"
)

// 分辨率边界（OpenAPI RdpSessionCreate；RDP 不支持运行中 resize）。
const (
	rdpMinWidth, rdpMaxWidth   = 640, 3840
	rdpMinHeight, rdpMaxHeight = 480, 2160
	rdpMinDPI, rdpMaxDPI       = 72, 120

	// WS 读限（契约 6.5）：1MB；读写缓冲。
	rdpWSReadLimit     = 1024 * 1024
	rdpWSBufferSize    = 64 * 1024
	rdpGuacContentType = "application/octet-stream"
)

// RegisterRdpRoutes 注册 W4 负责的端点。
//
// REST 端点挂 protected（requireAuth）；WS 端点挂裸 api 组——升级前的
// Origin→Cookie→业务校验由 handler 自行完成（契约 6.5 写死顺序）。
func (d *Deps) RegisterRdpRoutes(protected, api *gin.RouterGroup) {
	// 00012：JIT grant 申请（source=terminal、grant_kind=rdp）。
	d.guard(protected, http.MethodPost, "/rdp/grants", authz.PermTerminal, d.handleCreateRdpGrant)
	// 第 2 轮走查（low）：发起人可见的「已批准、待消费」授权列表（只返回本人）。
	d.guard(protected, http.MethodGet, "/rdp/grants", authz.PermTerminal, d.handleListRdpGrants)
	d.guard(protected, http.MethodPost, "/rdp/sessions", authz.PermTerminal, d.handleCreateRdpSession)
	// 列表/录制对 auditor 跨用户只读（audit:read），owner 只看自己。
	d.guardAny(protected, http.MethodGet, "/rdp/sessions", []authz.Permission{authz.PermTerminal, authz.PermAudit}, d.handleListRdpSessions)
	d.guardAny(protected, http.MethodGet, "/rdp/recordings", []authz.Permission{authz.PermTerminal, authz.PermAudit}, d.handleListRdpRecordings)
	d.guardAny(protected, http.MethodGet, "/rdp/recordings/:id", []authz.Permission{authz.PermTerminal, authz.PermAudit}, d.handleDownloadRdpRecording)
	// WS 端点挂裸 api 组：归属由 rdpx.Attach 的注册表校验（同 userID 才能接入）。
	api.GET("/rdp/sessions/:id/ws", d.handleRdpSessionWS)
}

// ---------------------------------------------------------------------------
// 会话 REST
// ---------------------------------------------------------------------------

type rdpSessionCreateRequest struct {
	ServerID string `json:"server_id"`
	// GrantID 是已批准、未消费的 source=terminal/grant_kind=rdp 审批 id。
	GrantID string `json:"grant_id"`
	Width   *int   `json:"width"`
	Height  *int   `json:"height"`
	DPI     *int   `json:"dpi"`
}

func (d *Deps) handleCreateRdpSession(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req rdpSessionCreateRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	if _, err := uuid.Parse(req.ServerID); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "server_id must be a uuid")
		return
	}
	width, height, dpi := 1280, 800, 96
	if req.Width != nil {
		width = *req.Width
	}
	if req.Height != nil {
		height = *req.Height
	}
	if req.DPI != nil {
		dpi = *req.DPI
	}
	if width < rdpMinWidth || width > rdpMaxWidth {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "width must be between 640 and 3840")
		return
	}
	if height < rdpMinHeight || height > rdpMaxHeight {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "height must be between 480 and 2160")
		return
	}
	if dpi < rdpMinDPI || dpi > rdpMaxDPI {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "dpi must be between 72 and 120")
		return
	}
	user, ok := userFromContext(c)
	if !ok {
		writeError(c, apierr.AuthRequired, "authentication required")
		return
	}
	// guacd 未配置即 RDP 禁用（GuacdAddr 空或网关未装配）。
	if d.RDP == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.GuacdUnavailable, "rdp gateway is not configured")
		return
	}
	server, err := d.Store.Servers().Get(c.Request.Context(), req.ServerID)
	if err != nil {
		d.handleStoreError(c, err, apierr.ServerNotFound)
		return
	}
	// 目标能力先于 grant 消费判定（不可用目标不应烧掉已批准的 grant）。
	if server.OS != store.ServerOSWindows {
		writeAPIError(c, http.StatusBadRequest, apierr.RdpUnsupported, "server os is not windows")
		return
	}
	if server.AuthMethod == store.AuthMethodPrivateKey || server.PasswordEncrypted == "" {
		writeAPIError(c, http.StatusBadRequest, apierr.RdpNoCredentials, "server has no password credential")
		return
	}
	// 00012：RDP 会话同样必须叠加他人批准的单次 rdp grant。
	// 契约 12.3 第 11 条：单管理员降级模式不得开 RDP（fail-closed）。
	if !d.requireSecondApprover(c, authz.PermTerminal, "rdp.session") {
		return
	}
	// 第 4 轮走查 medium：同终端路径，先预检同服务器活动会话，再消费 grant；
	// Open 失败时回滚 consumed_at 保留可重试。
	if active, cerr := d.Store.RdpSessions().CountActiveByServer(c.Request.Context(), server.ID); cerr != nil {
		d.logHandlerError(c, "rdp session precheck failed", cerr)
		writeError(c, apierr.Internal, "internal server error")
		return
	} else if active > 0 {
		writeAPIError(c, http.StatusConflict, apierr.RdpSessionExists, "server already has an active rdp session")
		return
	}
	grant, ok := d.consumeTerminalGrant(c, req.GrantID, server.ID, "rdp")
	if !ok {
		return
	}

	session, err := d.RDP.Open(c.Request.Context(), server, user.ID, width, height, dpi)
	if err != nil {
		d.releaseTerminalGrant(c, grant, "rdp")
		d.writeRdpOpenError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"session": session,
		"ws_url":  "/api/rdp/sessions/" + session.ID + "/ws",
	})
}

// writeRdpOpenError 映射 rdpx.Open 的哨兵错误（契约 9.6）。
func (d *Deps) writeRdpOpenError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, rdpx.ErrUnsupported):
		writeAPIError(c, http.StatusBadRequest, apierr.RdpUnsupported, "server os is not windows")
	case errors.Is(err, rdpx.ErrNoCredentials):
		writeAPIError(c, http.StatusBadRequest, apierr.RdpNoCredentials, "server has no password credential")
	case errors.Is(err, rdpx.ErrGuacdUnavailable):
		writeAPIError(c, http.StatusServiceUnavailable, apierr.GuacdUnavailable, "guacd is not configured or unreachable")
	case errors.Is(err, rdpx.ErrSessionExists):
		writeAPIError(c, http.StatusConflict, apierr.RdpSessionExists, "server already has an active rdp session")
	case errors.Is(err, rdpx.ErrSessionLimit):
		writeAPIError(c, http.StatusTooManyRequests, apierr.RdpSessionLimit, "rdp session limit reached")
	default:
		d.logHandlerError(c, "rdp open failed", err)
		writeError(c, apierr.Internal, "internal server error")
	}
}

func (d *Deps) handleListRdpSessions(c *gin.Context) {
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
	items, err := d.Store.RdpSessions().List(c.Request.Context(), filter)
	if err != nil {
		d.handleStoreError(c, err, apierr.RdpSessionNotFound)
		return
	}
	// 契约 12.3 第 4 条：非 audit:read 强制只看自己的会话。
	if !authz.Allowed(c, authz.PermAudit) {
		me := c.GetString("user_id")
		own := make([]store.RdpSession, 0, len(items))
		for _, it := range items {
			if it.UserID == me {
				own = append(own, it)
			}
		}
		items = own
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// ---------------------------------------------------------------------------
// 录制 REST（.guac 由 guacd 写入共享卷；转码不在二期范围）
// ---------------------------------------------------------------------------

// rdpRecordingItem 对应 OpenAPI RdpRecording schema。
type rdpRecordingItem struct {
	ID           string     `json:"id"`
	ServerID     string     `json:"server_id"`
	ServerName   string     `json:"server_name"`
	BytesWritten int64      `json:"bytes_written"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

func (d *Deps) handleListRdpRecordings(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	limit, ok := queryLimit(c)
	if !ok {
		return
	}
	// 有录制文件的已结束会话（openapi：status=closed 且 recording_file 非空）。
	items, err := d.Store.RdpSessions().List(c.Request.Context(), store.InteractiveSessionFilter{
		Status: store.SessionClosed,
		Limit:  limit,
	})
	if err != nil {
		d.handleStoreError(c, err, apierr.RdpSessionNotFound)
		return
	}
	auditor := authz.Allowed(c, authz.PermAudit)
	me := c.GetString("user_id")
	out := make([]rdpRecordingItem, 0, len(items))
	for _, s := range items {
		if !s.HasRecording {
			continue
		}
		if !auditor && s.UserID != me {
			continue
		}
		out = append(out, rdpRecordingItem{
			ID:           s.ID,
			ServerID:     s.ServerID,
			ServerName:   s.ServerName,
			BytesWritten: s.BytesWritten,
			StartedAt:    s.StartedAt,
			EndedAt:      s.EndedAt,
			CreatedAt:    s.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (d *Deps) handleDownloadRdpRecording(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "id must be a uuid")
		return
	}
	session, err := d.Store.RdpSessions().Get(c.Request.Context(), id)
	if err != nil {
		d.handleStoreError(c, err, apierr.RdpSessionNotFound)
		return
	}
	if !session.HasRecording {
		writeAPIError(c, http.StatusNotFound, apierr.RecordingNotFound, "session has no recording")
		return
	}
	// 契约 12.3 第 4 条：owner 或 auditor；其他用户一律 404（不泄露存在性）。
	me := c.GetString("user_id")
	if session.UserID != me {
		if !authz.Allowed(c, authz.PermAudit) {
			writeAPIError(c, http.StatusNotFound, apierr.RecordingNotFound, "recording not found")
			return
		}
		d.auditEvent(c, "session.recording_download", "rdp_session", session.ID, map[string]any{
			"owner_id": session.UserID, "server_id": session.ServerID,
		})
	}
	dir := d.rdpRecordingsDir()
	// 文件名即会话 UUID（DB 行校验过的 <uuid>.guac，无用户可控拼接）。
	path := filepath.Join(dir, session.ID+".guac")
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
	// .guac 是 guacd 私有二进制格式：固定 octet-stream + nosniff，
	// 防止浏览器嗅探执行；回放需 guacenc 转码（见 openapi 注记）。
	c.Header("Content-Type", rdpGuacContentType)
	c.Header("X-Content-Type-Options", "nosniff")
	http.ServeContent(c.Writer, c.Request, session.ID+".guac", info.ModTime(), f)
}

// rdpRecordingsDir 返回 .guac 录制目录（默认 <DataDir>/rdp-recordings）。
func (d *Deps) rdpRecordingsDir() string {
	if d.Config != nil && d.Config.RdpRecordingsDir != "" {
		return d.Config.RdpRecordingsDir
	}
	return "./data/rdp-recordings"
}

// ---------------------------------------------------------------------------
// RDP WebSocket（契约 6.5：文本帧透传 guacamole 指令；guacamole 子协议协商）
// ---------------------------------------------------------------------------

// rdpWSUpgrader 声明 guacamole 子协议：WebSocketTunnel 固定以子协议
// "guacamole" 发起升级（Sec-WebSocket-Protocol），不声明则浏览器握手
// 400（Upgrader 只在客户端提供子协议时回显，不影响裸客户端）；Origin
// 已在升级前精确校验。
const rdpWSSubprotocol = "guacamole"

var rdpWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  rdpWSBufferSize,
	WriteBufferSize: rdpWSBufferSize,
	Subprotocols:    []string{rdpWSSubprotocol},
	CheckOrigin:     func(*http.Request) bool { return true },
}

// rdpWSConn 把 gorilla/websocket 适配为 rdpx.WSConn（io.ReadWriteCloser，
// 契约 5.14）：消息级读写映射为字节流；读限 1MB（契约 6.5）。
type rdpWSConn struct {
	conn *websocket.Conn
	rbuf []byte // 消息内未消费的残余（帧边界与 guacamole 指令边界无关）
}

func newRdpWSConn(ws *websocket.Conn) *rdpWSConn {
	ws.SetReadLimit(rdpWSReadLimit)
	return &rdpWSConn{conn: ws}
}

func (a *rdpWSConn) Read(p []byte) (int, error) {
	for len(a.rbuf) == 0 {
		_, data, err := a.conn.ReadMessage()
		if err != nil {
			return 0, err
		}
		a.rbuf = data
	}
	n := copy(p, a.rbuf)
	a.rbuf = a.rbuf[n:]
	return n, nil
}

func (a *rdpWSConn) Write(p []byte) (int, error) {
	// 文本帧：官方 Guacamole WebSocket tunnel servlet 以文本帧发送指令，
	// guacamole-common-js 的 onmessage 对 event.data 按 string 解析
	// （.indexOf/.substring）；二进制帧在首帧即抛 TypeError（p2 sprint 3
	// 走查实测）。指令为 ASCII 长度前缀文本，切分边界任意。
	if err := a.conn.WriteMessage(websocket.TextMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (a *rdpWSConn) Close() error {
	_ = a.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	return a.conn.Close()
}

func (d *Deps) handleRdpSessionWS(c *gin.Context) {
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
	user, authSession, err := d.Auth.AuthenticateSession(c.Request.Context(), raw)
	if err != nil {
		if errors.Is(err, auth.ErrAuthRequired) {
			writeError(c, apierr.AuthRequired, "authentication required")
			return
		}
		d.logHandlerError(c, "authenticate failed", err)
		writeError(c, apierr.Internal, "internal server error")
		return
	}
	// must_change 门在 WS 手工鉴权路径同样生效（契约 12.3 第 2 条三处共用）。
	if user.MustChangePassword {
		writeErrorStatus(c, http.StatusForbidden, "PASSWORD_CHANGE_REQUIRED", "password change required")
		return
	}
	// 3. 权限：JIT 终端/RDP 需要 terminal:use；auditor 不能升级 WS。
	if !authz.Has(user.Role, authz.PermTerminal) {
		d.writeForbidden(c, user, authz.PermTerminal)
		return
	}
	// 业务校验：网关可用 + 会话存在且 active。
	// 归属校验在 Attach 内完成（rdpx 网关按会话注册表校验 userID，不匹配
	// 直接 ErrNotActive 断开，契约 5.14）。
	if d.RDP == nil || !d.requireStore(c) {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.GuacdUnavailable, "rdp gateway is not configured")
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "id must be a uuid")
		return
	}
	session, err := d.Store.RdpSessions().Get(c.Request.Context(), id)
	if err != nil {
		d.handleStoreError(c, err, apierr.RdpSessionNotFound)
		return
	}
	if session.Status != store.SessionActive {
		writeAPIError(c, http.StatusConflict, apierr.Conflict, "session is not active")
		return
	}
	// 契约 12.3 第 4 条：非 owner 一律 404（不泄露会话存在性）。此前只在
	// Attach 注册表校验，HTTP 层缺归属检查（评审发现）。
	if session.UserID != user.ID {
		writeAPIError(c, http.StatusNotFound, apierr.RdpSessionNotFound, "session not found")
		return
	}
	// 4. 升级 101（协商 guacamole 子协议）。
	ws, err := rdpWSUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade 已写非 101 响应；此处只记日志。
		d.logHandlerError(c, "ws upgrade failed", err)
		return
	}
	conn := newRdpWSConn(ws)

	// Attach 内部完成 guacd 拨号、握手（凭据注入）与完整生命周期收尾；
	// 失败时无应用层错误帧（契约 6.5），直接关连接由前端
	// guacamole-common-js 的 onerror/onstatechange 感知。
	if err := d.RDP.AttachForAuthSession(conn, id, user.ID, authSession.ID, requestIDOf(c), d.requestIP(c)); err != nil {
		d.logHandlerError(c, "rdp attach failed", err)
		_ = conn.Close()
	}
}
