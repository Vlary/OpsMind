package authz

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/store"
)

// ContextUserKey 是 requireAuth 注入当前用户的 gin context 键。
const ContextUserKey = "user"

// CodeForbidden 是缺少权限时的错误码（契约 9.6）。
const CodeForbidden = "FORBIDDEN"

// CurrentUser 返回 requireAuth 注入的用户。
func CurrentUser(c *gin.Context) (store.User, bool) {
	v, ok := c.Get(ContextUserKey)
	if !ok {
		return store.User{}, false
	}
	u, ok := v.(store.User)
	return u, ok
}

// DeniedRecorder 在拒绝一次请求后调用（用于写 authz.denied 审计）。
// 实现方必须自行限速；RequirePermission 保证只调用一次。
type DeniedRecorder func(c *gin.Context, user store.User, perm Permission)

// RequirePermission 返回权限守卫中间件。
//
// 认证由 requireAuth 完成；本中间件只从当前 user 的 role 计算权限，
// 无权限返回 403 FORBIDDEN（契约 12.4 第 1 条）。未知角色一律拒绝。
// 可选传入 record 以在拒绝时写审计（按 actor+permission 限速由调用方负责）。
func RequirePermission(perm Permission, record ...DeniedRecorder) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := CurrentUser(c)
		if !ok {
			writeAuthError(c, http.StatusUnauthorized, apierr.AuthRequired, "authentication required")
			return
		}
		if !Has(user.Role, perm) {
			for _, fn := range record {
				if fn != nil {
					fn(c, user, perm)
				}
			}
			writeAuthError(c, http.StatusForbidden, CodeForbidden, "permission denied")
			return
		}
		c.Next()
	}
}

// writeAuthError 写统一错误体。apierr.Status 的码表由共享模块维护，
// 这里显式给状态码，避免 RBAC 新码在码表落地前被映射成 500。
func writeAuthError(c *gin.Context, status int, code, message string) {
	err := apierr.New(code, message)
	err.RequestID = c.GetString("request_id")
	c.AbortWithStatusJSON(status, gin.H{"error": err})
}
