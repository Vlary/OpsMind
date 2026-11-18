package httpapi

import (
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/authz"
	"aiops/internal/config"
)

// TestRoutePermissions 是契约 12.3 第 1 条的覆盖测试：遍历
// gin.Engine.Routes()，断言每条 /api 路由都在 routeRules 登记，且
// 路由层守卫与登记一致；已装配的 guard 必须对应真实路由（防止表与
// 注册分叉）。handler 内联守卫（会话/WS/chat）在 routeRules 中以
// guardInHandler 显式登记，校验点见 conversation_authz.go / term/rdp。
func TestRoutePermissions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := NewRouter(&Deps{
		Config: &config.Config{AllowedOrigins: []string{"http://localhost"}},
		Clock:  time.Now,
	})

	applied := appliedGuardRules()
	seen := make(map[string]bool, len(routeRules))
	for _, ri := range router.Routes() {
		if !strings.HasPrefix(ri.Path, "/api/") {
			continue
		}
		key := ri.Method + " " + ri.Path
		rule, ok := ruleIndex[key]
		if !ok {
			t.Errorf("route %s has no permission rule (漏挂权限)", key)
			continue
		}
		seen[key] = true
		if _, ok := applied[key]; ok {
			if rule.Kind == guardInHandler {
				t.Errorf("route %s declared handler guard but has a route guard", key)
			}
			continue
		}
		if rule.Kind != guardInHandler && rule.Kind != guardAuthenticated && rule.Kind != guardExternal &&
			rule.Kind != guardPublic && rule.Kind != guardInstancePermission {
			t.Errorf("route %s is missing its permission guard", key)
		}
	}

	for key := range applied {
		if !seen[key] {
			t.Errorf("guard %s is registered but route does not exist", key)
		}
	}
}

// TestRouteRulePermissionsValid 断言每个权限串来自契约全集（防拼写错误
// 产生永不授予的权限）。
func TestRouteRulePermissionsValid(t *testing.T) {
	valid := make(map[authz.Permission]bool, len(authz.AllPermissions))
	for _, p := range authz.AllPermissions {
		valid[p] = true
	}
	for _, r := range routeRules {
		switch r.Kind {
		case guardPermission, guardInstancePermission:
			if !valid[r.Perm] {
				t.Errorf("%s %s uses unknown permission %q", r.Method, r.Path, r.Perm)
			}
		case guardAnyPermission:
			if len(r.Perms) == 0 {
				t.Errorf("%s %s has empty permission-any", r.Method, r.Path)
			}
			for _, p := range r.Perms {
				if !valid[p] {
					t.Errorf("%s %s uses unknown permission %q", r.Method, r.Path, p)
				}
			}
		}
	}
}
