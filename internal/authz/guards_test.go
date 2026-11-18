package authz

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"aiops/internal/store"
)

func init() { gin.SetMode(gin.TestMode) }

func newCtx(role store.Role) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/x", nil)
	if role != "" {
		c.Set(ContextUserKey, store.User{ID: "u1", Role: role, Enabled: true})
	}
	return c, rec
}

func TestPermissionMatrix(t *testing.T) {
	cases := []struct {
		role store.Role
		perm Permission
		want bool
	}{
		{store.RoleAdmin, PermPprof, true},
		{store.RoleAdmin, PermUsersWrite, true},
		{store.RoleAdmin, PermChat, true},
		{store.RoleOperator, PermChat, true},
		{store.RoleOperator, PermExecuteWrite, true},
		{store.RoleOperator, PermTerminal, true},
		{store.RoleOperator, PermInspection, true},
		{store.RoleOperator, PermConnectionTest, true},
		// 运维不能管理用户/凭据/供应商/告警配置/审批。
		{store.RoleOperator, PermUsersWrite, false},
		{store.RoleOperator, PermCredentialReset, false},
		{store.RoleOperator, PermProviders, false},
		{store.RoleOperator, PermAlertConfig, false},
		{store.RoleOperator, PermApprovalDecide, false},
		{store.RoleOperator, PermAudit, false},
		{store.RoleOperator, PermPprof, false},
		// 审批者只审批不发起：无 chat/执行/巡检/终端。
		{store.RoleApprover, PermAssetsRead, true},
		{store.RoleApprover, PermApprovalRead, true},
		{store.RoleApprover, PermApprovalDecide, true},
		{store.RoleApprover, PermChat, false},
		{store.RoleApprover, PermExecuteWrite, false},
		{store.RoleApprover, PermExecuteReadonly, false},
		{store.RoleApprover, PermTerminal, false},
		{store.RoleApprover, PermInspection, false},
		{store.RoleApprover, PermPolicyRequest, false},
		// 审计员只读治理。
		{store.RoleAuditor, PermAudit, true},
		{store.RoleAuditor, PermChat, false},
		{store.RoleAuditor, PermAssetsRead, false},
		{store.RoleAuditor, PermApprovalDecide, false},
		{store.RoleAuditor, PermTerminal, false},
		// 未知角色 fail-closed。
		{store.Role("root"), PermChat, false},
		{store.Role(""), PermAudit, false},
	}
	for _, tc := range cases {
		if got := Has(tc.role, tc.perm); got != tc.want {
			t.Errorf("Has(%q, %q) = %v, want %v", tc.role, tc.perm, got, tc.want)
		}
	}
}

func TestAdminHasAllPermissions(t *testing.T) {
	for _, p := range AllPermissions {
		if !Has(store.RoleAdmin, p) {
			t.Errorf("admin missing %q", p)
		}
	}
}

func TestUnknownRoleHasNoPermissions(t *testing.T) {
	if got := PermissionsFor(store.Role("nobody")); len(got) != 0 {
		t.Fatalf("unknown role permissions = %v, want empty", got)
	}
	if got := PermissionsOf(store.Role("nobody")); len(got) != 0 {
		t.Fatalf("unknown role permission strings = %v, want empty", got)
	}
}

func TestRoleLabels(t *testing.T) {
	if RoleLabel(store.RoleAdmin) != "管理员" || RoleLabel(store.Role("x")) != "" {
		t.Fatalf("unexpected role labels")
	}
}

func TestRequirePermissionFailClosed(t *testing.T) {
	// 未认证 → 401，不是 403/放行。
	c, rec := newCtx("")
	called := false
	h := RequirePermission(PermChat)
	h(c)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}
	_ = called
}

func TestRequirePermissionDeniesAndRecords(t *testing.T) {
	c, rec := newCtx(store.RoleAuditor)
	var recorded Permission
	h := RequirePermission(PermChat, func(_ *gin.Context, _ store.User, p Permission) { recorded = p })
	h(c)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if recorded != PermChat {
		t.Fatalf("recorded = %q, want %q", recorded, PermChat)
	}
}

func TestRequirePermissionAllows(t *testing.T) {
	c, rec := newCtx(store.RoleOperator)
	h := RequirePermission(PermChat)
	h(c)
	if rec.Code != http.StatusOK {
		// gin test recorder 在未写响应时保持 200 默认值。
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRequireAny(t *testing.T) {
	// auditor 无 chat 但有 audit：OR 守卫放行。
	c, rec := newCtx(store.RoleAuditor)
	RequireAny([]Permission{PermChat, PermAudit})(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("auditor OR status = %d, want 200", rec.Code)
	}
	// 空列表 fail-closed。
	c, rec = newCtx(store.RoleAdmin)
	RequireAny(nil)(c)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("empty OR status = %d, want 403", rec.Code)
	}
}

func TestAuthorizeAnyEmptyFailsClosed(t *testing.T) {
	c, rec := newCtx(store.RoleAdmin)
	if AuthorizeAny(c, nil) {
		t.Fatal("AuthorizeAny with no permissions must fail closed")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
