package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"aiops/internal/store"
)

// txErrUsers 让事务内的 Count 或 Create 返回注入错误，而外层调用正常，
// 用于覆盖 EnsureAdmin 事务内的错误传播分支。
type txErrUsersStore struct {
	*fakeStore
	txCountErr  error
	txCreateErr error
	inTx        bool
}

func (f *txErrUsersStore) WithTx(_ context.Context, fn func(store.Store) error) error {
	f.inTx = true
	defer func() { f.inTx = false }()
	return fn(f)
}

func (f *txErrUsersStore) Users() store.UserStore { return txErrUsers{f} }

type txErrUsers struct{ s *txErrUsersStore }

func (u txErrUsers) Count(ctx context.Context) (int64, error) {
	if u.s.inTx && u.s.txCountErr != nil {
		return 0, u.s.txCountErr
	}
	return u.s.fakeStore.Users().Count(ctx)
}
func (u txErrUsers) Create(ctx context.Context, p store.CreateUserParams) (store.User, error) {
	if u.s.inTx && u.s.txCreateErr != nil {
		return store.User{}, u.s.txCreateErr
	}
	return u.s.fakeStore.Users().Create(ctx, p)
}
func (u txErrUsers) ByUsername(ctx context.Context, username string) (store.User, error) {
	return u.s.fakeStore.Users().ByUsername(ctx, username)
}
func (u txErrUsers) ByID(ctx context.Context, id string) (store.User, error) {
	return u.s.fakeStore.Users().ByID(ctx, id)
}
func (u txErrUsers) UpdatePassword(ctx context.Context, id string, p store.UpdatePasswordParams) error {
	return u.s.fakeStore.Users().UpdatePassword(ctx, id, p)
}
func (u txErrUsers) UsernamesByIDs(ctx context.Context, ids []string) (map[string]string, error) {
	return u.s.fakeStore.Users().UsernamesByIDs(ctx, ids)
}

func TestNewServiceDefaultSessionTTL(t *testing.T) {
	svc := NewService(newFakeStore(), Config{})
	if svc.cfg.SessionTTL != 24*time.Hour {
		t.Fatalf("SessionTTL = %v, want 24h default", svc.cfg.SessionTTL)
	}
	if svc.cfg.Clock == nil {
		t.Fatal("Clock must default to time.Now")
	}
}

// 事务内 Count 失败必须使 EnsureAdmin 整体失败（不吞并发计数错误）。
func TestEnsureAdminTxCountError(t *testing.T) {
	txErr := errors.New("tx count failed")
	svc := NewService(&txErrUsersStore{fakeStore: newFakeStore(), txCountErr: txErr}, Config{SessionTTL: time.Hour})
	if err := svc.EnsureAdmin(context.Background(), "pw", nil); !errors.Is(err, txErr) {
		t.Fatalf("tx Count error = %v, want %v", err, txErr)
	}
}

// 事务内 Create 失败必须使 EnsureAdmin 整体失败。
func TestEnsureAdminTxCreateError(t *testing.T) {
	txErr := errors.New("tx create failed")
	base := newFakeStore()
	svc := NewService(&txErrUsersStore{fakeStore: base, txCreateErr: txErr}, Config{SessionTTL: time.Hour})
	if err := svc.EnsureAdmin(context.Background(), "pw", nil); !errors.Is(err, txErr) {
		t.Fatalf("tx Create error = %v, want %v", err, txErr)
	}
	if len(base.users) != 0 {
		t.Fatalf("users must stay empty after failed tx, got %d", len(base.users))
	}
}

// 未配置清理周期时按 TTL/24（下限 1 分钟）计算：小 TTL 也必须走到下限分支。
func TestStartDefaultIntervalLowerBound(t *testing.T) {
	svc := NewService(newFakeStore(), Config{SessionTTL: time.Minute})
	svc.Start() // interval = TTL/24 < 1min → 1min 下限
	svc.Stop()
	svc.Stop() // 幂等
}

// 令牌桶补充/截断：长时间未访问后 tokens 不得超过 capacity。
func TestLoginLimiterRefillCappedAtCapacity(t *testing.T) {
	now := time.Now()
	svc := NewLoginLimiter(2, time.Hour, func() time.Time { return now })
	if ok, _ := svc.Allow("1.2.3.4"); !ok {
		t.Fatal("first allow should pass")
	}
	// 耗掉第二个 token 后推进 3 个 refill 周期，补充量必须截断在 capacity。
	if ok, _ := svc.Allow("1.2.3.4"); !ok {
		t.Fatal("second allow should pass")
	}
	now = now.Add(3 * time.Hour)
	ok, wait := svc.Allow("1.2.3.4")
	if !ok {
		t.Fatalf("after refill cap: ok=false wait=%v, want allowed", wait)
	}
	svc.mu.Lock()
	tokens := svc.buckets["1.2.3.4"].tokens
	svc.mu.Unlock()
	if tokens > 2 {
		t.Fatalf("tokens = %v, must be capped at capacity 2", tokens)
	}
	if tokens != 1 { // 3h 补充到 2（截断），本次消耗 1
		t.Fatalf("tokens = %v, want 1", tokens)
	}
}

// 拒绝等待时间向下舍入为 0 时按一个 refill 周期兜底。
func TestLoginLimiterWaitFloor(t *testing.T) {
	now := time.Now()
	l := NewLoginLimiter(1, time.Nanosecond, func() time.Time { return now })
	l.mu.Lock()
	// tokens 无限接近 1（可表示的最小差），(1-tokens)*1ns 会舍入为 0。
	l.buckets["9.9.9.9"] = &bucket{tokens: 0.9999999999999999, lastSeen: now}
	l.mu.Unlock()
	now = now.Add(0) // elapsed == 0，不补充
	ok, wait := l.Allow("9.9.9.9")
	if ok {
		t.Fatal("tokens < 1 must be denied")
	}
	if wait != time.Nanosecond {
		t.Fatalf("wait = %v, want 1ns floor", wait)
	}
}
