package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// 审批
// ---------------------------------------------------------------------------

func createPendingApproval(t *testing.T, env *testEnv) store.Approval {
	t.Helper()
	env.ensureRequester(t)
	ap, err := env.apprSvc.Create(context.Background(), approvals.CreateRequest{
		ConversationID: testConvPtr("conv-1"), ToolCallID: testConvPtr("tc-1"), Command: "systemctl restart nginx",
		RiskLevel: "needs_approval", RuleID: "SERVICE_CHANGE", Reason: "service state change",
		RequestedBy: testRequesterID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ap
}

func TestApprovalListAndDecide(t *testing.T) {
	env := newTestEnv(t)
	ap := createPendingApproval(t, env)

	// 列表：全部 + 状态过滤。
	rec := env.do(http.MethodGet, "/api/approvals", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ap.ID) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/api/approvals?status=pending", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ap.ID) {
		t.Fatalf("pending list = %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/api/approvals?status=bogus", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d", rec.Code)
	}
	rec = env.do(http.MethodGet, "/api/approvals?limit=0", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit = %d", rec.Code)
	}

	// 详情。
	rec = env.do(http.MethodGet, "/api/approvals/"+ap.ID, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "systemctl restart nginx") {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/api/approvals/missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing approval = %d", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "APPROVAL_NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}

	// 批准。
	rec = env.do(http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve","note":"ok"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"approved"`) {
		t.Fatalf("approve body = %s", rec.Body.String())
	}

	// 重复决策 → 409 APPROVAL_NOT_PENDING。
	rec = env.do(http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"deny"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("double decide = %d %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "APPROVAL_NOT_PENDING" {
		t.Fatalf("code = %s", code)
	}

	// 决策不存在 → 404（用合法 UUID，避免与「非 UUID → 400」混淆）。
	rec = env.do(http.MethodPost, "/api/approvals/"+uuid.NewString()+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("decide missing = %d", rec.Code)
	}
}

func TestApprovalDecideValidation(t *testing.T) {
	env := newTestEnv(t)
	ap := createPendingApproval(t, env)

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"bad decision", `{"decision":"maybe"}`, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"note too long", `{"decision":"approve","note":"` + strings.Repeat("字", 501) + `"}`, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"malformed body", `{`, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"unknown field", `{"decision":"approve","x":1}`, http.StatusBadRequest, "VALIDATION_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodPost, "/api/approvals/"+ap.ID+"/decide", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if code, _ := errorBody(t, rec); code != tc.code {
				t.Fatalf("code = %s", code)
			}
		})
	}
	// 校验失败的请求不得改变审批状态。
	fresh, err := env.st.Approvals().Get(context.Background(), ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != store.ApprovalPending {
		t.Fatalf("status = %s, want pending", fresh.Status)
	}
}

// 第 8 轮走查 low：非 UUID 的审批 id 必须在入口 400。修复前 approve 经
// recheckRequester 把 store 的 UUID 转换错误映射成 503「approval store
// unavailable」，deny 则落 decide 兜底 500 INTERNAL——调用方会误判服务
// 故障并触发 5xx 告警；同一 id 的 GET 详情却是 400。approve/deny 两条
// 路径都必须与 GET 同口径。
func TestApprovalDecideRejectsNonUUIDID(t *testing.T) {
	env := newTestEnv(t)
	for _, decision := range []string{"approve", "deny"} {
		t.Run(decision, func(t *testing.T) {
			rec := env.do(http.MethodPost, "/api/approvals/not-a-uuid/decide",
				`{"decision":"`+decision+`"}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			code, msg := errorBody(t, rec)
			if code != "VALIDATION_FAILED" || !strings.Contains(msg, "uuid") {
				t.Fatalf("code = %q msg = %q", code, msg)
			}
		})
	}
}

// 守卫层兜底：真实 Postgres 对非法 UUID 返回 ErrInvalid（22P02），
// approvalRequesterID 不能把它当存储故障映射成 503（否则批准前的发起人
// 复检会误导调用方以为服务不可用）。
func TestApprovalRequesterLookupMapsInvalidIDTo400(t *testing.T) {
	env := newTestEnv(t)
	env.st.errApprovalsGet = store.ErrInvalid

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	id := uuid.NewString()
	c.Request = httptestPost("/api/approvals/"+id+"/decide", `{"decision":"approve"}`)
	c.Params = gin.Params{{Key: "id", Value: id}}

	deps := &Deps{Approvals: env.apprSvc, Store: env.st}
	if _, ok := deps.approvalRequesterID(c, id); ok {
		t.Fatal("store ErrInvalid must not authorize execution")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "VALIDATION_FAILED" {
		t.Fatalf("code = %q", code)
	}
}

func TestApprovalDecideStoreError(t *testing.T) {
	env := newTestEnv(t)
	ap := createPendingApproval(t, env)
	env.st.errApprovalsDecide = errStoreFailure

	rec := env.do(http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	env.st.errApprovalsDecide = nil

	// 审批服务未装配 → 503。
	gin.SetMode(gin.TestMode)
	rec2 := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec2)
	c.Request = httptestPost("/api/approvals/x/decide", `{"decision":"approve"}`)
	c.Params = gin.Params{{Key: "id", Value: ap.ID}}
	(&Deps{}).handleDecideApproval(c)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil approvals status = %d", rec2.Code)
	}
}

func TestApprovalDenyFlow(t *testing.T) {
	env := newTestEnv(t)
	ap := createPendingApproval(t, env)

	rec := env.do(http.MethodPost, "/api/approvals/"+ap.ID+"/decide", `{"decision":"deny","note":"危险操作"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("deny = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"denied"`) || !strings.Contains(rec.Body.String(), "危险操作") {
		t.Fatalf("deny body = %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 审计查询
// ---------------------------------------------------------------------------

func TestAuditList(t *testing.T) {
	env := newTestEnv(t)
	conv := createConversation(t, env) // 产生一条 conversation.create 审计

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := env.auditW.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	rec := env.do(http.MethodGet, "/api/audit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []store.AuditLog `json:"items"`
	}
	decodeBody(t, rec, &page)
	if len(page.Items) == 0 {
		t.Fatal("audit items empty")
	}
	found := false
	for _, l := range page.Items {
		if l.Action == "conversation.create" && l.ResourceID == conv {
			found = true
		}
	}
	if !found {
		t.Fatalf("conversation.create audit missing: %+v", page.Items)
	}

	// 过滤。
	rec = env.do(http.MethodGet, "/api/audit?action=conversation.create", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered status = %d", rec.Code)
	}
	decodeBody(t, rec, &page)
	for _, l := range page.Items {
		if l.Action != "conversation.create" {
			t.Fatalf("filter leaked: %+v", l)
		}
	}

	// 非法参数。
	for _, q := range []string{
		"?limit=0", "?limit=bad", "?before_id=0", "?before_id=x",
		"?conversation_id=not-a-uuid", "?server_id=not-a-uuid",
	} {
		rec = env.do(http.MethodGet, "/api/audit"+q, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("audit %s status = %d", q, rec.Code)
		}
	}

	// 合法 uuid 过滤。
	rec = env.do(http.MethodGet, "/api/audit?conversation_id=123e4567-e89b-12d3-a456-426614174000", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("uuid filter status = %d", rec.Code)
	}

	// 存储错误 → 500。
	env.st.errAuditList = errStoreFailure
	rec = env.do(http.MethodGet, "/api/audit", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("audit store error status = %d", rec.Code)
	}
	env.st.errAuditList = nil
}

// TestAuditListActorName 验证审计查询响应为 user 操作者装配 actor_name，
// model/system 记录与匹配不到用户的记录保持 null。
func TestAuditListActorName(t *testing.T) {
	env := newTestEnv(t)
	createConversation(t, env) // 产生 actor_type=user 的 conversation.create 审计

	// 一条 model 工具调用审计（actor_id 形如 conversation:<id>）。
	if err := env.auditW.Enqueue(context.Background(), audit.Event{
		ActorType: "model", ActorID: "conversation:conv-1",
		Action: "tool.call", ResourceType: "command", RequestID: "req-actor-name",
	}); err != nil {
		t.Fatal(err)
	}
	// 一条 actor_id 匹配不到用户的 user 审计（历史数据 / 用户被删），
	// 以及一条认证前事件（login_failed，actor_id 为空，无用户名可装配）。
	env.st.mu.Lock()
	env.st.auditLogs = append(env.st.auditLogs,
		store.AuditLog{
			ID: int64(len(env.st.auditLogs) + 1), ActorType: "user",
			ActorID: "0d9a4b9e-6f61-4a30-8e6e-2f2f4f7f8f9f",
			Action:  "server.test", ResourceType: "server", Detail: json.RawMessage(`{}`),
		},
		store.AuditLog{
			ID: int64(len(env.st.auditLogs) + 2), ActorType: "user",
			Action: "auth.login_failed", Detail: json.RawMessage(`{}`),
		},
	)
	env.st.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := env.auditW.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	rec := env.do(http.MethodGet, "/api/audit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"actor_name"`) {
		t.Fatalf("response missing actor_name field: %s", rec.Body.String())
	}
	var page struct {
		Items []struct {
			ActorType string  `json:"actor_type"`
			ActorID   string  `json:"actor_id"`
			Action    string  `json:"action"`
			ActorName *string `json:"actor_name"`
		} `json:"items"`
	}
	decodeBody(t, rec, &page)
	if len(page.Items) == 0 {
		t.Fatal("audit items empty")
	}
	userNamed, modelNull, unknownNull, emptyIDNull := false, false, false, false
	for _, it := range page.Items {
		switch {
		case it.ActorType == "user" && it.ActorID == "user-1":
			if it.ActorName == nil || *it.ActorName != "admin" {
				t.Fatalf("user-1 actor_name = %v, want admin", it.ActorName)
			}
			userNamed = true
		case it.ActorType == "model":
			if it.ActorName != nil {
				t.Fatalf("model actor_name = %v, want null", *it.ActorName)
			}
			modelNull = true
		case it.ActorID == "0d9a4b9e-6f61-4a30-8e6e-2f2f4f7f8f9f":
			if it.ActorName != nil {
				t.Fatalf("unknown user actor_name = %v, want null", *it.ActorName)
			}
			unknownNull = true
		case it.Action == "auth.login_failed":
			if it.ActorID != "" {
				t.Fatalf("login_failed actor_id = %q, want empty", it.ActorID)
			}
			if it.ActorName != nil {
				t.Fatalf("login_failed actor_name = %v, want null", *it.ActorName)
			}
			emptyIDNull = true
		}
	}
	if !userNamed {
		t.Fatal("no audit item for user-1")
	}
	if !modelNull {
		t.Fatal("no model audit item")
	}
	if !unknownNull {
		t.Fatal("no unknown-user audit item")
	}
	if !emptyIDNull {
		t.Fatal("no login_failed audit item")
	}
}

// TestApprovalDecideRejectsExpiredAndDoesNotExecute 第 11 轮走查 high：
// sweeper 尚未把过期 pending 行置为 expired 的窗口内，POST decide 必须
// 409 且不进入 console 同步执行——用户看到倒计时归零后不能再点到批准。
func TestApprovalDecideRejectsExpiredAndDoesNotExecute(t *testing.T) {
	env := newTestEnv(t)
	env.ensureRequester(t)
	serverID := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)

	apID := uuid.NewString()
	expiredAt := time.Now().UTC().Add(-2 * time.Second)
	env.st.mu.Lock()
	env.st.approvals[apID] = store.Approval{
		ID: apID, Source: "console", ServerID: &serverID,
		Command: "systemctl restart nginx", RiskLevel: "needs_approval",
		RuleID: "SERVICE_CHANGE", Status: store.ApprovalPending,
		RequestedBy: testRequesterID,
		RequestedAt: expiredAt.Add(-2 * time.Minute),
		ExpiresAt:   expiredAt, ApprovalMode: store.ApprovalModeNone,
	}
	env.st.mu.Unlock()

	rec := env.doAs(env.approverCookie(t), http.MethodPost, "/api/approvals/"+apID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("decide expired = %d %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "APPROVAL_NOT_PENDING" {
		t.Fatalf("code = %s", code)
	}
	if n := env.sshExec.callCount(); n != 0 {
		t.Fatalf("expired approval executed command %d time(s)", n)
	}
	got, err := env.st.Approvals().Get(context.Background(), apID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.ApprovalExpired || got.DecidedBy != nil {
		t.Fatalf("expired approval = status %s decided_by %v", got.Status, got.DecidedBy)
	}
}

// TestApprovalDecideRejectsExpiredPolicyWithoutApplying 第 11 轮走查 high
// 的 policy 分支：过期后批准不得把全局门限从 standard 放宽到 none。
func TestApprovalDecideRejectsExpiredPolicyWithoutApplying(t *testing.T) {
	env := newTestEnv(t)
	env.ensureRequester(t)

	from, to := string(store.ApprovalModeStandard), string(store.ApprovalModeNone)
	apID := uuid.NewString()
	expiredAt := time.Now().UTC().Add(-time.Second)
	env.st.mu.Lock()
	env.st.approvals[apID] = store.Approval{
		ID: apID, Source: "policy", Status: store.ApprovalPending,
		Command: "", RiskLevel: "needs_approval",
		RequestedBy: testRequesterID, RequestedAt: expiredAt.Add(-2 * time.Minute),
		ExpiresAt: expiredAt, PolicyFrom: &from, PolicyTo: &to,
		ApprovalMode: store.ApprovalModeStandard,
	}
	env.st.mu.Unlock()

	rec := env.doAs(env.approverCookie(t), http.MethodPost, "/api/approvals/"+apID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("decide expired policy = %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/api/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("settings = %d %s", rec.Code, rec.Body.String())
	}
	var settings struct {
		ApprovalMode string `json:"approval_mode"`
	}
	decodeBody(t, rec, &settings)
	if settings.ApprovalMode != string(store.ApprovalModeStandard) {
		t.Fatalf("approval_mode = %q, want standard (expired policy must not apply)", settings.ApprovalMode)
	}
}

// ---------------------------------------------------------------------------
// 风险预览
// ---------------------------------------------------------------------------

func TestPreviewRisk(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(http.MethodPost, "/api/ops/preview-risk", `{"command":"ls -la"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var preview struct {
		RiskLevel string `json:"risk_level"`
		RuleID    string `json:"rule_id"`
	}
	decodeBody(t, rec, &preview)
	if preview.RiskLevel != "safe" {
		t.Fatalf("safe preview = %+v", preview)
	}

	rec = env.do(http.MethodPost, "/api/ops/preview-risk", `{"command":"rm -rf /"}`)
	decodeBody(t, rec, &preview)
	if preview.RiskLevel != "forbidden" || preview.RuleID != "RM_PROTECTED" {
		t.Fatalf("forbidden preview = %+v", preview)
	}

	// 校验分支。
	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"empty command", `{"command":""}`, http.StatusBadRequest, "COMMAND_INVALID"},
		{"command too large", `{"command":"` + strings.Repeat("a", 16385) + `"}`, http.StatusBadRequest, "COMMAND_TOO_LARGE"},
		{"malformed body", `{`, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"unknown field", `{"command":"ls","x":1}`, http.StatusBadRequest, "VALIDATION_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodPost, "/api/ops/preview-risk", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			if code, _ := errorBody(t, rec); code != tc.code {
				t.Fatalf("code = %s", code)
			}
		})
	}
}

// testConvPtr 是测试辅助：字符串转指针。
func testConvPtr(s string) *string { return &s }
