// servers_winrm_test.go 覆盖 W1 的 servers os/winrm 字段与连接测试路由
// （契约 2.6/2.7/5.2A）：os×auth_method 组合校验、winrm_port 校验、
// WinRM 认证服务器的 /test 走 winrmx 且无主机密钥语义。
package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/winrmx"
)

// fakeWinRMExec 记录 Test/Execute 收到的目标，供路由断言。
type fakeWinRMExec struct {
	winrmx.Executor
	testedTargets []winrmx.Target
	execCommands  []string
	invalidated   []string
	testResult    winrmx.TestResult
	testErr       error
}

func (f *fakeWinRMExec) Test(ctx context.Context, target winrmx.Target) (winrmx.TestResult, error) {
	f.testedTargets = append(f.testedTargets, target)
	if f.testErr != nil {
		return winrmx.TestResult{}, f.testErr
	}
	return f.testResult, nil
}

func (f *fakeWinRMExec) Execute(ctx context.Context, target winrmx.Target, req winrmx.RunRequest) (winrmx.RunResult, error) {
	f.execCommands = append(f.execCommands, req.Command)
	return winrmx.RunResult{ExitCode: 0, Combined: "ok"}, nil
}

func (f *fakeWinRMExec) Invalidate(id string) { f.invalidated = append(f.invalidated, id) }
func (f *fakeWinRMExec) Close() error         { return nil }

func winrmStubServer(auth store.AuthMethod) store.Server {
	return store.Server{
		ID: "s-win", Name: "win-1", OS: store.ServerOSWindows, Host: "127.0.0.1",
		Port: 22, WinRMPort: 5985, Username: "Administrator", AuthMethod: auth,
	}
}

// TestHandleTestServerWinRMRoute 验证 WinRM 认证的服务器连接测试走 winrmx，
// 返回无主机密钥语义（fingerprint 空、first_seen false）。
func TestHandleTestServerWinRMRoute(t *testing.T) {
	fake := &fakeWinRMExec{testResult: winrmx.TestResult{
		LatencyMS: 12, ServerVersion: "5.1.19041.5007",
	}}
	sshFake := &fakeSSHExec{testFn: func(context.Context, sshx.Target) (sshx.TestResult, error) {
		t.Fatal("winrm-auth server must not hit the ssh executor")
		return sshx.TestResult{}, nil
	}}
	d := &Deps{
		Store: &connTestStubStore{servers: []store.Server{winrmStubServer(store.AuthMethodPasswordNTLM)}},
		WinRM: fake,
		SSH:   sshFake,
	}

	rec := performConnTest(d.handleTestServer, "s-win")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(fake.testedTargets) != 1 {
		t.Fatalf("winrm Test calls = %d, want 1", len(fake.testedTargets))
	}
	target := fake.testedTargets[0]
	if target.Host != "127.0.0.1" || target.Port != 5985 || target.Username != "Administrator" {
		t.Fatalf("target = %+v", target)
	}
	if target.AuthType != store.AuthMethodPasswordNTLM || target.UseHTTPS {
		t.Fatalf("auth type = %s, https = %v", target.AuthType, target.UseHTTPS)
	}
	body := rec.Body.String()
	for _, want := range []string{`"first_seen":false`, `"fingerprint":""`, `"server_version":"5.1.19041.5007"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %q missing %q", body, want)
		}
	}
}

// TestHandleTestServerWinRMTimeout 验证 WinRM 测试超时映射 504 SSH_TIMEOUT。
func TestHandleTestServerWinRMTimeout(t *testing.T) {
	d := &Deps{
		Store: &connTestStubStore{servers: []store.Server{winrmStubServer(store.AuthMethodPasswordNTLM)}},
		WinRM: &fakeWinRMExec{testErr: context.DeadlineExceeded},
	}
	status, code, _ := decodeConnTestError(t, performConnTest(d.handleTestServer, "s-win"))
	if status != http.StatusGatewayTimeout || code != "SSH_TIMEOUT" {
		t.Fatalf("status = %d, code = %q", status, code)
	}
}

// TestHandleTestServerWinRMBasicHTTPS 验证 password_basic 且 5986 时
// Target.UseHTTPS=true（契约 5.2A）。
func TestHandleTestServerWinRMBasicHTTPS(t *testing.T) {
	fake := &fakeWinRMExec{}
	srv := winrmStubServer(store.AuthMethodPasswordBasic)
	srv.WinRMPort = 5986
	d := &Deps{
		Store: &connTestStubStore{servers: []store.Server{srv}},
		WinRM: fake,
	}
	if rec := performConnTest(d.handleTestServer, "s-win"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !fake.testedTargets[0].UseHTTPS {
		t.Fatal("password_basic on 5986 must set UseHTTPS")
	}
}

// TestCreateServerWindowsValidation 覆盖 os/winrm 字段创建校验与组合规则。
func TestCreateServerWindowsValidation(t *testing.T) {
	env := newTestEnv(t)

	// 合法：windows + password_ntlm + winrm_port 5986。
	rec := env.do(http.MethodPost, "/api/servers",
		`{"name":"win-a","os":"windows","host":"10.0.0.9","winrm_port":5986,"username":"admin","auth_method":"password_ntlm","password":"p@ss"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create windows server = %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"os":"windows"`, `"winrm_port":5986`, `"auth_method":"password_ntlm"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %q missing %q", body, want)
		}
	}

	cases := []struct {
		name string
		body string
		want string
	}{
		{"linux with winrm auth rejected", `{"name":"bad1","host":"10.0.0.1","username":"u","auth_method":"password_ntlm","password":"x"}`, "auth_method is not allowed"},
		{"invalid os rejected", `{"name":"bad2","os":"darwin","host":"10.0.0.1","username":"u","auth_method":"password","password":"x"}`, "os must be linux or windows"},
		{"winrm auth with private key rejected", `{"name":"bad3","os":"windows","host":"10.0.0.1","username":"u","auth_method":"password_ntlm","private_key":"key"}`, "only password is allowed"},
		{"winrm_port out of range rejected", `{"name":"bad4","os":"windows","host":"10.0.0.1","winrm_port":65536,"username":"u","auth_method":"password_ntlm","password":"x"}`, "invalid winrm_port"},
		{"winrm_port zero rejected", `{"name":"bad5","os":"windows","host":"10.0.0.1","winrm_port":0,"username":"u","auth_method":"password_ntlm","password":"x"}`, "invalid winrm_port"},
	}
	for _, tc := range cases {
		rec := env.do(http.MethodPost, "/api/servers", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, body = %s", tc.name, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: body %q missing %q", tc.name, rec.Body.String(), tc.want)
		}
	}

	// 创建缺省 os=linux、winrm_port=5985。
	rec = env.do(http.MethodPost, "/api/servers",
		`{"name":"lin-1","host":"10.0.0.2","username":"u","auth_method":"password","password":"x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("default create = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"os":"linux"`) || !strings.Contains(rec.Body.String(), `"winrm_port":5985`) {
		t.Fatalf("defaults missing: %s", rec.Body.String())
	}
}

// TestUpdateServerOSAuthMismatch 验证修改 os 后认证方式不合法返回 400
// （契约 2.6）。
func TestUpdateServerOSAuthMismatch(t *testing.T) {
	env := newTestEnv(t)
	srv := createServer(t, env) // linux + password

	// linux 上直接换 winrm 认证：400。
	rec := env.do(http.MethodPatch, "/api/servers/"+srv.ID,
		`{"auth_method":"password_ntlm","password":"x"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "auth_method is not allowed") {
		t.Fatalf("linux+ntlm = %d %s", rec.Code, rec.Body.String())
	}

	// windows + password_ntlm 服务器把 os 改回 linux：400。
	rec = env.do(http.MethodPost, "/api/servers",
		`{"name":"win-b","os":"windows","host":"10.0.0.10","username":"u","auth_method":"password_ntlm","password":"x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create win-b = %d %s", rec.Code, rec.Body.String())
	}
	var winSrv store.Server
	decodeBody(t, rec, &winSrv)
	rec = env.do(http.MethodPatch, "/api/servers/"+winSrv.ID, `{"os":"linux"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "auth_method is not allowed") {
		t.Fatalf("windows→linux with ntlm = %d %s", rec.Code, rec.Body.String())
	}

	// 合法：windows + password_ntlm 改 winrm_port。
	rec = env.do(http.MethodPatch, "/api/servers/"+winSrv.ID, `{"winrm_port":5986}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"winrm_port":5986`) {
		t.Fatalf("update winrm_port = %d %s", rec.Code, rec.Body.String())
	}
	// 非法 winrm_port。
	rec = env.do(http.MethodPatch, "/api/servers/"+winSrv.ID, `{"winrm_port":70000}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid winrm_port") {
		t.Fatalf("update bad winrm_port = %d %s", rec.Code, rec.Body.String())
	}
}

// TestValidServerAuthMatrix 覆盖 os×auth_method 组合矩阵。
func TestValidServerAuthMatrix(t *testing.T) {
	for _, tc := range []struct {
		os   store.ServerOS
		auth store.AuthMethod
		want bool
	}{
		{store.ServerOSLinux, store.AuthMethodPassword, true},
		{store.ServerOSLinux, store.AuthMethodPrivateKey, true},
		{store.ServerOSLinux, store.AuthMethodPasswordNTLM, false},
		{store.ServerOSLinux, store.AuthMethodPasswordBasic, false},
		{store.ServerOSWindows, store.AuthMethodPassword, true},
		{store.ServerOSWindows, store.AuthMethodPrivateKey, true},
		{store.ServerOSWindows, store.AuthMethodPasswordNTLM, true},
		{store.ServerOSWindows, store.AuthMethodPasswordBasic, true},
		{store.ServerOS("darwin"), store.AuthMethodPassword, false},
	} {
		if got := validServerAuth(tc.os, tc.auth); got != tc.want {
			t.Errorf("validServerAuth(%s, %s) = %v, want %v", tc.os, tc.auth, got, tc.want)
		}
	}
}

// 编译期断言：fake 与接口一致。
var _ winrmx.Executor = (*fakeWinRMExec)(nil)
