package authz

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"aiops/internal/store"
)

// 本文件在 authz.go/middleware.go 既有权角色映射之上补齐执行面需要的能力：
// handler 内联守卫（会话归属等无法在路由层表达的端点）、多权限 OR 守卫
// （auditor 跨用户只读与 owner 自读共用同一端点）与角色中文名。
// 权限矩阵与 403 语义仍然只有一份实现（Has / RequirePermission）。

// RoleLabel 返回角色中文名（供 /auth/me、审计与前端展示）；未知角色空串。
func RoleLabel(role store.Role) string {
	switch role {
	case store.RoleAdmin:
		return "管理员"
	case store.RoleOperator:
		return "运维工程师"
	case store.RoleApprover:
		return "审批者"
	case store.RoleAuditor:
		return "审计员"
	default:
		return ""
	}
}

// PermissionsOf 是 PermissionStrings 的契约命名别名（12.4 第 3 条）。
func PermissionsOf(role store.Role) []string { return PermissionStrings(role) }

// Allowed 报告当前请求用户是否拥有某权限；未认证/未知角色恒 false。
func Allowed(c *gin.Context, perm Permission) bool {
	user, ok := CurrentUser(c)
	if !ok {
		return false
	}
	return Has(user.Role, perm)
}

// AllowedAny 报告当前用户是否拥有任一权限；空列表恒 false。
func AllowedAny(c *gin.Context, perms ...Permission) bool {
	for _, p := range perms {
		if Allowed(c, p) {
			return true
		}
	}
	return false
}

// Authorize 是 handler 内联守卫：成功返回 true；失败写统一 401/403 并
// 返回 false（调用方必须立即 return）。用于路由注册不在本包文件内
// （router.go）或需要先加载资源再判定归属的端点。
func Authorize(c *gin.Context, perm Permission, record ...DeniedRecorder) bool {
	user, ok := CurrentUser(c)
	if !ok {
		writeAuthError(c, http.StatusUnauthorized, "AUTH_REQUIRED", "authentication required")
		return false
	}
	if Has(user.Role, perm) {
		return true
	}
	for _, fn := range record {
		if fn != nil {
			fn(c, user, perm)
		}
	}
	writeAuthError(c, http.StatusForbidden, CodeForbidden, "permission denied")
	return false
}

// AuthorizeAny 是 Authorize 的多权限 OR 版本；空权限列表 fail-closed。
func AuthorizeAny(c *gin.Context, record DeniedRecorder, perms ...Permission) bool {
	user, ok := CurrentUser(c)
	if !ok {
		writeAuthError(c, http.StatusUnauthorized, "AUTH_REQUIRED", "authentication required")
		return false
	}
	for _, p := range perms {
		if Has(user.Role, p) {
			return true
		}
	}
	denied := Permission("any")
	if len(perms) > 0 {
		denied = perms[0]
	}
	if record != nil {
		record(c, user, denied)
	}
	writeAuthError(c, http.StatusForbidden, CodeForbidden, "permission denied")
	return false
}

// RequireAny 返回多权限 OR 守卫中间件（至少拥有其一）；空列表 fail-closed。
func RequireAny(perms []Permission, record ...DeniedRecorder) gin.HandlerFunc {
	list := make([]Permission, len(perms))
	copy(list, perms)
	return func(c *gin.Context) {
		if AllowedAny(c, list...) {
			c.Next()
			return
		}
		user, ok := CurrentUser(c)
		if !ok {
			writeAuthError(c, http.StatusUnauthorized, "AUTH_REQUIRED", "authentication required")
			return
		}
		target := Permission("any")
		if len(list) > 0 {
			target = list[0]
		}
		for _, fn := range record {
			if fn != nil {
				fn(c, user, target)
			}
		}
		writeAuthError(c, http.StatusForbidden, CodeForbidden, "permission denied")
	}
}
