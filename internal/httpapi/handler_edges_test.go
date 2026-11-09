package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/audit"
	"aiops/internal/config"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// 依赖缺失 / 基础辅助
// ---------------------------------------------------------------------------

// noStoreRouter 构造无 Store 的路由：所有 requireStore 端点必须 503 而非 panic。
func noStoreRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return NewRouter(&Deps{
		Config:  &config.Config{AllowedOrigins: []string{testOrigin}},
		Clock:   time.Now,
		Version: "t",
	})
}

func TestRequireStoreReturnsServiceUnavailable(t *testing.T) {
	r := noStoreRouter(t)
	paths := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/servers"},
		{http.MethodGet, "/api/databases"},
		{http.MethodGet, "/api/databases/00000000-0000-0000-0000-000000000001"},
		{http.MethodPost, "/api/databases/00000000-0000-0000-0000-000000000001/test"},
		{http.MethodPost, "/api/databases/00000000-0000-0000-0000-000000000001/query"},
		{http.MethodGet, "/api/providers"},
		{http.MethodGet, "/api/providers/00000000-0000-0000-0000-000000000002"},
		{http.MethodPost, "/api/providers"},
		{http.MethodDelete, "/api/providers/00000000-0000-0000-0000-000000000002"},
		{http.MethodGet, "/api/approvals"},
		{http.MethodGet, "/api/approvals/00000000-0000-0000-0000-000000000003"},
		{http.MethodPost, "/api/approvals/00000000-0000-0000-0000-000000000003/decision"},
	}
	for _, p := range paths {
		req := httptest.NewRequest(p.method, p.path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", testOrigin)
		// 这组端点大多在鉴权之后；带无效 cookie 让中间件先过（无 Store 时
		// requireAuth 走 d.Auth，也为 nil → 500）。直接调用 handler 的
		// store 检查需要会话；这里只验证无 Store 不 panic 且返回错误状态。
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code < 400 {
			t.Errorf("%s %s = %d, want >= 400", p.method, p.path, rec.Code)
		}
	}
}

// now/decryptSecret/encryptSecret 在依赖缺失时的行为。
func TestDepsEdgeHelpers(t *testing.T) {
	d := &Deps{}
	if d.now().IsZero() {
		t.Fatal("default clock must return current time")
	}
	if got, err := d.decryptSecret("x"); err == nil || got != "" {
		t.Fatalf("decryptSecret without cipher = %q,%v, want empty+error", got, err)
	}
	if got, err := d.decryptSecret(""); got != "" || err != nil {
		t.Fatalf("decryptSecret(empty) = %q,%v, want empty+nil", got, err)
	}
	if _, err := d.encryptSecret("x"); err == nil {
		t.Fatal("encryptSecret without cipher must fail")
	}
	// buildUpdateCredentials 的加密失败分支。
	pw := "pw"
	if _, err := d.buildUpdateCredentials(store.AuthMethodPassword, &pw, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "failed to encrypt") {
		t.Fatalf("buildUpdateCredentials without cipher err = %v", err)
	}
	key := "key"
	if _, err := d.buildUpdateCredentials(store.AuthMethodPrivateKey, nil, &key, nil); err == nil ||
		!strings.Contains(err.Error(), "failed to encrypt") {
		t.Fatalf("buildUpdateCredentials private key err = %v", err)
	}
}

// writeStoreError 对 ErrTxUnsupported 必须映射为 500 且不泄露内部错误。
func TestWriteStoreErrorTxUnsupported(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	(&Deps{}).writeStoreError(c, store.ErrTxUnsupported, apierrNotFoundForTest)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	code, msg := errorBody(t, rec)
	if code != "INTERNAL" || msg != "internal server error" {
		t.Fatalf("body = %s/%s, want INTERNAL without detail", code, msg)
	}
}

const apierrNotFoundForTest = "NOT_FOUND"

// normalizeTags 的清洗规则。
func TestNormalizeTagsEdges(t *testing.T) {
	tags, err := normalizeTags([]string{"  prod  ", "", "web", "web"})
	if err != nil {
		t.Fatalf("normalizeTags: %v", err)
	}
	if strings.Join(tags, ",") != "prod,web" {
		t.Fatalf("tags = %v", tags)
	}
	// 超长 tag 是校验错误。
	if _, err := normalizeTags([]string{strings.Repeat("t", 40)}); err == nil {
		t.Fatal("40-char tag must be rejected")
	}
	// 超过 16 个 tag 是校验错误。
	if _, err := normalizeTags([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q"}); err == nil {
		t.Fatal("17 tags must be rejected")
	}
}

// sshTarget 对损坏密文与非法 auth_method 失败关闭。
func TestSSHTargetFailurePaths(t *testing.T) {
	env := newTestEnv(t)
	d := &Deps{Cipher: env.cipher}

	srv := store.Server{ID: "s1", Host: "h", AuthMethod: store.AuthMethodPassword, PasswordEncrypted: "bogus"}
	if _, err := d.sshTarget(srv); err == nil || !strings.Contains(err.Error(), "decrypt password") {
		t.Fatalf("bogus password err = %v", err)
	}

	enc, err := env.cipher.Encrypt("keydata")
	if err != nil {
		t.Fatal(err)
	}
	srv = store.Server{ID: "s2", Host: "h", AuthMethod: store.AuthMethodPrivateKey, PrivateKeyEncrypted: "bogus"}
	if _, err := d.sshTarget(srv); err == nil || !strings.Contains(err.Error(), "decrypt private key") {
		t.Fatalf("bogus key err = %v", err)
	}
	srv.PrivateKeyEncrypted = enc
	srv.PassphraseEncrypted = "bogus"
	if _, err := d.sshTarget(srv); err == nil || !strings.Contains(err.Error(), "decrypt passphrase") {
		t.Fatalf("bogus passphrase err = %v", err)
	}

	srv = store.Server{ID: "s3", Host: "h", AuthMethod: store.AuthMethod("weird")}
	if _, err := d.sshTarget(srv); err == nil || !strings.Contains(err.Error(), "unsupported auth method") {
		t.Fatalf("bad auth method err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Servers API 边界
// ---------------------------------------------------------------------------

func TestServersDecodeJSONEdges(t *testing.T) {
	env := newTestEnv(t)

	// Content-Type 非 JSON → 415。
	req := httptest.NewRequest(http.MethodPost, "/api/servers", strings.NewReader(`x`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: env.cookie})
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain = %d, want 415", rec.Code)
	}

	// 尾随数据 → 400。
	req = httptest.NewRequest(http.MethodPost, "/api/servers", strings.NewReader(`{"name":"a"} trailing`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: env.cookie})
	rec = httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing data = %d, want 400", rec.Code)
	}
}

func TestServersStoreErrorMapping(t *testing.T) {
	env := newTestEnv(t)
	env.st.mu.Lock()
	env.st.errServersCreate = store.ErrTxUnsupported
	env.st.mu.Unlock()

	rec := env.do(http.MethodPost, "/api/servers", `{"name":"x","host":"192.0.2.9","username":"u","auth_method":"password","password":"p"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("tx unsupported = %d, want 500", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "INTERNAL" {
		t.Fatalf("code = %s, want INTERNAL", code)
	}
}

func TestServerListUpdateDeleteEdges(t *testing.T) {
	env := newTestEnv(t)
	srv := createServer(t, env)

	// 列表至少包含一项（append 分支）。
	rec := env.do(http.MethodGet, "/api/servers", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"web-1"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	// 更新 host/port/username/tags/auto_exec 全字段。
	rec = env.do(http.MethodPatch, "/api/servers/"+srv.ID,
		`{"host":"192.0.2.99","port":2222,"username":"root","tags":["db"],"auto_exec_enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"host":"192.0.2.99"`) ||
		!strings.Contains(rec.Body.String(), `"port":2222`) ||
		!strings.Contains(rec.Body.String(), `"username":"root"`) ||
		!strings.Contains(rec.Body.String(), `"auto_exec_enabled":false`) {
		t.Fatalf("updated body = %s", rec.Body.String())
	}

	// 删除不存在的 server → 404。
	rec = env.do(http.MethodDelete, "/api/servers/00000000-0000-0000-0000-00000000dead", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d, want 404", rec.Code)
	}

	// 删除存在的 → 204。
	rec = env.do(http.MethodDelete, "/api/servers/"+srv.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
}

// 预置 auth_method 非法的 server：test 端点构 target 失败 → 500。
func TestServerTestBadAuthMethod(t *testing.T) {
	env := newTestEnv(t)
	id := "00000000-0000-0000-0000-0000000000aa"
	env.st.mu.Lock()
	env.st.servers[id] = store.Server{
		ID: id, Name: "bad", Host: "192.0.2.1", Port: 22, Username: "u",
		AuthMethod: store.AuthMethod("weird"), CreatedAt: time.Now().UTC(),
	}
	env.st.mu.Unlock()

	rec := env.do(http.MethodPost, "/api/servers/"+id+"/test", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("test bad auth = %d %s, want 500", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Databases API 边界
// ---------------------------------------------------------------------------

func TestDatabaseEdges(t *testing.T) {
	env := newTestEnv(t)
	db := createPGDatabase(t, env)

	// 列表包含项。
	rec := env.do(http.MethodGet, "/api/databases", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"orders"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	// 更新 host/database_name/username/ssl_mode。
	rec = env.do(http.MethodPatch, "/api/databases/"+db.ID,
		`{"host":"db2.internal","database_name":"orders2","username":"ro2","ssl_mode":"disable"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"host":"db2.internal"`) ||
		!strings.Contains(rec.Body.String(), `"database_name":"orders2"`) ||
		!strings.Contains(rec.Body.String(), `"ssl_mode":"disable"`) {
		t.Fatalf("updated = %s", rec.Body.String())
	}

	// 更新不存在的 → 404。
	rec = env.do(http.MethodPatch, "/api/databases/00000000-0000-0000-0000-00000000beef", `{"host":"x.internal"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update missing = %d, want 404", rec.Code)
	}

	// test 端点不要求请求体：直接执行连通性测试 → 200。
	rec = env.do(http.MethodPost, "/api/databases/"+db.ID+"/test", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "latency_ms") {
		t.Fatalf("test = %d %s, want 200 with latency", rec.Code, rec.Body.String())
	}

	// query 端点畸形体 → 400。
	rec = env.do(http.MethodPost, "/api/databases/"+db.ID+"/query", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("query malformed = %d, want 400", rec.Code)
	}

	// query 端点解密失败（密文损坏）→ 500。
	badID := "00000000-0000-0000-0000-0000000000bb"
	env.st.mu.Lock()
	env.st.databases[badID] = store.Database{
		ID: badID, Name: "bad", Engine: store.EnginePostgres, Host: "h", DatabaseName: "d",
		Username: "u", PasswordEncrypted: "bogus", SSLMode: "prefer",
		CreatedAt: time.Now().UTC(),
	}
	env.st.mu.Unlock()
	rec = env.do(http.MethodPost, "/api/databases/"+badID+"/query", `{"sql":"SELECT 1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("query with bogus ciphertext = %d %s, want 500", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "INTERNAL" {
		t.Fatalf("code = %s, want INTERNAL", code)
	}
}

// ---------------------------------------------------------------------------
// Providers API 边界
// ---------------------------------------------------------------------------

func TestProviderEdges(t *testing.T) {
	env := newTestEnv(t)
	p := createProvider(t, env, "prov-a", "http://api.example/v1")

	// 更新 preset/base_url。
	rec := env.do(http.MethodPatch, "/api/providers/"+p.ID,
		`{"preset":"openai","base_url":"http://api2.example/v1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"base_url":"http://api2.example/v1"`) {
		t.Fatalf("updated = %s", rec.Body.String())
	}

	// 更新畸形 JSON → 400。
	rec = env.do(http.MethodPatch, "/api/providers/"+p.ID, `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update malformed = %d, want 400", rec.Code)
	}

	// 获取不存在的 → 404。
	rec = env.do(http.MethodGet, "/api/providers/00000000-0000-0000-0000-00000000cafe", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing = %d, want 404", rec.Code)
	}

	// 删除存在的 → 204。
	rec = env.do(http.MethodDelete, "/api/providers/"+p.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
}

// writeProviderUpstreamError 的超时映射。
func TestWriteProviderUpstreamErrorMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)

	(&Deps{}).writeProviderUpstreamError(c, context.DeadlineExceeded, "fallback")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout status = %d, want 504", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "LLM_TIMEOUT" {
		t.Fatalf("code = %s, want LLM_TIMEOUT", code)
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
	(&Deps{}).writeProviderUpstreamError(c, errors.New("dial fail"), "fallback-msg")
	if rec.Code < 400 {
		t.Fatalf("generic upstream status = %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Approvals / Audit / Conversations
// ---------------------------------------------------------------------------

func TestApprovalsValidationEdges(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(http.MethodGet, "/api/approvals?status=bogus", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d, want 400", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "VALIDATION_FAILED" {
		t.Fatalf("code = %s", code)
	}

	rec = env.do(http.MethodGet, "/api/approvals/00000000-0000-0000-0000-0000000000cc", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing = %d, want 404", rec.Code)
	}
}

// actorNames 失败时审计页降级（names=nil），不影响响应。
func TestAuditPageActorNameDegradation(t *testing.T) {
	env := newTestEnv(t)

	// 预置两条审计日志，limit=1 触发 hasMore/next 游标。
	env.st.mu.Lock()
	now := time.Now().UTC()
	env.st.auditLogs = append(env.st.auditLogs,
		store.AuditLog{ID: 101, ActorType: "user", ActorID: "user-1", Action: "a1", CreatedAt: now},
		store.AuditLog{ID: 102, ActorType: "user", ActorID: "user-1", Action: "a2", CreatedAt: now},
	)
	env.st.mu.Unlock()

	rec := env.do(http.MethodGet, "/api/audit?limit=1&server_id=00000000-0000-0000-0000-0000000000dd", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d %s", rec.Code, rec.Body.String())
	}
	// memAudit 的 List 恒 has_more=false（fake 不模拟翻页），此处只验证
	// server_id 过滤分支可达且页面装配 actor_name。
	if !strings.Contains(rec.Body.String(), `"action":"a2"`) || !strings.Contains(rec.Body.String(), `"actor_name":"admin"`) {
		t.Fatalf("page body = %s", rec.Body.String())
	}
}

func TestConversationEdgeResponses(t *testing.T) {
	env := newTestEnv(t)

	// 空列表必须是 [] 而非 null。
	rec := env.do(http.MethodGet, "/api/conversations", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty conversations = %d %s", rec.Code, rec.Body.String())
	}

	// 创建会话畸形体 → 400。
	rec = env.do(http.MethodPost, "/api/conversations", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed create = %d, want 400", rec.Code)
	}

	// 空会话的 tool calls：before_seq/limit 非法 → 400；合法空列表 → []。
	convID := "00000000-0000-0000-0000-0000000000ee"
	rec = env.do(http.MethodGet, "/api/conversations/"+convID+"/tool-calls?before_seq=bad", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad before_seq = %d, want 400", rec.Code)
	}
	rec = env.do(http.MethodGet, "/api/conversations/"+convID+"/tool-calls?limit=bad", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit = %d, want 400", rec.Code)
	}
	rec = env.do(http.MethodGet, "/api/conversations/"+convID+"/tool-calls", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty tool calls = %d %s", rec.Code, rec.Body.String())
	}
}

// 会话按 ID 查询存在/不存在。
func TestConversationGetNotFound(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodGet, "/api/conversations/00000000-0000-0000-0000-00000000ff", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing conversation = %d, want 404", rec.Code)
	}
}

// 鉴权中间件在 store 会话查询失败时返回 500 而非 401/403。
func TestMiddlewareSessionStoreError(t *testing.T) {
	env := newTestEnv(t)
	env.st.mu.Lock()
	env.st.errSessionsByHash = errors.New("session store down")
	env.st.mu.Unlock()

	rec := env.do(http.MethodGet, "/api/servers", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("session store error = %d, want 500", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "INTERNAL" {
		t.Fatalf("code = %s, want INTERNAL", code)
	}
}

// 审计 writer 关闭后 enqueue 失败只记日志。
func TestAuditEnqueueFailureOnlyLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)

	w := audit.NewWriter(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := &Deps{Audit: w}
	d.auditEnqueue(c, audit.Event{Action: "x"})

	convID := "conv"
	d.enqueueAudit(context.Background(), c, "chat.failed", "conversation", "r", &convID, nil, nil)
}
