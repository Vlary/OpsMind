package approvals

import (
	"context"
	"errors"
	"testing"
	"time"

	"aiops/internal/store"
)

// TestCreateRequiresRequestedBy 断言新审批必须带发起人（契约 12.3 第 5 条）。
func TestCreateRequiresRequestedBy(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	req := createReq("tc-nobody")
	req.RequestedBy = ""
	if _, err := svc.Create(context.Background(), req); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Create without RequestedBy error = %v, want ErrInvalid", err)
	}
}

// TestDecideRejectsSelfDecision 断言职责分离：requested_by == decided_by 时
// 403 语义（ErrSelfDecision），且审批保持 pending（契约 12.3 第 6 条）。
func TestDecideRejectsSelfDecision(t *testing.T) {
	svc, repo := newTestService(t, time.Minute)
	ctx := context.Background()
	ap, err := svc.Create(ctx, createReq("tc-self"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = svc.Decide(ctx, DecideRequest{
		ApprovalID: ap.ID, Approve: true, DecidedBy: "requester-1", RequestID: "req-2",
	})
	if !errors.Is(err, ErrSelfDecision) {
		t.Fatalf("Decide self error = %v, want ErrSelfDecision", err)
	}
	got, err := repo.Get(ctx, ap.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.ApprovalPending {
		t.Fatalf("status = %q, want pending (self-decision must not mutate)", got.Status)
	}
	if got.DecidedBy != nil {
		t.Fatalf("decided_by = %v, want nil", *got.DecidedBy)
	}
}

// TestDecideByAnotherUserAllowed 断言不同用户可正常批准。
func TestDecideByAnotherUserAllowed(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	ctx := context.Background()
	ap, err := svc.Create(ctx, createReq("tc-other"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	decided, err := svc.Decide(ctx, DecideRequest{
		ApprovalID: ap.ID, Approve: true, DecidedBy: "approver-1", RequestID: "req-3",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decided.Status != store.ApprovalApproved {
		t.Fatalf("status = %q, want approved", decided.Status)
	}
}

// TestDecidePersistedRequestedByWins 断言职责分离以持久化行为准：调用方
// 传入的 RequestedBy 不能覆盖 DB 的 requested_by 来绕过自审（契约 12.3
// 第 6 条；多实例/重启后进程内 map 为空时也必须成立）。
func TestDecidePersistedRequestedByWins(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	ctx := context.Background()
	ap, err := svc.Create(ctx, createReq("tc-explicit"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = svc.Decide(ctx, DecideRequest{
		ApprovalID: ap.ID, Approve: true, DecidedBy: "requester-1",
		RequestedBy: "someone-else", RequestID: "req-4",
	})
	if !errors.Is(err, ErrSelfDecision) {
		t.Fatalf("Decide with spoofed RequestedBy error = %v, want ErrSelfDecision", err)
	}
}

// TestDecideRejectsLegacyEditedApproval 断言 00012 之前的历史 pending 改写
// 行不能被决策（契约 12.3 第 7 条：409 APPROVAL_LEGACY_EDIT）。
func TestDecideRejectsLegacyEditedApproval(t *testing.T) {
	svc, repo := newTestService(t, time.Minute)
	ctx := context.Background()
	ap, err := svc.Create(ctx, createReq("tc-legacy"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	repo.mu.Lock()
	row := repo.items[ap.ID]
	row.Legacy = true
	row.EditedCommand = "systemctl restart nginx --no-block"
	repo.items[ap.ID] = row
	repo.mu.Unlock()

	_, err = svc.Decide(ctx, DecideRequest{
		ApprovalID: ap.ID, Approve: true, DecidedBy: "approver-1", RequestID: "req-legacy",
	})
	if !errors.Is(err, ErrLegacyEdit) {
		t.Fatalf("Decide legacy edited error = %v, want ErrLegacyEdit", err)
	}
}

// TestRequestedByLookup 断言装配层可读到发起人（decide 执行前复检权限用）。
func TestRequestedByLookup(t *testing.T) {
	svc, _ := newTestService(t, time.Minute)
	ap, err := svc.Create(context.Background(), createReq("tc-lookup"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, ok := svc.RequestedBy(ap.ID); !ok || got != "requester-1" {
		t.Fatalf("RequestedBy = %q,%v want requester-1,true", got, ok)
	}
}
