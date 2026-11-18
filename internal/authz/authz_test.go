package authz

import (
	"testing"

	"aiops/internal/store"
)

// 权限全集固定 19 条且无重复（契约 12.1）。
func TestPermissionSetIsComplete(t *testing.T) {
	if len(AllPermissions) != 19 {
		t.Fatalf("AllPermissions = %d, want 19", len(AllPermissions))
	}
	seen := make(map[Permission]bool, len(AllPermissions))
	for _, p := range AllPermissions {
		if p == "" {
			t.Fatal("empty permission string")
		}
		if seen[p] {
			t.Fatalf("duplicate permission %q", p)
		}
		seen[p] = true
	}
}

// admin 拥有全部 19 条；其余角色只持有全集子集；未知角色 fail-closed。
// 各角色条数与契约 12.2 的 ✔ 数量逐行对应，防止矩阵被误改。
func TestRolePermissionSets(t *testing.T) {
	all := make(map[Permission]bool, len(AllPermissions))
	for _, p := range AllPermissions {
		all[p] = true
	}
	wantCounts := map[store.Role]int{
		store.RoleAdmin:    19,
		store.RoleOperator: 8,
		store.RoleApprover: 3,
		store.RoleAuditor:  1,
	}
	for role, want := range wantCounts {
		if got := len(PermissionsFor(role)); got != want {
			t.Fatalf("%s permissions = %d, want %d", role, got, want)
		}
	}
	for _, role := range []store.Role{store.RoleOperator, store.RoleApprover, store.RoleAuditor} {
		perms := PermissionsFor(role)
		if len(perms) == 0 {
			t.Fatalf("%s has no permissions", role)
		}
		local := make(map[Permission]bool, len(perms))
		for _, p := range perms {
			if !all[p] {
				t.Fatalf("%s has unknown permission %q", role, p)
			}
			if local[p] {
				t.Fatalf("%s has duplicate permission %q", role, p)
			}
			local[p] = true
		}
	}
	if got := PermissionsFor(store.Role("unknown")); len(got) != 0 {
		t.Fatalf("unknown role permissions = %v", got)
	}
	if Has(store.Role("unknown"), PermChat) {
		t.Fatal("unknown role granted a permission")
	}
}

// PermissionsFor 返回副本，调用方修改不得污染映射。
func TestPermissionsForReturnsCopy(t *testing.T) {
	first := PermissionsFor(store.RoleAuditor)
	if len(first) == 0 {
		t.Fatal("auditor has no permissions")
	}
	first[0] = PermChat
	if Has(store.RoleAuditor, PermChat) {
		t.Fatal("mutating the returned slice changed the role map")
	}
}
