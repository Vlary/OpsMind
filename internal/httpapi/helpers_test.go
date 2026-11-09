package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/config"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// 查询参数解析
// ---------------------------------------------------------------------------

func queryContext(t *testing.T, rawQuery string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/x?"+rawQuery, nil)
	return c, rec
}

func TestQueryLimitParsing(t *testing.T) {
	cases := []struct {
		raw    string
		want   int
		wantOK bool
	}{
		{"", 50, true},
		{"limit=10", 10, true},
		{"limit=1", 1, true},
		{"limit=200", 200, true},
		{"limit=0", 0, false},
		{"limit=201", 0, false},
		{"limit=abc", 0, false},
		{"limit=-5", 0, false},
	}
	for _, tc := range cases {
		c, rec := queryContext(t, tc.raw)
		got, ok := queryLimit(c)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("queryLimit(%q) = %d,%v want %d,%v (rec=%d)", tc.raw, got, ok, tc.want, tc.wantOK, rec.Code)
		}
		if !tc.wantOK && rec.Code != http.StatusBadRequest {
			t.Errorf("queryLimit(%q) status = %d", tc.raw, rec.Code)
		}
	}
}

func TestQueryBeforeSeqAndIDParsing(t *testing.T) {
	// before_seq。
	for _, tc := range []struct {
		raw    string
		want   int64
		wantOK bool
	}{
		{"", 0, true},
		{"before_seq=42", 42, true},
		{"before_seq=0", 0, true},
		{"before_seq=-1", 0, false},
		{"before_seq=bad", 0, false},
	} {
		c, rec := queryContext(t, tc.raw)
		got, ok := queryBeforeSeq(c)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("beforeSeq(%q) = %d,%v", tc.raw, got, ok)
		}
		if !tc.wantOK && rec.Code != http.StatusBadRequest {
			t.Errorf("beforeSeq(%q) status = %d", tc.raw, rec.Code)
		}
	}
	// before_id。
	for _, tc := range []struct {
		raw    string
		want   int64
		wantOK bool
	}{
		{"", 0, true},
		{"before_id=7", 7, true},
		{"before_id=0", 0, false},
		{"before_id=x", 0, false},
	} {
		c, rec := queryContext(t, tc.raw)
		got, ok := queryBeforeID(c)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("beforeID(%q) = %d,%v", tc.raw, got, ok)
		}
		if !tc.wantOK && rec.Code != http.StatusBadRequest {
			t.Errorf("beforeID(%q) status = %d", tc.raw, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// 响应辅助
// ---------------------------------------------------------------------------

func TestHandleStoreErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"not found with custom code", store.ErrNotFound, http.StatusNotFound, apierr.ConversationNotFound},
		{"conflict", store.ErrConflict, http.StatusConflict, "CONFLICT"},
		{"invalid", store.ErrInvalid, http.StatusBadRequest, "VALIDATION_FAILED"},
		{"generic maps to internal", errors.New("boom"), http.StatusInternalServerError, "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
			notFoundCode := apierr.NotFound
			if tc.wantCode == apierr.ConversationNotFound {
				notFoundCode = apierr.ConversationNotFound
			}
			(&Deps{}).handleStoreError(c, tc.err, notFoundCode)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%v)", rec.Code, tc.wantStatus, tc.err)
			}
			if code, _ := errorBody(t, rec); code != tc.wantCode {
				t.Fatalf("code = %s, want %s", code, tc.wantCode)
			}
		})
	}
}

func TestBindJSONStrictRejectsTrailingData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptestPost("/x", `{"title":"a"}{"title":"b"}`)
	var dst struct {
		Title string `json:"title"`
	}
	if bindJSONStrict(c, &dst) {
		t.Fatal("trailing object must be rejected")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}

	// 正常解析。
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptestPost("/x", `{"title":"a"}`)
	if !bindJSONStrict(c, &dst) || dst.Title != "a" {
		t.Fatalf("bind = %q ok-status = %d", dst.Title, rec.Code)
	}
}

func TestBindJSONUsesGinBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptestPost("/x", `{"title":"a"}`)
	var dst struct {
		Title string `json:"title"`
	}
	if !bindJSON(c, &dst) || dst.Title != "a" {
		t.Fatalf("bindJSON = %q", dst.Title)
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptestPost("/x", `{"title":42}`)
	if bindJSON(c, &dst) {
		t.Fatal("type mismatch must fail")
	}
	if code, _ := errorBody(t, rec); code != "VALIDATION_FAILED" {
		t.Fatalf("code = %s", code)
	}
}

func TestUserFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	if _, ok := userFromContext(c); ok {
		t.Fatal("empty context must not yield user")
	}
	c.Set("user", "not-a-user")
	if _, ok := userFromContext(c); ok {
		t.Fatal("wrong type must not yield user")
	}
	c.Set("user", store.User{ID: "u1"})
	u, ok := userFromContext(c)
	if !ok || u.ID != "u1" {
		t.Fatalf("user = %+v ok = %v", u, ok)
	}
}

// ---------------------------------------------------------------------------
// providers 辅助函数
// ---------------------------------------------------------------------------

func TestProviderHelpersUnit(t *testing.T) {
	// parsePreset / parseKind。
	if v, ok := parsePreset(" openai "); !ok || v != store.PresetOpenAI {
		t.Fatalf("parsePreset = %v,%v", v, ok)
	}
	if _, ok := parsePreset("nope"); ok {
		t.Fatal("unknown preset must fail")
	}
	if v, ok := parseKind(" anthropic "); !ok || v != store.ProviderAnthropic {
		t.Fatalf("parseKind = %v,%v", v, ok)
	}
	if _, ok := parseKind("zzz"); ok {
		t.Fatal("unknown kind must fail")
	}

	// normalizeProviderBaseURL。
	if v, ok := normalizeProviderBaseURL(" http://x/v1/ "); !ok || v != "http://x/v1" {
		t.Fatalf("baseURL = %q,%v", v, ok)
	}
	for _, bad := range []string{"", "http://", "x/v1", "http://x/#f", strings.Repeat("h", 2049)} {
		if _, ok := normalizeProviderBaseURL(bad); ok {
			t.Fatalf("baseURL(%q) must fail", bad)
		}
	}

	// normalizeModels：去重；空项非法。
	models, ok := normalizeModels([]string{" a ", "a", "b"})
	if !ok || len(models) != 2 || models[0] != "a" {
		t.Fatalf("models = %v,%v", models, ok)
	}
	if _, ok := normalizeModels([]string{"a", ""}); ok {
		t.Fatal("empty model entry must fail")
	}
	big := make([]string, 501)
	if _, ok := normalizeModels(big); ok {
		t.Fatal("501 models must fail")
	}

	// lastRunes。
	if lastRunes("sk-1234", 4) != "1234" {
		t.Fatal("lastRunes(4) wrong")
	}
	if lastRunes("ab", 4) != "" {
		t.Fatal("short string must yield empty hint")
	}

	// maskProvider。
	p := store.Provider{HasAPIKey: true, APIKeyHint: "abcd"}
	if got := maskProvider(p).APIKeyMasked; got != "****abcd" {
		t.Fatalf("masked = %q", got)
	}
	p = store.Provider{HasAPIKey: true, APIKeyMasked: "kept"}
	if got := maskProvider(p).APIKeyMasked; got != "kept" {
		t.Fatalf("existing mask kept = %q", got)
	}
	p = store.Provider{HasAPIKey: false, APIKeyMasked: "leak"}
	if got := maskProvider(p).APIKeyMasked; got != "" {
		t.Fatalf("no key must clear mask = %q", got)
	}
	p = store.Provider{}
	if got := maskProvider(p).Models; got == nil || len(got) != 0 {
		t.Fatalf("nil models must become []: %v", got)
	}

	// timeNow：注入时钟与默认时钟都返回 UTC 时间。
	fixed := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if got := timeNow(&Deps{Clock: func() time.Time { return fixed }}); !got.Equal(fixed) {
		t.Fatalf("clock time = %v", got)
	}
	if timeNow(&Deps{}).IsZero() {
		t.Fatal("default clock must be used")
	}
}

// ---------------------------------------------------------------------------
// 静态站点与 404
// ---------------------------------------------------------------------------

func TestMountStaticAndNoRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 未配置 WebDir：/ 与未知路径都是 404；/api/* 返回 JSON 错误体。
	bare := NewRouter(&Deps{Config: &config.Config{WebDir: ""}, Clock: time.Now, Version: "t"})
	rec := httptest.NewRecorder()
	bare.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("api 404 = %d", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}
	rec = httptest.NewRecorder()
	bare.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz/typo", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("healthz typo = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	bare.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no webdir root = %d", rec.Code)
	}

	// 配置 WebDir：SPA 与 assets。
	webDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<html>app</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	assets := filepath.Join(webDir, "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRouter(&Deps{Config: &config.Config{WebDir: webDir}, Clock: time.Now, Version: "t"})

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app") {
		t.Fatalf("index = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "console.log") {
		t.Fatalf("asset = %d", rec.Code)
	}
	// SPA 回退：非 /api、非 /assets 的未知路径回 index。
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/conversations/xyz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "app") {
		t.Fatalf("spa fallback = %d %s", rec.Code, rec.Body.String())
	}
	// /assets/ 前缀的未知文件不回退（file server 404）。
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("asset 404 = %d", rec.Code)
	}
	// /api 仍是 JSON 404。
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/zzz", nil))
	if code, _ := errorBody(t, rec); code != "NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}
}

func TestTrustedProxiesConfig(t *testing.T) {
	if got := trustedProxies(nil); got != nil {
		t.Fatalf("nil config = %v", got)
	}
	if got := trustedProxies(&config.Config{TrustProxy: false}); got != nil {
		t.Fatalf("disabled = %v", got)
	}
	got := trustedProxies(&config.Config{TrustProxy: true})
	if len(got) != 2 || got[0] != "127.0.0.1" || got[1] != "::1" {
		t.Fatalf("enabled = %v", got)
	}
}
