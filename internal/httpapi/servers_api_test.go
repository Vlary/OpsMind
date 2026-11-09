package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"aiops/internal/sshx"
	"aiops/internal/store"
)

func createServer(t *testing.T, env *testEnv) store.Server {
	t.Helper()
	body := `{"name":"web-1","host":"192.0.2.10","username":"deploy","auth_method":"password","password":"s3cret","tags":["prod","web"],"auto_exec_enabled":true}`
	rec := env.do(http.MethodPost, "/api/servers", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create server status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var srv store.Server
	decodeBody(t, rec, &srv)
	return srv
}

func TestServerCRUD(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(http.MethodGet, "/api/servers", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty list = %d %s", rec.Code, rec.Body.String())
	}

	srv := createServer(t, env)
	if srv.HasPassword != true || srv.Name != "web-1" || srv.Port != 22 || !srv.AutoExecEnabled {
		t.Fatalf("server = %+v", srv)
	}
	// 密文绝不回传（json:"-"，校验响应体没有泄漏字段名）。
	if strings.Contains(strings.ToLower(rec.Body.String()), "passwordencrypted") {
		t.Fatal("password ciphertext leaked")
	}

	// 详情：tags 归一化为 []。
	rec = env.do(http.MethodGet, "/api/servers/"+srv.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"tags":["prod","web"]`) {
		t.Fatalf("tags missing: %s", rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/api/servers/missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing = %d", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "SERVER_NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}

	// 更新：改名 + 换凭据。
	rec = env.do(http.MethodPatch, "/api/servers/"+srv.ID,
		`{"name":"web-2","auth_method":"private_key","private_key":"KEY DATA","passphrase":"pp"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"has_private_key":true`) {
		t.Fatalf("update body = %s", rec.Body.String())
	}
	if len(env.sshExec.invalidated) == 0 {
		t.Fatal("SSH cache must be invalidated on update")
	}

	// 删除。
	env.sshExec.invalidated = nil
	rec = env.do(http.MethodDelete, "/api/servers/"+srv.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if len(env.sshExec.invalidated) != 1 {
		t.Fatalf("invalidated = %v", env.sshExec.invalidated)
	}
	rec = env.do(http.MethodDelete, "/api/servers/"+srv.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("double delete = %d", rec.Code)
	}
}

func TestCreateServerValidation(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name string
		body string
		code string
	}{
		{"empty name", `{"name":"","host":"h","username":"u","auth_method":"password","password":"p"}`, "invalid name"},
		{"name too long", `{"name":"` + strings.Repeat("n", 101) + `","host":"h","username":"u","auth_method":"password","password":"p"}`, "invalid name"},
		{"host with space", `{"name":"n","host":"a b","username":"u","auth_method":"password","password":"p"}`, "invalid host"},
		{"host with scheme", `{"name":"n","host":"http://x","username":"u","auth_method":"password","password":"p"}`, "invalid host"},
		{"empty host", `{"name":"n","host":"","username":"u","auth_method":"password","password":"p"}`, "invalid host"},
		{"empty username", `{"name":"n","host":"h","username":"","auth_method":"password","password":"p"}`, "invalid username"},
		{"port zero", `{"name":"n","host":"h","username":"u","port":0,"auth_method":"password","password":"p"}`, "invalid port"},
		{"port too large", `{"name":"n","host":"h","username":"u","port":70000,"auth_method":"password","password":"p"}`, "invalid port"},
		{"too many tags", `{"name":"n","host":"h","username":"u","auth_method":"password","password":"p","tags":[` + strings.Join(tagList(17), ",") + `]}`, "too many tags"},
		{"tag too long", `{"name":"n","host":"h","username":"u","auth_method":"password","password":"p","tags":["` + strings.Repeat("t", 33) + `"]}`, "tag exceeds"},
		{"password auth missing password", `{"name":"n","host":"h","username":"u","auth_method":"password"}`, "password is required"},
		{"password auth with private key", `{"name":"n","host":"h","username":"u","auth_method":"password","password":"p","private_key":"k"}`, "private_key is not allowed"},
		{"key auth missing key", `{"name":"n","host":"h","username":"u","auth_method":"private_key"}`, "private_key is required"},
		{"key auth with password", `{"name":"n","host":"h","username":"u","auth_method":"private_key","private_key":"k","password":"p"}`, "password is not allowed"},
		{"unknown auth method", `{"name":"n","host":"h","username":"u","auth_method":"agent"}`, "auth_method is not allowed"},
		{"credential too large", `{"name":"n","host":"h","username":"u","auth_method":"password","password":"` + strings.Repeat("p", 4097) + `"}`, "credential is too large"},
		{"unknown field", `{"name":"n","host":"h","username":"u","auth_method":"password","password":"p","x":1}`, "invalid request body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodPost, "/api/servers", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			_, msg := errorBody(t, rec)
			if !strings.Contains(msg, tc.code) {
				t.Fatalf("message = %q, want contains %q", msg, tc.code)
			}
		})
	}

	// 密码加密失败（cipher 缺失）→ 400。
	ginStub := newServerDepsWithoutCipher(t, env)
	c, rec := withParam(httptestPost("/api/servers", `{"name":"n","host":"h","username":"u","auth_method":"password","password":"p"}`), "", "")
	c.Params = nil
	ginStub.handleCreateServer(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no-cipher status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 重名冲突 → 409 专用文案（p3 sprint 1：中文场景化）。
	createServer(t, env)
	rec = env.do(http.MethodPost, "/api/servers", `{"name":"web-1","host":"h2","username":"u","auth_method":"password","password":"p"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d", rec.Code)
	}
	if _, msg := errorBody(t, rec); msg != "服务器名称已存在" {
		t.Fatalf("message = %q", msg)
	}

	// store 通用错误 → 500。
	env.st.errServersCreate = errors.New("db down")
	rec = env.do(http.MethodPost, "/api/servers", `{"name":"n2","host":"h","username":"u","auth_method":"password","password":"p"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("store error status = %d", rec.Code)
	}
	env.st.errServersCreate = nil

	// Store 未装配 → 503。
	c, rec = withParam(httptestPost("/api/servers", `{}`), "", "")
	c.Params = nil
	(&Deps{}).handleListServers(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-store status = %d", rec.Code)
	}
}

// newServerDepsWithoutCipher 构造缺失 cipher 的 Deps（触发凭据加密失败分支）。
func newServerDepsWithoutCipher(t *testing.T, env *testEnv) *Deps {
	t.Helper()
	return &Deps{Store: env.st, Clock: time.Now}
}

func TestUpdateServerValidation(t *testing.T) {
	env := newTestEnv(t)
	srv := createServer(t, env)

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"no fields", `{}`, http.StatusBadRequest, "at least one field is required"},
		{"auth_method without credentials", `{"auth_method":"password"}`, http.StatusBadRequest, "credentials are required when auth_method is provided"},
		{"credentials without auth_method", `{"password":"newpw"}`, http.StatusBadRequest, "auth_method is required when credentials are provided"},
		{"invalid name", `{"name":"  "}`, http.StatusBadRequest, "invalid name"},
		{"invalid host", `{"host":"a b"}`, http.StatusBadRequest, "invalid host"},
		{"invalid port", `{"port":65536}`, http.StatusBadRequest, "invalid port"},
		{"invalid username", `{"username":""}`, http.StatusBadRequest, "invalid username"},
		{"invalid tags", `{"tags":["` + strings.Repeat("x", 33) + `"]}`, http.StatusBadRequest, "tag exceeds"},
		{"bad auth method", `{"auth_method":"agent","password":"p"}`, http.StatusBadRequest, "auth_method must be"},
		{"malformed body", `{`, http.StatusBadRequest, "invalid request body"},
		{"missing server", `{"name":"x"}`, http.StatusNotFound, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/servers/" + srv.ID
			if tc.name == "missing server" {
				path = "/api/servers/ghost"
			}
			rec := env.do(http.MethodPatch, path, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			_, msg := errorBody(t, rec)
			if !strings.Contains(msg, tc.code) {
				t.Fatalf("message = %q, want contains %q", msg, tc.code)
			}
		})
	}

	// 重名冲突 → 409。
	createServerBody := `{"name":"second","host":"h","username":"u","auth_method":"password","password":"p"}`
	if rec := env.do(http.MethodPost, "/api/servers", createServerBody); rec.Code != http.StatusCreated {
		t.Fatalf("second server = %d %s", rec.Code, rec.Body.String())
	}
	rec := env.do(http.MethodPatch, "/api/servers/"+srv.ID, `{"name":"second"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("rename conflict = %d %s", rec.Code, rec.Body.String())
	}

	// store 更新错误 → 500。
	env.st.errServersUpdate = errors.New("db down")
	rec = env.do(http.MethodPatch, "/api/servers/"+srv.ID, `{"name":"third"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("update error = %d", rec.Code)
	}
	env.st.errServersUpdate = nil
}

func TestServerTestConnection(t *testing.T) {
	env := newTestEnv(t)
	srv := createServer(t, env)

	// 成功。
	rec := env.do(http.MethodPost, "/api/servers/"+srv.ID+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"server_version":"OpenSSH_9.6"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}

	// 首次发现主机密钥 → 回写 store。
	env.sshExec.testFn = func(context.Context, sshx.Target) (sshx.TestResult, error) {
		return sshx.TestResult{LatencyMS: 9, ServerVersion: "OpenSSH_9.8", FirstSeen: true,
			Fingerprint: "SHA256:xyz", HostKey: "ssh-ed25519 AAAA xyz"}, nil
	}
	rec = env.do(http.MethodPost, "/api/servers/"+srv.ID+"/test", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"first_seen":true`) {
		t.Fatalf("first-seen body = %s", rec.Body.String())
	}
	updated, err := env.st.Servers().Get(context.Background(), srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.HostKeyFingerprint != "SHA256:xyz" {
		t.Fatalf("host key not persisted: %+v", updated)
	}

	// 主机密钥变化 → 409。
	env.sshExec.testFn = func(context.Context, sshx.Target) (sshx.TestResult, error) {
		return sshx.TestResult{}, &sshx.HostKeyError{ServerID: srv.ID, Expected: "SHA256:old", Actual: "SHA256:new"}
	}
	rec = env.do(http.MethodPost, "/api/servers/"+srv.ID+"/test", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("host key changed = %d %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "SSH_HOST_KEY_CHANGED" {
		t.Fatalf("code = %s", code)
	}

	// 超时 → 504。
	env.sshExec.testFn = func(ctx context.Context, _ sshx.Target) (sshx.TestResult, error) {
		return sshx.TestResult{}, context.DeadlineExceeded
	}
	rec = env.do(http.MethodPost, "/api/servers/"+srv.ID+"/test", "")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout = %d", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "SSH_TIMEOUT" {
		t.Fatalf("code = %s", code)
	}

	// 通用失败 → 502。
	env.sshExec.testFn = func(context.Context, sshx.Target) (sshx.TestResult, error) {
		return sshx.TestResult{}, errors.New("dial tcp: connection refused")
	}
	rec = env.do(http.MethodPost, "/api/servers/"+srv.ID+"/test", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("generic failure = %d", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "SERVER_TEST_FAILED" {
		t.Fatalf("code = %s", code)
	}

	// 服务器不存在 → 404。
	rec = env.do(http.MethodPost, "/api/servers/ghost/test", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing server test = %d", rec.Code)
	}

	// SSH 未装配 → 503（直接调用 handler）。
	c, rec2 := withParam(httptestPost("/api/servers/x/test", ""), "id", srv.ID)
	(&Deps{Store: env.st, Clock: time.Now}).handleTestServer(c)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil ssh status = %d", rec2.Code)
	}
}

func TestServerPasswordIsEncryptedAtRest(t *testing.T) {
	env := newTestEnv(t)
	srv := createServer(t, env)

	raw, err := env.st.Servers().Get(context.Background(), srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw.PasswordEncrypted == "" || raw.PasswordEncrypted == "s3cret" {
		t.Fatalf("password not encrypted at rest: %q", raw.PasswordEncrypted)
	}
	if !strings.HasPrefix(raw.PasswordEncrypted, "v1:") {
		t.Fatalf("ciphertext format = %q", raw.PasswordEncrypted)
	}
}

// tagList 生成 n 个互不重复的 tag。
func tagList(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, `"tag-`+itoa(i)+`"`)
	}
	return out
}
