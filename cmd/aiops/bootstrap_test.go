// bootstrap_test.go 覆盖单管理员降级模式的启动告警与第二人引导
// （契约 12.3 第 25 条 / 5.10）。
package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aiops/internal/audit"
	"aiops/internal/auth"
	"aiops/internal/config"
	"aiops/internal/store"
)

// bootstrapStore 是启动引导需要的最小存储。
type bootstrapStore struct {
	store.Store
	users  map[string]store.User
	audits []store.CreateAuditParams
	outbox [][]byte
}

func newBootstrapStore() *bootstrapStore {
	return &bootstrapStore{users: map[string]store.User{}}
}

func (s *bootstrapStore) WithTx(_ context.Context, fn func(store.Store) error) error { return fn(s) }
func (s *bootstrapStore) Users() store.UserStore                                     { return bootstrapUsers{s: s} }
func (s *bootstrapStore) AuditLogs() store.AuditStore                                { return bootstrapAudit{s: s} }
func (s *bootstrapStore) BatchJobs() store.BatchJobStore                             { return nil }
func (s *bootstrapStore) EnqueueAuditOutbox(_ context.Context, payload []byte, _ time.Time) error {
	s.outbox = append(s.outbox, payload)
	return nil
}
func (s *bootstrapStore) PendingAuditOutbox(context.Context, int) ([]store.AuditOutboxEntry, error) {
	return nil, nil
}
func (s *bootstrapStore) ProcessAuditOutbox(context.Context, []int64, time.Time) error { return nil }
func (s *bootstrapStore) LockAuditChain(context.Context) error                         { return nil }

type bootstrapUsers struct {
	store.UserStore
	s *bootstrapStore
}

func (u bootstrapUsers) Create(context.Context, store.CreateUserParams) (store.User, error) {
	return store.User{}, store.ErrInvalid
}
func (u bootstrapUsers) CountEnabledApprovers(context.Context) (int64, error) {
	var n int64
	for _, v := range u.s.users {
		if v.Enabled && (v.Role == store.RoleAdmin || v.Role == store.RoleApprover) {
			n++
		}
	}
	return n, nil
}
func (u bootstrapUsers) ByUsername(_ context.Context, name string) (store.User, error) {
	v, ok := u.s.users[name]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return v, nil
}

type bootstrapAudit struct {
	store.AuditStore
	s *bootstrapStore
}

func (a bootstrapAudit) Append(_ context.Context, p store.CreateAuditParams) error {
	a.s.audits = append(a.s.audits, p)
	return nil
}

func (a bootstrapAudit) List(context.Context, store.AuditFilter) ([]store.AuditLog, bool, error) {
	return nil, false, nil
}

func (a bootstrapAudit) LastHash(context.Context) (string, error) { return "", nil }

// TestBootstrapWritesSingleAdminWarning：只有一个启用管理员且未配置
// AIOPS_BOOTSTRAP_APPROVER_EMAIL 时，启动必须写 rbac_single_admin_warning，
// 管理员才能知道系统处于降级模式。
func TestBootstrapWritesSingleAdminWarning(t *testing.T) {
	st := newBootstrapStore()
	st.users["admin"] = store.User{ID: "user-1", Username: "admin", Role: store.RoleAdmin, Enabled: true}
	aw := audit.NewWriter(st, nil, time.Now)
	aw.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = aw.Close(ctx)
	})
	cfg := &config.Config{}
	if err := bootstrapSecondApprover(context.Background(), st, aw, auth.NewService(st, auth.Config{}), cfg, nil); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := aw.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range st.audits {
		if row.Action == "rbac_single_admin_warning" && row.ActorType == "system" {
			found = true
			var detail map[string]any
			if err := json.Unmarshal(row.Detail, &detail); err != nil {
				t.Fatal(err)
			}
			if detail["reason"] != "single_admin_degraded" {
				t.Fatalf("detail = %v", detail)
			}
		}
	}
	if !found {
		t.Fatalf("rbac_single_admin_warning missing; audits=%+v", st.audits)
	}
}

// TestBootstrapSkipsWarningWithSecondApprover：已有第二个可用审批者时不写
// 降级告警。
func TestBootstrapSkipsWarningWithSecondApprover(t *testing.T) {
	st := newBootstrapStore()
	st.users["admin"] = store.User{ID: "user-1", Username: "admin", Role: store.RoleAdmin, Enabled: true}
	st.users["approver"] = store.User{ID: "user-2", Username: "approver", Role: store.RoleApprover, Enabled: true}
	aw := audit.NewWriter(st, nil, time.Now)
	aw.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = aw.Close(ctx)
	})
	if err := bootstrapSecondApprover(context.Background(), st, aw, nil, &config.Config{}, nil); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = aw.Flush(ctx)
	for _, row := range st.audits {
		if row.Action == "rbac_single_admin_warning" {
			t.Fatal("warning must not be written when a second approver exists")
		}
	}
}

// TestApproverUsernameFromEmail：引导账号的用户名必须满足 users.username
// CHECK（小写字母数字与 ._-，首字符字母/数字）。
func TestApproverUsernameFromEmail(t *testing.T) {
	cases := map[string]string{
		"Alice.Smith@example.com": "alice.smith",
		"Ops+Team@example.com":    "ops-team",
		"@example.com":            "example.com",
	}
	for email, want := range cases {
		if got := approverUsernameFromEmail(email); got != want {
			t.Errorf("approverUsernameFromEmail(%q) = %q, want %q", email, got, want)
		}
	}
}
