package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestChangePasswordUpdatesHashAndRevokesSessions(t *testing.T) {
	st := newFakeStore()
	svc := NewService(st, Config{SessionTTL: time.Hour, Clock: time.Now})
	if err := svc.EnsureAdmin(context.Background(), "old-pw", nil); err != nil {
		t.Fatal(err)
	}
	user := st.users[adminUsername]

	// 两个并发会话（模拟多端登录）。
	res1, err := svc.Login(context.Background(), "old-pw", "1.1.1.1", "ua-1")
	if err != nil {
		t.Fatal(err)
	}
	res2, err := svc.Login(context.Background(), "old-pw", "1.1.1.1", "ua-2")
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.ChangePassword(context.Background(), user.ID, "old-pw", "new-pw-42"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	// 哈希已更新为新密码。
	if !VerifyPassword(st.users[adminUsername].PasswordHash, "new-pw-42") {
		t.Fatal("stored hash does not match the new password")
	}
	// 全部会话被吊销：两个 token 都不再通过鉴权。
	if _, err := svc.Authenticate(context.Background(), res1.Token); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("session 1 still valid: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), res2.Token); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("session 2 still valid: %v", err)
	}
	// 新密码可登录，旧密码不行。
	if _, err := svc.Login(context.Background(), "old-pw", "1.1.1.1", "ua"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password accepted after change: %v", err)
	}
	if _, err := svc.Login(context.Background(), "new-pw-42", "1.1.1.1", "ua"); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
}

func TestChangePasswordWrongOldPassword(t *testing.T) {
	st := newFakeStore()
	svc := NewService(st, Config{SessionTTL: time.Hour})
	if err := svc.EnsureAdmin(context.Background(), "old-pw", nil); err != nil {
		t.Fatal(err)
	}
	user := st.users[adminUsername]
	res, err := svc.Login(context.Background(), "old-pw", "1.1.1.1", "ua")
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.ChangePassword(context.Background(), user.ID, "wrong", "new-pw"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	// 旧密码错误时不得改动哈希、不得吊销会话。
	if !VerifyPassword(st.users[adminUsername].PasswordHash, "old-pw") {
		t.Fatal("hash changed on failed attempt")
	}
	if _, err := svc.Authenticate(context.Background(), res.Token); err != nil {
		t.Fatalf("session revoked on failed attempt: %v", err)
	}
}

func TestChangePasswordNewPasswordBounds(t *testing.T) {
	st := newFakeStore()
	svc := NewService(st, Config{SessionTTL: time.Hour})
	if err := svc.EnsureAdmin(context.Background(), "old-pw", nil); err != nil {
		t.Fatal(err)
	}
	user := st.users[adminUsername]

	for name, pw := range map[string]string{
		"empty":               "",
		"oversized":           strings.Repeat("x", 73),
		"multibyte-oversized": strings.Repeat("密", 25), // 75 字节
	} {
		if err := svc.ChangePassword(context.Background(), user.ID, "old-pw", pw); !errors.Is(err, ErrPasswordInvalid) {
			t.Fatalf("%s: err = %v, want ErrPasswordInvalid", name, err)
		}
	}
	// 恰好 72 字节（bcrypt 上限）必须允许。
	if err := svc.ChangePassword(context.Background(), user.ID, "old-pw", strings.Repeat("x", 72)); err != nil {
		t.Fatalf("72-byte password rejected: %v", err)
	}
}

func TestChangePasswordStoreBranches(t *testing.T) {
	ctx := context.Background()
	base := newFakeStore()
	svc := NewService(base, Config{SessionTTL: time.Hour})
	if err := svc.EnsureAdmin(ctx, "old-pw", nil); err != nil {
		t.Fatal(err)
	}
	user := base.users[adminUsername]

	// 用户不存在 → ErrInvalidCredentials（不区分暴露原因）。
	if err := svc.ChangePassword(ctx, "no-such-user", "old-pw", "new-pw"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("missing user: %v", err)
	}

	// ByID 存储错误 → 原样传播。
	dbErr := errors.New("db unavailable")
	if err := NewService(&errUsersStore{fakeStore: base, byIDErr: dbErr}, Config{SessionTTL: time.Hour}).
		ChangePassword(ctx, user.ID, "old-pw", "new-pw"); !errors.Is(err, dbErr) {
		t.Fatalf("ByID error not propagated: %v", err)
	}

	// 事务内 UpdatePassword 失败 → 原样传播，且密码与会话都未被改动。
	if err := NewService(&errUsersStore{fakeStore: base, updateErr: dbErr}, Config{SessionTTL: time.Hour}).
		ChangePassword(ctx, user.ID, "old-pw", "new-pw"); !errors.Is(err, dbErr) {
		t.Fatalf("UpdatePassword error not propagated: %v", err)
	}
	if !VerifyPassword(base.users[adminUsername].PasswordHash, "old-pw") {
		t.Fatal("hash changed despite store error")
	}
}
