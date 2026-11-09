//go:build integration_mongodb

// MongoDB 执行器集成测试（G26，CONTRACT.md 9.2）：容器（mongo:8，只读账号）
// 实测 find/aggregate/count/distinct/estimatedDocumentCount、行数 1000/
// 字节 1MB 截断、maxTimeMS 双保险、auth_db（AuthSource）传递、ssl_mode
// 三态映射、无认证/有认证两种建库路径、Test 返回 buildInfo version。
// 纯逻辑（守卫）部分在 internal/sqlguard/mongo_test.go，不标注 tag。
//
// 环境变量：
//
//	AIOPS_MONGODB_TEST_ADMIN_URI   管理员连接串（建库、建只读账号、种子数据）
//	AIOPS_MONGODB_TEST_NOAUTH_URI  可选；提供时覆盖无认证建库路径
//
// 容器示例：
//
//	docker run -d --rm --name aiops-mongo-test -p 27018:27017 mongo:8
//	AIOPS_MONGODB_TEST_ADMIN_URI=mongodb://127.0.0.1:27018 \
//	AIOPS_MONGODB_TEST_NOAUTH_URI=mongodb://127.0.0.1:27018 \
//	go test -tags integration_mongodb ./internal/dbproxy -run Mongo -v
package dbproxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"aiops/internal/store"
)

const (
	mongoTestDB       = "aiops_guard_test"
	mongoTestReadOnly = "guard_ro"
	mongoTestPassword = "ro-password"
)

func mongoTestAdminURI(t *testing.T) string {
	t.Helper()
	uri := os.Getenv("AIOPS_MONGODB_TEST_ADMIN_URI")
	if uri == "" {
		t.Fatal("AIOPS_MONGODB_TEST_ADMIN_URI is required for integration_mongodb tests (contract 9.2: must fail, not skip)")
	}
	return uri
}

// mongoTestSetup 建库、只读账号与种子数据，返回认证用的 store.Database。
func mongoTestSetup(t *testing.T) store.Database {
	t.Helper()
	uri := mongoTestAdminURI(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("admin ping: %v", err)
	}

	db := client.Database(mongoTestDB)
	_ = db.Drop(ctx) // 幂等重跑

	// 只读账号：read 角色仅授予测试库（真正的只读闸之一，契约 5.4）。
	// 用户建在 admin（与连接的 AuthSource=admin 对应），roles 限定测试库。
	_ = client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "dropUser", Value: mongoTestReadOnly},
	}).Err()
	if err := client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: mongoTestReadOnly},
		{Key: "pwd", Value: mongoTestPassword},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: mongoTestDB}}}},
	}).Err(); err != nil {
		t.Fatalf("createUser: %v", err)
	}

	// orders：find/filter/sort/skip/count/distinct/aggregate 的种子。
	// bson.D 显式键序（结果列序断言依赖首行键序）；city 用 i%5 与
	// status(i%3) 去相关，保证 $group/distinct 有多组。
	orders := make([]any, 0, 21)
	for i := 0; i < 21; i++ {
		orders = append(orders, bson.D{
			{Key: "seq", Value: i},
			{Key: "status", Value: []string{"paid", "pending", "shipped"}[i%3]},
			{Key: "city", Value: []string{"sz", "sh", "bj", "gz", "hz"}[i%5]},
			{Key: "amount", Value: float64(i) * 1.5},
		})
	}
	if _, err := db.Collection("orders").InsertMany(ctx, orders); err != nil {
		t.Fatalf("seed orders: %v", err)
	}

	// big：1050 条小文档，验证行数 1000 截断。
	big := make([]any, 0, 1050)
	for i := 0; i < 1050; i++ {
		big = append(big, bson.M{"n": i, "pad": strings.Repeat("p", 16)})
	}
	if _, err := db.Collection("big").InsertMany(ctx, big); err != nil {
		t.Fatalf("seed big: %v", err)
	}

	// wide：单条 ~600KB 的文档 ×3，验证 1MB 字节截断（两条即越限）。
	wide := make([]any, 0, 3)
	for i := 0; i < 3; i++ {
		wide = append(wide, bson.M{"blob": strings.Repeat(fmt.Sprint(i), 600000)})
	}
	if _, err := db.Collection("wide").InsertMany(ctx, wide); err != nil {
		t.Fatalf("seed wide: %v", err)
	}

	// heavy：3 条 8MB 文档，maxTimeMS=1 的全扫必然超时。
	heavy := make([]any, 0, 3)
	for i := 0; i < 3; i++ {
		heavy = append(heavy, bson.M{"blob": strings.Repeat("h", 8<<20)})
	}
	if _, err := db.Collection("heavy").InsertMany(ctx, heavy); err != nil {
		t.Fatalf("seed heavy: %v", err)
	}

	host, port := hostPortFromURI(t, uri)
	return store.Database{
		Engine:       store.EngineMongoDB,
		Host:         host,
		Port:         port,
		DatabaseName: mongoTestDB,
		Username:     mongoTestReadOnly,
		AuthDB:       "admin",
	}
}

func hostPortFromURI(t *testing.T, uri string) (string, int) {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse uri: %v", err)
	}
	host := u.Hostname()
	port := 27017
	if p := u.Port(); p != "" {
		if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
			t.Fatalf("parse port: %v", err)
		}
	}
	return host, port
}

func TestMongoBackendIntegration(t *testing.T) {
	dbAsset := mongoTestSetup(t)
	pool := New(Config{}, slog.Default())
	defer pool.Close()
	ctx := context.Background()

	// ---- Test：hello+ping 连接测试，返回 buildInfo version ----
	testRes, err := pool.Test(ctx, dbAsset, mongoTestPassword)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !strings.HasPrefix(testRes.ServerVersion, "8.") {
		t.Errorf("server_version = %q; want 8.x (mongo:8 容器)", testRes.ServerVersion)
	}
	if testRes.LatencyMS < 0 {
		t.Errorf("latency_ms = %d", testRes.LatencyMS)
	}

	query := func(sqlText string) QueryResult {
		t.Helper()
		res, err := pool.Query(ctx, dbAsset, mongoTestPassword, QueryRequest{SQL: sqlText})
		if err != nil {
			t.Fatalf("Query(%s): %v", sqlText, err)
		}
		return res
	}

	// ---- find：filter/sort/limit/skip，列序 = 首行键序，ObjectID 字符串化 ----
	res := query(`{"find":"orders","filter":{"status":"paid"},"sort":{"seq":-1},"limit":3,"skip":1,"projection":{"_id":0}}`)
	if len(res.Columns) < 3 || res.Columns[0].Name != "seq" {
		t.Errorf("find columns = %+v; want 首行键序（projection 后 seq 在前）", res.Columns)
	}
	if res.RowCount != 3 || res.Truncated {
		t.Errorf("find paid skip1 limit3 → rows=%d truncated=%v", res.RowCount, res.Truncated)
	}

	res = query(`{"find":"orders","limit":2}`)
	if len(res.Columns) == 0 || res.Columns[0].Name != "_id" {
		t.Fatalf("find 默认列序应以 _id 开头，got %+v", res.Columns)
	}
	oid, _ := res.Rows[0][0].(string)
	if len(oid) != 24 {
		t.Errorf("_id 应为 24 位 hex 字符串，got %v", res.Rows[0][0])
	}

	// ---- aggregate：$match + $group 分组 ----
	res = query(`{"aggregate":"orders","pipeline":[{"$match":{"status":"paid"}},{"$group":{"_id":"$city","n":{"$sum":1}}},{"$sort":{"_id":1}}]}`)
	if res.RowCount != 5 || res.Columns[0].Name != "_id" {
		t.Errorf("aggregate group by city → %d rows %v; want 5（city 取模 5、status 取模 3 去相关）", res.RowCount, res.Columns)
	}

	// ---- count / distinct / estimatedDocumentCount ----
	res = query(`{"count":"orders","query":{"status":"paid"}}`)
	if res.RowCount != 1 || len(res.Rows) != 1 || res.Rows[0][0].(int64) != 7 {
		t.Errorf("count paid = %+v; want 7", res.Rows)
	}
	res = query(`{"distinct":"orders","key":"city"}`)
	if res.RowCount != 5 || res.Columns[0].Name != "value" {
		t.Errorf("distinct city → %+v", res)
	}
	res = query(`{"estimatedDocumentCount":"orders"}`)
	if res.RowCount != 1 || res.Rows[0][0].(int64) != 21 {
		t.Errorf("estimatedDocumentCount = %+v; want 21", res.Rows)
	}

	// ---- 行数 1000 截断 ----
	res = query(`{"find":"big"}`)
	if res.RowCount != 1000 || !res.Truncated {
		t.Errorf("find big → rows=%d truncated=%v; want 1000/true", res.RowCount, res.Truncated)
	}

	// ---- 用户 limit 与请求级行数上限取更严者 ----
	res = query(`{"find":"big","limit":5}`)
	if res.RowCount != 5 || res.Truncated {
		t.Errorf("find big limit5 → rows=%d truncated=%v; want 5/false", res.RowCount, res.Truncated)
	}

	// ---- 字节 1MB 截断：单文档 600KB，第二条越限 ----
	res = query(`{"find":"wide"}`)
	if res.RowCount != 1 || !res.Truncated {
		t.Errorf("find wide → rows=%d truncated=%v; want 1/true（1MB 字节上限）", res.RowCount, res.Truncated)
	}

	// ---- maxTimeMS 双保险：24MB 全扫 + maxTimeMS=1 → SQL_TIMEOUT ----
	_, err = pool.Query(ctx, dbAsset, mongoTestPassword, QueryRequest{
		SQL: `{"find":"heavy","maxTimeMS":1}`,
	})
	if err == nil {
		t.Fatalf("maxTimeMS=1 全扫 24MB 必须触发服务端硬停")
	}
	if !strings.Contains(err.Error(), ErrTimeout.Error()) {
		t.Errorf("maxTimeMS 超时应映射 ErrTimeout，got %v", err)
	}

	// ---- 写命令在执行器侧被守卫拒绝（第三道闸） ----
	_, err = pool.Query(ctx, dbAsset, mongoTestPassword, QueryRequest{SQL: `{"drop":"orders"}`})
	var nre *NotReadOnlyError
	if err == nil || !strings.Contains(fmt.Sprint(err), "not read-only") {
		t.Errorf("drop 命令必须被执行器守卫拒绝，got %v", err)
	}
	_ = nre

	// ---- Invalidate 后重建池仍可用 ----
	pool.Invalidate(dbAsset.ID)
	if _, err := pool.Test(ctx, dbAsset, mongoTestPassword); err != nil {
		t.Errorf("Invalidate 后 Test 失败: %v", err)
	}
}

// TestMongoBackendNoAuth 覆盖无认证建库路径（契约 G26：无认证/有认证两种）。
func TestMongoBackendNoAuth(t *testing.T) {
	uri := os.Getenv("AIOPS_MONGODB_TEST_NOAUTH_URI")
	if uri == "" {
		t.Fatal("AIOPS_MONGODB_TEST_NOAUTH_URI is required for the no-auth path (contract 9.2 G26)")
	}
	host, port := hostPortFromURI(t, uri)
	dbAsset := store.Database{
		Engine:       store.EngineMongoDB,
		Host:         host,
		Port:         port,
		DatabaseName: mongoTestDB,
		Username:     "", // 无认证：username/password 同空
		AuthDB:       "",
	}
	pool := New(Config{}, slog.Default())
	defer pool.Close()

	res, err := pool.Test(context.Background(), dbAsset, "")
	if err != nil {
		t.Fatalf("no-auth Test: %v", err)
	}
	if res.ServerVersion == "" {
		t.Errorf("no-auth server_version 为空")
	}

	q, err := pool.Query(context.Background(), dbAsset, "", QueryRequest{
		SQL: `{"estimatedDocumentCount":"orders"}`,
	})
	if err != nil || q.RowCount != 1 {
		t.Errorf("no-auth Query: %+v %v", q, err)
	}
}

// TestMongoSSLModeMapping 覆盖 ssl_mode 三态映射（G26）。
func TestMongoSSLModeMapping(t *testing.T) {
	if mongoTLSConfig(store.SSLModeDisable) != nil || mongoTLSConfig(store.SSLModePrefer) != nil {
		t.Error("disable/prefer 必须无 TLS")
	}
	if c := mongoTLSConfig(store.SSLModeRequire); c == nil || !c.InsecureSkipVerify {
		t.Error("require 必须 TLS 跳过证书校验")
	}
	if c := mongoTLSConfig(store.SSLModeVerifyCA); c == nil || c.InsecureSkipVerify {
		t.Error("verify-ca 必须系统信任链校验")
	}
	if c := mongoTLSConfig(store.SSLModeVerifyFull); c == nil || c.InsecureSkipVerify {
		t.Error("verify-full 必须系统信任链校验")
	}
}

// TestMongoBackendAuthSource 在启用访问控制的容器上实测 AuthSource 传递：
// 账号建在 admin，AuthDB=admin 连接成功、指向其他库认证失败。
//
// 容器示例：
//
//	docker run -d --rm --name aiops-mongo-auth -p 27019:27017 \
//	  -e MONGO_INITDB_ROOT_USERNAME=root -e MONGO_INITDB_ROOT_PASSWORD=example mongo:8
//	AIOPS_MONGODB_TEST_AUTH_URI=mongodb://root:example@127.0.0.1:27019 \
//	go test -tags integration_mongodb ./internal/dbproxy -run MongoBackendAuthSource -v
func TestMongoBackendAuthSource(t *testing.T) {
	uri := os.Getenv("AIOPS_MONGODB_TEST_AUTH_URI")
	if uri == "" {
		t.Fatal("AIOPS_MONGODB_TEST_AUTH_URI is required for the AuthSource test (auth-enabled container)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("root connect: %v", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("root ping: %v", err)
	}

	db := client.Database(mongoTestDB)
	_ = db.Drop(ctx)
	_ = client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "dropUser", Value: mongoTestReadOnly},
	}).Err()
	if err := client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: mongoTestReadOnly},
		{Key: "pwd", Value: mongoTestPassword},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "read"}, {Key: "db", Value: mongoTestDB}}}},
	}).Err(); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	if _, err := db.Collection("orders").InsertOne(ctx, bson.D{{Key: "seq", Value: 1}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	host, port := hostPortFromURI(t, uri)
	ro := store.Database{
		Engine:       store.EngineMongoDB,
		Host:         host,
		Port:         port,
		DatabaseName: mongoTestDB,
		Username:     mongoTestReadOnly,
		AuthDB:       "admin", // AuthSource 正向传递
	}
	pool := New(Config{}, slog.Default())
	defer pool.Close()

	if _, err := pool.Test(ctx, ro, mongoTestPassword); err != nil {
		t.Fatalf("AuthDB=admin 必须认证成功: %v", err)
	}
	if _, err := pool.Query(ctx, ro, mongoTestPassword, QueryRequest{
		SQL: `{"count":"orders"}`,
	}); err != nil {
		t.Fatalf("readonly query: %v", err)
	}

	// 账号建在 admin：AuthSource 指向其他库必然认证失败（auth_db 传递的反向实测）。
	wrong := ro
	wrong.AuthDB = mongoTestDB
	wrong.ID = "wrong-authdb" // 独立池条目，避免命中签名缓存
	if _, err := pool.Test(ctx, wrong, mongoTestPassword); err == nil {
		t.Errorf("错误的 auth_db 必须导致认证失败")
	}

	// 只读账号的写命令被服务端拒绝（第三道闸：服务端权限）。
	_, err = pool.Query(ctx, ro, mongoTestPassword, QueryRequest{SQL: `{"insert":"orders","documents":[{}]}`})
	if err == nil {
		t.Errorf("insert 必须先被守卫拒绝（NotReadOnlyError），不可能到达服务端")
	}
}
