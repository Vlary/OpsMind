package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// 错误注入包装：在既有 fakeStore 上覆盖个别方法
// ---------------------------------------------------------------------------

type stubErr struct{ err error }

func (s stubErr) apply() error { return s.err }

type errUsersStore struct {
	*fakeStore
	countErr      error
	byUsernameErr error
	byIDErr       error
	updateErr     error
	countCalls    atomic.Int64
	// forceCountAfterFirst 让第一次 Count 返回 0（触发建 admin），
	// 事务内第二次 Count 返回 1（模拟并发创建，走 errAdminExists）。
	forceCountAfterFirst bool
}

func (f *errUsersStore) Users() store.UserStore { return errUsers{f} }

// WithTx 把包装后的 store 传给事务回调，保证覆盖的方法在事务内同样生效。
func (f *errUsersStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return fn(f)
}

type errUsers struct{ s *errUsersStore }

func (u errUsers) Count(ctx context.Context) (int64, error) {
	if err := u.s.countErr; err != nil {
		return 0, err
	}
	n := u.s.countCalls.Add(1)
	if u.s.forceCountAfterFirst && n >= 2 {
		return 1, nil
	}
	return u.s.fakeStore.Users().Count(ctx)
}
func (u errUsers) Create(ctx context.Context, p store.CreateUserParams) (store.User, error) {
	return u.s.fakeStore.Users().Create(ctx, p)
}
func (u errUsers) ByUsername(ctx context.Context, username string) (store.User, error) {
	if err := u.s.byUsernameErr; err != nil {
		return store.User{}, err
	}
	return u.s.fakeStore.Users().ByUsername(ctx, username)
}
func (u errUsers) ByID(ctx context.Context, id string) (store.User, error) {
	if err := u.s.byIDErr; err != nil {
		return store.User{}, err
	}
	return u.s.fakeStore.Users().ByID(ctx, id)
}
func (u errUsers) UsernamesByIDs(ctx context.Context, ids []string) (map[string]string, error) {
	return u.s.fakeStore.Users().UsernamesByIDs(ctx, ids)
}
func (u errUsers) UpdatePassword(ctx context.Context, id string, p store.UpdatePasswordParams) error {
	if err := u.s.updateErr; err != nil {
		return err
	}
	return u.s.fakeStore.Users().UpdatePassword(ctx, id, p)
}

type errSessionsStore struct {
	*fakeStore
	createErr      error
	byTokenHashErr error
	deleteErr      error
}

func (f *errSessionsStore) Sessions() store.SessionStore { return errSessions{f} }

type errSessions struct{ s *errSessionsStore }

func (s errSessions) Create(ctx context.Context, p store.CreateSessionParams) (store.Session, error) {
	if err := s.s.createErr; err != nil {
		return store.Session{}, err
	}
	return s.s.fakeStore.Sessions().Create(ctx, p)
}
func (s errSessions) ByTokenHash(ctx context.Context, hash []byte) (store.Session, error) {
	if err := s.s.byTokenHashErr; err != nil {
		return store.Session{}, err
	}
	return s.s.fakeStore.Sessions().ByTokenHash(ctx, hash)
}
func (s errSessions) Touch(ctx context.Context, id string, at time.Time) error {
	return s.s.fakeStore.Sessions().Touch(ctx, id, at)
}
func (s errSessions) Delete(ctx context.Context, id string) error {
	if err := s.s.deleteErr; err != nil {
		return err
	}
	return s.s.fakeStore.Sessions().Delete(ctx, id)
}
func (s errSessions) DeleteByUser(ctx context.Context, userID string) error {
	return s.s.fakeStore.Sessions().DeleteByUser(ctx, userID)
}
func (s errSessions) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	return s.s.fakeStore.Sessions().DeleteExpired(ctx, before)
}

type errSettingsStore struct {
	*fakeStore
	setErr error
}

func (f *errSettingsStore) Settings() store.SettingStore { return errSettings{f} }

func (f *errSettingsStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return fn(f)
}

type errSettings struct{ s *errSettingsStore }

func (s errSettings) Get(ctx context.Context, key string) (store.Setting, error) {
	return s.s.fakeStore.Settings().Get(ctx, key)
}
func (s errSettings) Set(ctx context.Context, key string, value json.RawMessage, at time.Time) (store.Setting, error) {
	if err := s.s.setErr; err != nil {
		return store.Setting{}, err
	}
	return s.s.fakeStore.Settings().Set(ctx, key, value, at)
}
func (s errSettings) List(ctx context.Context) ([]store.Setting, error) {
	return s.s.fakeStore.Settings().List(ctx)
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

func TestNewLoginLimiterAppliesDefaults(t *testing.T) {
	l := NewLoginLimiter(0, 0, nil)
	if l.capacity != 1 {
		t.Fatalf("capacity = %d, want 1", l.capacity)
	}
	if l.refill != time.Second {
		t.Fatalf("refill = %v, want 1s", l.refill)
	}
	if l.clock == nil {
		t.Fatal("clock must default to time.Now")
	}
}

func TestLoginLimiterRejectsNewIPsAtCapacityLimit(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	l := NewLoginLimiter(1, time.Hour, func() time.Time { return now })

	// 4096 个不同 IP 各用掉唯一一个 token。
	for i := 0; i < loginLimiterMaxEntries; i++ {
		ip := "10." + itoa(i/256) + "." + itoa(i%256) + ".1"
		ok, _ := l.Allow(ip)
		if !ok {
			t.Fatalf("Allow(%s) denied before reaching the entry limit", ip)
		}
	}
	// 第 4097 个 IP：map 已满，必须拒绝而不是无界扩张。
	ok, wait := l.Allow("192.0.2.1")
	if ok {
		t.Fatal("new IP allowed after reaching loginLimiterMaxEntries")
	}
	if wait != time.Hour {
		t.Fatalf("wait = %v, want refill (1h)", wait)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// 用户不存在时 Login 仍对 dummy hash 做一次同成本 bcrypt（时间侧信道防护）。
func TestLoginWithMissingUserRunsDummyVerify(t *testing.T) {
	svc := NewService(newFakeStore(), Config{SessionTTL: time.Hour, Clock: time.Now})
	_, err := svc.Login(context.Background(), "whatever", "1.2.3.4", "ua")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	if len(dummyHash) == 0 {
		t.Fatal("dummy hash must be initialized by the missing-user login path")
	}
}

func TestLoginStoreErrors(t *testing.T) {
	base := newFakeStore()
	svc := NewService(base, Config{SessionTTL: time.Hour, Clock: time.Now})
	if err := svc.EnsureAdmin(context.Background(), "secret-pw", nil); err != nil {
		t.Fatal(err)
	}

	dbErr := errors.New("db unavailable")
	st := &errUsersStore{fakeStore: base, byUsernameErr: dbErr}
	_, err := NewService(st, Config{SessionTTL: time.Hour}).Login(context.Background(), "secret-pw", "1.1.1.1", "ua")
	if !errors.Is(err, dbErr) {
		t.Fatalf("ByUsername error not propagated: %v", err)
	}

	st2 := &errSessionsStore{fakeStore: base, createErr: dbErr}
	_, err = NewService(st2, Config{SessionTTL: time.Hour}).Login(context.Background(), "secret-pw", "1.1.1.1", "ua")
	if !errors.Is(err, dbErr) {
		t.Fatalf("Sessions().Create error not propagated: %v", err)
	}
}

func TestAuthenticateErrorBranches(t *testing.T) {
	// 空 token。
	svc := NewService(newFakeStore(), Config{SessionTTL: time.Hour})
	if _, err := svc.Authenticate(context.Background(), ""); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("empty token: %v", err)
	}

	dbErr := errors.New("db unavailable")
	// ByTokenHash 存储错误 → 原样返回。
	if _, err := NewService(&errSessionsStore{fakeStore: newFakeStore(), byTokenHashErr: dbErr},
		Config{SessionTTL: time.Hour}).Authenticate(context.Background(), "raw-token"); !errors.Is(err, dbErr) {
		t.Fatalf("ByTokenHash error not propagated: %v", err)
	}

	// 有效会话但用户已被删 → ErrAuthRequired。
	base := newFakeStore()
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	svc = NewService(base, Config{SessionTTL: time.Hour, Clock: func() time.Time { return now }})
	if err := svc.EnsureAdmin(context.Background(), "secret-pw", nil); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Login(context.Background(), "secret-pw", "1.1.1.1", "ua")
	if err != nil {
		t.Fatal(err)
	}
	for name := range base.users {
		delete(base.users, name)
	}
	if _, err := svc.Authenticate(context.Background(), res.Token); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("missing user: %v", err)
	}

	// 用户查询的存储错误 → 原样返回。
	base2 := newFakeStore()
	svc2 := NewService(base2, Config{SessionTTL: time.Hour, Clock: func() time.Time { return now }})
	if err := svc2.EnsureAdmin(context.Background(), "secret-pw", nil); err != nil {
		t.Fatal(err)
	}
	res2, err := svc2.Login(context.Background(), "secret-pw", "1.1.1.1", "ua")
	if err != nil {
		t.Fatal(err)
	}
	joined := &errUsersStore{fakeStore: base2, byIDErr: dbErr}
	if _, err := NewService(joined, Config{SessionTTL: time.Hour, Clock: func() time.Time { return now }}).
		Authenticate(context.Background(), res2.Token); !errors.Is(err, dbErr) {
		t.Fatalf("ByID error not propagated: %v", err)
	}

	// 过期会话删除失败时结论不变（仍 401）。
	base3 := newFakeStore()
	later := now.Add(2 * time.Hour)
	svc3 := NewService(base3, Config{SessionTTL: time.Minute, Clock: func() time.Time { return now }})
	if err := svc3.EnsureAdmin(context.Background(), "secret-pw", nil); err != nil {
		t.Fatal(err)
	}
	res3, err := svc3.Login(context.Background(), "secret-pw", "1.1.1.1", "ua")
	if err != nil {
		t.Fatal(err)
	}
	// 复用同一底层 fakeStore，但把时钟拨到过期之后并让 Delete 失败。
	expiredSvc := NewService(&errSessionsStore{fakeStore: base3, deleteErr: dbErr},
		Config{SessionTTL: time.Minute, Clock: func() time.Time { return later }})
	if _, err := expiredSvc.Authenticate(context.Background(), res3.Token); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("expired session with failing delete: %v", err)
	}
}

func TestLogoutBranches(t *testing.T) {
	base := newFakeStore()
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	svc := NewService(base, Config{SessionTTL: time.Hour, Clock: func() time.Time { return now }})
	if err := svc.EnsureAdmin(context.Background(), "secret-pw", nil); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Login(context.Background(), "secret-pw", "1.1.1.1", "ua")
	if err != nil {
		t.Fatal(err)
	}

	// 空 token 幂等成功。
	if err := svc.Logout(context.Background(), ""); err != nil {
		t.Fatalf("empty token: %v", err)
	}
	// 不存在的会话视为成功。
	if err := svc.Logout(context.Background(), "no-such-token"); err != nil {
		t.Fatalf("unknown token: %v", err)
	}

	dbErr := errors.New("db unavailable")
	// 查询存储错误 → 返回错误。
	if err := NewService(&errSessionsStore{fakeStore: base, byTokenHashErr: dbErr},
		Config{SessionTTL: time.Hour}).Logout(context.Background(), res.Token); !errors.Is(err, dbErr) {
		t.Fatalf("ByTokenHash error not propagated: %v", err)
	}
	// 删除存储错误 → 返回错误。
	if err := NewService(&errSessionsStore{fakeStore: base, deleteErr: dbErr},
		Config{SessionTTL: time.Hour}).Logout(context.Background(), res.Token); !errors.Is(err, dbErr) {
		t.Fatalf("Delete error not propagated: %v", err)
	}
	// 正常登出后再次登出（幂等）。
	if err := svc.Logout(context.Background(), res.Token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if err := svc.Logout(context.Background(), res.Token); err != nil {
		t.Fatalf("second Logout must be idempotent: %v", err)
	}
}

func TestEnsureAdminBranches(t *testing.T) {
	ctx := context.Background()

	// 已有用户 → 直接返回（不写 settings、不生成密码）。
	base := newFakeStore()
	svc := NewService(base, Config{SessionTTL: time.Hour})
	if err := svc.EnsureAdmin(ctx, "first-pw", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnsureAdmin(ctx, "second-pw", nil); err != nil {
		t.Fatalf("existing users must short-circuit: %v", err)
	}
	if len(base.settings) != 1 {
		t.Fatalf("settings written twice: %d", len(base.settings))
	}

	// Count 存储错误 → 返回错误。
	countErr := errors.New("count failed")
	if err := NewService(&errUsersStore{fakeStore: newFakeStore(), countErr: countErr},
		Config{SessionTTL: time.Hour}).EnsureAdmin(ctx, "pw", nil); !errors.Is(err, countErr) {
		t.Fatalf("Count error not propagated: %v", err)
	}

	// 密码超过 bcrypt 72 字节上限 → ErrPasswordHashing。
	if err := NewService(newFakeStore(), Config{SessionTTL: time.Hour}).
		EnsureAdmin(ctx, strings.Repeat("x", 73), nil); !errors.Is(err, ErrPasswordHashing) {
		t.Fatalf("oversized password: %v", err)
	}

	// 事务内发现并发创建（第二次 Count > 0）→ 静默成功。
	if err := NewService(&errUsersStore{fakeStore: newFakeStore(), forceCountAfterFirst: true},
		Config{SessionTTL: time.Hour}).EnsureAdmin(ctx, "pw", nil); err != nil {
		t.Fatalf("concurrent creation must be swallowed: %v", err)
	}

	// settings 写入失败 → 返回错误。
	setErr := errors.New("settings unavailable")
	if err := NewService(&errSettingsStore{fakeStore: newFakeStore(), setErr: setErr},
		Config{SessionTTL: time.Hour}).EnsureAdmin(ctx, "pw", nil); !errors.Is(err, setErr) {
		t.Fatalf("settings error not propagated: %v", err)
	}

	// 生成密码写入 out 失败 → 返回错误。
	writeErr := errors.New("write failed")
	if err := NewService(newFakeStore(), Config{SessionTTL: time.Hour}).
		EnsureAdmin(ctx, "", errWriter{writeErr}); !errors.Is(err, writeErr) {
		t.Fatalf("writer error not propagated: %v", err)
	}
	// out 为 nil 时不因写密码失败。
	if err := NewService(newFakeStore(), Config{SessionTTL: time.Hour}).EnsureAdmin(ctx, "", nil); err != nil {
		t.Fatalf("nil writer must not fail: %v", err)
	}
}

type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }

func TestHashPasswordRejectsLongInput(t *testing.T) {
	if _, err := HashPassword(strings.Repeat("x", 73)); !errors.Is(err, ErrPasswordHashing) {
		t.Fatalf("73-byte password must fail: %v", err)
	}
	if _, err := HashPassword(strings.Repeat("x", 72)); err != nil {
		t.Fatalf("72-byte password must pass: %v", err)
	}
}

func TestRandomPasswordLength(t *testing.T) {
	for _, n := range []int{24, 4, 1} {
		pw, err := randomPassword(n)
		if err != nil {
			t.Fatalf("randomPassword(%d): %v", n, err)
		}
		if len(pw) != n {
			t.Fatalf("randomPassword(%d) len = %d", n, len(pw))
		}
	}
}
