package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"aiops/internal/store"
)

func TestHashPasswordCost(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		t.Fatalf("bcrypt.Cost: %v", err)
	}
	if cost != 12 {
		t.Fatalf("bcrypt cost = %d, want 12", cost)
	}
	if BcryptCost != 12 {
		t.Fatalf("BcryptCost = %d, want 12", BcryptCost)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("VerifyPassword rejected the correct password")
	}
	if VerifyPassword(hash, "wrong") {
		t.Fatal("VerifyPassword accepted a wrong password")
	}
}

func TestNewSessionToken(t *testing.T) {
	raw1, hash1, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	raw2, hash2, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if raw1 == raw2 {
		t.Fatal("two tokens are identical")
	}
	if len(hash1) != 32 {
		t.Fatalf("hash length = %d, want 32", len(hash1))
	}
	if !bytes.Equal(hash1, HashToken(raw1)) {
		t.Fatal("HashToken does not reproduce the stored hash")
	}
	if bytes.Equal(hash1, hash2) {
		t.Fatal("hashes are identical")
	}
	if bytes.Contains(hash1, []byte(raw1)) {
		t.Fatal("stored hash contains the raw token")
	}
}

func TestEnsureAdminCreatesOnce(t *testing.T) {
	st := newFakeStore()
	svc := NewService(st, Config{SessionTTL: time.Hour})

	if err := svc.EnsureAdmin(context.Background(), "hunter2", nil); err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	if len(st.users) != 1 {
		t.Fatalf("users = %d, want 1", len(st.users))
	}
	if _, ok := st.settings[adminCreatedAtKey]; !ok {
		t.Fatal("bootstrap timestamp was not written")
	}
	firstHash := st.users[adminUsername].PasswordHash

	if err := svc.EnsureAdmin(context.Background(), "different", nil); err != nil {
		t.Fatalf("second EnsureAdmin: %v", err)
	}
	if st.users[adminUsername].PasswordHash != firstHash {
		t.Fatal("EnsureAdmin changed an existing password")
	}
}

func TestEnsureAdminGeneratesPassword(t *testing.T) {
	st := newFakeStore()
	svc := NewService(st, Config{SessionTTL: time.Hour})
	var out bytes.Buffer
	if err := svc.EnsureAdmin(context.Background(), "", &out); err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	line := bytes.TrimSpace(out.Bytes())
	const prefix = "admin password: "
	if !bytes.HasPrefix(line, []byte(prefix)) {
		t.Fatalf("output = %q, want %q prefix", line, prefix)
	}
	pw := string(line[len(prefix):])
	if len(pw) != 24 {
		t.Fatalf("generated password length = %d, want 24", len(pw))
	}
	if !VerifyPassword(st.users[adminUsername].PasswordHash, pw) {
		t.Fatal("printed password does not match stored hash")
	}
}

func TestLoginAndAuthenticate(t *testing.T) {
	st := newFakeStore()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	svc := NewService(st, Config{SessionTTL: time.Hour, Clock: clock})
	if err := svc.EnsureAdmin(context.Background(), "secret-pw", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Login(context.Background(), "wrong", "1.2.3.4", "ua"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login(wrong) error = %v, want ErrInvalidCredentials", err)
	}

	res, err := svc.Login(context.Background(), "secret-pw", "1.2.3.4", "ua")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.Token == "" {
		t.Fatal("empty token")
	}
	if !res.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("ExpiresAt = %v", res.ExpiresAt)
	}

	var stored store.Session
	for _, s := range st.sessions {
		stored = s
	}
	if bytes.Equal(stored.TokenHash, []byte(res.Token)) {
		t.Fatal("session stored the raw token")
	}
	if !bytes.Equal(stored.TokenHash, HashToken(res.Token)) {
		t.Fatal("session did not store SHA-256(token)")
	}

	user, err := svc.Authenticate(context.Background(), res.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if user.Username != adminUsername {
		t.Fatalf("user = %q", user.Username)
	}

	if _, err := svc.Authenticate(context.Background(), "bogus"); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("Authenticate(bogus) = %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), ""); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("Authenticate(empty) = %v", err)
	}

	if err := svc.Logout(context.Background(), res.Token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), res.Token); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("Authenticate after logout = %v", err)
	}
}

func TestAuthenticateExpiredDeletesSession(t *testing.T) {
	st := newFakeStore()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	svc := NewService(st, Config{SessionTTL: time.Hour, Clock: clock})
	if err := svc.EnsureAdmin(context.Background(), "pw", nil); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Login(context.Background(), "pw", "", "")
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Hour)
	if _, err := svc.Authenticate(context.Background(), res.Token); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("Authenticate expired = %v", err)
	}
	if st.sessionCount() != 0 {
		t.Fatalf("expired session was not deleted: %d left", st.sessionCount())
	}
}

func TestLoginLimiter(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l := NewLoginLimiter(2, time.Second, clock)

	if ok, _ := l.Allow("1.1.1.1"); !ok {
		t.Fatal("first attempt denied")
	}
	if ok, _ := l.Allow("1.1.1.1"); !ok {
		t.Fatal("second attempt denied")
	}
	ok, retry := l.Allow("1.1.1.1")
	if ok {
		t.Fatal("third attempt allowed")
	}
	if retry <= 0 {
		t.Fatalf("retryAfter = %v, want > 0", retry)
	}
	if ok, _ := l.Allow("2.2.2.2"); !ok {
		t.Fatal("independent IP denied")
	}

	now = now.Add(time.Second)
	if ok, _ := l.Allow("1.1.1.1"); !ok {
		t.Fatal("attempt after refill denied")
	}
}

func TestLoginLimiterCleansIdleBuckets(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l := NewLoginLimiter(1, time.Second, clock)
	l.Allow("old")
	now = now.Add(11 * time.Minute)
	l.Allow("new")
	if _, ok := l.buckets["old"]; ok {
		t.Fatal("idle bucket was not cleaned")
	}
}

// ---- fake store -----------------------------------------------------------

type fakeStore struct {
	store.Store
	// mu 保护 sessions：janitor 测试里后台 goroutine 会并发 DeleteExpired，
	// 测试主 goroutine 同时读计数；其余集合仅单 goroutine 使用。
	mu       sync.Mutex
	users    map[string]store.User
	sessions map[string]store.Session
	settings map[string]store.Setting
	inTx     bool
	seq      int
}

// sessionCount 带锁读取会话数（供 janitor 测试轮询）。
func (f *fakeStore) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:    make(map[string]store.User),
		sessions: make(map[string]store.Session),
		settings: make(map[string]store.Setting),
	}
}

func (f *fakeStore) WithTx(_ context.Context, fn func(store.Store) error) error {
	if f.inTx {
		return store.ErrTxUnsupported
	}
	f.inTx = true
	defer func() { f.inTx = false }()
	return fn(f)
}

func (f *fakeStore) Users() store.UserStore       { return fakeUsers{f} }
func (f *fakeStore) Sessions() store.SessionStore { return fakeSessions{f} }
func (f *fakeStore) Settings() store.SettingStore { return fakeSettings{f} }

type fakeUsers struct{ f *fakeStore }

func (u fakeUsers) Count(context.Context) (int64, error) { return int64(len(u.f.users)), nil }

func (u fakeUsers) Create(_ context.Context, p store.CreateUserParams) (store.User, error) {
	if _, ok := u.f.users[p.Username]; ok {
		return store.User{}, store.ErrConflict
	}
	u.f.seq++
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	user := store.User{
		ID:           fmt.Sprintf("user-%d", u.f.seq),
		Username:     p.Username,
		PasswordHash: p.PasswordHash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	u.f.users[p.Username] = user
	return user, nil
}

func (u fakeUsers) ByUsername(_ context.Context, username string) (store.User, error) {
	user, ok := u.f.users[username]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return user, nil
}

func (u fakeUsers) ByID(_ context.Context, id string) (store.User, error) {
	for _, user := range u.f.users {
		if user.ID == id {
			return user, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

func (u fakeUsers) UsernamesByIDs(_ context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		for _, user := range u.f.users {
			if user.ID == id {
				out[id] = user.Username
				break
			}
		}
	}
	return out, nil
}

func (u fakeUsers) UpdatePassword(_ context.Context, id string, p store.UpdatePasswordParams) error {
	for name, user := range u.f.users {
		if user.ID == id {
			user.PasswordHash = p.PasswordHash
			user.UpdatedAt = p.UpdatedAt
			u.f.users[name] = user
			return nil
		}
	}
	return store.ErrNotFound
}

type fakeSessions struct{ f *fakeStore }

func (s fakeSessions) Create(_ context.Context, p store.CreateSessionParams) (store.Session, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.seq++
	session := store.Session{
		ID:         fmt.Sprintf("sess-%d", s.f.seq),
		UserID:     p.UserID,
		TokenHash:  append([]byte(nil), p.TokenHash...),
		ExpiresAt:  p.ExpiresAt,
		LastSeenAt: p.CreatedAt,
		IP:         p.IP,
		UserAgent:  p.UserAgent,
		CreatedAt:  p.CreatedAt,
	}
	s.f.sessions[session.ID] = session
	return session, nil
}

func (s fakeSessions) ByTokenHash(_ context.Context, hash []byte) (store.Session, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	for _, session := range s.f.sessions {
		if bytes.Equal(session.TokenHash, hash) {
			return session, nil
		}
	}
	return store.Session{}, store.ErrNotFound
}

func (s fakeSessions) Touch(_ context.Context, id string, at time.Time) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	session, ok := s.f.sessions[id]
	if !ok {
		return store.ErrNotFound
	}
	session.LastSeenAt = at
	s.f.sessions[id] = session
	return nil
}

func (s fakeSessions) Delete(_ context.Context, id string) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if _, ok := s.f.sessions[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.f.sessions, id)
	return nil
}

func (s fakeSessions) DeleteByUser(_ context.Context, userID string) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	for id, session := range s.f.sessions {
		if session.UserID == userID {
			delete(s.f.sessions, id)
		}
	}
	return nil
}

func (s fakeSessions) DeleteExpired(_ context.Context, before time.Time) (int64, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	var n int64
	for id, session := range s.f.sessions {
		if !session.ExpiresAt.After(before) {
			delete(s.f.sessions, id)
			n++
		}
	}
	return n, nil
}

type fakeSettings struct{ f *fakeStore }

func (s fakeSettings) Get(_ context.Context, key string) (store.Setting, error) {
	v, ok := s.f.settings[key]
	if !ok {
		return store.Setting{}, store.ErrNotFound
	}
	return v, nil
}

func (s fakeSettings) Set(_ context.Context, key string, value json.RawMessage, at time.Time) (store.Setting, error) {
	setting := store.Setting{Key: key, Value: append(json.RawMessage(nil), value...), UpdatedAt: at}
	s.f.settings[key] = setting
	return setting, nil
}

func (s fakeSettings) List(context.Context) ([]store.Setting, error) {
	out := make([]store.Setting, 0, len(s.f.settings))
	for _, v := range s.f.settings {
		out = append(out, v)
	}
	return out, nil
}

// Authenticate 成功后必须刷新 last_seen_at（Touch 在生产路径的接线）。
func TestAuthenticateTouchesSession(t *testing.T) {
	st := newFakeStore()
	start := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	now := start
	clock := func() time.Time { return now }
	svc := NewService(st, Config{SessionTTL: time.Hour, Clock: clock})
	if err := svc.EnsureAdmin(context.Background(), "secret-pw", nil); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Login(context.Background(), "secret-pw", "1.2.3.4", "ua")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	var stored store.Session
	for _, s := range st.sessions {
		stored = s
	}
	if !stored.LastSeenAt.Equal(start) {
		t.Fatalf("initial LastSeenAt = %v, want %v", stored.LastSeenAt, start)
	}

	now = start.Add(10 * time.Minute)
	if _, err := svc.Authenticate(context.Background(), res.Token); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	for _, s := range st.sessions {
		stored = s
	}
	if !stored.LastSeenAt.Equal(now) {
		t.Fatalf("LastSeenAt after Authenticate = %v, want %v", stored.LastSeenAt, now)
	}
}

// janitor 周期删除过期会话：没有它，sessions 表只增不减，过期会话只有在
// 同一 token 再次提交时才被惰性删除。
func TestJanitorDeletesExpiredSessions(t *testing.T) {
	st := newFakeStore()
	var mu sync.Mutex
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	// janitor goroutine 与测试 goroutine 共用 clock，now 必须加锁读写。
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	svc := NewService(st, Config{
		SessionTTL:             time.Hour,
		SessionCleanupInterval: 10 * time.Millisecond,
		Clock:                  clock,
	})
	if err := svc.EnsureAdmin(context.Background(), "secret-pw", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(context.Background(), "secret-pw", "1.2.3.4", "ua"); err != nil {
		t.Fatal(err)
	}
	if st.sessionCount() != 1 {
		t.Fatalf("sessions = %d, want 1", st.sessionCount())
	}

	svc.Start()
	defer svc.Stop()

	// 会话过期后（不再提交该 token），janitor 也必须把行删掉。
	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st.sessionCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expired session still present: %d", st.sessionCount())
}

// Stop 之后 janitor 退出（不留泄漏的 goroutine）。
func TestJanitorStopIsIdempotent(t *testing.T) {
	st := newFakeStore()
	svc := NewService(st, Config{SessionTTL: time.Hour, SessionCleanupInterval: 10 * time.Millisecond})
	svc.Start()
	svc.Stop()
	svc.Stop()
}
