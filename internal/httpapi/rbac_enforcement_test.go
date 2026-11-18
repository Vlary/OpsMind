package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"aiops/internal/approvals"
	"aiops/internal/store"
)

// TestRoutePermissionMatrix 逐路由验证权限矩阵（契约 12.2）与 403 统一
// 错误码：无权限一律 403 FORBIDDEN，不因资源是否存在而变化。
func TestRoutePermissionMatrix(t *testing.T) {
	env := newTestEnv(t)
	addTestUser(t, env, "op", "op-pw", store.RoleOperator, true)
	addTestUser(t, env, "rev", "rev-pw", store.RoleApprover, true)
	addTestUser(t, env, "aud", "aud-pw", store.RoleAuditor, true)
	op := loginAs(t, env, "op", "op-pw")
	rev := loginAs(t, env, "rev", "rev-pw")
	aud := loginAs(t, env, "aud", "aud-pw")

	cases := []struct {
		name   string
		cookie string
		method string
		path   string
		want   int
	}{
		// operator：日常执行面可读/可发起，管理面与审批面 403。
		{"operator servers read", op, http.MethodGet, "/api/servers", http.StatusOK},
		{"operator conversations", op, http.MethodGet, "/api/conversations", http.StatusOK},
		{"operator audit denied", op, http.MethodGet, "/api/audit", http.StatusForbidden},
		{"operator users denied", op, http.MethodGet, "/api/users", http.StatusForbidden},
		{"operator approvals denied", op, http.MethodGet, "/api/approvals", http.StatusForbidden},
		{"operator providers write denied", op, http.MethodPost, "/api/providers", http.StatusForbidden},
		{"operator delete server denied", op, http.MethodDelete, "/api/servers/does-not-exist", http.StatusForbidden},

		// approver：只读资产 + 审批；不能发起对话/执行。
		{"approver approvals", rev, http.MethodGet, "/api/approvals", http.StatusOK},
		{"approver servers read", rev, http.MethodGet, "/api/servers", http.StatusOK},
		{"approver conversations denied", rev, http.MethodGet, "/api/conversations", http.StatusForbidden},
		{"approver chat denied", rev, http.MethodPost, "/api/chat", http.StatusForbidden},
		{"approver execute denied", rev, http.MethodPost, "/api/databases/does-not-exist/query", http.StatusForbidden},
		{"approver audit denied", rev, http.MethodGet, "/api/audit", http.StatusForbidden},

		// auditor：治理只读；不能聊天/审批/执行/开终端。
		{"auditor audit", aud, http.MethodGet, "/api/audit", http.StatusOK},
		{"auditor replays", aud, http.MethodGet, "/api/replays", http.StatusOK},
		// 第 4 轮走查 medium：audit:read 的会话目录与 /api/replays 同源
		//（传 NULL 拿全量），auditor 不再 403。
		{"auditor conversations", aud, http.MethodGet, "/api/conversations", http.StatusOK},
		{"auditor term sessions", aud, http.MethodGet, "/api/term/sessions", http.StatusOK},
		{"auditor servers denied", aud, http.MethodGet, "/api/servers", http.StatusForbidden},
		{"auditor approvals denied", aud, http.MethodGet, "/api/approvals", http.StatusForbidden},
		{"auditor chat denied", aud, http.MethodPost, "/api/chat", http.StatusForbidden},
		{"auditor term open denied", aud, http.MethodPost, "/api/term/sessions", http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.doAs(tc.cookie, tc.method, tc.path, "")
			if rec.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d: %s", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusForbidden {
				if code, _ := errorBody(t, rec); code != "FORBIDDEN" {
					t.Fatalf("403 code = %q, want FORBIDDEN", code)
				}
			}
		})
	}
}

// TestConversationOwnershipEndpoints 断言会话私有语义：非 owner 对
// GET/PATCH/DELETE/messages/tool-calls/replay 一律 404（不泄露存在性），
// admin 不例外；审计员可跨用户只读 replay。
func TestConversationOwnershipEndpoints(t *testing.T) {
	env := newTestEnv(t)
	alice := addTestUser(t, env, "alice", "alice-pw", store.RoleOperator, true)
	addTestUser(t, env, "bob", "bob-pw", store.RoleOperator, true)
	addTestUser(t, env, "silent", "silent-pw", store.RoleAuditor, true)
	aliceCookie := loginAs(t, env, "alice", "alice-pw")
	bobCookie := loginAs(t, env, "bob", "bob-pw")
	auditorCookie := loginAs(t, env, "silent", "silent-pw")

	rec := env.doAs(aliceCookie, http.MethodPost, "/api/conversations", `{"title":"alice only"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var conv store.Conversation
	if err := json.Unmarshal(rec.Body.Bytes(), &conv); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if conv.UserID != "" {
		t.Fatalf("conversation response must not expose user_id")
	}
	stored, err := env.st.Conversations().Get(context.Background(), conv.ID)
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if stored.UserID != alice.ID {
		t.Fatalf("conversation owner = %q, want %q", stored.UserID, alice.ID)
	}

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/conversations/" + conv.ID, ""},
		{http.MethodPatch, "/api/conversations/" + conv.ID, `{"title":"hijack"}`},
		{http.MethodDelete, "/api/conversations/" + conv.ID, ""},
		{http.MethodGet, "/api/conversations/" + conv.ID + "/messages", ""},
		{http.MethodGet, "/api/conversations/" + conv.ID + "/tool-calls", ""},
		{http.MethodGet, "/api/conversations/" + conv.ID + "/replay", ""},
	} {
		rec := env.doAs(bobCookie, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("non-owner %s %s = %d, want 404: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if code, _ := errorBody(t, rec); code != "CONVERSATION_NOT_FOUND" {
			t.Fatalf("non-owner code = %q, want CONVERSATION_NOT_FOUND", code)
		}
	}

	// owner 自己可读。
	if rec := env.doAs(aliceCookie, http.MethodGet, "/api/conversations/"+conv.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("owner get = %d: %s", rec.Code, rec.Body.String())
	}
	// actor 审计只读：跨用户 replay 200。
	if rec := env.doAs(auditorCookie, http.MethodGet, "/api/conversations/"+conv.ID+"/replay", ""); rec.Code != http.StatusOK {
		t.Fatalf("auditor replay = %d: %s", rec.Code, rec.Body.String())
	}
	// admin 也不能读他人会话（会话私有，不设 admin 例外）。
	if rec := env.do(http.MethodGet, "/api/conversations/"+conv.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("admin get other conversation = %d, want 404", rec.Code)
	}
}

// TestConversationCatalogAuditRead 覆盖第 4 轮走查 medium：GET /api/conversations
// 与 GET /api/replays 同源——非 audit:read 只看自己；audit:read（admin 与
// auditor）传 NULL 拿全量只读目录。修复前 admin 只看到自己的 2 条、
// auditor 直接 403，同页「回放执行」却能看到全量，口径自相矛盾。
func TestConversationCatalogAuditRead(t *testing.T) {
	env := newTestEnv(t)
	addTestUser(t, env, "cat-alice", "alice-pw", store.RoleOperator, true)
	addTestUser(t, env, "cat-bob", "bob-pw", store.RoleOperator, true)
	addTestUser(t, env, "cat-aud", "aud-pw", store.RoleAuditor, true)
	aliceCookie := loginAs(t, env, "cat-alice", "alice-pw")
	bobCookie := loginAs(t, env, "cat-bob", "bob-pw")
	audCookie := loginAs(t, env, "cat-aud", "aud-pw")

	for _, tc := range []struct {
		cookie string
		title  string
	}{
		{aliceCookie, "alice-conv"},
		{bobCookie, "bob-conv-1"},
		{bobCookie, "bob-conv-2"},
	} {
		rec := env.doAs(tc.cookie, http.MethodPost, "/api/conversations", `{"title":"`+tc.title+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d: %s", tc.title, rec.Code, rec.Body.String())
		}
	}

	count := func(cookie string) int {
		rec := env.doAs(cookie, http.MethodGet, "/api/conversations?limit=200", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET conversations = %d: %s", rec.Code, rec.Body.String())
		}
		var page struct {
			Items []store.Conversation `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode conversations: %v", err)
		}
		return len(page.Items)
	}

	// 非 audit:read：SQL 过滤到本人（alice 1 / bob 2）。
	if got := count(aliceCookie); got != 1 {
		t.Fatalf("alice conversations = %d, want 1", got)
	}
	if got := count(bobCookie); got != 2 {
		t.Fatalf("bob conversations = %d, want 2", got)
	}
	// audit:read：admin 与 auditor 都拿全量（3），不再是「只看自己」/403。
	if got := count(env.cookie); got != 3 {
		t.Fatalf("admin (audit:read) conversations = %d, want 3", got)
	}
	if got := count(audCookie); got != 3 {
		t.Fatalf("auditor conversations = %d, want 3", got)
	}

	// 目录是只读的：auditor 无 chat:use，读他人会话详情仍被权限守卫 403
	//（audit:read 只放开目录/回放，不改变会话详情的权限矩阵）。
	rec := env.doAs(bobCookie, http.MethodPost, "/api/conversations", `{"title":"bob-conv-3"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create third bob conversation = %d: %s", rec.Code, rec.Body.String())
	}
	var created store.Conversation
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if rec := env.doAs(audCookie, http.MethodGet, "/api/conversations/"+created.ID, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("auditor get other conversation = %d, want 403", rec.Code)
	}
}

// TestApprovalSoDAndRequesterException 断言审批职责分离（DB requested_by
// 优先）、发起人例外读取与 decide 禁止 edited_command。
func TestApprovalSoDAndRequesterException(t *testing.T) {
	env := newTestEnv(t)
	requester := addTestUser(t, env, "req", "req-pw", store.RoleOperator, true)
	addTestUser(t, env, "other", "other-pw", store.RoleOperator, true)
	addTestUser(t, env, "rev", "rev-pw", store.RoleApprover, true)
	reqCookie := loginAs(t, env, "req", "req-pw")
	otherCookie := loginAs(t, env, "other", "other-pw")
	revCookie := loginAs(t, env, "rev", "rev-pw")

	ap, err := env.apprSvc.Create(context.Background(), approvals.CreateRequest{
		ConversationID: testConvPtr("conv-rbac"), ToolCallID: testConvPtr("tc-rbac"),
		Command: "systemctl restart nginx", RiskLevel: "needs_approval",
		RuleID: "SERVICE_CHANGE", Reason: "service change", RequestedBy: requester.ID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 发起人可读自己的审批；同角色非发起人 404（不泄露存在性）。
	if rec := env.doAs(reqCookie, http.MethodGet, "/api/approvals/"+ap.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("requester get = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := env.doAs(otherCookie, http.MethodGet, "/api/approvals/"+ap.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("non-requester get = %d, want 404", rec.Code)
	}

	// decide 只接受 decision/note：edited_command 是未知字段 → 400，且
	// 审批保持 pending。
	rec := env.doAs(revCookie, http.MethodPost, "/api/approvals/"+ap.ID+"/decide",
		`{"decision":"approve","edited_command":"rm -rf /"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("edited decide = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if fresh, err := env.apprSvc.Get(context.Background(), ap.ID); err != nil || fresh.Status != store.ApprovalPending {
		t.Fatalf("approval after rejected edit = %+v, err=%v", fresh, err)
	}

	// 有界审批上下文：approver 只读，响应形状固定。
	rec = env.doAs(revCookie, http.MethodGet, "/api/approvals/"+ap.ID+"/context", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("context = %d: %s", rec.Code, rec.Body.String())
	}
	var ctxResp struct {
		Approval store.Approval `json:"approval"`
		Messages []any          `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ctxResp); err != nil {
		t.Fatalf("context json: %v", err)
	}
	if ctxResp.Approval.ID != ap.ID || ctxResp.Messages == nil {
		t.Fatalf("context = %+v", ctxResp)
	}

	// 第 7 轮走查 medium：无 approval:decide 的 operator 直连 decide 必须
	// 403 FORBIDDEN 且审批保持 pending——批量详情/诊断修复/容器操作三处
	// UI 按同一事实（approval:decide + requested_by != 当前用户）收敛
	// 决定按钮，服务端仍是唯一授权点。
	rec = env.doAs(otherCookie, http.MethodPost, "/api/approvals/"+ap.ID+"/decide",
		`{"decision":"approve"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator decide = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "FORBIDDEN" {
		t.Fatalf("operator decide code = %q, want FORBIDDEN body=%s", code, rec.Body.String())
	}
	if fresh, err := env.apprSvc.Get(context.Background(), ap.ID); err != nil || fresh.Status != store.ApprovalPending {
		t.Fatalf("approval after operator denial = %+v, err=%v", fresh, err)
	}

	// 审批人批准他人发起的审批：允许。
	rec = env.doAs(revCookie, http.MethodPost, "/api/approvals/"+ap.ID+"/decide",
		`{"decision":"approve","note":"ok"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approver decide = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"approved"`) {
		t.Fatalf("decide body = %s", rec.Body.String())
	}

	// 自审：admin 发起并自己批准 → 403 APPROVAL_SELF_DECISION，保持 pending。
	self, err := env.apprSvc.Create(context.Background(), approvals.CreateRequest{
		ConversationID: testConvPtr("conv-self"), ToolCallID: testConvPtr("tc-self"),
		Command: "echo self", RiskLevel: "needs_approval", RuleID: "DEFAULT_ASK",
		RequestedBy: "user-1",
	})
	if err != nil {
		t.Fatalf("Create self: %v", err)
	}
	rec = env.do(http.MethodPost, "/api/approvals/"+self.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self decide = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "APPROVAL_SELF_DECISION" {
		t.Fatalf("self decide code = %q", code)
	}
	if fresh, err := env.apprSvc.Get(context.Background(), self.ID); err != nil || fresh.Status != store.ApprovalPending {
		t.Fatalf("self approval after denial = %+v, err=%v", fresh, err)
	}
}
