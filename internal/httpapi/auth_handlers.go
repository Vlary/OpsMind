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
)

const sessionCookieName = "opsmind_session"

// SessionCookieName 是会话 Cookie 的公开名（cmd 装配层的 pprof 鉴权等复用）。
const SessionCookieName = sessionCookieName

type loginRequest struct {
	Password string `json:"password"`
}

// handleLogin 校验密码并设置会话 Cookie。
func (d *Deps) handleLogin(c *gin.Context) {
	var req loginRequest
	if !bindJSONStrict(c, &req) {
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
	result, err := d.Auth.Login(c.Request.Context(), req.Password, ip, c.Request.UserAgent())
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
	c.JSON(http.StatusOK, gin.H{"user": result.User})
}

// handleLogout 删除当前会话并清除 Cookie。
func (d *Deps) handleLogout(c *gin.Context) {
	raw, _ := c.Cookie(sessionCookieName)
	if d.Auth != nil && raw != "" {
		if err := d.Auth.Logout(c.Request.Context(), raw); err != nil {
			d.logHandlerError(c, "logout failed", err)
			writeError(c, apierr.Internal, "internal server error")
			return
		}
	}

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

// handleMe 返回当前登录用户。
func (d *Deps) handleMe(c *gin.Context) {
	user, ok := userFromContext(c)
	if !ok {
		writeError(c, apierr.AuthRequired, "authentication required")
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": user})
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
	// Service.ChangePassword 校验并返回 ErrPasswordInvalid。
	if req.OldPassword == "" || len(req.OldPassword) > 256 {
		writeError(c, apierr.ValidationFailed, "old_password is required")
		return
	}

	ip := d.requestIP(c)
	if d.Auth == nil {
		writeError(c, apierr.Internal, "authentication unavailable")
		return
	}
	err := d.Auth.ChangePassword(c.Request.Context(), user.ID, req.OldPassword, req.NewPassword)
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

	d.auditEnqueue(c, audit.Event{
		ActorType: "user",
		ActorID:   user.ID,
		Action:    "auth.password_changed",
		IP:        ip,
	})

	// 全部会话已吊销：清除客户端 Cookie，让其回到登录页。
	secure := d.Config != nil && d.Config.SecureCookies
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(sessionCookieName, "", -1, "/", "", secure, true)
	c.Status(http.StatusNoContent)
}
