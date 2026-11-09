// phase6_integration_test.go 覆盖六期 BE-P 的 postgres 新列读写（契约
// 3.3/3.4/迁移 00008）：approvals.approval_mode 快照与空串兜底、
// edited_* 同 UPDATE 落库、conversations.approval_mode 覆盖写/清除。
package postgres

import (
	"context"
	"testing"
	"time"

	"aiops/internal/store"
)

func TestPhase6ApprovalModeAndEditedColumns(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	server, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "srv-p6", Host: "h", Port: 22, Username: "u",
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 空串 ApprovalMode 按 standard 落库（契约 3.4）。
	ap, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source:   "console",
		ServerID: &server.ID, ServerName: "srv-p6",
		Command: "systemctl restart nginx", RiskLevel: "needs_approval",
		RuleID: "SERVICE_CHANGE", Reason: "r",
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ap.ApprovalMode != store.ApprovalModeStandard {
		t.Fatalf("default approval_mode = %q, want standard", ap.ApprovalMode)
	}
	// 显式快照。
	high := store.ApprovalModeHigh
	ap2, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "console", ServerID: &server.ID,
		Command: "docker stop 9f2c", RiskLevel: "needs_approval",
		RuleID: "CONTAINER_CHANGE", Reason: "r", ApprovalMode: string(high),
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ap2.ApprovalMode != store.ApprovalModeHigh {
		t.Fatalf("approval_mode = %q, want high", ap2.ApprovalMode)
	}

	// Decide 同 UPDATE 落库 edited_*；原命令不动。
	decided, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{
		ID: ap.ID, Status: store.ApprovalApproved, DecidedBy: "", Note: "ok",
		EditedCommand:   "systemctl restart nginx --no-block",
		EditedRiskLevel: "needs_approval",
		EditedRuleID:    "SERVICE_CHANGE",
		DecidedAt:       time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decided.EditedCommand != "systemctl restart nginx --no-block" {
		t.Fatalf("edited_command = %q", decided.EditedCommand)
	}
	if decided.Command != "systemctl restart nginx" {
		t.Fatalf("original command mutated: %q", decided.Command)
	}
	got, err := testDB.Approvals().Get(ctx, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EditedRiskLevel != "needs_approval" || got.EditedRuleID != "SERVICE_CHANGE" {
		t.Fatalf("edited levels = %q/%q", got.EditedRiskLevel, got.EditedRuleID)
	}
	if got.ApprovalMode != store.ApprovalModeStandard {
		t.Fatalf("approval_mode after decide = %q", got.ApprovalMode)
	}
}

func TestPhase6ConversationApprovalMode(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	conv, err := testDB.Conversations().Create(ctx, store.CreateConversationParams{Title: "p6"})
	if err != nil {
		t.Fatal(err)
	}
	if conv.ApprovalMode != nil {
		t.Fatalf("default approval_mode = %v, want nil", conv.ApprovalMode)
	}
	// 设置覆盖。
	none := store.ApprovalModeNone
	updated, err := testDB.Conversations().SetApprovalMode(ctx, conv.ID, &none, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if updated.ApprovalMode == nil || *updated.ApprovalMode != store.ApprovalModeNone {
		t.Fatalf("approval_mode = %v, want none", updated.ApprovalMode)
	}
	got, err := testDB.Conversations().Get(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ApprovalMode == nil || *got.ApprovalMode != store.ApprovalModeNone {
		t.Fatalf("get approval_mode = %v, want none", got.ApprovalMode)
	}
	// 清除覆盖（NULL=跟随全局）。
	cleared, err := testDB.Conversations().SetApprovalMode(ctx, conv.ID, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cleared.ApprovalMode != nil {
		t.Fatalf("cleared approval_mode = %v, want nil", cleared.ApprovalMode)
	}
}
