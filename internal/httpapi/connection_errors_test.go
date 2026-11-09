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

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/mongo/address"
	"go.mongodb.org/mongo-driver/mongo/description"
	"go.mongodb.org/mongo-driver/x/mongo/driver/topology"

	"aiops/internal/dbproxy"
	"aiops/internal/netdev"
	"aiops/internal/sshx"
	"aiops/internal/store"
)

// 第 6 轮打磨：连接测试失败的 message 必须透出底层原因
// （dial 拒绝 / unable to open database file 等），否则运维无法
// 区分防火墙、地址错、认证失败还是文件不存在。

// stubStore 仅实现 handler 路径用到的仓储方法；未覆盖的方法由
// 嵌入接口在误用时 panic，保证测试聚焦。
type connTestStubStore struct {
	store.Store
	servers   []store.Server
	databases []store.Database
}

type stubServerStore struct {
	store.ServerStore
	servers []store.Server
}

func (s *stubServerStore) Get(_ context.Context, id string) (store.Server, error) {
	for _, srv := range s.servers {
		if srv.ID == id {
			return srv, nil
		}
	}
	return store.Server{}, store.ErrNotFound
}

type stubDatabaseStore struct {
	store.DatabaseStore
	databases []store.Database
}

func (s *stubDatabaseStore) Get(_ context.Context, id string) (store.Database, error) {
	for _, db := range s.databases {
		if db.ID == id {
			return db, nil
		}
	}
	return store.Database{}, store.ErrNotFound
}

func (s *connTestStubStore) Servers() store.ServerStore {
	return &stubServerStore{servers: s.servers}
}

func (s *connTestStubStore) Databases() store.DatabaseStore {
	return &stubDatabaseStore{databases: s.databases}
}

type stubSSHExecutor struct {
	sshx.Executor
	testErr error
}

func (f *stubSSHExecutor) Test(context.Context, sshx.Target) (sshx.TestResult, error) {
	return sshx.TestResult{}, f.testErr
}

type stubDBExecutor struct {
	dbproxy.Executor
	testErr error
}

func (f *stubDBExecutor) Test(context.Context, store.Database, string) (dbproxy.TestResult, error) {
	return dbproxy.TestResult{}, f.testErr
}

func performConnTest(handler gin.HandlerFunc, id string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/test", nil)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Set("request_id", "test-req")
	handler(c)
	return rec
}

func decodeConnTestError(t *testing.T, rec *httptest.ResponseRecorder) (int, string, string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, body.Error.Code, body.Error.Message
}

func stubServer() store.Server {
	return store.Server{
		ID: "s1", Name: "web-1", Host: "127.0.0.1", Port: 1,
		Username: "root", AuthMethod: store.AuthMethodPassword,
	}
}

func TestHandleTestServerSurfacesUnderlyingError(t *testing.T) {
	dialErr := errors.New("sshx: dial [127.0.0.1]:1: dial tcp 127.0.0.1:1: connect: connection refused")
	d := &Deps{
		Store: &connTestStubStore{servers: []store.Server{stubServer()}},
		SSH:   &stubSSHExecutor{testErr: dialErr},
	}

	status, code, msg := decodeConnTestError(t, performConnTest(d.handleTestServer, "s1"))
	if status != http.StatusBadGateway || code != "SERVER_TEST_FAILED" {
		t.Fatalf("status = %d, code = %q", status, code)
	}
	if !strings.Contains(msg, "ssh connection test failed") {
		t.Errorf("message = %q, want prefix", msg)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("message = %q, want underlying dial error surfaced", msg)
	}
	// p2 sprint 3：中文分类短语前置（无法连接 host:port：连接被拒绝），
	// 关键原因不再埋在英文错误链末尾。
	if !strings.Contains(msg, "无法连接 127.0.0.1:1：连接被拒绝") {
		t.Errorf("message = %q, want localized dial classification", msg)
	}
}

// TestHandleTestServerWinRMFailureLocalized 覆盖 p2 sprint 3：WinRM 测试
// 失败的 message 带中文分类短语，并折叠 winrm 库的冗余包装——真实链路
// 实测（走查）为 "winrmx: run command: unknown error Post \"...\": ntlm:
// negotiate message encryption: ntlm: negotiate message encryption: dial
// tcp ...: connect: connection refused"，同一短语重复两次且 "unknown
// error" 无信息量。
func TestHandleTestServerWinRMFailureLocalized(t *testing.T) {
	// 按库源码形态逐层复刻（ntlm_sealing_transport.go 两层同短语包装 +
	// http.go "unknown error " + winrmx runScript 包装）。
	dial := errors.New("dial tcp 127.0.0.1:5985: connect: connection refused")
	lvl4 := fmt.Errorf("ntlm: negotiate message encryption: %w", dial)
	lvl3 := fmt.Errorf("ntlm: negotiate message encryption: %w", lvl4)
	lvl2 := fmt.Errorf("Post %q: %w", "http://127.0.0.1:5985/wsman", lvl3)
	lvl1 := fmt.Errorf("unknown error %w", lvl2)
	lvl0 := fmt.Errorf("winrmx: run command: %w", lvl1)

	d := &Deps{
		Store: &connTestStubStore{servers: []store.Server{winrmStubServer(store.AuthMethodPasswordNTLM)}},
		WinRM: &fakeWinRMExec{testErr: lvl0},
	}
	status, code, msg := decodeConnTestError(t, performConnTest(d.handleTestServer, "s-win"))
	if status != http.StatusBadGateway || code != "SERVER_TEST_FAILED" {
		t.Fatalf("status = %d, code = %q", status, code)
	}
	if !strings.Contains(msg, "winrm connection test failed") {
		t.Errorf("message = %q, want channel prefix", msg)
	}
	if !strings.Contains(msg, "无法连接 127.0.0.1:5985：连接被拒绝") {
		t.Errorf("message = %q, want localized classification with winrm port", msg)
	}
	if strings.Contains(msg, "unknown error") {
		t.Errorf("message = %q, want placeholder wrap collapsed", msg)
	}
	if got := strings.Count(msg, "ntlm: negotiate message encryption:"); got != 1 {
		t.Errorf("message = %q, want duplicated negotiate wrap collapsed (count=%d)", msg, got)
	}
	// 底层原因保留（URL 与 dial 错误供排障）。
	if !strings.Contains(msg, `Post "http://127.0.0.1:5985/wsman"`) || !strings.Contains(msg, "connection refused") {
		t.Errorf("message = %q, want underlying cause preserved", msg)
	}
}

func TestHandleTestServerTimeoutSurfacesDeadline(t *testing.T) {
	d := &Deps{
		Store: &connTestStubStore{servers: []store.Server{stubServer()}},
		SSH:   &stubSSHExecutor{testErr: context.DeadlineExceeded},
	}

	status, code, msg := decodeConnTestError(t, performConnTest(d.handleTestServer, "s1"))
	if status != http.StatusGatewayTimeout || code != "SSH_TIMEOUT" {
		t.Fatalf("status = %d, code = %q", status, code)
	}
	if !strings.Contains(msg, "context deadline exceeded") {
		t.Errorf("message = %q, want deadline cause", msg)
	}
}

func TestHandleTestServerHostKeyErrorKeepsDedicatedCode(t *testing.T) {
	d := &Deps{
		Store: &connTestStubStore{servers: []store.Server{stubServer()}},
		SSH: &stubSSHExecutor{testErr: &sshx.HostKeyError{
			ServerID: "s1", Expected: "SHA256:aaa", Actual: "SHA256:bbb",
		}},
	}

	status, code, msg := decodeConnTestError(t, performConnTest(d.handleTestServer, "s1"))
	if status != http.StatusConflict || code != "SSH_HOST_KEY_CHANGED" {
		t.Fatalf("status = %d, code = %q", status, code)
	}
	// 主机密钥变化保持固定文案与 409，不因拼接改动破坏既有契约。
	if msg != "host key changed" {
		t.Errorf("message = %q, want fixed host key message", msg)
	}
}

func TestHandleTestDatabaseSurfacesUnderlyingError(t *testing.T) {
	d := &Deps{
		Store: &connTestStubStore{databases: []store.Database{{
			ID: "d1", Name: "local", Engine: store.EngineSQLite,
		}}},
		DB: &stubDBExecutor{testErr: errors.New("unable to open database file (14)")},
	}

	status, code, msg := decodeConnTestError(t, performConnTest(d.handleTestDatabase, "d1"))
	if status != http.StatusBadGateway || code != "DATABASE_TEST_FAILED" {
		t.Fatalf("status = %d, code = %q", status, code)
	}
	if !strings.Contains(msg, "database connection test failed") {
		t.Errorf("message = %q, want prefix", msg)
	}
	if !strings.Contains(msg, "unable to open database file") {
		t.Errorf("message = %q, want underlying sqlite error surfaced", msg)
	}
}

// TestDescribeDatabaseFailureLocalized 覆盖 p2 sprint 5：数据库侧连接
// 测试/查询失败的 message 带中文分类短语（对齐服务器侧
// describeConnTestFailure），底层原因保留；未识别形态回落前缀 + 原因。
func TestDescribeDatabaseFailureLocalized(t *testing.T) {
	redis := store.Database{ID: "d1", Engine: store.EngineRedis, Host: "127.0.0.1", Port: 6399}
	mysql := store.Database{ID: "d2", Engine: store.EngineMySQL, Host: "10.0.0.8", Port: 3306}
	pg := store.Database{ID: "d3", Engine: store.EnginePostgres, Host: "db.example", Port: 5432}
	sqlite := store.Database{ID: "d4", Engine: store.EngineSQLite}

	cases := []struct {
		name     string
		prefix   string
		db       store.Database
		err      error
		contains []string
	}{
		{
			name:   "redis 连接拒绝：中文分类 + host:port + 底层原因",
			prefix: "database connection test failed",
			db:     redis,
			err:    errors.New("dial tcp 127.0.0.1:6399: connect: connection refused"),
			contains: []string{
				"database connection test failed：无法连接 127.0.0.1:6399：连接被拒绝（端口未开放或被防火墙拦截）",
				"dial tcp 127.0.0.1:6399: connect: connection refused",
			},
		},
		{
			name:   "mysql 认证失败（Access denied for user）",
			prefix: "database connection test failed",
			db:     mysql,
			err:    errors.New("Error 1045 (28000): Access denied for user 'root'@'10.0.0.8' (using password: YES)"),
			contains: []string{
				"无法连接 10.0.0.8:3306：认证失败（用户名或密码不正确）",
				"Access denied for user",
			},
		},
		{
			name:   "postgres 认证失败（password authentication failed）",
			prefix: "查询失败",
			db:     pg,
			err:    errors.New("failed to connect to `host=db.example`: server error: FATAL: password authentication failed for user \"ops\" (SQLSTATE 28P01)"),
			contains: []string{
				"查询失败：无法连接 db.example:5432：认证失败（用户名或密码不正确）",
				"password authentication failed",
			},
		},
		{
			name:   "redis WRONGPASS 认证失败",
			prefix: "database connection test failed",
			db:     redis,
			err:    errors.New("WRONGPASS invalid username-password pair or password is not set for user."),
			contains: []string{
				"认证失败（用户名或密码不正确）",
			},
		},
		{
			name:   "mssql 登录失败（Login failed for user）",
			prefix: "查询失败",
			db:     mysql,
			err:    errors.New("Login failed for user 'sa'."),
			contains: []string{
				"认证失败（用户名或密码不正确）",
			},
		},
		{
			name:   "TLS 握手失败",
			prefix: "database connection test failed",
			db:     pg,
			err:    errors.New("tls: handshake failure"),
			contains: []string{
				"TLS 握手失败",
				"tls: handshake failure",
			},
		},
		{
			name:   "连接超时",
			prefix: "database connection test failed",
			db:     redis,
			err:    errors.New("dial tcp 10.255.255.1:6379: i/o timeout"),
			contains: []string{
				"连接超时（未在时限内完成连接）",
				"i/o timeout",
			},
		},
		{
			name:   "域名解析失败",
			prefix: "database connection test failed",
			db:     pg,
			err:    errors.New("dial tcp: lookup db.example: no such host"),
			contains: []string{
				"域名解析失败",
				"no such host",
			},
		},
		{
			name:   "sqlite 无 host:port：回落前缀 + 原因（不出现「无法连接」）",
			prefix: "database connection test failed",
			db:     sqlite,
			err:    errors.New("unable to open database file (14)"),
			contains: []string{
				"database connection test failed：unable to open database file (14)",
			},
		},
		{
			name:   "未识别形态：回落前缀 + 原因",
			prefix: "查询失败",
			db:     mysql,
			err:    errors.New("Error 1146 (42S02): table doesn't exist"),
			contains: []string{
				"查询失败：Error 1146 (42S02): table doesn't exist",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := describeDatabaseFailure(tc.prefix, tc.db, tc.err)
			for _, want := range tc.contains {
				if !strings.Contains(msg, want) {
					t.Errorf("message = %q, want contains %q", msg, want)
				}
			}
			if tc.db.Engine == store.EngineSQLite && strings.Contains(msg, "无法连接") {
				t.Errorf("message = %q, sqlite 不应拼 host:port 语境", msg)
			}
		})
	}
}

// TestDescribeNetdevTestFailureLocalized 覆盖 p3 sprint 5：netdev 连接
// 测试的登录/执行期哨兵失败（ErrLoginFailed/ErrExecFailed，expect.go 以
// %v 包装底层错误、不入 Unwrap 链，ClassifyDialFailure 对 channel
// closed/凭据被拒等形态返回 DialUnknown）此前 detail 只有整条英文链
// （走查 sprint 5：「netdev: device login failed: channel closed: EOF」），
// 仅 netdev 登录路径无中文分类——与 SSH/WinRM 通道不一致。修复后按
// 哨兵与底层形态给中文分类短语、剥哨兵前缀留底层原因；拨号期失败仍
// 走网络层分类。
func TestDescribeNetdevTestFailureLocalized(t *testing.T) {
	const prefix = "network device connection test failed"
	cases := []struct {
		name     string
		err      error
		contains []string
		banned   []string
	}{
		{
			name: "登录期通道断开（走查实测形态）",
			err:  fmt.Errorf("%w: channel closed: EOF", netdev.ErrLoginFailed),
			contains: []string{
				prefix + ": 无法登录 192.0.2.123:23：设备侧在登录阶段断开通道；原因：channel closed: EOF",
			},
			banned: []string{"netdev: device login failed"},
		},
		{
			name: "登录期凭据被拒",
			err:  fmt.Errorf("%w: credentials rejected (login prompt re-appeared)", netdev.ErrLoginFailed),
			contains: []string{
				"设备拒绝登录凭据（登录提示重新出现）",
				"credentials rejected (login prompt re-appeared)",
			},
			banned: []string{"netdev: device login failed"},
		},
		{
			name: "登录期总预算超时",
			err:  fmt.Errorf("%w: expect timeout after total budget", netdev.ErrLoginFailed),
			contains: []string{
				"无法登录 192.0.2.123:23：登录阶段超时（未在时限内等到设备提示符）",
				"expect timeout after total budget",
			},
			banned: []string{"netdev: device login failed"},
		},
		{
			name: "裸登录哨兵（无底层原因尾巴）",
			err:  netdev.ErrLoginFailed,
			contains: []string{
				prefix + ": 无法登录 192.0.2.123:23：登录阶段失败（设备提示符未出现）",
			},
			banned: []string{"；原因：", "netdev: device login failed"},
		},
		{
			name: "执行期通道断开",
			err:  fmt.Errorf("%w: channel closed: EOF", netdev.ErrExecFailed),
			contains: []string{
				"无法登录 192.0.2.123:23：设备侧在命令输出阶段断开通道；原因：channel closed: EOF",
			},
			banned: []string{"netdev: device command execution failed"},
		},
		{
			name: "执行期超时",
			err:  fmt.Errorf("%w: expect timeout after total budget", netdev.ErrExecFailed),
			contains: []string{
				"命令输出阶段超时（未在时限内等到设备提示符）",
			},
			banned: []string{"netdev: device command execution failed"},
		},
		{
			name: "拨号期失败仍走网络层分类（不落哨兵分支）",
			err:  errors.New("netdev: telnet dial: dial tcp 192.0.2.123:23: connect: connection refused"),
			contains: []string{
				"无法连接 192.0.2.123:23：连接被拒绝（端口未开放或被防火墙拦截）",
			},
		},
		{
			name: "拨号期失败（真实 %w 多层链，p3 sprint 6）：剥包名哨兵只留 dial tcp 级原因",
			err: fmt.Errorf("netdev: telnet dial: %w",
				fmt.Errorf("telnetx: dial %s: %w", "192.0.2.123:23",
					errors.New("dial tcp 192.0.2.123:23: connect: connection refused"))),
			contains: []string{
				"无法连接 192.0.2.123:23：连接被拒绝（端口未开放或被防火墙拦截）",
				"；原因：dial tcp 192.0.2.123:23: connect: connection refused",
			},
			banned: []string{"netdev: telnet dial", "telnetx: dial"},
		},
		{
			name: "未识别形态回落前缀 + 整条原因",
			err:  errors.New("netdev: ssh handshake: weird failure"),
			contains: []string{
				prefix + ": netdev: ssh handshake: weird failure",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := describeNetdevTestFailure(prefix, "192.0.2.123", 23, tc.err)
			for _, want := range tc.contains {
				if !strings.Contains(msg, want) {
					t.Errorf("message = %q, want contains %q", msg, want)
				}
			}
			for _, ban := range tc.banned {
				if strings.Contains(msg, ban) {
					t.Errorf("message = %q, sentinel/edge %q must be stripped", msg, ban)
				}
			}
		})
	}
}

// TestDescribeDatabaseFailureMongoPingLocalized 覆盖 p3 sprint 6：mongo
// ping 失败的 reason 此前把 "dbproxy: mongo ping: " 包名哨兵与驱动
// ServerSelectionError 尾部的 topology dump 原样透传（走查 round 6 弹窗
// 拖数百字符 "current topology: { Type: Unknown, Servers: […] }"）。
// 修复后：哨兵前缀剥掉、dump 截断到 server selection 级原因；中文分类
// 短语基于原始错误全文（topology 内含 connection refused），不受清洗
// 影响。错误按真实产生路径复刻：internal/dbproxy/mongo.go:134 的 %w
// 包装 + mongo-driver v1.17.4 x/mongo/driver/topology/errors.go 的
// ServerSelectionError（Unwrap 链通、"server selection error: %s,
// current topology: { %s }" 非前缀式文本）。
func TestDescribeDatabaseFailureMongoPingLocalized(t *testing.T) {
	topo := description.Topology{
		// Kind/Server.Kind 留零值：驱动对无效 kind 的 String() 输出
		// "Unknown"，与实测弹窗中的拓扑 dump 形态一致。
		Servers: []description.Server{{
			Addr:      address.Address("127.0.0.1:27017"),
			LastError: errors.New("dial tcp 127.0.0.1:27017: connect: connection refused"),
		}},
	}
	pingErr := fmt.Errorf("dbproxy: mongo ping: %w", topology.ServerSelectionError{
		Desc:    topo,
		Wrapped: topology.ErrServerSelectionTimeout,
	})

	db := store.Database{ID: "d1", Engine: store.EngineMongoDB, Host: "127.0.0.1", Port: 27017}
	msg := describeDatabaseFailure("查询失败", db, pingErr)
	for _, want := range []string{
		"查询失败：无法连接 127.0.0.1:27017：连接被拒绝（端口未开放或被防火墙拦截）",
		"；原因：server selection error: server selection timeout",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want contains %q", msg, want)
		}
	}
	for _, ban := range []string{"dbproxy: mongo ping", "current topology", "Servers: [{"} {
		if strings.Contains(msg, ban) {
			t.Errorf("message = %q, noise %q must be stripped", msg, ban)
		}
	}
}
