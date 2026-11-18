// wait_cross_process_test.go 覆盖走查第 3 轮 medium：主机侧
// `aiops break-glass`（直接 UPDATE approvals，不经过本进程 notify）批准后，
// 等待中的会话流必须在秒级被唤醒，而不是挂到审批 TTL 才继续执行。
package approvals

import (
	"context"
	"testing"
	"time"

	"aiops/internal/store"
)

// TestWaitSeesCrossProcessApproval：绕过 Service.Decide 直接改库（模拟
// 另一进程的 break-glass），Wait 必须在一个轮询周期内返回 approved。
func TestWaitSeesCrossProcessApproval(t *testing.T) {
	svc, repo := newTestService(t, time.Minute)
	ctx := context.Background()

	ap, err := svc.Create(ctx, createReq("tc-cross-process"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	type result struct {
		d   Decision
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		d, err := svc.Wait(ctx, ap.ID)
		done <- result{d: d, err: err}
	}()

	// 等 Wait 注册并完成首次读库，再模拟外部批准。
	time.Sleep(100 * time.Millisecond)
	repo.mu.Lock()
	row := repo.items[ap.ID]
	row.Status = store.ApprovalApproved
	row.BreakGlass = true
	now := time.Now().UTC()
	row.DecidedAt = &now
	repo.items[ap.ID] = row
	repo.mu.Unlock()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Wait: %v", got.err)
		}
		if got.d.Status != store.ApprovalApproved {
			t.Fatalf("status = %s, want approved", got.d.Status)
		}
		// 默认 TTL 是 1m；跨进程批准必须在轮询周期（1s）量级内唤醒。
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("Wait took %v，跨进程批准没有唤醒等待方（旧实现会挂到 TTL）", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait 未在一个轮询周期内看到外部批准（跨进程决策丢失）")
	}
}

// TestWaitKeepsWaitingWhilePending：轮询不得把仍 pending 的审批误判为终态。
func TestWaitKeepsWaitingWhilePending(t *testing.T) {
	svc, repo := newTestService(t, time.Minute)
	ctx := context.Background()
	ap, err := svc.Create(ctx, createReq("tc-still-pending"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	done := make(chan Decision, 1)
	go func() {
		d, _ := svc.Wait(ctx, ap.ID)
		done <- d
	}()

	// 跨过 2 个轮询周期仍是 pending：Wait 必须继续等。
	select {
	case d := <-done:
		t.Fatalf("Wait returned %+v while approval was still pending", d)
	case <-time.After(2200 * time.Millisecond):
	}
	repo.mu.Lock()
	row := repo.items[ap.ID]
	if row.Status != store.ApprovalPending {
		t.Fatalf("approval status = %s, want pending", row.Status)
	}
	repo.mu.Unlock()

	// 正常 decide 仍能立刻唤醒。
	if _, err := svc.Decide(ctx, DecideRequest{ApprovalID: ap.ID, Approve: true, DecidedBy: "user-1"}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	select {
	case d := <-done:
		if d.Status != store.ApprovalApproved {
			t.Fatalf("status = %s, want approved", d.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Decide 未唤醒等待方")
	}
}
