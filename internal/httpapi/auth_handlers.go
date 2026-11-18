package httpapi

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/audit"
	"aiops/internal/auth"
	"aiops/internal/authz"
	"aiops/internal/store"
)

const sessionCookieName = "opsmind_session"

// SessionCookieName 是会话 Cookie 的公开名（cmd 装配层的 pprof 鉴权等复用）。
const SessionCookieName = sessionCookieName

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin 按用户名 + 密码登录并设置会话 Cookie。
//
// 未知用户名、停用用户与错误密码统一 401 AUTH_INVALID（auth.Service 保证
// 每次都做一次 bcrypt）。
func (d *Deps) handleLogin(c *gin.Context) {
	var req loginRequest
	if !bindJSONStrict(c, &req) {
		return
	}
	if req.Username == "" || len(req.Username) > 64 {
		writeError(c, apierr.ValidationFailed, "username is required")
		return
	}
	if req.Password == "" || len(req.Password) > 256 {
		writeError(c, apierr.ValidationFailed, "password is required")
		return
	}

	ip := d.requestIP(c)
	if d.Limiter != nil {
		ok, retryAfter := d.Limiter.Allow(ip)
		if !ok {
			if retryAfter <= 0 {
				retryAfter = time.Second
			}
			c.Header("Retry-After", strconv.Itoa(int(retryAfter.Seconds()+0.999)))
			d.auditEnqueue(c, audit.Event{
				ActorType: "user",
				Action:    "auth.rate_limited",
				IP:        ip,
				Detail:    map[string]any{"retry_after_ms": retryAfter.Milliseconds()},
			})
			writeError(c, apierr.AuthRateLimited, "too many login attempts")
			return
		}
	}

	if d.Auth == nil {
		writeError(c, apierr.Internal, "authentication unavailable")
		return
	}
	result, err := d.Auth.Login(c.Request.Context(), req.Username, req.Password, ip, c.Request.UserAgent())
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			d.auditEnqueue(c, audit.Event{
				ActorType: "user",
				Action:    "auth.login_failed",
				IP:        ip,
			})
			writeError(c, apierr.AuthInvalid, "invalid credentials")
			return
		}
		d.logHandlerError(c, "login failed", err)
		writeError(c, apierr.Internal, "internal server error")
		return
	}

	maxAge := int(math.Ceil(result.ExpiresAt.Sub(d.Clock()).Seconds()))
	if maxAge < 1 {
		maxAge = 1
	}
	secure := d.Config != nil && d.Config.SecureCookies
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(sessionCookieName, result.Token, maxAge, "/", "", secure, true)

	d.auditEnqueue(c, audit.Event{
		ActorType: "user",
		ActorID:   result.User.ID,
		Action:    "auth.login_succeeded",
		IP:        ip,
	})
	c.JSON(http.StatusOK, gin.H{
		"user":        result.User,
		"permissions": authz.PermissionStrings(result.User.Role),
	})
}

// handleLogout 删除当前会话并清除 Cookie。
//
// 登出后该 auth session 名下的交互连接（终端/RDP WS 与 SSE 流）必须立即
// 断开：仅删 sessions 行不会终止已建立的 shell（评审 high 实测登出 35s 后
// 仍可通过既有 WS 输入命令）。契约 12.3 第 13 条。
func (d *Deps) handleLogout(c *gin.Context) {
	authSessionID := c.GetString("auth_session_id")
	raw, _ := c.Cookie(sessionCookieName)
	if d.Auth != nil && raw != "" {
		// requireAuth 未注入时（历史路径/单测）按 token 反查，保证仍能精确关闭。
		if authSessionID == "" {
			if session, err := d.Auth.SessionByToken(c.Request.Context(), raw); err == nil {
				authSessionID = session.ID
			}
		}
		if err := d.Auth.Logout(c.Request.Context(), raw); err != nil {
			d.logHandlerError(c, "logout failed", err)
			writeError(c, apierr.Internal, "internal server error")
			return
		}
	}
	d.closeInteractiveByAuthSession(c.Request.Context(), authSessionID)

	if user, ok := userFromContext(c); ok {
		d.auditEnqueue(c, audit.Event{
			ActorType: "user",
			ActorID:   user.ID,
			Action:    "auth.logout",
		})
	}

	secure := d.Config != nil && d.Config.SecureCookies
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(sessionCookieName, "", -1, "/", "", secure, true)
	c.Status(http.StatusNoContent)
}

// handleMe 返回当前登录用户、角色与服务端计算的权限。
func (d *Deps) handleMe(c *gin.Context) {
	user, ok := userFromContext(c)
	if !ok {
		writeError(c, apierr.AuthRequired, "authentication required")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"user":        user,
		"permissions": authz.PermissionStrings(user.Role),
	})
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// handleChangePassword 校验旧密码后更新管理员密码：成功即吊销该用户全部
// 会话（含当前会话），清除 Cookie 并要求重新登录。
func (d *Deps) handleChangePassword(c *gin.Context) {
	user, ok := userFromContext(c)
	if !ok {
		writeError(c, apierr.AuthRequired, "authentication required")
		return
	}
	var req changePasswordRequest
	if !bindJSONStrict(c, &req) {
		return
	}
	// 长度上限与登录一致（256）；新密码另有 bcrypt 72 字节上限，由
	// Service.ChangePasswordAudited 校验并返回 ErrPasswordInvalid。
	if req.OldPassword == "" || len(req.OldPassword) > 256 {
		writeError(c, apierr.ValidationFailed, "old_password is required")
		return
	}

	ip := d.requestIP(c)
	if d.Auth == nil {
		writeError(c, apierr.Internal, "authentication unavailable")
		return
	}
	// 审计在改密/吊销会话的同一事务内写 audit_outbox：客户端断开或进程
	// 崩溃时密码已变但审计不能丢。只有存储不支持 outbox（内存 fake）
	// 时才在提交后回落到进程内 Enqueue。
	buildAudit := func() audit.Event {
		return audit.Event{
			ActorType:    "user",
			ActorID:      user.ID,
			Action:       "auth.password_changed",
			ResourceType: "user",
			ResourceID:   user.ID,
			IP:           ip,
			RequestID:    requestIDOf(c),
		}
	}
	auditInTx, err := d.Auth.ChangePasswordAudited(c.Request.Context(), user.ID, req.OldPassword, req.NewPassword, buildAudit)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			d.auditEnqueue(c, audit.Event{
				ActorType: "user",
				ActorID:   user.ID,
				Action:    "auth.password_change_failed",
				IP:        ip,
			})
			writeError(c, apierr.AuthInvalid, "invalid credentials")
			return
		}
		if errors.Is(err, auth.ErrPasswordInvalid) {
			writeError(c, apierr.ValidationFailed, "new_password must be 1..72 bytes")
			return
		}
		d.logHandlerError(c, "change password failed", err)
		writeError(c, apierr.Internal, "internal server error")
		return
	}

	// 改密后全部会话已被吊销；已建立的交互终端/RDP 也必须立即断开
	//（契约 12.3 第 13 条）。
	d.revokeLiveInteractive(c.Request.Context(), user.ID)
	if !auditInTx {
		d.auditEnqueue(c, buildAudit())
	}

	// 全部会话已吊销：清除客户端 Cookie，让其回到登录页。
	secure := d.Config != nil && d.Config.SecureCookies
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(sessionCookieName, "", -1, "/", "", secure, true)
	c.Status(http.StatusNoContent)
}

// acceptInviteRequest 是 POST /api/auth/invite/accept 的请求体。
type acceptInviteRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
	DisplayName string `json:"display_name"`
}

// handleAcceptInvite 兑换一次性 invite/reset token 并设置新密码。
//
// 公开端点：Origin 由全局 originCheck 校验；按 IP 复用登录限流。
// token 只存 SHA-256、30 分钟、单次使用；创建者 IP 兑换 403。
func (d *Deps) handleAcceptInvite(c *gin.Context) {
	var req acceptInviteRequest
	if !bindJSONStrict(c, &req) {
		return
	}
	if len(req.Token) < 32 || len(req.Token) > 128 {
		writeError(c, apierr.ValidationFailed, "invalid token")
		return
	}
	if len(req.NewPassword) == 0 || len(req.NewPassword) > 72 {
		writeError(c, apierr.ValidationFailed, "new_password must be 1..72 bytes")
		return
	}
	if len([]rune(req.DisplayName)) > 100 {
		writeError(c, apierr.ValidationFailed, "display_name is too long")
		return
	}

	ip := d.requestIP(c)
	if d.Limiter != nil {
		ok, retryAfter := d.Limiter.Allow(ip)
		if !ok {
			if retryAfter <= 0 {
				retryAfter = time.Second
			}
			c.Header("Retry-After", strconv.Itoa(int(retryAfter.Seconds()+0.999)))
			d.auditEnqueue(c, audit.Event{
				ActorType: "user",
				Action:    "auth.rate_limited",
				IP:        ip,
				Detail:    map[string]any{"retry_after_ms": retryAfter.Milliseconds()},
			})
			writeError(c, apierr.AuthRateLimited, "too many attempts")
			return
		}
	}
	if d.Auth == nil {
		writeError(c, apierr.Internal, "authentication unavailable")
		return
	}

	// 受邀角色可能是 admin：「谁在何时授权了这次访问」必须与 token 兑换、
	// 用户启用/建行、改密同事务提交。审计写 audit_outbox；客户端断开或
	// 进程崩溃都不会丢。内存 fake 不支持 outbox 时回落到进程内 Enqueue。
	buildAudit := func(kind store.UserTokenKind, userID string) audit.Event {
		action := "user.invite_accepted"
		if kind == store.UserTokenReset {
			action = "auth.password_changed"
		}
		return audit.Event{
			ActorType:    "user",
			ActorID:      userID,
			Action:       action,
			ResourceType: "user",
			ResourceID:   userID,
			IP:           ip,
			RequestID:    requestIDOf(c),
		}
	}
	kind, userID, auditInTx, err := d.Auth.AcceptTokenAudited(c.Request.Context(), req.Token, req.NewPassword, req.DisplayName, ip, buildAudit)
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrPasswordInvalid):
		writeError(c, apierr.ValidationFailed, "new_password must be 1..72 bytes")
		return
	case errors.Is(err, auth.ErrSelfRedeem):
		writeErrorStatus(c, http.StatusForbidden, "SELF_REDEEM_FORBIDDEN", "token cannot be redeemed by its creator")
		return
	case errors.Is(err, auth.ErrInvalidToken):
		// 不存在/已使用/过期/角色非法统一 400，不泄露 token 状态。
		writeError(c, apierr.ValidationFailed, "invalid or expired token")
		return
	default:
		d.logHandlerError(c, "accept invite failed", err)
		writeError(c, apierr.Internal, "internal server error")
		return
	}

	if !auditInTx {
		d.auditEnqueue(c, buildAudit(kind, userID))
	}
	c.Status(http.StatusNoContent)
}
