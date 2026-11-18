package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"aiops/internal/store"
)

// TestUsersRBACGuards 覆盖多用户 CRUD、最后管理员守卫与 token 存储。
func TestUsersRBACGuards(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	admin := mustUser(t) // role=admin, enabled=true
	op, err := testDB.Users().Create(ctx, store.CreateUserParams{
		Username:     "ops01",
		PasswordHash: "$2a$12$ops",
		Role:         store.RoleOperator,
		Enabled:      true,
		DisplayName:  "运维一号",
		Email:        "ops01@example.com",
		CreatedBy:    &admin.ID,
	})
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}
	if op.Role != store.RoleOperator || !op.Enabled || op.CreatedBy == nil || *op.CreatedBy != admin.ID {
		t.Fatalf("operator = %+v", op)
	}

	list, err := testDB.Users().List(ctx, 50)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %d, %v", len(list), err)
	}
	if _, err := testDB.Users().List(ctx, 201); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("List(201) = %v, want ErrInvalid", err)
	}

	// 唯一启用的 admin 不能被停用/降级。
	disabled := false
	operatorRole := store.RoleOperator
	err = testDB.WithTx(ctx, func(tx store.Store) error {
		_, err := tx.Users().Update(ctx, admin.ID, store.UpdateUserParams{Enabled: &disabled, UpdatedAt: now})
		return err
	})
	if !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("disable last admin = %v, want ErrLastAdmin", err)
	}
	err = testDB.WithTx(ctx, func(tx store.Store) error {
		_, err := tx.Users().Update(ctx, admin.ID, store.UpdateUserParams{Role: &operatorRole, UpdatedAt: now})
		return err
	})
	if !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("demote last admin = %v, want ErrLastAdmin", err)
	}
	// 守卫失败必须回滚，admin 仍是 admin。
	if got, _ := testDB.Users().ByID(ctx, admin.ID); got.Role != store.RoleAdmin || !got.Enabled {
		t.Fatalf("admin changed after rejected update: %+v", got)
	}

	// 第二个 admin 出现后可以降级第一个；此时第二个成为最后管理员。
	admin2, err := testDB.Users().Create(ctx, store.CreateUserParams{
		Username: "admin2", PasswordHash: "$2a$12$admin2",
		Role: store.RoleAdmin, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create admin2: %v", err)
	}
	var updated store.User
	err = testDB.WithTx(ctx, func(tx store.Store) error {
		u, err := tx.Users().Update(ctx, admin.ID, store.UpdateUserParams{
			Role: &operatorRole, DisplayName: strPtr("已降级"), UpdatedAt: now,
		})
		updated = u
		return err
	})
	if err != nil {
		t.Fatalf("demote with second admin: %v", err)
	}
	if updated.Role != store.RoleOperator || updated.DisplayName != "已降级" {
		t.Fatalf("updated = %+v", updated)
	}

	// 第 9 轮走查 medium：同名重发邀请要把新邮箱写入账号行，且不能顺带
	// 清掉未提交的 role/display_name（COALESCE 语义）。
	newEmail := "ops01-new@example.com"
	err = testDB.WithTx(ctx, func(tx store.Store) error {
		u, err := tx.Users().Update(ctx, op.ID, store.UpdateUserParams{
			Email: &newEmail, UpdatedAt: now.Add(time.Minute),
		})
		updated = u
		return err
	})
	if err != nil {
		t.Fatalf("update email: %v", err)
	}
	if updated.Email != newEmail {
		t.Fatalf("updated email = %q, want %q", updated.Email, newEmail)
	}
	if updated.Role != store.RoleOperator || updated.DisplayName != "运维一号" {
		t.Fatalf("email update clobbered other fields: %+v", updated)
	}
	if got, _ := testDB.Users().ByID(ctx, op.ID); got.Email != newEmail {
		t.Fatalf("stored email = %q, want %q", got.Email, newEmail)
	}
	err = testDB.WithTx(ctx, func(tx store.Store) error {
		_, err := tx.Users().Update(ctx, admin2.ID, store.UpdateUserParams{Enabled: &disabled, UpdatedAt: now})
		return err
	})
	if !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("disable last remaining admin = %v, want ErrLastAdmin", err)
	}

	// 密码更新与 must_change。
	mustChange := true
	if err := testDB.Users().UpdatePassword(ctx, admin.ID, store.UpdatePasswordParams{
		PasswordHash: "$2a$12$new", MustChangePassword: &mustChange, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}
	if got, _ := testDB.Users().ByID(ctx, admin.ID); !got.MustChangePassword || got.PasswordHash != "$2a$12$new" {
		t.Fatalf("after UpdatePassword = %+v", got)
	}
	if err := testDB.Users().UpdateLastLogin(ctx, admin.ID, now); err != nil {
		t.Fatalf("UpdateLastLogin: %v", err)
	}
	if got, _ := testDB.Users().ByID(ctx, admin.ID); got.LastLoginAt == nil {
		t.Fatal("last_login_at not stored")
	}
}

func TestUserTokensStore(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	admin := mustUser(t)
	pending, err := testDB.Users().Create(ctx, store.CreateUserParams{
		Username: "pending", PasswordHash: "!pending",
		Role: store.RoleApprover, Enabled: false, Email: "pending@example.com",
	})
	if err != nil {
		t.Fatalf("create pending: %v", err)
	}
	hash := make([]byte, 32)
	for i := range hash {
		hash[i] = byte(i)
	}
	role := store.RoleApprover
	token, err := testDB.Users().CreateToken(ctx, store.CreateUserTokenParams{
		Kind: store.UserTokenInvite, Username: "pending", Email: "pending@example.com",
		Role: &role, TokenHash: hash, ExpiresAt: now.Add(30 * time.Minute),
		CreatedBy: &admin.ID, CreatorIP: "203.0.113.9", DeliveredChannel: "smtp",
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if token.Username != "pending" || token.UserID != nil || token.Role == nil || *token.Role != role {
		t.Fatalf("token = %+v", token)
	}
	if ok, err := testDB.Users().PendingInvite(ctx, "pending"); err != nil || !ok {
		t.Fatalf("PendingInvite = %v, %v", ok, err)
	}
	got, err := testDB.Users().TokenByHash(ctx, hash)
	if err != nil || got.ID != token.ID {
		t.Fatalf("TokenByHash = %+v, %v", got, err)
	}
	// ByUsername 的 invite_pending 派生字段。
	if u, _ := testDB.Users().ByID(ctx, pending.ID); !u.InvitePending {
		t.Fatal("invite_pending = false, want true")
	}
	if ok, err := testDB.Users().MarkTokenUsed(ctx, token.ID, now); err != nil || !ok {
		t.Fatalf("MarkTokenUsed = %v, %v", ok, err)
	}
	if ok, _ := testDB.Users().MarkTokenUsed(ctx, token.ID, now); ok {
		t.Fatal("MarkTokenUsed second call = true")
	}
	if ok, _ := testDB.Users().PendingInvite(ctx, "pending"); ok {
		t.Fatal("PendingInvite after use = true")
	}
	if u, _ := testDB.Users().ByID(ctx, pending.ID); u.InvitePending {
		t.Fatal("invite_pending after use = true")
	}
}

// TestUserInviteExpiryAndReissue：过期 invite 仍计入 invite_pending 与
// UnusedInviteToken（管理员必须走重发邀请），RevokeUnusedInvites 作废旧链接。
func TestUserInviteExpiryAndReissue(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	admin := mustUser(t)
	pending, err := testDB.Users().Create(ctx, store.CreateUserParams{
		Username: "expired-pending", PasswordHash: "!pending",
		Role: store.RoleOperator, Enabled: false, Email: "expired@example.com",
	})
	if err != nil {
		t.Fatalf("create pending: %v", err)
	}
	hash := make([]byte, 32)
	for i := range hash {
		hash[i] = 0xAB
	}
	role := store.RoleOperator
	if _, err := testDB.Users().CreateToken(ctx, store.CreateUserTokenParams{
		Kind: store.UserTokenInvite, Username: "expired-pending", Email: "expired@example.com",
		Role: &role, TokenHash: hash, ExpiresAt: now.Add(-time.Minute),
		CreatedBy: &admin.ID, CreatorIP: "203.0.113.9", DeliveredChannel: "smtp",
		CreatedAt: now.Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateToken(expired): %v", err)
	}

	// 已过期 → PendingInvite=false，但仍有未使用 invite。
	if ok, err := testDB.Users().PendingInvite(ctx, "expired-pending"); err != nil || ok {
		t.Fatalf("PendingInvite(expired) = %v, %v; want false", ok, err)
	}
	if ok, err := testDB.Users().UnusedInviteToken(ctx, "expired-pending"); err != nil || !ok {
		t.Fatalf("UnusedInviteToken(expired) = %v, %v; want true", ok, err)
	}
	if u, _ := testDB.Users().ByID(ctx, pending.ID); !u.InvitePending {
		t.Fatal("invite_pending must stay true for an expired unused invite")
	}

	// 重发邀请前作废旧链接。
	n, err := testDB.Users().RevokeUnusedInvites(ctx, "expired-pending", now)
	if err != nil || n != 1 {
		t.Fatalf("RevokeUnusedInvites = %d, %v; want 1", n, err)
	}
	if ok, _ := testDB.Users().UnusedInviteToken(ctx, "expired-pending"); ok {
		t.Fatal("UnusedInviteToken after revoke = true")
	}
	if u, _ := testDB.Users().ByID(ctx, pending.ID); u.InvitePending {
		t.Fatal("invite_pending after revoke = true")
	}
	if n, _ := testDB.Users().RevokeUnusedInvites(ctx, "expired-pending", now); n != 0 {
		t.Fatalf("second RevokeUnusedInvites = %d, want 0", n)
	}
}

func strPtr(s string) *string { return &s }
