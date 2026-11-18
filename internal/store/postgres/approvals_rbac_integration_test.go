package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"aiops/internal/approvals"
	"aiops/internal/store"
)

// TestApprovalDecideSoDAndGrantConsume 契约 12.3 第 6/12 条：
//   - Decide 的 UPDATE 自带 requested_by <> decided_by（DB 第二道防线）；
//   - terminal grant 单次消费，类型不匹配/重复消费均失败。
func TestApprovalDecideSoDAndGrantConsume(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	owner := rbacUserID(t)
	approver, err := testDB.Users().Create(ctx, store.CreateUserParams{
		Username: "rbac-approver", PasswordHash: "$2a$12$rbacapprover",
		Role: store.RoleApprover, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "srv-sod", Host: "h", Port: 22, Username: "u",
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	ap, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "console", ServerID: &server.ID, ServerName: server.Name,
		Command: "uptime", RiskLevel: "safe",
		ExpiresAt: now.Add(time.Hour), CreatedAt: now, RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 发起人自审 → 0 行（UPDATE 条件命中 requested_by <> decided_by），
	// store 返回 ErrConflict 且审批保持 pending。
	if _, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{
		ID: ap.ID, Status: store.ApprovalApproved, DecidedBy: owner, DecidedAt: now,
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("self decide = %v, want ErrConflict", err)
	}
	if got, _ := testDB.Approvals().Get(ctx, ap.ID); got.Status != store.ApprovalPending {
		t.Fatalf("status after self decide = %s, want pending", got.Status)
	}
	if _, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{
		ID: ap.ID, Status: store.ApprovalApproved, DecidedBy: approver.ID, DecidedAt: now,
	}); err != nil {
		t.Fatalf("approver decide: %v", err)
	}

	grants := store.GrantStoreOf(testDB)
	if grants == nil {
		t.Fatal("postgres approval store does not implement GrantStore")
	}
	kind := "term"
	expires := now.Add(30 * time.Minute)
	grant, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "terminal", ServerID: &server.ID, ServerName: server.Name,
		Command: "", RiskLevel: "safe", GrantKind: &kind, GrantExpiresAt: &expires,
		ExpiresAt: expires, CreatedAt: now, RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{
		ID: grant.ID, Status: store.ApprovalApproved, DecidedBy: approver.ID, DecidedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	consumed, err := grants.ConsumeGrant(ctx, store.ConsumeGrantParams{
		ApprovalID: grant.ID, UserID: owner, ServerID: server.ID, GrantKind: "term", Now: now.Add(time.Minute),
	})
	if err != nil || consumed.ConsumedAt == nil {
		t.Fatalf("consume = %+v, %v", consumed, err)
	}
	// 单次使用。
	if _, err := grants.ConsumeGrant(ctx, store.ConsumeGrantParams{
		ApprovalID: grant.ID, UserID: owner, ServerID: server.ID, GrantKind: "term", Now: now.Add(2 * time.Minute),
	}); !errors.Is(err, store.ErrGrantUnavailable) {
		t.Fatalf("second consume = %v, want ErrGrantUnavailable", err)
	}

	// 第 4 轮走查 medium：Open 失败时回滚消费（精确匹配本次 consumed_at），
	// 授权保留可重试；旧时间戳/其他用户不能误释放。
	released, err := grants.ReleaseGrant(ctx, store.ReleaseGrantParams{
		ApprovalID: grant.ID, UserID: owner, ConsumedAt: *consumed.ConsumedAt,
	})
	if err != nil || released.ConsumedAt != nil {
		t.Fatalf("release = %+v, %v (want consumed_at cleared)", released, err)
	}
	reconsumed, err := grants.ConsumeGrant(ctx, store.ConsumeGrantParams{
		ApprovalID: grant.ID, UserID: owner, ServerID: server.ID, GrantKind: "term", Now: now.Add(3 * time.Minute),
	})
	if err != nil || reconsumed.ConsumedAt == nil {
		t.Fatalf("re-consume after release = %+v, %v", reconsumed, err)
	}
	if _, err := grants.ReleaseGrant(ctx, store.ReleaseGrantParams{
		ApprovalID: grant.ID, UserID: owner, ConsumedAt: *consumed.ConsumedAt,
	}); !errors.Is(err, store.ErrGrantUnavailable) {
		t.Fatalf("stale release = %v, want ErrGrantUnavailable", err)
	}
	if _, err := grants.ReleaseGrant(ctx, store.ReleaseGrantParams{
		ApprovalID: grant.ID, UserID: approver.ID, ConsumedAt: *reconsumed.ConsumedAt,
	}); !errors.Is(err, store.ErrGrantUnavailable) {
		t.Fatalf("foreign release = %v, want ErrGrantUnavailable", err)
	}

	// 类型不匹配：rdp grant 用于 term。
	rdpKind := "rdp"
	rdpGrant, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "terminal", ServerID: &server.ID, ServerName: server.Name,
		Command: "", RiskLevel: "safe", GrantKind: &rdpKind, GrantExpiresAt: &expires,
		ExpiresAt: expires, CreatedAt: now, RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{
		ID: rdpGrant.ID, Status: store.ApprovalApproved, DecidedBy: approver.ID, DecidedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := grants.ConsumeGrant(ctx, store.ConsumeGrantParams{
		ApprovalID: rdpGrant.ID, UserID: owner, ServerID: server.ID, GrantKind: "term", Now: now.Add(time.Minute),
	}); !errors.Is(err, store.ErrGrantKindMismatch) {
		t.Fatalf("kind mismatch = %v, want ErrGrantKindMismatch", err)
	}
}

// TestExpirePendingWritesOutboxInTx 覆盖第 4 轮走查 medium：pending 审批
// TTL 过期必须与 approval.expired 的 audit_outbox 同事务提交，而不是先
// 裸 UPDATE 再走进程内队列。此处验证真实 pgstore 上两条事实同时落库
// （状态 expired + outbox 载荷），worker 尚未处理也能看到。
func TestExpirePendingWritesOutboxInTx(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	owner := rbacUserID(t)
	now := time.Now().UTC()
	ap, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "console", Command: "uptime", RiskLevel: "safe",
		ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-2 * time.Minute),
		RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := approvals.NewService(testDB, nil, time.Hour, time.Now, nil)
	n, err := svc.Expire(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("Expire = %d, %v, want 1", n, err)
	}
	stored, err := testDB.Approvals().Get(ctx, ap.ID)
	if err != nil || stored.Status != store.ApprovalExpired {
		t.Fatalf("status = %q, %v, want expired", stored.Status, err)
	}

	outbox := store.OutboxOf(testDB)
	if outbox == nil {
		t.Fatal("postgres store does not implement AuditOutboxStore")
	}
	entries, err := outbox.PendingAuditOutbox(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var perRow, aggregate int
	for _, entry := range entries {
		var payload store.CreateAuditParams
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatalf("outbox payload: %v", err)
		}
		if payload.Action != "approval.expired" {
			continue
		}
		if payload.ResourceID == ap.ID {
			perRow++
		} else if payload.ResourceID == "" {
			aggregate++
		}
	}
	if perRow != 1 || aggregate != 1 {
		t.Fatalf("outbox approval.expired per-row=%d aggregate=%d, want 1/1", perRow, aggregate)
	}
}

// TestUsersRoleDefaultIsLeastPrivilege 迁移 00013：绕过应用直接 INSERT
// users 时默认角色是 auditor（失败关闭），不会静默获得 execute:write。
func TestUsersRoleDefaultIsLeastPrivilege(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	if _, err := testDB.Users().Create(ctx, store.CreateUserParams{
		Username: "admin", PasswordHash: "$2a$12$rbacadmin", Role: store.RoleAdmin, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	var role string
	if err := testDB.pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash) VALUES ('default-role', 'x') RETURNING role`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "auditor" {
		t.Fatalf("default role = %q, want auditor", role)
	}
}

// TestAuditOutboxRoundTrip 契约 12.3 第 19 条：业务事务写入 outbox、worker
// 读取后标记 processed；未实现 outbox 时审计只能异步入队（旧缺陷）。
func TestAuditOutboxRoundTrip(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	outbox := store.OutboxOf(testDB)
	if outbox == nil {
		t.Fatal("postgres store does not implement AuditOutboxStore")
	}
	payload := []byte(`{"Action":"user.create","ActorType":"user","Detail":{}}`)
	err := testDB.WithTx(ctx, func(tx store.Store) error {
		txOutbox := store.OutboxOf(tx)
		if txOutbox == nil {
			t.Fatal("transaction store does not implement AuditOutboxStore")
		}
		return txOutbox.EnqueueAuditOutbox(ctx, payload, time.Now().UTC())
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := outbox.PendingAuditOutbox(ctx, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("pending = %d, %v", len(entries), err)
	}
	if err := outbox.ProcessAuditOutbox(ctx, []int64{entries[0].ID}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	after, err := outbox.PendingAuditOutbox(ctx, 10)
	if err != nil || len(after) != 0 {
		t.Fatalf("pending after process = %d, %v", len(after), err)
	}
}

// TestListAvailableGrantsScopedToUser 覆盖第 2 轮走查 low 的 store 侧：
// GET /api/term|rdp/grants 只返回本人 approved + 未消费 + 未过期的 grant，
// 别人的已批准授权一条都不能出现（授权是个人凭据）。
func TestListAvailableGrantsScopedToUser(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	owner := rbacUserID(t)
	other, err := testDB.Users().Create(ctx, store.CreateUserParams{
		Username: "rbac-other", PasswordHash: "$2a$12$rbacother",
		Role: store.RoleOperator, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	approver, err := testDB.Users().Create(ctx, store.CreateUserParams{
		Username: "rbac-approver2", PasswordHash: "$2a$12$rbacapprover2",
		Role: store.RoleApprover, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "srv-grant", Host: "h", Port: 22, Username: "u",
		Credentials: store.ServerCredentials{AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "enc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	grants := store.GrantStoreOf(testDB)
	if grants == nil {
		t.Fatal("postgres approval store does not implement GrantStore")
	}

	create := func(userID, kind string, expires time.Time, approve bool) store.Approval {
		t.Helper()
		k := kind
		ap, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
			Source: "terminal", ServerID: &server.ID, ServerName: server.Name,
			Command: "", RiskLevel: "safe", GrantKind: &k, GrantExpiresAt: &expires,
			ExpiresAt: expires, CreatedAt: now, RequestedBy: userID,
		})
		if err != nil {
			t.Fatalf("create grant: %v", err)
		}
		if approve {
			// 第 11 轮走查 high：现在批准过期审批会被 DB 兜底拒绝。这里若要
			// 构造「批准后授权已过期」的历史行，决策时刻必须落在 expires_at
			// 之前（模拟真实的时间推进），而不是在当前时刻批准一条过期行。
			decidedAt := now
			if !expires.After(now) {
				decidedAt = expires.Add(-time.Second)
			}
			decided, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{
				ID: ap.ID, Status: store.ApprovalApproved, DecidedBy: approver.ID, DecidedAt: decidedAt,
			})
			if err != nil {
				t.Fatalf("approve grant: %v", err)
			}
			return decided
		}
		return ap
	}

	ownerTerm := create(owner, "term", now.Add(20*time.Minute), true)
	ownerRdp := create(owner, "rdp", now.Add(10*time.Minute), true)
	ownerPending := create(owner, "term", now.Add(20*time.Minute), false)    // 未批准 → 不可见
	ownerExpired := create(owner, "term", now.Add(-time.Minute), true)       // 已过期 → 不可见
	otherApproved := create(other.ID, "term", now.Add(20*time.Minute), true) // 他人 → 不可见
	if ownerPending.Status != store.ApprovalPending || ownerExpired.Status != store.ApprovalApproved {
		t.Fatalf("fixture status = %s / %s", ownerPending.Status, ownerExpired.Status)
	}

	// term 过滤：只有本人那条，且最先到期的排前面（rdp 不在 term 结果里）。
	items, err := grants.ListAvailableGrants(ctx, store.ListAvailableGrantsParams{
		UserID: owner, GrantKind: "term", Now: now, Limit: 50,
	})
	if err != nil {
		t.Fatalf("list term: %v", err)
	}
	if len(items) != 1 || items[0].ID != ownerTerm.ID {
		t.Fatalf("term grants = %+v, want only %s", items, ownerTerm.ID)
	}

	// 不过滤类型：本人两条，按到期时间升序（rdp 更早到期）。
	items, err = grants.ListAvailableGrants(ctx, store.ListAvailableGrantsParams{
		UserID: owner, Now: now, Limit: 50,
	})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(items) != 2 || items[0].ID != ownerRdp.ID || items[1].ID != ownerTerm.ID {
		t.Fatalf("grants = %+v, want [rdp, term]", items)
	}
	for _, it := range items {
		if it.RequestedBy != owner {
			t.Fatalf("leaked grant %s of user %s", it.ID, it.RequestedBy)
		}
	}

	// 他人视角只有自己那条。
	items, err = grants.ListAvailableGrants(ctx, store.ListAvailableGrantsParams{
		UserID: other.ID, Now: now, Limit: 50,
	})
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(items) != 1 || items[0].ID != otherApproved.ID {
		t.Fatalf("other grants = %+v, want only %s", items, otherApproved.ID)
	}

	// 消费后从列表消失（服务端唯一事实源，不接受前端缓存）。
	if _, err := grants.ConsumeGrant(ctx, store.ConsumeGrantParams{
		ApprovalID: ownerTerm.ID, UserID: owner, ServerID: server.ID, GrantKind: "term", Now: now,
	}); err != nil {
		t.Fatalf("consume: %v", err)
	}
	items, err = grants.ListAvailableGrants(ctx, store.ListAvailableGrantsParams{
		UserID: owner, GrantKind: "term", Now: now, Limit: 50,
	})
	if err != nil || len(items) != 0 {
		t.Fatalf("after consume term grants = %+v, err = %v", items, err)
	}

	// 参数校验：空用户 / 非法 kind / 超上限。
	if _, err := grants.ListAvailableGrants(ctx, store.ListAvailableGrantsParams{Now: now}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("empty user = %v, want ErrInvalid", err)
	}
	if _, err := grants.ListAvailableGrants(ctx, store.ListAvailableGrantsParams{
		UserID: owner, GrantKind: "ssh", Now: now,
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("bad kind = %v, want ErrInvalid", err)
	}
	if _, err := grants.ListAvailableGrants(ctx, store.ListAvailableGrantsParams{
		UserID: owner, Now: now, Limit: 201,
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("limit 201 = %v, want ErrInvalid", err)
	}
}

// TestDecideRejectsExpiredApproval 第 11 轮走查 high：Decide 的 UPDATE 自带
// expires_at > decided_at（DB 兜底）。sweeper 尚未收敛的过期 pending 行
// 不能被批准/拒绝，状态保持 pending 交给 sweeper；系统 expired 迁移
// （Cancel/重启收敛）不受 TTL 限制。
func TestDecideRejectsExpiredApproval(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	owner := rbacUserID(t)
	decider, err := testDB.Users().Create(ctx, store.CreateUserParams{
		Username: "rbac-ttl-decider", PasswordHash: "$2a$12$rbacttldecider",
		Role: store.RoleApprover, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ap, err := testDB.Approvals().Create(ctx, store.CreateApprovalParams{
		Source: "console", Command: "uptime", RiskLevel: "safe", Reason: "r",
		ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-2 * time.Minute),
		RequestedBy: owner,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{
		ID: ap.ID, Status: store.ApprovalApproved, DecidedBy: decider.ID, DecidedAt: now,
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("decide expired = %v, want ErrConflict", err)
	}
	got, err := testDB.Approvals().Get(ctx, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.ApprovalPending || got.DecidedBy != nil {
		t.Fatalf("expired decide changed row: status=%s decided_by=%v", got.Status, got.DecidedBy)
	}

	// 系统 expired 迁移不受 TTL 限制（Cancel/重启收敛走同一 Decide）。
	expired, err := testDB.Approvals().Decide(ctx, store.DecideApprovalParams{
		ID: ap.ID, Status: store.ApprovalExpired, Note: "server_shutting_down", DecidedAt: now,
	})
	if err != nil || expired.Status != store.ApprovalExpired {
		t.Fatalf("system expire = (%+v, %v)", expired, err)
	}
}
