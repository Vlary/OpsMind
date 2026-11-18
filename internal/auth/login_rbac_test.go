package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"aiops/internal/store"
)

// captureDeliverer 记录最近一次投递的 token（测试用）。
type captureDeliverer struct {
	kind   store.UserTokenKind
	email  string
	token  string
	expiry time.Time
	err    error
}

func (c *captureDeliverer) DeliverToken(_ context.Context, kind store.UserTokenKind, email, _ string, rawToken string, expiresAt time.Time) error {
	if c.err != nil {
		return c.err
	}
	c.kind = kind
	c.email = email
	c.token = rawToken
	c.expiry = expiresAt
	return nil
}

func mustHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// 未知用户/停用用户/错密码统一 ErrInvalidCredentials；启用用户可登录并写 last_login_at。
func TestLoginMultiUserUniformErrors(t *testing.T) {
	st := newFakeStore()
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	svc := NewService(st, Config{SessionTTL: time.Hour, Clock: func() time.Time { return now }})
	if err := svc.EnsureAdmin(context.Background(), "admin-pw", nil); err != nil {
		t.Fatal(err)
	}
	st.users["ops"] = store.User{
		ID: "user-ops", Username: "ops", PasswordHash: mustHash(t, "ops-pw"),
		Role: store.RoleOperator, Enabled: true,
	}
	st.users["suspended"] = store.User{
		ID: "user-susp", Username: "suspended", PasswordHash: mustHash(t, "susp-pw"),
		Role: store.RoleOperator, Enabled: false,
	}

	for _, tc := range []struct{ name, user, password string }{
		{"unknown", "nobody", "whatever"},
		{"wrong password", "ops", "bad"},
		{"disabled", "suspended", "susp-pw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Login(context.Background(), tc.user, tc.password, "1.1.1.1", "ua"); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("Login = %v, want ErrInvalidCredentials", err)
			}
		})
	}

	res, err := svc.Login(context.Background(), "ops", "ops-pw", "1.1.1.1", "ua")
	if err != nil {
		t.Fatalf("Login(ops): %v", err)
	}
	if res.User.Role != store.RoleOperator {
		t.Fatalf("role = %q", res.User.Role)
	}
	if res.User.LastLoginAt == nil || !res.User.LastLoginAt.Equal(now) {
		t.Fatalf("LastLoginAt = %v, want %v", res.User.LastLoginAt, now)
	}
	if got, _ := svc.Authenticate(context.Background(), res.Token); got.ID != "user-ops" {
		t.Fatalf("Authenticate user = %q", got.ID)
	}
	// AuthenticateSession 为 WS 30 秒复检返回命中的 auth session。
	if u, sess, err := svc.AuthenticateSession(context.Background(), res.Token); err != nil ||
		u.ID != "user-ops" || sess.UserID != "user-ops" || sess.ID == "" {
		t.Fatalf("AuthenticateSession = %+v %+v %v", u, sess, err)
	}

	// 停用后存量会话立即失效。
	u := st.users["ops"]
	u.Enabled = false
	st.users["ops"] = u
	if _, err := svc.Authenticate(context.Background(), res.Token); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("disabled user session = %v, want ErrAuthRequired", err)
	}
}

// EnsureAdmin 创建 enabled admin；已有用户时不改写角色/密码。
func TestEnsureAdminRole(t *testing.T) {
	st := newFakeStore()
	svc := NewService(st, Config{SessionTTL: time.Hour})
	if err := svc.EnsureAdmin(context.Background(), "pw", nil); err != nil {
		t.Fatal(err)
	}
	admin := st.users[adminUsername]
	if admin.Role != store.RoleAdmin || !admin.Enabled {
		t.Fatalf("bootstrap admin = %+v", admin)
	}
	firstHash := admin.PasswordHash

	// 模拟升级后的已有部署：EnsureAdmin 不得改动。
	if err := svc.EnsureAdmin(context.Background(), "other", nil); err != nil {
		t.Fatal(err)
	}
	if got := st.users[adminUsername]; got.Role != store.RoleAdmin || !got.Enabled || got.PasswordHash != firstHash {
		t.Fatalf("EnsureAdmin modified existing admin: %+v", got)
	}
}

func TestInviteAndResetTokenLifecycle(t *testing.T) {
	st := newFakeStore()
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	d := &captureDeliverer{}
	svc := NewService(st, Config{
		SessionTTL: time.Hour,
		Clock:      func() time.Time { return now },
		Deliverer:  d,
	})

	// invite：停用用户接受后启用并设置密码。
	st.users["invitee"] = store.User{
		ID: "user-inv", Username: "invitee", PasswordHash: "!pending",
		Role: store.RoleOperator, Enabled: false, Email: "invitee@example.com",
	}
	token, err := svc.IssueInviteToken(context.Background(), st.users["invitee"], store.RoleOperator, "admin-1", "203.0.113.9")
	if err != nil {
		t.Fatalf("IssueInviteToken: %v", err)
	}
	if d.token == "" || d.kind != store.UserTokenInvite || d.email != "invitee@example.com" {
		t.Fatalf("deliverer got %+v", d)
	}
	if !token.ExpiresAt.Equal(now.Add(OneTimeTokenTTL)) {
		t.Fatalf("expires = %v", token.ExpiresAt)
	}
	// 创建者 IP 兑换被拒。
	if _, _, err := svc.AcceptToken(context.Background(), d.token, "new-pw", "", "203.0.113.9"); !errors.Is(err, ErrSelfRedeem) {
		t.Fatalf("self redeem = %v", err)
	}
	kind, userID, err := svc.AcceptToken(context.Background(), d.token, "new-pw", "邀请用户", "198.51.100.7")
	if err != nil || kind != store.UserTokenInvite || userID == "" {
		t.Fatalf("AcceptToken = %q %q %v", kind, userID, err)
	}
	invitee := st.users["invitee"]
	if !invitee.Enabled || invitee.DisplayName != "邀请用户" || !VerifyPassword(invitee.PasswordHash, "new-pw") {
		t.Fatalf("invitee after accept = %+v", invitee)
	}
	// token 单次使用。
	if _, _, err := svc.AcceptToken(context.Background(), d.token, "again", "", "198.51.100.7"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("replayed token = %v", err)
	}

	// reset：接受后密码更新并吊销该用户全部会话。
	session, err := svc.Login(context.Background(), "invitee", "new-pw", "1.1.1.1", "ua")
	if err != nil {
		t.Fatalf("login before reset: %v", err)
	}
	if _, err := svc.IssueResetToken(context.Background(), st.users["invitee"], "admin-1", "203.0.113.9"); err != nil {
		t.Fatalf("IssueResetToken: %v", err)
	}
	if d.kind != store.UserTokenReset || d.email != "invitee@example.com" {
		t.Fatalf("reset deliverer got %+v", d)
	}
	if _, _, err := svc.AcceptToken(context.Background(), d.token, "reset-pw", "", "198.51.100.7"); err != nil {
		t.Fatalf("AcceptToken(reset): %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), session.Token); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("session survived reset: %v", err)
	}
	if !VerifyPassword(st.users["invitee"].PasswordHash, "reset-pw") {
		t.Fatal("reset password not stored")
	}
	if _, err := svc.Login(context.Background(), "invitee", "reset-pw", "1.1.1.1", "ua"); err != nil {
		t.Fatalf("login with reset password: %v", err)
	}
}

// TestDeliveryUnavailable 确认未配置通道时签发被拒绝。
func TestDeliveryUnavailable(t *testing.T) {
	st := newFakeStore()
	svc := NewService(st, Config{SessionTTL: time.Hour})
	if svc.CanDeliver() {
		t.Fatal("CanDeliver = true without Deliverer")
	}
	user := store.User{ID: "u1", Username: "x", Email: "x@example.com"}
	if _, err := svc.IssueInviteToken(context.Background(), user, store.RoleOperator, "", ""); !errors.Is(err, ErrDeliveryUnavailable) {
		t.Fatalf("IssueInviteToken = %v, want ErrDeliveryUnavailable", err)
	}
	if _, err := svc.IssueResetToken(context.Background(), user, "", ""); !errors.Is(err, ErrDeliveryUnavailable) {
		t.Fatalf("IssueResetToken = %v, want ErrDeliveryUnavailable", err)
	}
}
