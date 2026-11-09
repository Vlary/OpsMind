package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiops/internal/store"
)

// modelsUpstream 起一个返回 OpenAI 风格 /models 的假上游。
func modelsUpstream(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"data":[{"id":"m-b"},{"id":"m-a"},{"id":"m-a"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func createProvider(t *testing.T, env *testEnv, name, baseURL string) store.Provider {
	t.Helper()
	body := `{"name":"` + name + `","preset":"custom","kind":"openai_compatible","base_url":"` + baseURL +
		`","default_model":"m-1","models":["m-1"],"api_key":"sk-secret-0000"}`
	rec := env.do(http.MethodPost, "/api/providers", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create provider status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var p store.Provider
	decodeBody(t, rec, &p)
	return p
}

func TestProviderPresetsAndList(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(http.MethodGet, "/api/providers/presets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("presets = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"preset":"openai"`) || !strings.Contains(rec.Body.String(), `"api_key_required":true`) {
		t.Fatalf("presets body = %s", rec.Body.String())
	}

	rec = env.do(http.MethodGet, "/api/providers", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	p := createProvider(t, env, "primary", "http://127.0.0.1:9/v1")
	if p.APIKeyMasked != "****0000" || !p.HasAPIKey {
		t.Fatalf("provider = %+v", p)
	}
	// 列表也要脱敏，且密文不出现。
	rec = env.do(http.MethodGet, "/api/providers", "")
	if !strings.Contains(rec.Body.String(), `"api_key_masked":"****0000"`) {
		t.Fatalf("masked missing: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-secret-0000") {
		t.Fatal("api key plaintext leaked")
	}

	rec = env.do(http.MethodGet, "/api/providers/"+p.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
	rec = env.do(http.MethodGet, "/api/providers/ghost", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing = %d", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "PROVIDER_NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}
}

func TestCreateProviderValidation(t *testing.T) {
	env := newTestEnv(t)
	valid := `{"name":"n","preset":"custom","kind":"openai_compatible","base_url":"http://x/v1","default_model":"m"}`

	cases := []struct {
		name string
		body string
		msg  string
	}{
		{"empty name", `{"name":"","preset":"custom","kind":"openai_compatible","base_url":"http://x/v1","default_model":"m"}`, "name must be"},
		{"name too long", `{"name":"` + strings.Repeat("名", 101) + `","preset":"custom","kind":"openai_compatible","base_url":"http://x/v1","default_model":"m"}`, "name must be"},
		{"unknown preset", `{"name":"n","preset":"nope","kind":"openai_compatible","base_url":"http://x/v1","default_model":"m"}`, "unknown preset"},
		{"preset kind mismatch", `{"name":"n","preset":"custom","kind":"anthropic","base_url":"http://x/v1","default_model":"m"}`, "preset and kind do not match"},
		{"unknown kind", `{"name":"n","preset":"custom","kind":"zzz","base_url":"http://x/v1","default_model":"m"}`, "preset and kind do not match"},
		{"relative base_url", `{"name":"n","preset":"custom","kind":"openai_compatible","base_url":"/v1","default_model":"m"}`, "base_url must be"},
		{"ftp base_url", `{"name":"n","preset":"custom","kind":"openai_compatible","base_url":"ftp://x/v1","default_model":"m"}`, "base_url must be"},
		{"base_url with query", `{"name":"n","preset":"custom","kind":"openai_compatible","base_url":"http://x/v1?q=1","default_model":"m"}`, "base_url must be"},
		{"base_url with user", `{"name":"n","preset":"custom","kind":"openai_compatible","base_url":"http://u:p@x/v1","default_model":"m"}`, "base_url must be"},
		{"empty default_model", `{"name":"n","preset":"custom","kind":"openai_compatible","base_url":"http://x/v1","default_model":" "}`, "default_model must be"},
		{"default_model too long", `{"name":"n","preset":"custom","kind":"openai_compatible","base_url":"http://x/v1","default_model":"` + strings.Repeat("m", 256) + `"}`, "default_model must be"},
		{"empty model entry", `{"name":"n","preset":"custom","kind":"openai_compatible","base_url":"http://x/v1","default_model":"m","models":["  "]}`, "models are invalid"},
		{"model too long", `{"name":"n","preset":"custom","kind":"openai_compatible","base_url":"http://x/v1","default_model":"m","models":["` + strings.Repeat("m", 256) + `"]}`, "models are invalid"},
		{"too many models", `{"name":"n","preset":"custom","kind":"openai_compatible","base_url":"http://x/v1","default_model":"m","models":[` + strings.Join(modelList(501), ",") + `]}`, "models are invalid"},
		{"api key required preset", `{"name":"n","preset":"openai","kind":"openai_compatible","base_url":"http://x/v1","default_model":"m"}`, "api_key is required"},
		{"api key too long", `{"name":"n","preset":"custom","kind":"openai_compatible","base_url":"http://x/v1","default_model":"m","api_key":"` + strings.Repeat("k", 8193) + `"}`, "api_key is too long"},
		{"unknown field", `{"name":"n","preset":"custom","kind":"openai_compatible","base_url":"http://x/v1","default_model":"m","zzz":1}`, "invalid request body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodPost, "/api/providers", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			_, msg := errorBody(t, rec)
			if !strings.Contains(msg, tc.msg) {
				t.Fatalf("message = %q, want contains %q", msg, tc.msg)
			}
		})
	}
	_ = valid

	// 错误 Content-Type → 415。
	req := httptest.NewRequest(http.MethodPost, "/api/providers", strings.NewReader(valid))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: env.cookie})
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type = %d", rec.Code)
	}

	// 重名 → 409。
	createProvider(t, env, "primary", "http://127.0.0.1:9/v1")
	rec = env.do(http.MethodPost, "/api/providers", `{"name":"primary","preset":"custom","kind":"openai_compatible","base_url":"http://y/v1","default_model":"m"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict = %d %s", rec.Code, rec.Body.String())
	}
	if _, msg := errorBody(t, rec); msg != "provider name already exists" {
		t.Fatalf("message = %q", msg)
	}

	// Store 错误 → 500。
	env.st.errProvidersCreate = errStoreFailure
	rec = env.do(http.MethodPost, "/api/providers", valid)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("store error = %d", rec.Code)
	}
	env.st.errProvidersCreate = nil

	// Store 缺失 → 503。
	c, rec2 := withParam(httptestPost("/api/providers", valid), "", "")
	c.Params = nil
	(&Deps{}).handleListProviders(c)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store = %d", rec2.Code)
	}

	// cipher 缺失 → 500（带 api_key 时加密失败）。
	d := &Deps{Store: env.st, Clock: time.Now}
	withKey := `{"name":"nc","preset":"custom","kind":"openai_compatible","base_url":"http://x/v1","default_model":"m","api_key":"sk-1"}`
	c, rec2 = withParam(httptestPost("/api/providers", withKey), "", "")
	c.Params = nil
	d.handleCreateProvider(c)
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("no cipher = %d body = %s", rec2.Code, rec2.Body.String())
	}
}

func modelList(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, `"model-`+itoa(i)+`"`)
	}
	return out
}

func TestUpdateProvider(t *testing.T) {
	env := newTestEnv(t)
	p := createProvider(t, env, "primary", "http://127.0.0.1:9/v1")

	// 空 body → 400。
	rec := env.do(http.MethodPatch, "/api/providers/"+p.ID, `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty update = %d", rec.Code)
	}
	// api_key + clear_api_key 互斥。
	rec = env.do(http.MethodPatch, "/api/providers/"+p.ID, `{"api_key":"k","clear_api_key":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mutex update = %d", rec.Code)
	}
	// 不存在 → 404。
	rec = env.do(http.MethodPatch, "/api/providers/ghost", `{"name":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing update = %d", rec.Code)
	}
	// 各字段校验。
	for _, tc := range []struct {
		name string
		body string
		msg  string
	}{
		{"empty name", `{"name":" "}`, "name must be"},
		{"unknown preset", `{"preset":"nope"}`, "unknown preset"},
		{"unknown kind", `{"kind":"zzz"}`, "unknown provider kind"},
		{"preset kind mismatch", `{"preset":"anthropic"}`, "preset and kind do not match"},
		{"bad base_url", `{"base_url":"http://"}`, "base_url must be"},
		{"empty default_model", `{"default_model":" "}`, "default_model must be"},
		{"bad models", `{"models":[""]}`, "models are invalid"},
		{"api key too long", `{"api_key":"` + strings.Repeat("k", 8193) + `"}`, "api_key is too long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodPatch, "/api/providers/"+p.ID, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			_, msg := errorBody(t, rec)
			if !strings.Contains(msg, tc.msg) {
				t.Fatalf("message = %q", msg)
			}
		})
	}

	// 正常更新：换名 + 换模型 + 换 key。
	rec = env.do(http.MethodPatch, "/api/providers/"+p.ID,
		`{"name":"backup","default_model":"m-2","models":["m-2","m-2","m-3"],"api_key":"sk-new-9999"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"api_key_masked":"****9999"`) {
		t.Fatalf("masked = %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"models":["m-2","m-3"]`) {
		t.Fatalf("models dedup = %s", rec.Body.String())
	}

	// 空 api_key 字符串 → 清除。
	rec = env.do(http.MethodPatch, "/api/providers/"+p.ID, `{"api_key":""}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"has_api_key":false`) {
		t.Fatalf("clear key = %s", rec.Body.String())
	}

	// 重新设置 key 后用 clear_api_key 清除。
	env.do(http.MethodPatch, "/api/providers/"+p.ID, `{"api_key":"sk-again-11"}`)
	rec = env.do(http.MethodPatch, "/api/providers/"+p.ID, `{"clear_api_key":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"has_api_key":false`) {
		t.Fatalf("clear flag = %s", rec.Body.String())
	}
}

func TestDeleteProvider(t *testing.T) {
	env := newTestEnv(t)
	p := createProvider(t, env, "primary", "http://127.0.0.1:9/v1")

	rec := env.do(http.MethodDelete, "/api/providers/"+p.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodDelete, "/api/providers/"+p.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("double delete = %d", rec.Code)
	}
}

func TestProviderTestAndRefreshModels(t *testing.T) {
	env := newTestEnv(t)
	upstream := modelsUpstream(t, http.StatusOK)
	p := createProvider(t, env, "primary", upstream.URL+"/v1")

	// 连通性测试。
	rec := env.do(http.MethodPost, "/api/providers/"+p.ID+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"model_count":2`) {
		t.Fatalf("test body = %s", rec.Body.String())
	}

	// 刷新模型列表（排序去重）。
	rec = env.do(http.MethodPost, "/api/providers/"+p.ID+"/models/refresh", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"models":["m-a","m-b"]`) {
		t.Fatalf("refresh body = %s", rec.Body.String())
	}
	fresh, err := env.st.Providers().Get(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Models) != 2 || fresh.Models[0] != "m-a" {
		t.Fatalf("stored models = %v", fresh.Models)
	}

	// 上游 500 → PROVIDER_TEST_FAILED / PROVIDER_MODELS_FAILED。
	broken := createProvider(t, env, "broken", "http://127.0.0.1:1/v1") // 不可达端口
	rec = env.do(http.MethodPost, "/api/providers/"+broken.ID+"/test", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("broken test = %d %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "PROVIDER_TEST_FAILED" {
		t.Fatalf("code = %s", code)
	}
	rec = env.do(http.MethodPost, "/api/providers/"+broken.ID+"/models/refresh", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("broken refresh = %d", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "PROVIDER_MODELS_FAILED" {
		t.Fatalf("code = %s", code)
	}

	// 不存在 → PROVIDER_NOT_FOUND。
	rec = env.do(http.MethodPost, "/api/providers/ghost/test", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ghost test = %d", rec.Code)
	}
	rec = env.do(http.MethodPost, "/api/providers/ghost/models/refresh", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ghost refresh = %d", rec.Code)
	}

	// LLM 未装配 → 500。
	c, rec2 := withParam(httptestPost("/api/providers/x/test", ""), "id", p.ID)
	(&Deps{}).handleTestProvider(c)
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("nil llm test = %d", rec2.Code)
	}
	c, rec2 = withParam(httptestPost("/api/providers/x/models/refresh", ""), "id", p.ID)
	(&Deps{}).handleRefreshProviderModels(c)
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("nil llm refresh = %d", rec2.Code)
	}

	// refresh 且 Store 缺失 → 503。
	d := &Deps{LLM: env.llmReg, Clock: time.Now}
	c, rec2 = withParam(httptestPost("/api/providers/x/models/refresh", ""), "id", p.ID)
	d.handleRefreshProviderModels(c)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil store refresh = %d body = %s", rec2.Code, rec2.Body.String())
	}
}

// 冲刺 1 打磨：供应商测试/拉取模型失败的 message 必须透出底层原因
// （对齐 round 6 ssh/database 的 ": %v" 模式），否则运维无法区分
// 防火墙、地址错还是域名不存在。
func TestProviderTestSurfacesUnderlyingError(t *testing.T) {
	env := newTestEnv(t)
	broken := createProvider(t, env, "broken", "http://127.0.0.1:1/v1") // 不可达端口

	rec := env.do(http.MethodPost, "/api/providers/"+broken.ID+"/test", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("broken test = %d %s", rec.Code, rec.Body.String())
	}
	code, msg := errorBody(t, rec)
	if code != "PROVIDER_TEST_FAILED" {
		t.Fatalf("code = %s", code)
	}
	if !strings.Contains(msg, "provider request failed") {
		t.Errorf("message = %q, want prefix", msg)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("message = %q, want underlying dial error surfaced", msg)
	}

	rec = env.do(http.MethodPost, "/api/providers/"+broken.ID+"/models/refresh", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("broken refresh = %d %s", rec.Code, rec.Body.String())
	}
	code, msg = errorBody(t, rec)
	if code != "PROVIDER_MODELS_FAILED" {
		t.Fatalf("code = %s", code)
	}
	if !strings.Contains(msg, "provider request failed") {
		t.Errorf("message = %q, want prefix", msg)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("message = %q, want underlying dial error surfaced", msg)
	}
}
