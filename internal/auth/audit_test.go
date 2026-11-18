package auth

import (
	"context"
	"testing"
	"time"

	"aiops/internal/audit"
	"aiops/internal/store"
)

// 内存 fake 不实现 audit_outbox：AcceptTokenAudited/ChangePasswordAudited
// 必须返回 auditInTx=false，调用方据此在提交后回落到进程内 Enqueue。
func TestAcceptTokenAuditedFallsBackWithoutOutbox(t *testing.T) {
	st := newFakeStore()
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	d := &captureDeliverer{}
	svc := NewService(st, Config{
		SessionTTL: time.Hour,
		Clock:      func() time.Time { return now },
		Deliverer:  d,
	})
	st.users["invitee"] = store.User{
		ID: "user-inv", Username: "invitee", PasswordHash: "!pending",
		Role: store.RoleOperator, Enabled: false, Email: "invitee@example.com",
	}
	if _, err := svc.IssueInviteToken(context.Background(), st.users["invitee"], store.RoleOperator, "admin-1", "203.0.113.9"); err != nil {
		t.Fatalf("IssueInviteToken: %v", err)
	}

	auditCalled := false
	kind, userID, auditInTx, err := svc.AcceptTokenAudited(context.Background(), d.token, "new-pw", "受邀者", "198.51.100.7",
		func(k store.UserTokenKind, uid string) audit.Event {
			auditCalled = true
			return audit.Event{ActorType: "user", ActorID: uid, Action: "user.invite_accepted", ResourceID: uid}
		})
	if err != nil {
		t.Fatalf("AcceptTokenAudited: %v", err)
	}
	if kind != store.UserTokenInvite || userID != "user-inv" {
		t.Fatalf("kind/user = %q/%q", kind, userID)
	}
	if auditInTx {
		t.Fatal("auditInTx = true for store without outbox")
	}
	if !auditCalled {
		t.Fatal("audit event factory was not called")
	}
}

func TestChangePasswordAuditedFallsBackWithoutOutbox(t *testing.T) {
	st := newFakeStore()
	svc := NewService(st, Config{SessionTTL: time.Hour, Clock: time.Now})
	if err := svc.EnsureAdmin(context.Background(), "old-pw", nil); err != nil {
		t.Fatal(err)
	}
	user := st.users[adminUsername]

	auditCalled := false
	auditInTx, err := svc.ChangePasswordAudited(context.Background(), user.ID, "old-pw", "new-pw", func() audit.Event {
		auditCalled = true
		return audit.Event{ActorType: "user", ActorID: user.ID, Action: "auth.password_changed"}
	})
	if err != nil {
		t.Fatalf("ChangePasswordAudited: %v", err)
	}
	if auditInTx {
		t.Fatal("auditInTx = true for store without outbox")
	}
	if !auditCalled {
		t.Fatal("audit event factory was not called")
	}
}
