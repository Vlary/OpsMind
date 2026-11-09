// decide_edit_test.go 覆盖六期 BE-P 审批命令改写（契约 2.11 约定 3 /
// 5.5 Decide 流程）：deny 携带、长度/NUL、无 server_id、改写 forbidden
// 保持 pending、改写落库 edited_* 与 Decision.EditedCommand 透传、
// audit detail 字段。
package approvals

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"aiops/internal/risk"
	"aiops/internal/store"
)

// fakeServerStore 提供改写重分类所需的 Servers().Get。
type fakeServerStore struct {
	store.ServerStore
	mu      sync.Mutex
	servers map[string]store.Server
}

func (f *fakeServerStore) Get(_ context.Context, id string) (store.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if srv, ok := f.servers[id]; ok {
		return srv, nil
	}
	return store.Server{}, store.ErrNotFound
}

type editStore struct {
	store.Store
	approvals *fakeApprovals
	servers   *fakeServerStore
}

func (s *editStore) Approvals() store.ApprovalStore { return s.approvals }
func (s *editStore) Servers() store.ServerStore     { return s.servers }

func newEditService(t *testing.T) (*Service, *fakeApprovals, *fakeServerStore) {
	t.Helper()
	ap := newFakeApprovals()
	servers := &fakeServerStore{servers: map[string]store.Server{}}
	st := &editStore{approvals: ap, servers: servers}
	svc := NewService(st, nil, time.Minute, nil, nil)
	return svc, ap, servers
}

func editReq(approvalID string, approve bool, edited string) DecideRequest {
	return DecideRequest{
		ApprovalID:    approvalID,
		Approve:       approve,
		DecidedBy:     "user-1",
		Note:          "",
		EditedCommand: edited,
		RequestID:     "req-edit",
	}
}

func createEditApproval(t *testing.T, svc *Service, serverID string) store.Approval {
	t.Helper()
	var sid *string
	if serverID != "" {
		sid = &serverID
	}
	convID := "conv-1"
	tc := "tc-" + serverID + time.Now().Format("150405.000000000")
	ap, err := svc.Create(context.Background(), CreateRequest{
		ConversationID: &convID,
		ToolCallID:     &tc,
		ServerID:       sid,
		ServerName:     "web-1",
		Command:        "systemctl restart nginx",
		RiskLevel:      "needs_approval",
		RuleID:         "SERVICE_CHANGE",
		Reason:         "service change",
		RequestID:      "req-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return ap
}

// TestDecideEditedCommandValidation 校验矩阵（契约 2.11 约定 3）。
func TestDecideEditedCommandValidation(t *testing.T) {
	ctx := context.Background()
	t.Run("deny with edited command rejected", func(t *testing.T) {
		svc, _, servers := newEditService(t)
		servers.servers["srv-1"] = store.Server{ID: "srv-1", OS: store.ServerOSLinux}
		ap := createEditApproval(t, svc, "srv-1")
		if _, err := svc.Decide(ctx, editReq(ap.ID, false, "uptime")); !isErrInvalid(err) {
			t.Fatalf("deny+edited error = %v, want ErrInvalid", err)
		}
	})
	t.Run("edited too long rejected", func(t *testing.T) {
		svc, _, servers := newEditService(t)
		servers.servers["srv-1"] = store.Server{ID: "srv-1", OS: store.ServerOSLinux}
		ap := createEditApproval(t, svc, "srv-1")
		long := strings.Repeat("a", 2001)
		if _, err := svc.Decide(ctx, editReq(ap.ID, true, long)); !isErrInvalid(err) {
			t.Fatalf("len 2001 error = %v, want ErrInvalid", err)
		}
		if _, err := svc.Decide(ctx, editReq(ap.ID, true, strings.Repeat("a", 2000))); err != nil {
			t.Fatalf("len 2000 should pass validation: %v", err)
		}
	})
	t.Run("edited with NUL rejected", func(t *testing.T) {
		svc, _, servers := newEditService(t)
		servers.servers["srv-1"] = store.Server{ID: "srv-1", OS: store.ServerOSLinux}
		ap := createEditApproval(t, svc, "srv-1")
		if _, err := svc.Decide(ctx, editReq(ap.ID, true, "ls\x00-r")); !isErrInvalid(err) {
			t.Fatalf("NUL error = %v, want ErrInvalid", err)
		}
	})
	t.Run("approval without server rejected", func(t *testing.T) {
		svc, _, _ := newEditService(t)
		ap := createEditApproval(t, svc, "")
		if _, err := svc.Decide(ctx, editReq(ap.ID, true, "uptime")); !isErrInvalid(err) {
			t.Fatalf("no server error = %v, want ErrInvalid", err)
		}
	})
}

// TestDecideEditedForbiddenKeepsPending：改写后 forbidden → ErrForbidden，
// 审批保持 pending（不落库）。
func TestDecideEditedForbiddenKeepsPending(t *testing.T) {
	svc, repo, servers := newEditService(t)
	ctx := context.Background()
	servers.servers["srv-1"] = store.Server{ID: "srv-1", OS: store.ServerOSLinux}
	ap := createEditApproval(t, svc, "srv-1")
	if _, err := svc.Decide(ctx, editReq(ap.ID, true, "rm -rf /")); !isErrForbidden(err) {
		t.Fatalf("forbidden edit error = %v, want ErrForbidden", err)
	}
	got, err := svc.Get(ctx, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.ApprovalPending {
		t.Fatalf("status = %q, want pending (forbidden edit must not decide)", got.Status)
	}
	if got.EditedCommand != "" {
		t.Fatalf("edited_command = %q, want empty", got.EditedCommand)
	}
	_ = repo
}

// TestDecideEditedPersistAndDecision：改写成功落库 edited_*、原命令不动、
// Decision.EditedCommand 透传给等待方。
func TestDecideEditedPersistAndDecision(t *testing.T) {
	svc, _, servers := newEditService(t)
	ctx := context.Background()
	servers.servers["srv-1"] = store.Server{ID: "srv-1", OS: store.ServerOSLinux}
	ap := createEditApproval(t, svc, "srv-1")

	edited := "systemctl restart nginx --no-block"
	if _, err := svc.Decide(ctx, editReq(ap.ID, true, edited)); err != nil {
		t.Fatalf("Decide with edit: %v", err)
	}
	got, err := svc.Get(ctx, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EditedCommand != edited {
		t.Fatalf("edited_command = %q, want %q", got.EditedCommand, edited)
	}
	if got.Command != "systemctl restart nginx" {
		t.Fatalf("original command mutated: %q", got.Command)
	}
	if got.EditedRiskLevel != string(risk.Classify(edited, risk.OSLinux).Level) {
		t.Fatalf("edited_risk_level = %q", got.EditedRiskLevel)
	}
	if got.Status != store.ApprovalApproved {
		t.Fatalf("status = %q, want approved", got.Status)
	}

	// 等待方收到的 Decision 携带改写命令（runner 据此执行）。
	waitCh := make(chan Decision, 1)
	go func() {
		waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		d, err := svc.Wait(waitCtx, ap.ID)
		if err != nil {
			return
		}
		waitCh <- d
	}()
	// 已决定的审批：Wait 立即返回现状。
	select {
	case d := <-waitCh:
		if d.EditedCommand != edited {
			t.Fatalf("Decision.EditedCommand = %q, want %q", d.EditedCommand, edited)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return decided approval")
	}
}

// TestDecideNetworkDeviceReclassify：network_device 目标走 ClassifyNet 分册。
func TestDecideNetworkDeviceReclassify(t *testing.T) {
	svc, repo, servers := newEditService(t)
	ctx := context.Background()
	servers.servers["net-1"] = store.Server{
		ID: "net-1", DeviceKind: store.DeviceKindNetworkDevice,
		OS: store.ServerOSNetwork, NetdevBrand: store.NetdevBrandCisco,
	}
	ap := createEditApproval(t, svc, "net-1")
	if _, err := svc.Decide(ctx, editReq(ap.ID, true, "show version")); err != nil {
		t.Fatalf("cisco reclassify: %v", err)
	}
	got, _ := repo.Get(ctx, ap.ID)
	want := risk.ClassifyNet("show version", risk.NetBrandCisco)
	if got.EditedRiskLevel != string(want.Level) || got.EditedRuleID != want.RuleID {
		t.Fatalf("edited = %q/%q, want %q/%q", got.EditedRiskLevel, got.EditedRuleID, want.Level, want.RuleID)
	}
}

func isErrInvalid(err error) bool {
	return err != nil && err.Error() == ErrInvalid.Error()
}

func isErrForbidden(err error) bool {
	return err != nil && err.Error() == ErrForbidden.Error()
}
