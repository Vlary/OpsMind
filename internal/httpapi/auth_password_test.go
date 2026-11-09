package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// flushAudit 冲刷审计 writer（审计是异步批量写的）。
func flushAudit(env *testEnv) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return env.auditW.Flush(ctx)
}

// auditHas 带锁检查是否已写入指定 action 的审计事件。
func auditHas(env *testEnv, action string) bool {
	env.st.mu.Lock()
	defer env.st.mu.Unlock()
	for _, l := range env.st.auditLogs {
		if l.Action == action {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// POST /api/auth/password（修改密码）
// ---------------------------------------------------------------------------

// rawLogin 不带会话 Cookie 直接登录，返回响应记录器。
// password 必须是 JSON 安全的 ASCII 串（当前用例均满足）。
func rawLogin(env *testEnv, password string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"`+password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec
}

func TestChangePasswordFlow(t *testing.T) {
	env := newTestEnv(t)

	// 成功：204 + 清除 Cookie + 吊销当前会话 + 新密码可登录。
	body := `{"old_password":"` + testPassword + `","new_password":"rotated-pass-1"}`
	rec := env.do(http.MethodPost, "/api/auth/password", body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("change status = %d, body = %s", rec.Code, rec.Body.String())
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, sessionCookieName+"=") || !strings.Contains(setCookie, "Max-Age=0") {
		t.Fatalf("change Set-Cookie = %q", setCookie)
	}

	// 旧会话 Cookie 已吊销 → 401。
	rec = env.do(http.MethodGet, "/api/auth/me", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after change status = %d", rec.Code)
	}

	// 旧密码登录失败，新密码登录成功。
	if got := rawLogin(env, testPassword); got.Code != http.StatusUnauthorized {
		t.Fatalf("old password accepted after change: %d", got.Code)
	}
	if got := rawLogin(env, "rotated-pass-1"); got.Code != http.StatusOK {
		t.Fatalf("new password login status = %d, body = %s", got.Code, got.Body.String())
	}

	// 审计包含成功事件（writer 异步批量写，先 Flush 再带锁读）。
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	if !auditHas(env, "auth.password_changed") {
		t.Fatalf("audit missing auth.password_changed: %+v", env.st.auditLogs)
	}
}

func TestChangePasswordFailures(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"wrong old password", `{"old_password":"nope","new_password":"x"}`, http.StatusUnauthorized, "AUTH_INVALID"},
		{"empty old password", `{"old_password":"","new_password":"x"}`, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"empty new password", `{"old_password":"` + testPassword + `","new_password":""}`, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"new password over bcrypt limit", `{"old_password":"` + testPassword + `","new_password":"` + strings.Repeat("x", 73) + `"}`, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"malformed json", `{`, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"unknown field", `{"old_password":"` + testPassword + `","new_password":"x","extra":1}`, http.StatusBadRequest, "VALIDATION_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodPost, "/api/auth/password", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			code, _ := errorBody(t, rec)
			if code != tc.code {
				t.Fatalf("code = %s, want %s", code, tc.code)
			}
		})
	}

	// 失败后当前会话必须仍然有效（未被吊销）。
	rec := env.do(http.MethodGet, "/api/auth/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("me after failed attempts status = %d", rec.Code)
	}

	// 错误旧密码写审计 auth.password_change_failed。
	if err := flushAudit(env); err != nil {
		t.Fatal(err)
	}
	if !auditHas(env, "auth.password_change_failed") {
		t.Fatalf("audit missing auth.password_change_failed: %+v", env.st.auditLogs)
	}
}

func TestChangePasswordRequiresAuth(t *testing.T) {
	env := newTestEnv(t)
	req := httptestPost("/api/auth/password", `{"old_password":"a","new_password":"b"}`)
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie status = %d", rec.Code)
	}
	code, _ := errorBody(t, rec)
	if code != "AUTH_REQUIRED" {
		t.Fatalf("code = %s", code)
	}
}
