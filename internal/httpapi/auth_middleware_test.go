package httpapi

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/auth"
	"aiops/internal/config"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// 登录 / 登出 / me
// ---------------------------------------------------------------------------

func TestLoginLogoutMeFlow(t *testing.T) {
	env := newTestEnv(t)

	// me：带会话。
	rec := env.do(http.MethodGet, "/api/auth/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("me status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var me struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	decodeBody(t, rec, &me)
	if me.User.Username != "admin" {
		t.Fatalf("me user = %+v", me)
	}

	// 登出：清除 cookie + 204。
	rec = env.do(http.MethodPost, "/api/auth/logout", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, body = %s", rec.Code, rec.Body.String())
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, sessionCookieName+"=") || !strings.Contains(setCookie, "Max-Age=0") {
		t.Fatalf("logout Set-Cookie = %q", setCookie)
	}

	// 登出后再 me（cookie 仍传旧值）→ 401。
	rec = env.do(http.MethodGet, "/api/auth/me", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout status = %d", rec.Code)
	}
	code, _ := errorBody(t, rec)
	if code != "AUTH_REQUIRED" {
		t.Fatalf("code = %s", code)
	}
}

func TestLoginValidationAndFailures(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name   string
		body   string
		ct     string
		status int
		code   string
	}{
		{"empty password", `{"password":""}`, "application/json", http.StatusBadRequest, "VALIDATION_FAILED"},
		{"password too long", `{"password":"` + strings.Repeat("x", 257) + `"}`, "application/json", http.StatusBadRequest, "VALIDATION_FAILED"},
		{"malformed json", `{`, "application/json", http.StatusBadRequest, "VALIDATION_FAILED"},
		{"unknown field", `{"password":"x","extra":1}`, "application/json", http.StatusBadRequest, "VALIDATION_FAILED"},
		{"wrong content type", `{"password":"x"}`, "text/plain", http.StatusUnsupportedMediaType, "VALIDATION_FAILED"},
		{"wrong password", `{"password":"wrong-password"}`, "application/json", http.StatusUnauthorized, "AUTH_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.ct)
			req.Header.Set("Origin", testOrigin)
			rec := httptest.NewRecorder()
			env.router.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			code, _ := errorBody(t, rec)
			if code != tc.code {
				t.Fatalf("code = %s, want %s", code, tc.code)
			}
		})
	}
}

func TestLoginRateLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := newMemStore()
	st.users["admin"] = store.User{
		ID: "user-1", Username: "admin", PasswordHash: getAdminHash(),
		CreatedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
	}
	authSvc := auth.NewService(st, auth.Config{SessionTTL: time.Hour})
	limiter := auth.NewLoginLimiter(1, time.Hour, time.Now)
	router := NewRouter(&Deps{
		Config:  &config.Config{AllowedOrigins: []string{testOrigin}},
		Auth:    authSvc,
		Limiter: limiter,
		Clock:   time.Now,
	})

	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
			strings.NewReader(`{"password":"whatever"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", testOrigin)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	first := do()
	if first.Code == http.StatusTooManyRequests {
		t.Fatal("first attempt must not be rate limited")
	}
	second := do()
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	if retry := second.Header().Get("Retry-After"); retry == "" {
		t.Fatal("Retry-After header missing")
	}
	code, _ := errorBody(t, second)
	if code != "AUTH_RATE_LIMITED" {
		t.Fatalf("code = %s", code)
	}
}

func TestRequireAuthBranches(t *testing.T) {
	env := newTestEnv(t)

	// 无 cookie → 401。
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie status = %d", rec.Code)
	}

	// 伪造 cookie（找不到会话）→ 401。
	req = httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "forged-token"})
	rec = httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie status = %d", rec.Code)
	}
	code, _ := errorBody(t, rec)
	if code != "AUTH_REQUIRED" {
		t.Fatalf("code = %s", code)
	}

	// 会话查询存储错误 → 500。
	env.st.errSessionsByHash = errors.New("session backend down")
	req = httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "any-token"})
	rec = httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("store error status = %d, body = %s", rec.Code, rec.Body.String())
	}
	env.st.errSessionsByHash = nil
}

// ---------------------------------------------------------------------------
// 中间件
// ---------------------------------------------------------------------------

func TestOriginCheckBlocksDisallowedOrigins(t *testing.T) {
	env := newTestEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"password":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("evil origin status = %d", rec.Code)
	}
	code, _ := errorBody(t, rec)
	if code != "ORIGIN_FORBIDDEN" {
		t.Fatalf("code = %s", code)
	}

	// 读请求不校验 Origin。
	req = httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Origin", "http://evil.example")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: env.cookie})
	rec = httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET with evil origin status = %d", rec.Code)
	}
}

func TestBodyLimitRejectsOversizedJSON(t *testing.T) {
	env := newTestEnv(t)
	big := `{"title":"` + strings.Repeat("a", 1<<20) + `"}`
	rec := env.do(http.MethodPost, "/api/conversations", big)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	code, _ := errorBody(t, rec)
	if code != "VALIDATION_FAILED" {
		t.Fatalf("code = %s", code)
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var logs bytes.Buffer
	d := &Deps{Log: newJSONLogger(&logs)}
	r := gin.New()
	r.Use(d.recovery())
	r.GET("/boom", func(c *gin.Context) { panic("kaboom") })
	r.GET("/boom-after-write", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"partial": true})
		panic("late panic")
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	code, _ := errorBody(t, rec)
	if code != "INTERNAL" {
		t.Fatalf("code = %s", code)
	}
	if !strings.Contains(logs.String(), "panic_recovered") {
		t.Fatalf("panic log missing: %s", logs.String())
	}

	// 已写入响应后 panic：只中止，不重复写。
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom-after-write", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "partial") {
		t.Fatalf("late panic: status = %d body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "INTERNAL") {
		t.Fatalf("must not overwrite started response: %s", rec.Body.String())
	}
}

func TestRequestIDMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	d := &Deps{}
	r := gin.New()
	r.Use(d.requestID())
	var got string
	r.GET("/echo", func(c *gin.Context) { got = c.GetString("request_id") })

	// 合法请求 ID 原样透传。
	req := httptest.NewRequest(http.MethodGet, "/echo", nil)
	req.Header.Set("X-Request-ID", "Client-Req.42_x")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if got != "Client-Req.42_x" || rec.Header().Get("X-Request-ID") != "Client-Req.42_x" {
		t.Fatalf("request id = %q / header %q", got, rec.Header().Get("X-Request-ID"))
	}

	// 非法请求 ID 生成 16 位十六进制。
	req = httptest.NewRequest(http.MethodGet, "/echo", nil)
	req.Header.Set("X-Request-ID", "bad id!")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if len(got) != 16 {
		t.Fatalf("generated id = %q", got)
	}
	for _, c := range got {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("generated id = %q", got)
		}
	}
}

func TestSecurityHeadersPresent(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodGet, "/api/auth/me", "")
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 健康检查
// ---------------------------------------------------------------------------

func TestHandleHealthStatuses(t *testing.T) {
	env := newTestEnv(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var health struct {
		Status   string `json:"status"`
		Version  string `json:"version"`
		Database string `json:"database"`
	}
	decodeBody(t, rec, &health)
	if health.Status != "ok" || health.Database != "ok" || health.Version != "test" {
		t.Fatalf("health = %+v", health)
	}

	// ping 失败 → 503 degraded。
	env.st.pingErr = errors.New("unreachable")
	rec = httptest.NewRecorder()
	env.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("degraded status = %d", rec.Code)
	}
	decodeBody(t, rec, &health)
	if health.Status != "degraded" || health.Database != "error" {
		t.Fatalf("degraded health = %+v", health)
	}
	env.st.pingErr = nil

	// Store 为 nil → 503（骨架调试路径）。
	gin.SetMode(gin.TestMode)
	rec = httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	(&Deps{Clock: time.Now, Version: "dev"}).handleHealth(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil store status = %d", rec.Code)
	}
}

func newJSONLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}
