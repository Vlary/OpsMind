package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"aiops/internal/auth"
	"aiops/internal/store"
)

// captureDeliverer 记录 invite/reset token（测试用投递通道）。
type captureDeliverer struct {
	kind  store.UserTokenKind
	email string
	token string
	err   error
}

func (c *captureDeliverer) DeliverToken(_ context.Context, kind store.UserTokenKind, email, _ string, rawToken string, _ time.Time) error {
	if c.err != nil {
		return c.err
	}
	c.kind = kind
	c.email = email
	c.token = rawToken
	return nil
}

func addTestUser(t *testing.T, env *testEnv, username, password string, role store.Role, enabled bool) store.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	u := store.User{
		ID: uuid.NewString(), Username: username, PasswordHash: hash,
		Role: role, Enabled: enabled, Email: username + "@example.com",
		CreatedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
	}
	env.st.users[username] = u
	return u
}

// loginAs 以指定用户登录并返回会话 cookie。
func loginAs(t *testing.T, env *testEnv, username, password string) string {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s status = %d body = %s", username, rec.Code, rec.Body.String())
	}
	raw := rec.Header().Get("Set-Cookie")
	parts := strings.SplitN(raw, ";", 2)
	return strings.TrimPrefix(parts[0], sessionCookieName+"=")
}

// doAs 发起带指定 cookie / Origin / 源 IP 的请求。
func doAs(env *testEnv, method, path, body, cookie, remoteAddr string) *httptest.ResponseRecorder {
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
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec
}

func TestUsersRequirePermission(t *testing.T) {
	env := newTestEnv(t)
	addTestUser(t, env, "ops01", "ops-pw", store.RoleOperator, true)
	opsCookie := loginAs(t, env, "ops01", "ops-pw")

	rec := doAs(env, http.MethodGet, "/api/users", "", opsCookie, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator GET /users = %d, body = %s", rec.Code, rec.Body.String())
	}
	code, _ := errorBody(t, rec)
	if code != "FORBIDDEN" {
		t.Fatalf("code = %s, want FORBIDDEN", code)
	}
	rec = doAs(env, http.MethodPost, "/api/users", `{"username":"x","email":"x@y.z","role":"auditor"}`, opsCookie, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator POST /users = %d", rec.Code)
	}

	// admin 列表含 admin、预置第二管理员与 ops01。
	rec = doAs(env, http.MethodGet, "/api/users", "", env.cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET /users = %d body = %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []store.User `json:"items"`
	}
	decodeBody(t, rec, &list)
	if len(list.Items) != 3 {
		t.Fatalf("items = %d, want 3: %+v", len(list.Items), list.Items)
	}
}

func TestUsersDeliveryUnavailable(t *testing.T) {
	env := newTestEnv(t)

	rec := doAs(env, http.MethodPost, "/api/users",
		`{"username":"alice","email":"alice@example.com","role":"operator"}`, env.cookie, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("create without deliverer = %d body = %s", rec.Code, rec.Body.String())
	}
	code, _ := errorBody(t, rec)
	if code != "DELIVERY_UNAVAILABLE" {
		t.Fatalf("code = %s", code)
	}
	env.st.mu.Lock()
	_, exists := env.st.users["alice"]
	env.st.mu.Unlock()
	if exists {
		t.Fatal("user created despite delivery unavailable")
	}
}

func TestCreateUserAndAcceptInvite(t *testing.T) {
	env := newTestEnv(t)
	d := &captureDeliverer{}
	env.authSvc.SetDeliverer(d)

	rec := doAs(env, http.MethodPost, "/api/users",
		`{"username":"alice","email":"alice@example.com","role":"operator","display_name":"Alice"}`,
		env.cookie, "203.0.113.5:1234")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d body = %s", rec.Code, rec.Body.String())
	}
	var created store.User
	decodeBody(t, rec, &created)
	if created.Enabled || !created.InvitePending || created.Role != store.RoleOperator {
		t.Fatalf("created = %+v", created)
	}
	if strings.Contains(rec.Body.String(), d.token) {
		t.Fatal("response leaked the invite token")
	}
	if d.kind != store.UserTokenInvite || d.email != "alice@example.com" || d.token == "" {
		t.Fatalf("deliverer = %+v", d)
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	if !auditHas(env, "user.create") {
		t.Fatal("audit missing user.created")
	}

	// 创建者 IP 兑换 → 403 SELF_REDEEM_FORBIDDEN。
	body := fmt.Sprintf(`{"token":%q,"new_password":"alice-pw"}`, d.token)
	rec = doAs(env, http.MethodPost, "/api/auth/invite/accept", body, "", "203.0.113.5:1234")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self redeem = %d body = %s", rec.Code, rec.Body.String())
	}

	// 其他 IP 兑换 → 204，随后可登录。
	rec = doAs(env, http.MethodPost, "/api/auth/invite/accept", body, "", "198.51.100.7:1234")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("accept = %d body = %s", rec.Code, rec.Body.String())
	}
	cookie := loginAs(t, env, "alice", "alice-pw")
	if cookie == "" {
		t.Fatal("empty cookie")
	}
	env.st.mu.Lock()
	alice := env.st.users["alice"]
	env.st.mu.Unlock()
	if !alice.Enabled || alice.InvitePending {
		t.Fatalf("alice after accept = %+v", alice)
	}
}

func TestUpdateUserGuardsAndSessionRevocation(t *testing.T) {
	env := newTestEnv(t)
	bob := addTestUser(t, env, "bob", "bob-pw", store.RoleOperator, true)
	bobCookie := loginAs(t, env, "bob", "bob-pw")

	// 改角色 → 200 且目标会话立即失效。
	rec := doAs(env, http.MethodPatch, "/api/users/"+bob.ID, `{"role":"auditor"}`, env.cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("patch role = %d body = %s", rec.Code, rec.Body.String())
	}
	var updated store.User
	decodeBody(t, rec, &updated)
	if updated.Role != store.RoleAuditor {
		t.Fatalf("role = %q", updated.Role)
	}
	if rec := doAs(env, http.MethodGet, "/api/auth/me", "", bobCookie, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bob session after role change = %d", rec.Code)
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	if !auditHas(env, "user.update") {
		t.Fatal("audit missing user.role_changed")
	}

	// 自我停用/自我改 role → 409 SELF_OPERATION。
	adminID := env.st.users["admin"].ID
	for _, body := range []string{`{"enabled":false}`, `{"role":"auditor"}`} {
		rec := doAs(env, http.MethodPatch, "/api/users/"+adminID, body, env.cookie, "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("self op %s = %d body = %s", body, rec.Code, rec.Body.String())
		}
		code, _ := errorBody(t, rec)
		if code != "SELF_OPERATION" {
			t.Fatalf("code = %s, want SELF_OPERATION", code)
		}
	}

	// revoke-sessions → 204，目标会话失效。
	bobCookie2 := loginAs(t, env, "bob", "bob-pw")
	if rec := doAs(env, http.MethodPost, "/api/users/"+bob.ID+"/revoke-sessions", "", env.cookie, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec := doAs(env, http.MethodGet, "/api/auth/me", "", bobCookie2, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bob session after revoke = %d", rec.Code)
	}
}

func TestResetUserPasswordFlow(t *testing.T) {
	env := newTestEnv(t)
	d := &captureDeliverer{}
	env.authSvc.SetDeliverer(d)
	bob := addTestUser(t, env, "bob", "bob-pw", store.RoleOperator, true)
	bobCookie := loginAs(t, env, "bob", "bob-pw")

	rec := doAs(env, http.MethodPost, "/api/users/"+bob.ID+"/password", "", env.cookie, "203.0.113.5:1234")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reset = %d body = %s", rec.Code, rec.Body.String())
	}
	var reset struct {
		UserID    string `json:"user_id"`
		Delivered bool   `json:"delivered"`
	}
	decodeBody(t, rec, &reset)
	if reset.UserID != bob.ID || !reset.Delivered {
		t.Fatalf("reset response = %+v", reset)
	}
	if strings.Contains(rec.Body.String(), d.token) {
		t.Fatal("response leaked the reset token")
	}
	if rec := doAs(env, http.MethodGet, "/api/auth/me", "", bobCookie, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bob session after reset = %d", rec.Code)
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	if !auditHas(env, "user.credential_reset") {
		t.Fatal("audit missing user.password_reset")
	}

	// 兑换 reset token（非创建者 IP）→ 新密码生效。
	body := fmt.Sprintf(`{"token":%q,"new_password":"bob-new"}`, d.token)
	rec = doAs(env, http.MethodPost, "/api/auth/invite/accept", body, "", "198.51.100.7:1234")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("accept reset = %d body = %s", rec.Code, rec.Body.String())
	}
	if cookie := loginAs(t, env, "bob", "bob-new"); cookie == "" {
		t.Fatal("empty login cookie")
	}

	// 不能重置自己 → 409 SELF_OPERATION。
	adminID := env.st.users["admin"].ID
	rec = doAs(env, http.MethodPost, "/api/users/"+adminID+"/password", "", env.cookie, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("self reset = %d body = %s", rec.Code, rec.Body.String())
	}
}

// 仓储错误必须映射为 HTTP 错误并终止：ErrNotFound → 404，冲突 → 409，
// 未知错误 → 500。历史上 writeUserStoreError 自递归会让进程栈溢出。
func TestUserStoreErrorMapping(t *testing.T) {
	env := newTestEnv(t)

	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		rec := doAs(env, http.MethodGet, "/api/users/"+id, "", env.cookie, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET /users/%s = %d body = %s, want 404", id, rec.Code, rec.Body.String())
		}
		code, _ := errorBody(t, rec)
		if code != "NOT_FOUND" {
			t.Fatalf("code = %q", code)
		}
	}

	// 已存在用户名 → 409（handler 在创建前按 username 预检）。
	d := &captureDeliverer{}
	env.authSvc.SetDeliverer(d)
	rec := doAs(env, http.MethodPost, "/api/users",
		`{"username":"admin","email":"admin2@example.com","role":"auditor"}`, env.cookie, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate username = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
}

// disableUserForTest 直接改内存 store 的 enabled（构造部署形态用，不经过
// HTTP；HTTP 路径的守卫由既有用例覆盖）。
func disableUserForTest(t *testing.T, env *testEnv, username string) {
	t.Helper()
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	u, ok := env.st.users[username]
	if !ok {
		t.Fatalf("user %q not found", username)
	}
	u.Enabled = false
	env.st.users[username] = u
}

// enableUserForTest 恢复 enabled=true。
func enableUserForTest(t *testing.T, env *testEnv, username string) {
	t.Helper()
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	u, ok := env.st.users[username]
	if !ok {
		t.Fatalf("user %q not found", username)
	}
	u.Enabled = true
	env.st.users[username] = u
}

// failingDeliverer 模拟「配置了 SMTP 但投递失败」。
type failingDeliverer struct{ err error }

func (f *failingDeliverer) DeliverToken(context.Context, store.UserTokenKind, string, string, string, time.Time) error {
	return f.err
}

// TestCreateUserDeliveryFailureRollsBack：投递失败时用户行/token/审计全部
// 回滚，同名可重试（评审 medium：此前留下永久占用用户名的僵尸账号，重试
// 同名 409、界面无重发/删除入口）。
func TestCreateUserDeliveryFailureRollsBack(t *testing.T) {
	env := newTestEnv(t)
	env.authSvc.SetDeliverer(&failingDeliverer{err: errors.New("smtp: connection reset")})

	body := `{"username":"smtpvictim","email":"smtpvictim@example.com","role":"operator"}`
	rec := doAs(env, http.MethodPost, "/api/users", body, env.cookie, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("create with failing delivery = %d body = %s", rec.Code, rec.Body.String())
	}
	code, message := errorBody(t, rec)
	if code != "DELIVERY_UNAVAILABLE" || !strings.Contains(message, "retry is safe") {
		t.Fatalf("code/message = %s / %s", code, message)
	}
	env.st.mu.Lock()
	_, exists := env.st.users["smtpvictim"]
	env.st.mu.Unlock()
	if exists {
		t.Fatal("user row must be rolled back when delivery fails")
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	if auditHas(env, "user.create") {
		t.Fatal("user.create audit must not be written when delivery fails")
	}

	// 修复通道后可同名重试 → 201（不 409）。
	cap := &captureDeliverer{}
	env.authSvc.SetDeliverer(cap)
	rec = doAs(env, http.MethodPost, "/api/users", body, env.cookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("retry after delivery fixed = %d body = %s", rec.Code, rec.Body.String())
	}
}

// TestResetPasswordDeliveryFailureKeepsSessions：重置密码投递失败时不得
// 先踢人下线（评审 medium：503 但会话已删、管理员以为没生效）。
func TestResetPasswordDeliveryFailureKeepsSessions(t *testing.T) {
	env := newTestEnv(t)
	env.authSvc.SetDeliverer(&failingDeliverer{err: errors.New("smtp: connection reset")})
	bob := addTestUser(t, env, "bob", "bob-pw", store.RoleOperator, true)
	bobCookie := loginAs(t, env, "bob", "bob-pw")

	rec := doAs(env, http.MethodPost, "/api/users/"+bob.ID+"/password", "", env.cookie, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("reset with failing delivery = %d body = %s", rec.Code, rec.Body.String())
	}
	code, message := errorBody(t, rec)
	if code != "DELIVERY_UNAVAILABLE" || !strings.Contains(message, "no session was revoked") {
		t.Fatalf("code/message = %s / %s", code, message)
	}
	if rec := doAs(env, http.MethodGet, "/api/auth/me", "", bobCookie, ""); rec.Code != http.StatusOK {
		t.Fatalf("bob session after failed reset = %d, want 200", rec.Code)
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	if auditHas(env, "user.credential_reset") {
		t.Fatal("user.credential_reset must not be written when delivery fails")
	}
	// 第 9 轮走查：reset 成功会补写 user.sessions_revoked；投递失败整体回滚时
	// 这条「会话已吊销」审计同样不得出现（否则审计与真实状态不一致）。
	if auditHas(env, "user.sessions_revoked") {
		t.Fatal("user.sessions_revoked must not be written when delivery fails")
	}

	// 修复通道后重试 → 202 且会话被吊销、审计带 expires_at。
	env.authSvc.SetDeliverer(&captureDeliverer{})
	rec = doAs(env, http.MethodPost, "/api/users/"+bob.ID+"/password", "", env.cookie, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reset retry = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec := doAs(env, http.MethodGet, "/api/auth/me", "", bobCookie, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bob session after successful reset = %d, want 401", rec.Code)
	}
}

// TestUserAuditHasRequestMetadata：用户管理审计（事务内 outbox 路径）必须
// 带来源 IP 与 request_id（评审 medium：审计页两列恒空，无法与请求日志关联）。
func TestUserAuditHasRequestMetadata(t *testing.T) {
	env := newTestEnv(t)
	bob := addTestUser(t, env, "bob", "bob-pw", store.RoleOperator, true)

	rec := doAs(env, http.MethodPatch, "/api/users/"+bob.ID, `{"enabled":false}`, env.cookie, "203.0.113.77:4242")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d body = %s", rec.Code, rec.Body.String())
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	var row *store.AuditLog
	for i := range env.st.auditLogs {
		if env.st.auditLogs[i].Action == "user.disable" {
			row = &env.st.auditLogs[i]
		}
	}
	if row == nil {
		t.Fatal("user.disable audit missing")
	}
	if row.IP != "203.0.113.77" {
		t.Fatalf("audit ip = %q, want 203.0.113.77", row.IP)
	}
	if row.RequestID == "" {
		t.Fatal("audit request_id is empty")
	}
	if env.st.auditLogs[len(env.st.auditLogs)-1].RequestID == "" {
		t.Fatal("later audit rows lost request_id")
	}
}

// TestResetPasswordAuditCarriesExpiresAt：reset 审计 detail 必须含
// expires_at（评审 medium：此前在 outbox 序列化后才写入，审计重建不出
// 令牌有效期）。
func TestResetPasswordAuditCarriesExpiresAt(t *testing.T) {
	env := newTestEnv(t)
	env.authSvc.SetDeliverer(&captureDeliverer{})
	bob := addTestUser(t, env, "bob", "bob-pw", store.RoleOperator, true)

	rec := doAs(env, http.MethodPost, "/api/users/"+bob.ID+"/password", "", env.cookie, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reset = %d body = %s", rec.Code, rec.Body.String())
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	var row *store.AuditLog
	for i := range env.st.auditLogs {
		if env.st.auditLogs[i].Action == "user.credential_reset" {
			row = &env.st.auditLogs[i]
		}
	}
	if row == nil {
		t.Fatal("user.credential_reset audit missing")
	}
	var detail map[string]any
	if err := json.Unmarshal(row.Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	expires, _ := detail["expires_at"].(string)
	if expires == "" {
		t.Fatalf("detail = %v, want expires_at", detail)
	}
	if _, err := time.Parse(time.RFC3339, expires); err != nil {
		t.Fatalf("expires_at = %q: %v", expires, err)
	}
}

// TestResendInviteUpdatesEmailAndDeliveryTarget：同名未激活账号重发邀请时，
// 本次请求里的新邮箱必须写入 users 并作为投递目标（第 9 轮走查 medium：
// 此前只同步 role/display_name，界面承诺投递到目标邮箱，实际仍发旧地址）。
func TestResendInviteUpdatesEmailAndDeliveryTarget(t *testing.T) {
	env := newTestEnv(t)
	d := &captureDeliverer{}
	env.authSvc.SetDeliverer(d)

	rec := doAs(env, http.MethodPost, "/api/users",
		`{"username":"resendui","email":"old@example.com","role":"auditor"}`, env.cookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d body = %s", rec.Code, rec.Body.String())
	}
	if d.email != "old@example.com" {
		t.Fatalf("initial delivery target = %q, want old@example.com", d.email)
	}

	rec = doAs(env, http.MethodPost, "/api/users",
		`{"username":"resendui","email":"new@example.com","role":"approver"}`, env.cookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("resend = %d body = %s", rec.Code, rec.Body.String())
	}
	var resent store.User
	decodeBody(t, rec, &resent)
	if resent.Email != "new@example.com" {
		t.Fatalf("response email = %q, want new@example.com", resent.Email)
	}
	if resent.Role != store.RoleApprover {
		t.Fatalf("response role = %q, want approver", resent.Role)
	}
	if d.email != "new@example.com" {
		t.Fatalf("delivery target = %q, want new@example.com", d.email)
	}

	env.st.mu.Lock()
	stored := env.st.users["resendui"]
	env.st.mu.Unlock()
	if stored.Email != "new@example.com" {
		t.Fatalf("stored email = %q, want new@example.com", stored.Email)
	}

	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	var row *store.AuditLog
	for i := range env.st.auditLogs {
		if env.st.auditLogs[i].Action == "user.invite_resent" {
			row = &env.st.auditLogs[i]
		}
	}
	if row == nil {
		t.Fatal("user.invite_resent audit missing")
	}
	var detail map[string]any
	if err := json.Unmarshal(row.Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if changed, _ := detail["email_changed"].(bool); !changed {
		t.Fatalf("detail = %v, want email_changed=true", detail)
	}
	if detail["role"] != string(store.RoleApprover) {
		t.Fatalf("detail role = %v, want approver", detail["role"])
	}
	// 审计不落邮箱明文（PII）：只记录是否变化。
	if strings.Contains(string(row.Detail), "new@example.com") ||
		strings.Contains(string(row.Detail), "old@example.com") {
		t.Fatalf("audit detail leaked email: %s", row.Detail)
	}
}

// TestResetPasswordWritesSessionsRevokedAudit：reset 会吊销目标全部会话，
// 审计必须同时写 user.sessions_revoked（detail action=reset），否则审计员
// 按「吊销用户会话」筛选看不到这次踢下线（第 9 轮走查 low，契约 9.7）。
func TestResetPasswordWritesSessionsRevokedAudit(t *testing.T) {
	env := newTestEnv(t)
	env.authSvc.SetDeliverer(&captureDeliverer{})
	bob := addTestUser(t, env, "bob", "bob-pw", store.RoleOperator, true)

	rec := doAs(env, http.MethodPost, "/api/users/"+bob.ID+"/password", "", env.cookie, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reset = %d body = %s", rec.Code, rec.Body.String())
	}
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}

	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	var row *store.AuditLog
	count := 0
	for i := range env.st.auditLogs {
		if env.st.auditLogs[i].Action == "user.sessions_revoked" {
			row = &env.st.auditLogs[i]
			count++
		}
	}
	if row == nil || count != 1 {
		t.Fatalf("user.sessions_revoked rows = %d, want 1", count)
	}
	if row.ResourceType != "user" || row.ResourceID != bob.ID {
		t.Fatalf("resource = %s/%s, want user/%s", row.ResourceType, row.ResourceID, bob.ID)
	}
	var detail map[string]any
	if err := json.Unmarshal(row.Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail["action"] != "reset" {
		t.Fatalf("detail action = %v, want reset", detail["action"])
	}
	if detail["target_user_id"] != bob.ID {
		t.Fatalf("detail target_user_id = %v, want %s", detail["target_user_id"], bob.ID)
	}
	changes, ok := detail["changes"].(map[string]any)
	if !ok || changes["sessions"] != "revoked" {
		t.Fatalf("detail changes = %v, want sessions=revoked", detail["changes"])
	}
	for _, forbidden := range []string{"token", "password"} {
		if _, exists := detail[forbidden]; exists {
			t.Fatalf("detail must not contain %q: %v", forbidden, detail)
		}
	}
}
