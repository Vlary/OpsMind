// servers_netdev_test.go 覆盖三期（N1）servers handler 网络设备路径
// （契约 2.6/2.8 与验收 S20①②）：telnet 明文确认、device_kind/os/brand
// 绑定校验、响应新字段与凭据不泄漏、enable_secret 替换/清空、连接测试
// 通道路由与错误码映射、telnet 网络设备开终端 400 TERM_UNSUPPORTED、
// preview-risk 的 netdev_brand 路由。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"aiops/internal/netdev"
	"aiops/internal/store"
)

// fakeNetCLI 是 netdev.Executor 的测试假实现。
type fakeNetCLI struct {
	testFn    func(context.Context, netdev.Target) (netdev.TestResult, error)
	executeFn func(context.Context, netdev.Target, netdev.RunRequest) (netdev.RunResult, error)

	invalidated []string
	closed      bool
}

func (f *fakeNetCLI) Test(ctx context.Context, t netdev.Target) (netdev.TestResult, error) {
	if f.testFn == nil {
		return netdev.TestResult{LatencyMS: 5, ServerVersion: "15.2(4)E7"}, nil
	}
	return f.testFn(ctx, t)
}

func (f *fakeNetCLI) Execute(ctx context.Context, t netdev.Target, req netdev.RunRequest) (netdev.RunResult, error) {
	if f.executeFn == nil {
		return netdev.RunResult{ExitCode: 0, Combined: "OK"}, nil
	}
	return f.executeFn(ctx, t, req)
}

func (f *fakeNetCLI) Invalidate(serverID string) {
	f.invalidated = append(f.invalidated, serverID)
}

func (f *fakeNetCLI) Close() error { f.closed = true; return nil }

// createNetdevDevice 建一台 cisco 网络设备（telnet 需显式确认）。
func createNetdevDevice(t *testing.T, env *testEnv, extra string) (id string, body string) {
	t.Helper()
	payload := `{"name":"core-sw-1","device_kind":"network_device","netdev_brand":"cisco",` +
		`"access_proto":"telnet","port":23,"host":"127.0.0.1","username":"admin",` +
		`"auth_method":"password","password":"s3cret","confirm_telnet_cleartext":true` + extra + `}`
	rec := env.do(http.MethodPost, "/api/servers", payload)
	body = rec.Body.String()
	if rec.Code != http.StatusCreated {
		t.Fatalf("create network device = %d %s", rec.Code, body)
	}
	var srv struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &srv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return srv.ID, body
}

// TestCreateNetworkDeviceTelnetConfirm 验证 telnet 明文确认与响应字段
// （契约 2.6：缺失 confirm → 400 不落库；带确认 → 201，响应含三期字段、
// 无任何密文字段——S20①）。
func TestCreateNetworkDeviceTelnetConfirm(t *testing.T) {
	env := newTestEnv(t)

	// 不带确认 → 400 VALIDATION_FAILED。
	rec := env.do(http.MethodPost, "/api/servers",
		`{"name":"sw-a","device_kind":"network_device","netdev_brand":"cisco","access_proto":"telnet",`+
			`"host":"127.0.0.1","username":"admin","auth_method":"password","password":"s3cret"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing confirm = %d %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "VALIDATION_FAILED" {
		t.Fatalf("code = %s", code)
	}
	// 不落库。
	if strings.Contains(env.do(http.MethodGet, "/api/servers", "").Body.String(), "sw-a") {
		t.Fatal("unconfirmed telnet device must not be persisted")
	}

	// netdev_brand 缺失 → 400。
	rec = env.do(http.MethodPost, "/api/servers",
		`{"name":"sw-b","device_kind":"network_device","access_proto":"ssh",`+
			`"host":"127.0.0.1","username":"admin","auth_method":"password","password":"s3cret"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing brand = %d %s", rec.Code, rec.Body.String())
	}

	// os 与 device_kind 双向绑定：network_device + os=linux → 400。
	rec = env.do(http.MethodPost, "/api/servers",
		`{"name":"sw-c","device_kind":"network_device","netdev_brand":"cisco","os":"linux",`+
			`"host":"127.0.0.1","username":"admin","auth_method":"password","password":"s3cret"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("os=linux with network_device = %d", rec.Code)
	}

	// 密钥认证失败关闭 → 400。
	rec = env.do(http.MethodPost, "/api/servers",
		`{"name":"sw-d","device_kind":"network_device","netdev_brand":"cisco",`+
			`"host":"127.0.0.1","username":"admin","auth_method":"private_key","private_key":"KEY"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("private_key on network device = %d", rec.Code)
	}

	// server 资产带 enable_secret / netdev_brand → 400。
	rec = env.do(http.MethodPost, "/api/servers",
		`{"name":"srv-x","host":"127.0.0.1","username":"root","auth_method":"password",`+
			`"password":"s3cret","enable_secret":"e"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enable_secret on server = %d", rec.Code)
	}
	rec = env.do(http.MethodPost, "/api/servers",
		`{"name":"srv-y","host":"127.0.0.1","username":"root","auth_method":"password",`+
			`"password":"s3cret","netdev_brand":"cisco"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("netdev_brand on server = %d", rec.Code)
	}

	// 带确认 → 201，响应含三期字段、无密文。
	_, body := createNetdevDevice(t, env, "")
	for _, want := range []string{
		`"device_kind":"network_device"`, `"os":"network"`,
		`"netdev_brand":"cisco"`, `"access_proto":"telnet"`, `"has_enable_secret":false`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %s: %s", want, body)
		}
	}
	low := strings.ToLower(body)
	for _, leak := range []string{"passwordencrypted", "enablesecretencrypted", `"s3cret"`} {
		if strings.Contains(low, strings.ToLower(leak)) {
			t.Errorf("credential leaked in response: %s", leak)
		}
	}
}

// TestCreateNetworkDeviceSSHAndEnableSecret 验证 ssh 缺省形态与
// enable_secret 加密存储（提供即设置）。
func TestCreateNetworkDeviceSSHAndEnableSecret(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodPost, "/api/servers",
		`{"name":"sw-ssh","device_kind":"network_device","netdev_brand":"huawei",`+
			`"host":"127.0.0.1","username":"admin","auth_method":"password",`+
			`"password":"s3cret","enable_secret":"super-secret"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"has_enable_secret":true`) {
		t.Fatalf("has_enable_secret missing: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "super-secret") {
		t.Fatal("enable secret plaintext leaked")
	}
	// 密文落库且可解密（store 侧拿到的是密文）。
	var id string
	if err := json.Unmarshal([]byte(rec.Body.String()), &struct {
		ID *string `json:"id"`
	}{ID: &id}); err != nil {
		t.Fatalf("decode id: %v", err)
	}
	srv, err := env.st.Servers().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if srv.EnableSecretEncrypted == "" || srv.EnableSecretEncrypted == "super-secret" {
		t.Fatalf("enable secret not encrypted at rest: %q", srv.EnableSecretEncrypted)
	}
	plain, err := env.cipher.Decrypt(srv.EnableSecretEncrypted)
	if err != nil || plain != "super-secret" {
		t.Fatalf("enable secret roundtrip failed: %v %q", err, plain)
	}
}

// TestUpdateNetworkDeviceTelnetConfirm 验证更新终态为 telnet 时必须确认
// （openapi 近似表达的跨请求终态由服务端强制）与 enable_secret 清空。
func TestUpdateNetworkDeviceTelnetConfirm(t *testing.T) {
	env := newTestEnv(t)
	id, _ := createNetdevDevice(t, env, "")

	// 改用户名（终态仍 telnet）不带确认 → 400。
	rec := env.do(http.MethodPatch, "/api/servers/"+id, `{"username":"ops"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update without confirm on telnet device = %d %s", rec.Code, rec.Body.String())
	}
	// 带确认 → 200。
	rec = env.do(http.MethodPatch, "/api/servers/"+id,
		`{"username":"ops","confirm_telnet_cleartext":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update with confirm = %d %s", rec.Code, rec.Body.String())
	}
	// 切换到 ssh 后不再要求确认。
	rec = env.do(http.MethodPatch, "/api/servers/"+id, `{"access_proto":"ssh"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch to ssh = %d %s", rec.Code, rec.Body.String())
	}
	// enable_secret 空串清空。
	if !strings.Contains(env.do(http.MethodPatch, "/api/servers/"+id, `{"enable_secret":"e1"}`).Body.String(),
		`"has_enable_secret":true`) {
		t.Fatal("enable secret set failed")
	}
	if !strings.Contains(env.do(http.MethodPatch, "/api/servers/"+id, `{"enable_secret":""}`).Body.String(),
		`"has_enable_secret":false`) {
		t.Fatal("enable secret clear failed")
	}
}

// TestNetdevDeviceInvalidatesPools 验证创建/更新/删除调用 NetCLI.Invalidate。
func TestNetdevDeviceInvalidatesPools(t *testing.T) {
	env := newTestEnv(t)
	env.netExec.invalidated = nil
	id, _ := createNetdevDevice(t, env, "")
	if len(env.netExec.invalidated) != 1 {
		t.Fatalf("create must invalidate NetCLI: %v", env.netExec.invalidated)
	}
	env.netExec.invalidated = nil
	env.do(http.MethodPatch, "/api/servers/"+id, `{"confirm_telnet_cleartext":true,"tags":["core"]}`)
	if len(env.netExec.invalidated) != 1 {
		t.Fatalf("update must invalidate NetCLI: %v", env.netExec.invalidated)
	}
	env.netExec.invalidated = nil
	env.do(http.MethodDelete, "/api/servers/"+id, "")
	if len(env.netExec.invalidated) != 1 {
		t.Fatalf("delete must invalidate NetCLI: %v", env.netExec.invalidated)
	}
}

// TestNetworkDeviceTestRouting 验证连接测试走 netcli：登录失败映射
// NETCLI_LOGIN_FAILED(502)、执行失败 NETCLI_EXEC_FAILED(502)——S20②。
func TestNetworkDeviceTestRouting(t *testing.T) {
	env := newTestEnv(t)
	id, _ := createNetdevDevice(t, env, "")

	// 走查 sprint 5 实测形态（expect.go wait 通道断开）：
	// "netdev: device login failed: channel closed: EOF"。
	env.netExec.testFn = func(context.Context, netdev.Target) (netdev.TestResult, error) {
		return netdev.TestResult{}, fmt.Errorf("%w: channel closed: EOF", netdev.ErrLoginFailed)
	}
	rec := env.do(http.MethodPost, "/api/servers/"+id+"/test", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("login failed = %d %s", rec.Code, rec.Body.String())
	}
	code, msg := errorBody(t, rec)
	if code != "NETCLI_LOGIN_FAILED" {
		t.Fatalf("code = %s", code)
	}
	// p3 sprint 5：detail 以中文分类开头（对齐 SSH/WinRM 通道），不再
	// 整条英文链直达弹窗；底层原因保留供排障。
	for _, want := range []string{
		"network device connection test failed",
		"无法登录 127.0.0.1:23：设备侧在登录阶段断开通道",
		"channel closed: EOF",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("login message missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "netdev: device login failed") {
		t.Errorf("login message keeps sentinel wrap: %s", msg)
	}

	env.netExec.testFn = func(context.Context, netdev.Target) (netdev.TestResult, error) {
		return netdev.TestResult{}, netdev.ErrExecFailed
	}
	rec = env.do(http.MethodPost, "/api/servers/"+id+"/test", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("exec failed = %d %s", rec.Code, rec.Body.String())
	}
	code, msg = errorBody(t, rec)
	if code != "NETCLI_EXEC_FAILED" {
		t.Fatalf("code = %s", code)
	}
	// 执行期哨兵（裸哨兵：无底层原因尾巴）同样给中文分类。
	if !strings.Contains(msg, "无法登录 127.0.0.1:23：命令执行阶段失败") {
		t.Errorf("exec message missing localized classification: %s", msg)
	}
	if strings.Contains(msg, "netdev: device command execution failed") {
		t.Errorf("exec message keeps sentinel wrap: %s", msg)
	}

	// 不可达（普通 dial 错误）→ SERVER_TEST_FAILED 502。
	env.netExec.testFn = func(context.Context, netdev.Target) (netdev.TestResult, error) {
		return netdev.TestResult{}, errors.New("netdev: telnet dial: connection refused")
	}
	rec = env.do(http.MethodPost, "/api/servers/"+id+"/test", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("unreachable = %d %s", rec.Code, rec.Body.String())
	}
	code, _ = errorBody(t, rec)
	if code != "SERVER_TEST_FAILED" {
		t.Fatalf("code = %s", code)
	}
	// 非哨兵（拨号期）失败仍走网络层分类。
	if _, msg = errorBody(t, rec); !strings.Contains(msg, "无法连接 127.0.0.1:23：连接被拒绝") {
		t.Errorf("dial message missing localized classification: %s", msg)
	}

	// 成功：响应含探针版本；telnet 无主机密钥语义。
	env.netExec.testFn = func(context.Context, netdev.Target) (netdev.TestResult, error) {
		return netdev.TestResult{LatencyMS: 9, ServerVersion: "15.2(4)E7"}, nil
	}
	rec = env.do(http.MethodPost, "/api/servers/"+id+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ok = %d %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"ok":true`, `"server_version":"15.2(4)E7"`, `"fingerprint":""`, `"first_seen":false`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("test response missing %s: %s", want, rec.Body.String())
		}
	}
}

// TestTermSessionTelnetNetworkDevice 验证 telnet 网络设备开终端 400
// TERM_UNSUPPORTED（契约 2.8.6）。手工塞 uuid id（term handler 要求 uuid）。
func TestTermSessionTelnetNetworkDevice(t *testing.T) {
	env := newTestEnv(t)
	id := uuid.NewString()
	env.st.mu.Lock()
	env.st.servers[id] = store.Server{
		ID: id, Name: "sw-term", DeviceKind: store.DeviceKindNetworkDevice,
		OS: store.ServerOSNetwork, NetdevBrand: store.NetdevBrandCisco,
		AccessProto: store.AccessProtoTelnet, Host: "127.0.0.1", Port: 23,
		Username: "admin", AuthMethod: store.AuthMethodPassword,
		Tags: []string{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	env.st.mu.Unlock()

	rec := env.do(http.MethodPost, "/api/term/sessions", `{"server_id":"`+id+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("term on telnet device = %d %s", rec.Code, rec.Body.String())
	}
	if code, _ := errorBody(t, rec); code != "TERM_UNSUPPORTED" {
		t.Fatalf("code = %s", code)
	}
}

// TestPreviewRiskNetdevBrand 验证 preview-risk 的 os=network + netdev_brand
// 路由与 server_id 上下文覆盖（契约 7.3C 分册路由；S20③⑤）。
func TestPreviewRiskNetdevBrand(t *testing.T) {
	env := newTestEnv(t)

	preview := func(body string) (level, rule string) {
		rec := env.do(http.MethodPost, "/api/ops/preview-risk", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview %s = %d %s", body, rec.Code, rec.Body.String())
		}
		var out struct {
			Level  string `json:"risk_level"`
			RuleID string `json:"rule_id"`
		}
		if err := json.Unmarshal([]byte(rec.Body.String()), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Level, out.RuleID
	}

	cases := []struct {
		body       string
		wantLevel  string
		wantRuleID string
	}{
		{`{"command":"reload","os":"network","netdev_brand":"cisco"}`, "forbidden", "NET_CISCO_RELOAD"},
		{`{"command":"show version","os":"network","netdev_brand":"cisco"}`, "safe", "NET_CISCO_SAFE_READ"},
		{`{"command":"show running-config","os":"network","netdev_brand":"cisco"}`, "needs_approval", "NET_CISCO_CONFIG_DUMP"},
		{`{"command":"configure terminal","os":"network","netdev_brand":"cisco"}`, "needs_approval", "NET_CISCO_CONF_MODE"},
		{`{"command":"sh ver","os":"network","netdev_brand":"cisco"}`, "safe", "NET_CISCO_SAFE_READ"},
		{`{"command":"reboot","os":"network"}`, "forbidden", "NET_GENERIC_RELOAD"},
		{`{"command":"display current-configuration","os":"network","netdev_brand":"huawei"}`, "needs_approval", "NET_HUAWEI_CONFIG_DUMP"},
	}
	for _, tc := range cases {
		level, rule := preview(tc.body)
		if level != tc.wantLevel || rule != tc.wantRuleID {
			t.Errorf("preview(%s) = %s/%s, want %s/%s", tc.body, level, rule, tc.wantLevel, tc.wantRuleID)
		}
	}

	// 非法 brand / os → 400。
	rec := env.do(http.MethodPost, "/api/ops/preview-risk", `{"command":"x","os":"network","netdev_brand":"nokia"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid brand = %d", rec.Code)
	}
	rec = env.do(http.MethodPost, "/api/ops/preview-risk", `{"command":"x","os":"plan9"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid os = %d", rec.Code)
	}

	// server_id 上下文覆盖：linux 服务器 + 显式 os=network → 按 linux 分类。
	srv := createServer(t, env)
	level, _ := preview(`{"command":"ls -la","os":"network","server_id":"` + srv.ID + `"}`)
	if level != "safe" {
		t.Errorf("server context override failed: level = %s", level)
	}
	// network_device 服务器 + 显式 brand=huawei → 按设备实际 brand（cisco）。
	id, _ := createNetdevDevice(t, env, "")
	level, rule := preview(`{"command":"reload","os":"network","netdev_brand":"huawei","server_id":"` + id + `"}`)
	if level != "forbidden" || rule != "NET_CISCO_RELOAD" {
		t.Errorf("network device context override = %s/%s, want forbidden/NET_CISCO_RELOAD", level, rule)
	}
}

// TestListServersIncludesNetdevFields 验证列表响应包含三期字段（S20 形态）。
func TestListServersIncludesNetdevFields(t *testing.T) {
	env := newTestEnv(t)
	createServer(t, env)
	createNetdevDevice(t, env, "")

	rec := env.do(http.MethodGet, "/api/servers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"device_kind":"server"`, `"netdev_brand":""`, `"access_proto":"ssh"`,
		`"device_kind":"network_device"`, `"netdev_brand":"cisco"`, `"access_proto":"telnet"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("list missing %s: %s", want, body)
		}
	}
}

// 编译期抑制 time 未用告警（保留给后续扩展）。
var _ = time.Second

// TestTelnetConfirmAuditMarker 契约 9.7：telnet 明文确认必须体现在
// server.create/server.update 审计 detail.confirm_telnet_cleartext=true
// （布尔标记，不含凭据）——三期评审发现该标记从未落审计。
func TestTelnetConfirmAuditMarker(t *testing.T) {
	env := newTestEnv(t)
	id, _ := createNetdevDevice(t, env, "")

	// Detail 是 json.RawMessage；审计经 writer 500ms 批量刷写，轮询等待。
	waitFlag := func(action string) (flag any, present bool) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			env.st.mu.Lock()
			for i := len(env.st.auditLogs) - 1; i >= 0; i-- {
				l := env.st.auditLogs[i]
				if l.Action != action {
					continue
				}
				var detail map[string]any
				if err := json.Unmarshal(l.Detail, &detail); err != nil {
					env.st.mu.Unlock()
					t.Fatalf("unmarshal %s detail: %v", action, err)
				}
				v, ok := detail["confirm_telnet_cleartext"]
				env.st.mu.Unlock()
				return v, ok
			}
			env.st.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
		}
		return nil, false
	}

	if v, ok := waitFlag("server.create"); !ok || v != true {
		t.Fatalf("create detail.confirm_telnet_cleartext = %v(ok=%v), want true", v, ok)
	}

	rec := env.do(http.MethodPatch, "/api/servers/"+id, `{"confirm_telnet_cleartext":true,"tags":["core"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	if v, ok := waitFlag("server.update"); !ok || v != true {
		t.Fatalf("update detail.confirm_telnet_cleartext = %v(ok=%v), want true", v, ok)
	}
}

// TestUpdateNetdevBrandEmptyStringClears 网络设备切回 server 形态时前端发
// netdev_brand:""（openapi：anyOf NetdevBrand | 空串常量，store 支持置空），
// 不得按枚举外拒绝（三期评审：形态切换被 400 卡死）。
func TestUpdateNetdevBrandEmptyStringClears(t *testing.T) {
	env := newTestEnv(t)
	id, _ := createNetdevDevice(t, env, "")

	// 设备形态切回 server：device_kind/os/netdev_brand/access_proto 一起换。
	rec := env.do(http.MethodPatch, "/api/servers/"+id,
		`{"device_kind":"server","os":"linux","netdev_brand":"","access_proto":"ssh"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch to server kind = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"netdev_brand":""`) {
		t.Fatalf("netdev_brand not cleared: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"device_kind":"server"`) {
		t.Fatalf("device_kind not switched: %s", rec.Body.String())
	}

	// 非空非法值仍然 400（枚举校验不放宽）；换名避免与前一台冲突。
	id2, _ := createNetdevDevice(t, env, `,"name":"core-sw-2"`)
	rec = env.do(http.MethodPatch, "/api/servers/"+id2, `{"netdev_brand":"nokia"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid brand must stay 400, got %d", rec.Code)
	}

	// 网络设备终态仍要求非空品牌（终态校验不放宽）。
	rec = env.do(http.MethodPatch, "/api/servers/"+id2, `{"netdev_brand":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("network device with empty brand must stay 400, got %d", rec.Code)
	}
}
