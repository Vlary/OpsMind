package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiops/internal/audit"
	"aiops/internal/store"
)

// TestTerminalGrantFlow 覆盖 00012 JIT grant 全链路（评审 high）：
// 申请 → 他人批准 → 单次消费；无 grant/重复消费/类型不匹配均 403。
func TestTerminalGrantFlow(t *testing.T) {
	env := newTermTestEnv(t)
	serverID := env.createServer(t, "linux", store.AuthMethodPassword)
	otherServer := env.createServer(t, "linux", store.AuthMethodPassword)

	// 无 grant 直接开会话 → 403 TERMINAL_APPROVAL_REQUIRED。
	rec := env.do(http.MethodPost, "/api/term/sessions", `{"server_id":"`+serverID+`"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "TERMINAL_APPROVAL_REQUIRED") {
		t.Fatalf("session without grant = %d %s", rec.Code, rec.Body.String())
	}

	// 申请 grant → 202，source=terminal/grant_kind=term。
	rec = env.do(http.MethodPost, "/api/term/grants", `{"server_id":"`+serverID+`"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("request grant = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Approval store.Approval `json:"approval"`
		Grant    struct {
			ApprovalID string `json:"approval_id"`
			GrantKind  string `json:"grant_kind"`
		} `json:"grant"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if created.Approval.Source != "terminal" || created.Approval.GrantKind == nil ||
		*created.Approval.GrantKind != "term" || created.Grant.GrantKind != "term" {
		t.Fatalf("grant = %+v", created)
	}
	grantID := created.Approval.ID

	// 发起人自审 → 403，审批保持 pending。
	rec = env.do(http.MethodPost, "/api/approvals/"+grantID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "APPROVAL_SELF_DECISION") {
		t.Fatalf("self approve = %d %s", rec.Code, rec.Body.String())
	}

	// 第二人批准（admin 之外再加一个管理员）。
	env.st.mu.Lock()
	env.st.users["admin2"] = store.User{
		ID: "user-2", Username: "admin2", PasswordHash: getAdminHash(),
		Role: store.RoleAdmin, Enabled: true, CreatedAt: time.Now().UTC(),
	}
	env.st.mu.Unlock()
	admin2 := loginUser(t, env.router, "admin2")
	rec = doAsCookie(env.router, admin2, http.MethodPost, "/api/approvals/"+grantID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve grant = %d %s", rec.Code, rec.Body.String())
	}

	// 消费成功 → 201。
	rec = env.do(http.MethodPost, "/api/term/sessions", `{"server_id":"`+serverID+`","grant_id":"`+grantID+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("consume grant = %d %s", rec.Code, rec.Body.String())
	}
	// 单次消费：换一台服务器复用同一 grant → 403。
	rec = env.do(http.MethodPost, "/api/term/sessions", `{"server_id":"`+otherServer+`","grant_id":"`+grantID+`"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "TERMINAL_APPROVAL_REQUIRED") {
		t.Fatalf("reuse grant = %d %s", rec.Code, rec.Body.String())
	}

	// grant_kind 不匹配：申请 rdp grant 用于 term 会话 → 403 MISMATCH。
	rec = env.do(http.MethodPost, "/api/rdp/grants", `{"server_id":"`+otherServer+`"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("request rdp grant = %d %s", rec.Code, rec.Body.String())
	}
	var rdpCreated struct {
		Approval store.Approval `json:"approval"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rdpCreated); err != nil {
		t.Fatal(err)
	}
	rec = doAsCookie(env.router, admin2, http.MethodPost,
		"/api/approvals/"+rdpCreated.Approval.ID+"/decide", `{"decision":"approve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve rdp grant = %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodPost, "/api/term/sessions",
		`{"server_id":"`+otherServer+`","grant_id":"`+rdpCreated.Approval.ID+`"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "TERMINAL_GRANT_KIND_MISMATCH") {
		t.Fatalf("kind mismatch = %d %s", rec.Code, rec.Body.String())
	}
}

// loginUser 用固定测试密码登录任意用户名并返回会话 token。
func loginUser(t *testing.T, router http.Handler, username string) string {
	t.Helper()
	body := `{"username":"` + username + `","password":"` + testPassword + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s = %d %s", username, rec.Code, rec.Body.String())
	}
	raw := rec.Header().Get("Set-Cookie")
	parts := strings.SplitN(raw, ";", 2)
	return strings.TrimPrefix(parts[0], sessionCookieName+"=")
}

func doAsCookie(router http.Handler, cookie, method, path, body string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestMustChangePasswordRESTGate 覆盖评审 low：must_change 此前只在两个 WS
// handler 生效，REST 可调用除 me/logout/change 外的全部 API。
func TestMustChangePasswordRESTGate(t *testing.T) {
	env := newTestEnv(t)
	env.st.mu.Lock()
	user := env.st.users["admin"]
	user.MustChangePassword = true
	env.st.users["admin"] = user
	env.st.mu.Unlock()

	rec := env.do(http.MethodGet, "/api/servers", "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "PASSWORD_CHANGE_REQUIRED") {
		t.Fatalf("must_change REST = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(http.MethodGet, "/api/auth/me", ""); rec.Code != http.StatusOK {
		t.Fatalf("me under must_change = %d %s", rec.Code, rec.Body.String())
	}
}

// TestAuditVerifyEndpoint 覆盖评审 low：/api/audit/verify 此前只在权限表
// 登记、从未注册，AUDIT_CHAIN_BROKEN 不可达。
func TestAuditVerifyEndpoint(t *testing.T) {
	env := newTestEnv(t)
	for i := 0; i < 3; i++ {
		if err := env.auditW.Enqueue(context.Background(), audit.Event{
			ActorType: "user", ActorID: "user-1", Action: "test.action",
			ResourceType: "test", ResourceID: "r", Detail: map[string]any{"i": i},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	rec := env.do(http.MethodGet, "/api/audit/verify", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("verify = %d %s", rec.Code, rec.Body.String())
	}
	var okResp struct {
		OK      bool  `json:"ok"`
		Checked int   `json:"checked"`
		HeadID  int64 `json:"head_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &okResp); err != nil {
		t.Fatal(err)
	}
	if !okResp.OK || okResp.Checked < 3 {
		t.Fatalf("verify response = %+v", okResp)
	}

	// 篡改中间行 → 409 AUDIT_CHAIN_BROKEN。
	env.st.mu.Lock()
	env.st.auditLogs[1].Detail = json.RawMessage(`{"i":999}`)
	env.st.mu.Unlock()
	rec = env.do(http.MethodGet, "/api/audit/verify", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("tampered verify = %d %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "AUDIT_CHAIN_BROKEN" {
		t.Fatalf("tampered code = %q", code)
	}
}
