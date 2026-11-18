// rbac_round5_test.go 覆盖第 5 轮走查的两项 medium：
//
//  1. 接受邀请 / 自助改密的审计必须与业务变更同事务写 audit_outbox，
//     客户端断开或进程崩溃不会丢；
//  2. 待接受邀请（含邀请已过期）的账号不得被直接启用/重置密码，管理员
//     应走「重发邀请」（POST /api/users 同名重发）。
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"aiops/internal/auth"
	"aiops/internal/store"
)

// outboxPayloads 返回内存 outbox 中已解码的审计载荷（不 Flush）。
func outboxPayloads(t *testing.T, env *testEnv) []store.CreateAuditParams {
	t.Helper()
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	out := make([]store.CreateAuditParams, 0, len(env.st.outbox))
	for _, entry := range env.st.outbox {
		var payload store.CreateAuditParams
		if err := json.Unmarshal(entry.Payload, &payload); err != nil {
			t.Fatalf("outbox payload: %v", err)
		}
		out = append(out, payload)
	}
	return out
}

func findOutboxAction(t *testing.T, env *testEnv, action string) *store.CreateAuditParams {
	t.Helper()
	for _, payload := range outboxPayloads(t, env) {
		if payload.Action == action {
			p := payload
			return &p
		}
	}
	return nil
}

// 改密审计必须在改密事务内写 outbox：无需 Flush，审计已可查，且带
// actor_id/IP/request_id。
func TestChangePasswordAuditCommittedInTx(t *testing.T) {
	env := newTestEnv(t)

	body := `{"old_password":"` + testPassword + `","new_password":"round5-new-pw"}`
	rec := env.do(http.MethodPost, "/api/auth/password", body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("change password = %d body = %s", rec.Code, rec.Body.String())
	}

	row := findOutboxAction(t, env, "auth.password_changed")
	if row == nil {
		t.Fatal("auth.password_changed must be written to audit_outbox in the password-change transaction")
	}
	if row.ActorID != "user-1" || row.ResourceType != "user" || row.ResourceID != "user-1" {
		t.Fatalf("audit actor/resource = %q %q %q", row.ActorID, row.ResourceType, row.ResourceID)
	}
	if row.IP == "" || row.RequestID == "" {
		t.Fatalf("audit metadata missing: ip=%q request_id=%q", row.IP, row.RequestID)
	}
}

// 邀请兑换审计必须在 AcceptToken 事务内写 outbox：受邀角色可能是 admin，
// 必须在客户端断开/进程崩溃时仍能证明谁在何时激活了账号。
func TestAcceptInviteAuditCommittedInTx(t *testing.T) {
	env := newTestEnv(t)
	d := &captureDeliverer{}
	env.authSvc.SetDeliverer(d)

	rec := doAs(env, http.MethodPost, "/api/users",
		`{"username":"round5-invitee","email":"invitee@example.com","role":"admin"}`,
		env.cookie, "203.0.113.5:1234")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d body = %s", rec.Code, rec.Body.String())
	}

	body := fmt.Sprintf(`{"token":%q,"new_password":"round5-invitee-pw"}`, d.token)
	rec = doAs(env, http.MethodPost, "/api/auth/invite/accept", body, "", "198.51.100.7:1234")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("accept invite = %d body = %s", rec.Code, rec.Body.String())
	}

	row := findOutboxAction(t, env, "user.invite_accepted")
	if row == nil {
		t.Fatal("user.invite_accepted must be written to audit_outbox in the AcceptToken transaction")
	}
	if row.ActorID == "" || row.ActorID != row.ResourceID {
		t.Fatalf("audit actor/resource = %q/%q, want the invited user id", row.ActorID, row.ResourceID)
	}
	if row.ActorType != "user" || row.ResourceType != "user" {
		t.Fatalf("audit actor/resource type = %q/%q", row.ActorType, row.ResourceType)
	}
	if row.RequestID == "" {
		t.Fatal("invite acceptance audit lost request_id")
	}
}

// 兑换 reset token 的审计必须指向目标用户（此前 acceptReset 返回空 userID，
// ActorID/ResourceID 为空，审计员无法定位账号）。
func TestAcceptResetAuditHasUserID(t *testing.T) {
	env := newTestEnv(t)
	d := &captureDeliverer{}
	env.authSvc.SetDeliverer(d)
	bob := addTestUser(t, env, "round5-reset-target", "bob-pw", store.RoleOperator, true)

	rec := doAs(env, http.MethodPost, "/api/users/"+bob.ID+"/password", "", env.cookie, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reset = %d body = %s", rec.Code, rec.Body.String())
	}
	body := fmt.Sprintf(`{"token":%q,"new_password":"round5-bob-new"}`, d.token)
	rec = doAs(env, http.MethodPost, "/api/auth/invite/accept", body, "", "198.51.100.9:1234")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("accept reset = %d body = %s", rec.Code, rec.Body.String())
	}

	row := findOutboxAction(t, env, "auth.password_changed")
	if row == nil {
		t.Fatal("reset acceptance must write auth.password_changed to audit_outbox")
	}
	if row.ActorID != bob.ID || row.ResourceID != bob.ID {
		t.Fatalf("audit actor/resource = %q/%q, want %q", row.ActorID, row.ResourceID, bob.ID)
	}
}

// 待接受邀请的账号：启用必须 409 INVITE_PENDING，且邀请链接仍可用。
func TestEnableInvitePendingUserRejected(t *testing.T) {
	env := newTestEnv(t)
	d := &captureDeliverer{}
	env.authSvc.SetDeliverer(d)

	rec := doAs(env, http.MethodPost, "/api/users",
		`{"username":"round5-pending","email":"pending@example.com","role":"operator"}`,
		env.cookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d body = %s", rec.Code, rec.Body.String())
	}
	var created store.User
	decodeBody(t, rec, &created)

	rec = doAs(env, http.MethodPatch, "/api/users/"+created.ID, `{"enabled":true}`, env.cookie, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("enable pending = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != codeInvitePending {
		t.Fatalf("code = %q, want %q", code, codeInvitePending)
	}

	env.st.mu.Lock()
	still := env.st.users["round5-pending"]
	env.st.mu.Unlock()
	if still.Enabled {
		t.Fatal("pending-invite user must stay disabled")
	}

	// 拒绝启用不能消耗或作废邀请：原 token 仍可完成激活。
	body := fmt.Sprintf(`{"token":%q,"new_password":"round5-pending-pw"}`, d.token)
	rec = doAs(env, http.MethodPost, "/api/auth/invite/accept", body, "", "198.51.100.10:1234")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("invite after rejected enable = %d body = %s", rec.Code, rec.Body.String())
	}
}

// 待接受邀请的账号：重置密码必须 409 INVITE_PENDING，且不签发 reset token
// （否则管理员看到 202，目标接受后仍因 enabled=false 登录 401，同时旧邀请
// 链接失效）。
func TestResetInvitePendingUserRejected(t *testing.T) {
	env := newTestEnv(t)
	d := &captureDeliverer{}
	env.authSvc.SetDeliverer(d)

	rec := doAs(env, http.MethodPost, "/api/users",
		`{"username":"round5-reset-pending","email":"reset-pending@example.com","role":"operator"}`,
		env.cookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d body = %s", rec.Code, rec.Body.String())
	}
	var created store.User
	decodeBody(t, rec, &created)
	inviteToken := d.token

	rec = doAs(env, http.MethodPost, "/api/users/"+created.ID+"/password", "", env.cookie, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("reset pending = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != codeInvitePending {
		t.Fatalf("code = %q, want %q", code, codeInvitePending)
	}
	// 没有后续投递：最近一次仍是 invite。
	if d.kind != store.UserTokenInvite {
		t.Fatalf("reset token was issued despite pending invite: kind=%q", d.kind)
	}

	// 邀请链接仍可用。
	body := fmt.Sprintf(`{"token":%q,"new_password":"round5-reset-pw"}`, inviteToken)
	rec = doAs(env, http.MethodPost, "/api/auth/invite/accept", body, "", "198.51.100.11:1234")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("invite after rejected reset = %d body = %s", rec.Code, rec.Body.String())
	}
}

// 同名未激活账号：POST /api/users 重发邀请（不重复建行），旧链接立即失效。
func TestCreateUserReissuesInviteForUnactivatedAccount(t *testing.T) {
	env := newTestEnv(t)
	d := &captureDeliverer{}
	env.authSvc.SetDeliverer(d)
	payload := `{"username":"round5-reissue","email":"reissue@example.com","role":"operator","display_name":"Reissue"}`

	rec := doAs(env, http.MethodPost, "/api/users", payload, env.cookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d body = %s", rec.Code, rec.Body.String())
	}
	var first store.User
	decodeBody(t, rec, &first)
	oldToken := d.token

	// 同名重发：返回同一用户行，签发新 token。
	rec = doAs(env, http.MethodPost, "/api/users", payload, env.cookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("reissue = %d body = %s, want 201", rec.Code, rec.Body.String())
	}
	var second store.User
	decodeBody(t, rec, &second)
	if second.ID != first.ID {
		t.Fatalf("reissue created a new user: %q != %q", second.ID, first.ID)
	}
	if d.token == oldToken {
		t.Fatal("reissue did not deliver a new invite token")
	}
	newToken := d.token

	// 旧链接立即失效。
	oldBody := fmt.Sprintf(`{"token":%q,"new_password":"round5-old-pw"}`, oldToken)
	rec = doAs(env, http.MethodPost, "/api/auth/invite/accept", oldBody, "", "198.51.100.12:1234")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("old invite after reissue = %d body = %s, want 400", rec.Code, rec.Body.String())
	}
	// 新链接可用。
	newBody := fmt.Sprintf(`{"token":%q,"new_password":"round5-new-pw"}`, newToken)
	rec = doAs(env, http.MethodPost, "/api/auth/invite/accept", newBody, "", "198.51.100.12:1234")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("new invite after reissue = %d body = %s", rec.Code, rec.Body.String())
	}

	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	if !auditHas(env, "user.invite_resent") {
		t.Fatal("audit missing user.invite_resent")
	}
}

// 邀请已过期：POST /api/users 同名重发仍可用（此前 409 卡死）。
func TestCreateUserReissuesExpiredInvite(t *testing.T) {
	env := newTestEnv(t)
	d := &captureDeliverer{}
	env.authSvc.SetDeliverer(d)
	payload := `{"username":"round5-expired","email":"expired@example.com","role":"operator"}`

	rec := doAs(env, http.MethodPost, "/api/users", payload, env.cookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d body = %s", rec.Code, rec.Body.String())
	}
	oldToken := d.token

	// 把唯一 invite 置为过期。
	tokenRow, err := env.st.Users().TokenByHash(t.Context(), auth.HashToken(oldToken))
	if err != nil {
		t.Fatalf("TokenByHash: %v", err)
	}
	env.st.mu.Lock()
	tok := env.st.tokens[tokenRow.ID]
	tok.ExpiresAt = time.Now().Add(-time.Minute)
	env.st.tokens[tokenRow.ID] = tok
	env.st.mu.Unlock()
	if ok, err := env.st.Users().UnusedInviteToken(t.Context(), "round5-expired"); err != nil || !ok {
		t.Fatalf("UnusedInviteToken(expired) = %v, %v; want true", ok, err)
	}

	rec = doAs(env, http.MethodPost, "/api/users", payload, env.cookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("reissue expired = %d body = %s, want 201", rec.Code, rec.Body.String())
	}
	if d.token == oldToken {
		t.Fatal("expired invite was not replaced")
	}

	newBody := fmt.Sprintf(`{"token":%q,"new_password":"round5-expired-pw"}`, d.token)
	rec = doAs(env, http.MethodPost, "/api/auth/invite/accept", newBody, "", "198.51.100.13:1234")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("accept reissued invite = %d body = %s", rec.Code, rec.Body.String())
	}
}
