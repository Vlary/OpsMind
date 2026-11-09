package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/dbproxy"
	"aiops/internal/sqlguard"
	"aiops/internal/store"
)

func createPGDatabase(t *testing.T, env *testEnv) store.Database {
	t.Helper()
	body := `{"name":"orders","engine":"postgres","host":"db.internal","database_name":"orders","username":"ro","password":"dbpass","ssl_mode":"require"}`
	rec := env.do(http.MethodPost, "/api/databases", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create database status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var db store.Database
	decodeBody(t, rec, &db)
	return db
}

func TestDatabaseCRUD(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(http.MethodGet, "/api/databases", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty list = %d %s", rec.Code, rec.Body.String())
	}

	db := createPGDatabase(t, env)
	if db.Port != 5432 || db.SSLMode != store.SSLModeRequire || db.Name != "orders" {
		t.Fatalf("db = %+v", db)
	}
	rec = env.do(http.MethodGet, "/api/databases/"+db.ID, "")
	if !strings.Contains(rec.Body.String(), `"has_password":true`) {
		t.Fatalf("has_password missing: %s", rec.Body.String())
	}

	// MySQL 默认端口。
	rec = env.do(http.MethodPost, "/api/databases",
		`{"name":"cache","engine":"mysql","host":"m","database_name":"c","username":"u"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mysql create = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"port":3306`) {
		t.Fatalf("mysql default port missing: %s", rec.Body.String())
	}

	// SQLite。
	rec = env.do(http.MethodPost, "/api/databases",
		`{"name":"local","engine":"sqlite","sqlite_path":"/var/lib/aiops/local.db"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("sqlite create = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ssl_mode":"disable"`) {
		t.Fatalf("sqlite ssl default: %s", rec.Body.String())
	}

	// 详情 / 更新 / 删除。
	rec = env.do(http.MethodGet, "/api/databases/"+db.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
	rec = env.do(http.MethodGet, "/api/databases/ghost", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing = %d", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "DATABASE_NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}

	rec = env.do(http.MethodPatch, "/api/databases/"+db.ID, `{"name":"orders2","port":5433}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"port":5433`) {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}

	env.dbExec.calls = nil
	rec = env.do(http.MethodDelete, "/api/databases/"+db.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodDelete, "/api/databases/"+db.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("double delete = %d", rec.Code)
	}
}

func TestCreateDatabaseValidation(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name string
		body string
		code string
	}{
		{"empty name", `{"name":"","engine":"postgres","host":"h","database_name":"d","username":"u"}`, "invalid name"},
		{"unknown engine", `{"name":"n","engine":"oracle"}`, "engine must be"},
		{"sqlite memory", `{"name":"n","engine":"sqlite","sqlite_path":":memory:"}`, "invalid sqlite_path"},
		{"sqlite relative", `{"name":"n","engine":"sqlite","sqlite_path":"data/x.db"}`, "invalid sqlite_path"},
		{"sqlite empty path", `{"name":"n","engine":"sqlite","sqlite_path":"  "}`, "invalid sqlite_path"},
		{"sqlite path too long", `{"name":"n","engine":"sqlite","sqlite_path":"/` + strings.Repeat("d", 4096) + `"}`, "invalid sqlite_path"},
		{"sqlite with host", `{"name":"n","engine":"sqlite","sqlite_path":"/tmp/a.db","host":"h"}`, "sqlite does not accept"},
		{"sqlite with port", `{"name":"n","engine":"sqlite","sqlite_path":"/tmp/a.db","port":5432}`, "sqlite does not accept"},
		{"sqlite with username", `{"name":"n","engine":"sqlite","sqlite_path":"/tmp/a.db","username":"u"}`, "sqlite does not accept"},
		{"sqlite with database_name", `{"name":"n","engine":"sqlite","sqlite_path":"/tmp/a.db","database_name":"d"}`, "sqlite does not accept"},
		{"sqlite with password", `{"name":"n","engine":"sqlite","sqlite_path":"/tmp/a.db","password":"p"}`, "sqlite does not accept"},
		{"pg empty host", `{"name":"n","engine":"postgres","host":" ","database_name":"d","username":"u"}`, "invalid host"},
		{"pg empty database", `{"name":"n","engine":"postgres","host":"h","database_name":"","username":"u"}`, "invalid database_name"},
		{"pg empty username", `{"name":"n","engine":"postgres","host":"h","database_name":"d","username":""}`, "invalid username"},
		{"pg with sqlite_path", `{"name":"n","engine":"postgres","host":"h","database_name":"d","username":"u","sqlite_path":"/tmp/x"}`, "sqlite_path is only valid"},
		{"pg bad port", `{"name":"n","engine":"postgres","host":"h","database_name":"d","username":"u","port":0}`, "invalid port"},
		{"bad ssl_mode", `{"name":"n","engine":"postgres","host":"h","database_name":"d","username":"u","ssl_mode":"bogus"}`, "invalid ssl_mode"},
		{"unknown field", `{"name":"n","engine":"postgres","host":"h","database_name":"d","username":"u","x":1}`, "invalid request body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodPost, "/api/databases", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			_, msg := errorBody(t, rec)
			if !strings.Contains(msg, tc.code) {
				t.Fatalf("message = %q, want contains %q", msg, tc.code)
			}
		})
	}

	// 重名 → 409，且 message 必须指出冲突在名称字段（第 4 轮打磨：
	// 与 servers/providers 对齐，前端 apiError 据此拼接具体原因）。
	createPGDatabase(t, env)
	rec := env.do(http.MethodPost, "/api/databases",
		`{"name":"orders","engine":"postgres","host":"h2","database_name":"d","username":"u"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict = %d %s", rec.Code, rec.Body.String())
	}
	if _, msg := errorBody(t, rec); msg != "database name already exists" {
		t.Fatalf("conflict message = %q, want %q", msg, "database name already exists")
	}

	// Store 缺失 → 503。
	c, rec2 := withParam(httptestPost("/api/databases", `{}`), "", "")
	c.Params = nil
	(&Deps{}).handleCreateDatabase(c)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store = %d", rec2.Code)
	}
}

func TestUpdateDatabaseValidation(t *testing.T) {
	env := newTestEnv(t)
	pg := createPGDatabase(t, env)
	rec := env.do(http.MethodPost, "/api/databases", `{"name":"local","engine":"sqlite","sqlite_path":"/tmp/a.db"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("sqlite create = %d %s", rec.Code, rec.Body.String())
	}
	var sqliteDB store.Database
	decodeBody(t, rec, &sqliteDB)

	cases := []struct {
		name string
		id   string
		body string
		code string
	}{
		{"no fields", pg.ID, `{}`, "at least one field is required"},
		{"empty name", pg.ID, `{"name":""}`, "invalid name"},
		{"bad host", pg.ID, `{"host":"a b"}`, "invalid host"},
		{"bad port", pg.ID, `{"port":70000}`, "invalid port"},
		{"empty database_name", pg.ID, `{"database_name":" "}`, "invalid database_name"},
		{"empty username", pg.ID, `{"username":" "}`, "invalid username"},
		{"pg rejects sqlite_path", pg.ID, `{"sqlite_path":"/tmp/x"}`, "sqlite_path is only valid"},
		{"bad ssl_mode", pg.ID, `{"ssl_mode":"nope"}`, "invalid ssl_mode"},
		{"sqlite rejects host", sqliteDB.ID, `{"host":"h"}`, "sqlite does not accept"},
		{"sqlite rejects port", sqliteDB.ID, `{"port":1}`, "sqlite does not accept"},
		{"sqlite rejects database_name", sqliteDB.ID, `{"database_name":"d"}`, "sqlite does not accept"},
		{"sqlite rejects username", sqliteDB.ID, `{"username":"u"}`, "sqlite does not accept"},
		{"sqlite rejects password", sqliteDB.ID, `{"password":"p"}`, "sqlite does not accept"},
		{"sqlite bad path", sqliteDB.ID, `{"sqlite_path":"rel/x"}`, "invalid sqlite_path"},
		{"missing database", "ghost", `{"name":"x"}`, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodPatch, "/api/databases/"+tc.id, tc.body)
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			_, msg := errorBody(t, rec)
			if !strings.Contains(msg, tc.code) {
				t.Fatalf("message = %q, want contains %q", msg, tc.code)
			}
		})
	}

	// 更新密码（pg）→ has_password true。
	rec = env.do(http.MethodPatch, "/api/databases/"+pg.ID, `{"password":"newpass"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"has_password":true`) {
		t.Fatalf("password update = %d %s", rec.Code, rec.Body.String())
	}
	// sqlite 上改 ssl_mode 被忽略（不作为字段）→ 无字段错误。
	rec = env.do(http.MethodPatch, "/api/databases/"+sqliteDB.ID, `{"ssl_mode":"require"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sqlite ssl_mode only = %d %s", rec.Code, rec.Body.String())
	}
	// sqlite 换路径。
	rec = env.do(http.MethodPatch, "/api/databases/"+sqliteDB.ID, `{"sqlite_path":"/tmp/b.db"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/tmp/b.db") {
		t.Fatalf("sqlite path update = %d %s", rec.Code, rec.Body.String())
	}

	// 更新撞重名（把 orders 改成已存在的 local）→ 409 + 名称冲突文案。
	rec = env.do(http.MethodPatch, "/api/databases/"+pg.ID, `{"name":"local"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("update conflict = %d %s", rec.Code, rec.Body.String())
	}
	if _, msg := errorBody(t, rec); msg != "database name already exists" {
		t.Fatalf("update conflict message = %q, want %q", msg, "database name already exists")
	}
}

func TestDatabaseTestConnection(t *testing.T) {
	env := newTestEnv(t)
	db := createPGDatabase(t, env)

	rec := env.do(http.MethodPost, "/api/databases/"+db.ID+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"server_version":"PostgreSQL 16.2"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}

	// 超时 → 504。
	env.dbExec.testFn = func(context.Context, store.Database, string) (dbproxy.TestResult, error) {
		return dbproxy.TestResult{}, context.DeadlineExceeded
	}
	rec = env.do(http.MethodPost, "/api/databases/"+db.ID+"/test", "")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout = %d %s", rec.Code, rec.Body.String())
	}

	// 通用失败 → 502。
	env.dbExec.testFn = func(context.Context, store.Database, string) (dbproxy.TestResult, error) {
		return dbproxy.TestResult{}, errors.New("connect: connection refused")
	}
	rec = env.do(http.MethodPost, "/api/databases/"+db.ID+"/test", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("generic = %d", rec.Code)
	}
	if code, _ := errorBody(t, rec); code != "DATABASE_TEST_FAILED" {
		t.Fatalf("code = %s", code)
	}

	// 解密失败 → 500。
	env.dbExec.testFn = func(context.Context, store.Database, string) (dbproxy.TestResult, error) {
		return dbproxy.TestResult{LatencyMS: 1}, nil
	}
	stored, err := env.st.Databases().Get(context.Background(), db.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.PasswordEncrypted = "not-a-v1-ciphertext"
	env.st.mu.Lock()
	env.st.databases[db.ID] = stored
	env.st.mu.Unlock()
	rec = env.do(http.MethodPost, "/api/databases/"+db.ID+"/test", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("decrypt fail = %d %s", rec.Code, rec.Body.String())
	}

	// 不存在 → 404。
	rec = env.do(http.MethodPost, "/api/databases/ghost/test", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing db test = %d", rec.Code)
	}

	// 执行器缺失 → 503。
	c, rec2 := withParam(httptestPost("/api/databases/x/test", ""), "id", db.ID)
	(&Deps{Store: env.st, Clock: time.Now}).handleTestDatabase(c)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil executor = %d", rec2.Code)
	}
}

func TestDatabaseQuery(t *testing.T) {
	env := newTestEnv(t)
	db := createPGDatabase(t, env)

	// 正常只读查询。
	rec := env.do(http.MethodPost, "/api/databases/"+db.ID+"/query", `{"sql":"SELECT id FROM orders"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("query = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"row_count":1`) {
		t.Fatalf("body = %s", rec.Body.String())
	}

	// 非只读 → 422，message 中文并带守卫规则标识。
	env.dbExec.queryFn = func(context.Context, store.Database, string, dbproxy.QueryRequest) (dbproxy.QueryResult, error) {
		return dbproxy.QueryResult{}, &dbproxy.NotReadOnlyError{Result: guardResult("SQL_GUARD_DML")}
	}
	rec = env.do(http.MethodPost, "/api/databases/"+db.ID+"/query", `{"sql":"DELETE FROM orders"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("not readonly = %d %s", rec.Code, rec.Body.String())
	}
	if code, msg := errorBody(t, rec); code != "SQL_NOT_READONLY" || msg != "查询被只读守卫拒绝：SQL_GUARD_DML" {
		t.Fatalf("not readonly error = %s %q", code, msg)
	}

	// 超时 → 504，message 中文。
	env.dbExec.queryFn = func(context.Context, store.Database, string, dbproxy.QueryRequest) (dbproxy.QueryResult, error) {
		return dbproxy.QueryResult{}, dbproxy.ErrTimeout
	}
	rec = env.do(http.MethodPost, "/api/databases/"+db.ID+"/query", `{"sql":"SELECT 1"}`)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout = %d %s", rec.Code, rec.Body.String())
	}
	if code, msg := errorBody(t, rec); code != "SQL_TIMEOUT" || msg != "查询超时" {
		t.Fatalf("timeout error = %s %q", code, msg)
	}

	// 通用失败 → 502，message 透出底层原因（round 3：不再是无信息量英文裸串）。
	env.dbExec.queryFn = func(context.Context, store.Database, string, dbproxy.QueryRequest) (dbproxy.QueryResult, error) {
		return dbproxy.QueryResult{}, errors.New("backend closed")
	}
	rec = env.do(http.MethodPost, "/api/databases/"+db.ID+"/query", `{"sql":"SELECT 1"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("generic = %d %s", rec.Code, rec.Body.String())
	}
	if code, msg := errorBody(t, rec); code != "SQL_QUERY_FAILED" || msg != "查询失败：backend closed" {
		t.Fatalf("generic error = %s %q", code, msg)
	}

	// 校验。
	cases := []struct {
		name string
		body string
		code string
	}{
		{"empty sql", `{"sql":"  "}`, "sql is required"},
		{"sql too large", `{"sql":"` + strings.Repeat("s", 65537) + `"}`, "sql exceeds"},
		{"unknown field", `{"sql":"SELECT 1","x":1}`, "invalid request body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodPost, "/api/databases/"+db.ID+"/query", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			_, msg := errorBody(t, rec)
			if !strings.Contains(msg, tc.code) {
				t.Fatalf("message = %q", msg)
			}
		})
	}

	// 数据库不存在 → 404。
	rec = env.do(http.MethodPost, "/api/databases/ghost/query", `{"sql":"SELECT 1"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing db query = %d", rec.Code)
	}

	// 执行器缺失 → 503。
	c, rec2 := withParam(httptestPost("/api/databases/x/query", `{"sql":"SELECT 1"}`), "id", db.ID)
	(&Deps{Store: env.st, Clock: time.Now}).handleQueryDatabase(c)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil executor = %d", rec2.Code)
	}
}

func guardResult(rule string) sqlguard.Result {
	return sqlguard.Result{Allowed: false, Kind: sqlguard.KindDelete, RuleID: rule}
}

// TestCreateDatabaseMSSQLRedis W2（二期冲刺 1）：mssql/redis 引擎此前在
// handleCreateDatabase 落入 default 分支 400（"engine must be postgres,
// mysql or sqlite"），前端表单整条流程第一步被打死。这里锁定新分支的
// 正常路径与契约校验（契约 2.6/4.4）。
func TestCreateDatabaseMSSQLRedis(t *testing.T) {
	env := newTestEnv(t)

	// mssql 端口直连（默认 1433）+ 密码必填。
	rec := env.do(http.MethodPost, "/api/databases",
		`{"name":"win-sql","engine":"mssql","host":"10.0.0.9","database_name":"master","username":"sa","password":"SecretPass99"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mssql create = %d %s", rec.Code, rec.Body.String())
	}
	var mssqlDB store.Database
	decodeBody(t, rec, &mssqlDB)
	if mssqlDB.Port != 1433 || mssqlDB.InstanceName != "" || !mssqlDB.HasPassword {
		t.Fatalf("mssql defaults = %+v", mssqlDB)
	}

	// mssql 命名实例：port 置 0。
	rec = env.do(http.MethodPost, "/api/databases",
		`{"name":"win-sql-inst","engine":"mssql","host":"10.0.0.9","instance_name":"SQLEXPRESS","database_name":"master","username":"sa","password":"p"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mssql instance create = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"port":0`) ||
		!strings.Contains(rec.Body.String(), `"instance_name":"SQLEXPRESS"`) {
		t.Fatalf("mssql instance body = %s", rec.Body.String())
	}

	// redis：host/port 必填、redis_db 默认 0、密码可选。
	rec = env.do(http.MethodPost, "/api/databases",
		`{"name":"cache-1","engine":"redis","host":"10.0.0.10","port":6380,"username":"","redis_db":7,"password":"auth"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("redis create = %d %s", rec.Code, rec.Body.String())
	}
	var redisDB store.Database
	decodeBody(t, rec, &redisDB)
	if redisDB.Port != 6380 || redisDB.RedisDB != 7 || redisDB.DatabaseName != "" || !redisDB.HasPassword {
		t.Fatalf("redis fields = %+v", redisDB)
	}
	// redis 默认端口 6379 / redis_db 0。
	rec = env.do(http.MethodPost, "/api/databases",
		`{"name":"cache-2","engine":"redis","host":"10.0.0.10"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("redis minimal create = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"port":6379`) ||
		!strings.Contains(rec.Body.String(), `"redis_db":0`) ||
		!strings.Contains(rec.Body.String(), `"has_password":false`) {
		t.Fatalf("redis defaults = %s", rec.Body.String())
	}

	// 校验矩阵（契约 2.6 分支约束）。
	cases := []struct {
		name string
		body string
		code string
	}{
		{"mssql missing password",
			`{"name":"x1","engine":"mssql","host":"h","database_name":"d","username":"u"}`, "mssql requires a password"},
		{"mssql empty password",
			`{"name":"x1","engine":"mssql","host":"h","database_name":"d","username":"u","password":""}`, "mssql requires a password"},
		{"mssql missing database_name",
			`{"name":"x1","engine":"mssql","host":"h","username":"u","password":"p"}`, "invalid database_name"},
		{"mssql missing username",
			`{"name":"x1","engine":"mssql","host":"h","database_name":"d","password":"p"}`, "invalid username"},
		{"mssql instance with port",
			`{"name":"x1","engine":"mssql","host":"h","instance_name":"SQLEXPRESS","port":1433,"database_name":"d","username":"u","password":"p"}`,
			"instance_name and port are mutually exclusive"},
		{"mssql bad port",
			`{"name":"x1","engine":"mssql","host":"h","port":70000,"database_name":"d","username":"u","password":"p"}`, "invalid port"},
		{"mssql with sqlite_path",
			`{"name":"x1","engine":"mssql","host":"h","database_name":"d","username":"u","password":"p","sqlite_path":"/tmp/x"}`,
			"sqlite_path is only valid"},
		{"mssql with redis_db",
			`{"name":"x1","engine":"mssql","host":"h","database_name":"d","username":"u","password":"p","redis_db":1}`,
			"redis_db is only valid"},
		{"mssql instance too long",
			`{"name":"x1","engine":"mssql","host":"h","instance_name":"` + strings.Repeat("i", 101) + `","database_name":"d","username":"u","password":"p"}`,
			"invalid instance_name"},
		{"redis missing host",
			`{"name":"x2","engine":"redis","port":6379}`, "invalid host"},
		{"redis bad port",
			`{"name":"x2","engine":"redis","host":"h","port":0}`, "invalid port"},
		{"redis with database_name",
			`{"name":"x2","engine":"redis","host":"h","port":6379,"database_name":"d"}`, "redis does not accept database_name"},
		{"redis redis_db over 1023",
			`{"name":"x2","engine":"redis","host":"h","port":6379,"redis_db":1024}`, "redis_db must be between 0 and 1023"},
		{"redis redis_db negative",
			`{"name":"x2","engine":"redis","host":"h","port":6379,"redis_db":-1}`, "redis_db must be between 0 and 1023"},
		{"redis with sqlite_path",
			`{"name":"x2","engine":"redis","host":"h","port":6379,"sqlite_path":"/tmp/x"}`, "sqlite_path is only valid"},
		{"redis with instance_name",
			`{"name":"x2","engine":"redis","host":"h","port":6379,"instance_name":"inst"}`, "instance_name is only valid"},
		{"pg with instance_name",
			`{"name":"x3","engine":"postgres","host":"h","database_name":"d","username":"u","instance_name":"inst"}`, "instance_name is only valid"},
		{"pg with redis_db",
			`{"name":"x3","engine":"postgres","host":"h","database_name":"d","username":"u","redis_db":1}`, "redis_db is only valid"},
		{"sqlite with instance_name",
			`{"name":"x4","engine":"sqlite","sqlite_path":"/tmp/a.db","instance_name":"inst"}`, "instance_name is only valid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(http.MethodPost, "/api/databases", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			_, msg := errorBody(t, rec)
			if !strings.Contains(msg, tc.code) {
				t.Fatalf("message = %q, want contains %q", msg, tc.code)
			}
		})
	}
}

// TestUpdateDatabaseMSSQLRedis W2：mssql/redis 的 PATCH 分支与
// instance↔port 互斥的合并视图校验。
func TestUpdateDatabaseMSSQLRedis(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(http.MethodPost, "/api/databases",
		`{"name":"win-sql","engine":"mssql","host":"10.0.0.9","database_name":"master","username":"sa","password":"p"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mssql create = %d %s", rec.Code, rec.Body.String())
	}
	var mssqlDB store.Database
	decodeBody(t, rec, &mssqlDB)

	// 补 instance → port 自动语义由调用方负责置 0；只改 instance 而留
	// 旧端口非 0 → 互斥 400（合并视图校验）。
	rec = env.do(http.MethodPatch, "/api/databases/"+mssqlDB.ID, `{"instance_name":"SQLEXPRESS"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("instance without port zero = %d %s", rec.Code, rec.Body.String())
	}
	if _, msg := errorBody(t, rec); !strings.Contains(msg, "mutually exclusive") {
		t.Fatalf("message = %q", msg)
	}
	// 同时改 instance + port=0 → 200。
	rec = env.do(http.MethodPatch, "/api/databases/"+mssqlDB.ID,
		`{"instance_name":"SQLEXPRESS","port":0}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"port":0`) {
		t.Fatalf("instance update = %d %s", rec.Code, rec.Body.String())
	}
	// 已有 instance 时单改 port 非 0 → 400。
	rec = env.do(http.MethodPatch, "/api/databases/"+mssqlDB.ID, `{"port":1433}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("port with instance = %d %s", rec.Code, rec.Body.String())
	}
	// 清 instance 恢复直连端口。
	rec = env.do(http.MethodPatch, "/api/databases/"+mssqlDB.ID,
		`{"instance_name":"","port":1433}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"port":1433`) {
		t.Fatalf("clear instance = %d %s", rec.Code, rec.Body.String())
	}
	// mssql 拒绝 redis_db。
	rec = env.do(http.MethodPatch, "/api/databases/"+mssqlDB.ID, `{"redis_db":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mssql rejects redis_db = %d %s", rec.Code, rec.Body.String())
	}

	// redis 更新。
	rec = env.do(http.MethodPost, "/api/databases",
		`{"name":"cache","engine":"redis","host":"10.0.0.10","port":6379}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("redis create = %d %s", rec.Code, rec.Body.String())
	}
	var redisDB store.Database
	decodeBody(t, rec, &redisDB)

	rec = env.do(http.MethodPatch, "/api/databases/"+redisDB.ID, `{"redis_db":9,"port":6380}`)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"redis_db":9`) ||
		!strings.Contains(rec.Body.String(), `"port":6380`) {
		t.Fatalf("redis update = %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodPatch, "/api/databases/"+redisDB.ID, `{"redis_db":2000}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("redis_db over = %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodPatch, "/api/databases/"+redisDB.ID, `{"database_name":"d"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("redis rejects database_name = %d %s", rec.Code, rec.Body.String())
	}
}

// TestWriteDatabaseStoreError 第 4 轮打磨：databases 创建/更新的唯一约束
// 只有 name（migrations 00001 databases_name_key），409 必须返回具体
// message（对齐 servers 的「服务器名称已存在」），前端 apiError
// 机制据此告知用户冲突在名称字段；其余错误码维持通用映射不变。
func TestWriteDatabaseStoreError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{
			name:       "conflict reports duplicate name",
			err:        store.ErrConflict,
			wantStatus: http.StatusConflict,
			wantCode:   "CONFLICT",
			wantMsg:    "database name already exists",
		},
		{
			name:       "not found keeps generic mapping",
			err:        store.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "DATABASE_NOT_FOUND",
			wantMsg:    "not found",
		},
		{
			name:       "invalid keeps generic mapping",
			err:        store.ErrInvalid,
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_FAILED",
			wantMsg:    "invalid argument",
		},
		{
			name:       "wrapped conflict still matches",
			err:        errors.Join(store.ErrConflict, errors.New("23505")),
			wantStatus: http.StatusConflict,
			wantCode:   "CONFLICT",
			wantMsg:    "database name already exists",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Deps{}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Set("request_id", "test-req")
			d.writeDatabaseStoreError(c, tc.err)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
			}
			if body.Error.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", body.Error.Code, tc.wantCode)
			}
			if body.Error.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", body.Error.Message, tc.wantMsg)
			}
		})
	}
}
