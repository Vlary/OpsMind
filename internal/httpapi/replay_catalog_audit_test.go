// replay_catalog_audit_test.go 覆盖走查第 3 轮 low：审计员每打开一次
// 「回放执行」目录，此前会为目录里每个他人会话各写一条 replay.viewed
// （单页最多 200 条），浏览行为把治理信号淹没（实测 admin 201 行、au1 55 行，
// 而会话只有 76 个）。现在目录读取合并成一条 replay.viewed，detail 带
// count / conversation_ids 摘要；打开单个回放仍逐条写。
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"aiops/internal/store"
)

// replayViewedRows 返回内存审计里的全部 replay.viewed 行。
func replayViewedRows(t *testing.T, env *testEnv) []store.AuditLog {
	t.Helper()
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	out := make([]store.AuditLog, 0, 8)
	for _, row := range env.st.auditLogs {
		if row.Action == "replay.viewed" {
			out = append(out, row)
		}
	}
	return out
}

// seedForeignConversations 造 n 个归属 admin 的会话 + 1 个归属 ops 的会话。
func seedForeignConversations(t *testing.T, env *testEnv, opsID string, n int) (foreignIDs []string, ownID string) {
	t.Helper()
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("55555555-5555-4555-8555-5555555555%02d", i)
		at := base.Add(-time.Duration(i) * time.Minute)
		env.st.conversations[id] = store.Conversation{
			ID: id, Title: fmt.Sprintf("admin-conv-%02d", i), UserID: "user-1",
			CreatedAt: at, UpdatedAt: at, LastMessageAt: at,
		}
		foreignIDs = append(foreignIDs, id)
	}
	ownID = "66666666-6666-4666-8666-666666666666"
	ownAt := base.Add(-time.Hour)
	env.st.conversations[ownID] = store.Conversation{
		ID: ownID, Title: "ops-own", UserID: opsID,
		CreatedAt: ownAt, UpdatedAt: ownAt, LastMessageAt: ownAt,
	}
	return foreignIDs, ownID
}

// TestReplayCatalogWritesSingleAudit：目录页只写一条 replay.viewed，
// detail 带跨用户条数与 conversation_id 摘要。
func TestReplayCatalogWritesSingleAudit(t *testing.T) {
	env := newTestEnv(t)
	ops := addTestUser(t, env, "ops-replay-audit", testPassword, store.RoleOperator, true)
	addTestUser(t, env, "auditor-replay-audit", testPassword, store.RoleAuditor, true)
	auditorCookie := loginAs(t, env, "auditor-replay-audit", testPassword)
	foreignIDs, ownID := seedForeignConversations(t, env, ops.ID, 5)

	rec := doAs(env, http.MethodGet, "/api/replays?limit=50", "", auditorCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog = %d body = %s", rec.Code, rec.Body.String())
	}

	rows := replayViewedRows(t, env)
	if len(rows) != 1 {
		t.Fatalf("replay.viewed rows = %d, want 1（目录读取必须合并为一条）: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.ResourceType != "replay_catalog" {
		t.Fatalf("resource_type = %q, want replay_catalog", row.ResourceType)
	}
	var detail struct {
		Count           int      `json:"count"`
		PageCount       int      `json:"page_count"`
		ConversationIDs []string `json:"conversation_ids"`
		Limit           int      `json:"limit"`
	}
	if err := json.Unmarshal(row.Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	// 对审计员而言「他人会话」= admin 的 5 条 + operator 的 1 条。
	wantForeign := len(foreignIDs) + 1
	if detail.Count != wantForeign {
		t.Fatalf("detail.count = %d, want %d（他人会话数）", detail.Count, wantForeign)
	}
	if detail.PageCount != wantForeign {
		t.Fatalf("detail.page_count = %d, want %d", detail.PageCount, wantForeign)
	}
	seen := map[string]bool{}
	for _, id := range detail.ConversationIDs {
		seen[id] = true
	}
	for _, id := range foreignIDs {
		if !seen[id] {
			t.Fatalf("detail.conversation_ids 缺 %s: %v", id, detail.ConversationIDs)
		}
	}
	// operator 自己的会话对审计员同样是「他人会话」，必须出现在摘要里
	//（「自己的不审计」只针对目录 owner 本人，见
	// TestOwnReplayCatalogNotAudited）。
	if !seen[ownID] {
		t.Fatalf("auditor 视角下 operator 会话也应计入: %v", detail.ConversationIDs)
	}
}

// TestReplayCatalogAuditIDsBounded：明细 ID 上限 20，超出时带 truncated
// 标记（防审计 detail 无界膨胀）。
func TestReplayCatalogAuditIDsBounded(t *testing.T) {
	env := newTestEnv(t)
	ops := addTestUser(t, env, "ops-replay-bulk", testPassword, store.RoleOperator, true)
	addTestUser(t, env, "auditor-replay-bulk", testPassword, store.RoleAuditor, true)
	auditorCookie := loginAs(t, env, "auditor-replay-bulk", testPassword)
	// 25 个他人会话：超过 replayCatalogAuditIDLimit(20)。
	seedForeignConversations(t, env, ops.ID, 25)

	if rec := doAs(env, http.MethodGet, "/api/replays?limit=200", "", auditorCookie, ""); rec.Code != http.StatusOK {
		t.Fatalf("catalog = %d body = %s", rec.Code, rec.Body.String())
	}
	rows := replayViewedRows(t, env)
	if len(rows) != 1 {
		t.Fatalf("replay.viewed rows = %d, want 1", len(rows))
	}
	var detail struct {
		Count           int      `json:"count"`
		ConversationIDs []string `json:"conversation_ids"`
		Truncated       bool     `json:"conversation_ids_truncated"`
	}
	if err := json.Unmarshal(rows[0].Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail.Count != 26 {
		t.Fatalf("count = %d, want 26（25 条 admin + 1 条 operator）", detail.Count)
	}
	if len(detail.ConversationIDs) != replayCatalogAuditIDLimit {
		t.Fatalf("conversation_ids = %d, want bounded %d", len(detail.ConversationIDs), replayCatalogAuditIDLimit)
	}
	if !detail.Truncated {
		t.Fatal("conversation_ids_truncated must be true when ids exceed the bound")
	}
}

// TestOwnReplayCatalogNotAudited：本人在自己的目录里浏览不写审计。
func TestOwnReplayCatalogNotAudited(t *testing.T) {
	env := newTestEnv(t)
	env.ensureRequester(t)
	// requester 是 operator：只有自己的会话。
	env.st.mu.Lock()
	env.st.conversations["77777777-7777-4777-8777-777777777777"] = store.Conversation{
		ID: "77777777-7777-4777-8777-777777777777", Title: "own", UserID: testRequesterID,
		LastMessageAt: time.Now().UTC(),
	}
	env.st.mu.Unlock()
	cookie := loginAs(t, env, testRequesterUsername, testPassword)

	if rec := doAs(env, http.MethodGet, "/api/replays", "", cookie, ""); rec.Code != http.StatusOK {
		t.Fatalf("catalog = %d body = %s", rec.Code, rec.Body.String())
	}
	if rows := replayViewedRows(t, env); len(rows) != 0 {
		t.Fatalf("own catalog wrote %d replay.viewed rows, want 0: %+v", len(rows), rows)
	}
}

// TestSingleReplayViewStillAudited：打开单个他人回放仍逐条写
// replay.viewed（resource_type=conversation，带 owner_id）——合并只针对
// 目录读取，不能借此丢掉单条治理信号。
func TestSingleReplayViewStillAudited(t *testing.T) {
	env := newTestEnv(t)
	ops := addTestUser(t, env, "ops-single-replay", testPassword, store.RoleOperator, true)
	addTestUser(t, env, "auditor-single-replay", testPassword, store.RoleAuditor, true)
	auditorCookie := loginAs(t, env, "auditor-single-replay", testPassword)
	_, ownID := seedForeignConversations(t, env, ops.ID, 1)

	if rec := doAs(env, http.MethodGet, "/api/conversations/"+ownID+"/replay", "", auditorCookie, ""); rec.Code != http.StatusOK {
		t.Fatalf("replay = %d body = %s", rec.Code, rec.Body.String())
	}
	rows := replayViewedRows(t, env)
	if len(rows) != 1 {
		t.Fatalf("single replay rows = %d, want 1", len(rows))
	}
	if rows[0].ResourceType != "conversation" || rows[0].ResourceID != ownID {
		t.Fatalf("single replay audit = %+v, want conversation/%s", rows[0], ownID)
	}
}
