package dbproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"aiops/internal/sqlguard"
	"aiops/internal/store"
)

func TestRedisTLSConfig(t *testing.T) {
	if got := redisTLSConfig(store.SSLModeDisable); got != nil {
		t.Fatalf("disable must not enable TLS, got %+v", got)
	}
	if got := redisTLSConfig(store.SSLModePrefer); got != nil {
		t.Fatalf("prefer must not enable TLS, got %+v", got)
	}
	if got := redisTLSConfig(""); got != nil {
		t.Fatalf("empty mode must not enable TLS, got %+v", got)
	}
	got := redisTLSConfig(store.SSLModeRequire)
	if got == nil || !got.InsecureSkipVerify {
		t.Fatalf("require must skip certificate verification, got %+v", got)
	}
	got = redisTLSConfig(store.SSLModeVerifyCA)
	if got == nil || got.InsecureSkipVerify {
		t.Fatalf("verify-ca must verify certificates, got %+v", got)
	}
	got = redisTLSConfig(store.SSLModeVerifyFull)
	if got == nil || got.InsecureSkipVerify {
		t.Fatalf("verify-full must verify certificates, got %+v", got)
	}
}

func TestNewRedisBackendValidation(t *testing.T) {
	cfg := Config{MaxConnsPerDatabase: 4, IdleTimeout: time.Minute, DialTimeout: time.Second}
	valid := func(mutate func(*store.Database)) store.Database {
		db := store.Database{Engine: store.EngineRedis, Host: "127.0.0.1", Port: 6379,
			SSLMode: store.SSLModeDisable}
		mutate(&db)
		return db
	}
	cases := []struct {
		name string
		db   store.Database
	}{
		{"missing host", valid(func(d *store.Database) { d.Host = "" })},
		{"port zero", valid(func(d *store.Database) { d.Port = 0 })},
		{"port too high", valid(func(d *store.Database) { d.Port = 65536 })},
		{"redis_db negative", valid(func(d *store.Database) { d.RedisDB = -1 })},
		{"redis_db too high", valid(func(d *store.Database) { d.RedisDB = 1024 })},
	}
	for _, tc := range cases {
		b, err := newRedisBackend(cfg, tc.db, "")
		if err == nil {
			b.Close()
			t.Fatalf("newRedisBackend(%s): want error, got nil", tc.name)
		}
		if !strings.HasPrefix(err.Error(), "dbproxy: redis") {
			t.Fatalf("newRedisBackend(%s) error = %v", tc.name, err)
		}
	}

	b, err := newRedisBackend(cfg, valid(func(d *store.Database) { d.RedisDB = 7 }), "pw")
	if err != nil {
		t.Fatalf("newRedisBackend(valid): %v", err)
	}
	defer b.Close()
	opts := b.client.Options()
	if opts.Addr != "127.0.0.1:6379" {
		t.Fatalf("addr = %q", opts.Addr)
	}
	if opts.DB != 7 { // G19：redis_db 必须传给连接选项
		t.Fatalf("options.DB = %d, want 7", opts.DB)
	}
	if opts.Password != "pw" || opts.Username != "" {
		t.Fatalf("credentials not passed: %q/%q", opts.Username, opts.Password)
	}
}

func TestParseRedisVersion(t *testing.T) {
	v, ok := parseRedisVersion("# Server\r\nredis_version:7.4.1\r\nredis_mode:standalone\r\n")
	if !ok || v != "7.4.1" {
		t.Fatalf("parseRedisVersion = %q/%v, want 7.4.1/true", v, ok)
	}
	if _, ok := parseRedisVersion("# Clients\r\nconnected_clients:1\r\n"); ok {
		t.Fatal("parseRedisVersion must fail without redis_version")
	}
}

// redisTestBackend 起一个 miniredis 并返回指向它的 backend 与 store 描述。
func redisTestBackend(t *testing.T) (*redisBackend, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	host, portStr, err := net.SplitHostPort(mr.Addr())
	if err != nil {
		t.Fatalf("miniredis addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	db := store.Database{ID: "r1", Engine: store.EngineRedis, Host: host, Port: port, SSLMode: store.SSLModeDisable}
	b, err := newRedisBackend(Config{MaxConnsPerDatabase: 4, IdleTimeout: time.Minute, DialTimeout: 2 * time.Second}, db, "")
	if err != nil {
		t.Fatalf("newRedisBackend: %v", err)
	}
	t.Cleanup(b.Close)
	return b, mr
}

func mustQuery(t *testing.T, b *redisBackend, cmd string) QueryResult {
	t.Helper()
	res, err := b.Query(context.Background(), cmd, 1000, 1048576)
	if err != nil {
		t.Fatalf("Query(%q): %v", cmd, err)
	}
	return res
}

func assertRows(t *testing.T, res QueryResult, want [][]any) {
	t.Helper()
	if len(res.Rows) != len(want) {
		t.Fatalf("rows = %v, want %v", res.Rows, want)
	}
	for i := range want {
		if len(res.Rows[i]) != len(want[i]) {
			t.Fatalf("row %d = %v, want %v", i, res.Rows[i], want[i])
		}
		for j := range want[i] {
			if res.Rows[i][j] != want[i][j] {
				t.Fatalf("row %d col %d = %#v, want %#v", i, j, res.Rows[i][j], want[i][j])
			}
		}
	}
}

// TestRedisQueryResultMapping 覆盖 G19 的结果映射：单值/数组/SCAN(cursor,key)/
// HSCAN(cursor,field,value)/INFO 按行/二进制 base64/整数与状态回复字符串化。
func TestRedisQueryResultMapping(t *testing.T) {
	b, mr := redisTestBackend(t)
	mr.Set("str", "hello")
	mr.Set("bin", "\x00\x01\xff")
	mr.HSet("h", "f1", "v1", "f2", "v2")
	mr.ZAdd("z", 1, "a")
	mr.ZAdd("z", 2, "b")
	if _, err := mr.Push("l", "x1", "x2", "x3"); err != nil {
		t.Fatalf("push: %v", err)
	}

	// 单值回复（bulk）→ [value] 一行。
	res := mustQuery(t, b, "GET str")
	if len(res.Columns) != 1 || res.Columns[0].Name != "value" || res.Columns[0].Type != "text" {
		t.Fatalf("GET columns = %+v", res.Columns)
	}
	assertRows(t, res, [][]any{{"hello"}})

	// 不存在的 key：redis.Nil → 一行 null（不视为错误）。
	res = mustQuery(t, b, "GET missing")
	assertRows(t, res, [][]any{{nil}})

	// 整数回复字符串化。
	res = mustQuery(t, b, "STRLEN str")
	assertRows(t, res, [][]any{{"5"}})
	res = mustQuery(t, b, "DBSIZE")
	assertRows(t, res, [][]any{{"5"}})

	// 状态回复字符串化。
	res = mustQuery(t, b, "TYPE str")
	assertRows(t, res, [][]any{{"string"}})

	// 二进制安全回复 base64（\x00\x01\xff → AAH/）。
	res = mustQuery(t, b, "GET bin")
	assertRows(t, res, [][]any{{"AAH/"}})

	// 数组回复 → [index,value]。
	res = mustQuery(t, b, "MGET str missing bin")
	if len(res.Columns) != 2 || res.Columns[0].Name != "index" || res.Columns[1].Name != "value" {
		t.Fatalf("MGET columns = %+v", res.Columns)
	}
	assertRows(t, res, [][]any{{"0", "hello"}, {"1", nil}, {"2", "AAH/"}})

	// SCAN → [cursor,key]。
	res = mustQuery(t, b, "SCAN 0")
	if len(res.Columns) != 2 || res.Columns[0].Name != "cursor" || res.Columns[1].Name != "key" {
		t.Fatalf("SCAN columns = %+v", res.Columns)
	}
	if len(res.Rows) != 5 {
		t.Fatalf("SCAN rows = %v, want 5 keys", res.Rows)
	}
	for _, row := range res.Rows {
		if row[0] != "0" { // miniredis 一次扫完，游标回到 0
			t.Fatalf("SCAN cursor = %v, want 0", row[0])
		}
	}

	// HSCAN → [cursor,field,value] 三列。
	res = mustQuery(t, b, "HSCAN h 0")
	if len(res.Columns) != 3 || res.Columns[1].Name != "field" || res.Columns[2].Name != "value" {
		t.Fatalf("HSCAN columns = %+v", res.Columns)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("HSCAN rows = %v, want 2", res.Rows)
	}
	fields := map[string]string{}
	for _, row := range res.Rows {
		fields[row[1].(string)] = row[2].(string)
	}
	if fields["f1"] != "v1" || fields["f2"] != "v2" {
		t.Fatalf("HSCAN fields = %v", fields)
	}

	// ZRANGE WITHSCORES → 扁平数组按 [index,value]。
	res = mustQuery(t, b, "ZRANGE z 0 -1 WITHSCORES")
	assertRows(t, res, [][]any{{"0", "a"}, {"1", "1"}, {"2", "b"}, {"3", "2"}})

	// LRANGE → 数组回复。
	res = mustQuery(t, b, "LRANGE l 0 -1")
	assertRows(t, res, [][]any{{"0", "x1"}, {"1", "x2"}, {"2", "x3"}})

	// INFO → 按行拆分 [line] 多行。
	res = mustQuery(t, b, "INFO")
	if len(res.Columns) != 1 || res.Columns[0].Name != "line" {
		t.Fatalf("INFO columns = %+v", res.Columns)
	}
	if len(res.Rows) < 2 || res.RowCount != len(res.Rows) {
		t.Fatalf("INFO rows = %v", res.Rows)
	}

	// 行数上限截断。
	res, err := b.Query(context.Background(), "LRANGE l 0 -1", 2, 1048576)
	if err != nil {
		t.Fatalf("Query(LRANGE, maxRows=2): %v", err)
	}
	if len(res.Rows) != 2 || !res.Truncated {
		t.Fatalf("truncation: rows=%d truncated=%v, want 2/true", len(res.Rows), res.Truncated)
	}

	// 字节上限截断。
	res, err = b.Query(context.Background(), "LRANGE l 0 -1", 1000, 1)
	if err != nil {
		t.Fatalf("Query(LRANGE, maxBytes=1): %v", err)
	}
	if len(res.Rows) != 0 || !res.Truncated {
		t.Fatalf("byte truncation: rows=%d truncated=%v, want 0/true", len(res.Rows), res.Truncated)
	}

	// 空命令与解析失败。
	if _, err := b.Query(context.Background(), "   ", 10, 1024); err == nil {
		t.Fatal("empty command must fail")
	}
	if _, err := b.Query(context.Background(), "GET 'unterminated", 10, 1024); err == nil {
		t.Fatal("unterminated quote must fail")
	}
}

// TestRedisBackendWriteRejectedByExecutorGuard 验证执行器内第三道闸：
// 写命令（SET）即使绕过调用方也必须被 executor.go 的 sqlguard.Check 拒绝。
func TestRedisBackendWriteRejectedByExecutorGuard(t *testing.T) {
	mr := miniredis.RunT(t)
	host, portStr, _ := net.SplitHostPort(mr.Addr())
	port, _ := strconv.Atoi(portStr)
	p := New(Config{MaxConnsPerDatabase: 2, IdleTimeout: time.Minute, DialTimeout: 2 * time.Second}, nil)
	defer p.Close()
	db := store.Database{ID: "r2", Engine: store.EngineRedis, Host: host, Port: port, SSLMode: store.SSLModeDisable}

	_, err := p.Query(context.Background(), db, "", QueryRequest{SQL: "SET k v", Timeout: 5 * time.Second})
	var nre *NotReadOnlyError
	if !errors.As(err, &nre) {
		t.Fatalf("SET must be rejected by the in-executor guard, got %v", err)
	}
	if nre.Result.RuleID != sqlguard.RuleRedisNotWhitelisted {
		t.Fatalf("rule = %q, want REDIS_NOT_WHITELISTED", nre.Result.RuleID)
	}

	// 池化路径：白名单命令经 ExecutorPool 执行成功。
	res, err := p.Query(context.Background(), db, "", QueryRequest{SQL: "DBSIZE", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("DBSIZE via pool: %v", err)
	}
	if res.RowCount != 1 || res.Rows[0][0] != "0" {
		t.Fatalf("DBSIZE rows = %v", res.Rows)
	}
}

// fakeRedisServer 是最小的 RESP2 服务器，用于验证 Version()（PING +
// INFO server → redis_version）的编排——miniredis 不支持 INFO 的
// server section，无法覆盖该契约路径。
type fakeRedisServer struct {
	ln net.Listener

	mu       sync.Mutex
	requests [][]string
}

func newFakeRedisServer(t *testing.T) *fakeRedisServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeRedisServer{ln: ln}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeRedisServer) addr() (host string, port int) {
	host, portStr, _ := net.SplitHostPort(s.ln.Addr().String())
	port, _ = strconv.Atoi(portStr)
	return host, port
}

func (s *fakeRedisServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeRedisServer) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		args, err := readRESPArray(r)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, args)
		s.mu.Unlock()

		switch strings.ToUpper(args[0]) {
		case "HELLO":
			// 拒绝 RESP3 协商，go-redis 回退到 RESP2。
			_, _ = w.WriteString("-ERR unknown command 'HELLO'\r\n")
		case "PING":
			_, _ = w.WriteString("+PONG\r\n")
		case "INFO":
			payload := "# Server\r\nredis_version:7.4.1\r\nos:linux\r\n"
			_, _ = fmt.Fprintf(w, "$%d\r\n%s\r\n", len(payload), payload)
		case "SELECT", "CLIENT":
			_, _ = w.WriteString("+OK\r\n")
		default:
			_, _ = w.WriteString("+OK\r\n")
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

// readRESPArray 解析 *N 与 $len 形式的请求帧。
func readRESPArray(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) == 0 || line[0] != '*' {
		return nil, fmt.Errorf("bad array header %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("bad array size %q", line)
	}
	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		head, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		head = strings.TrimRight(head, "\r\n")
		if len(head) == 0 || head[0] != '$' {
			return nil, fmt.Errorf("bad bulk header %q", head)
		}
		size, err := strconv.Atoi(head[1:])
		if err != nil || size < 0 {
			return nil, fmt.Errorf("bad bulk size %q", head)
		}
		buf := make([]byte, size+2) // payload + \r\n
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func (s *fakeRedisServer) receivedCommands() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.requests))
	copy(out, s.requests)
	return out
}

// TestRedisVersionViaExecutorTest 验证契约 5.4 的 Test 编排：
// PING + INFO server，server_version 取 redis_version。
func TestRedisVersionViaExecutorTest(t *testing.T) {
	fake := newFakeRedisServer(t)
	host, port := fake.addr()
	p := New(Config{MaxConnsPerDatabase: 2, IdleTimeout: time.Minute, DialTimeout: 2 * time.Second}, nil)
	defer p.Close()
	db := store.Database{ID: "r3", Engine: store.EngineRedis, Host: host, Port: port,
		RedisDB: 5, SSLMode: store.SSLModeDisable}

	res, err := p.Test(context.Background(), db, "")
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if res.ServerVersion != "7.4.1" {
		t.Fatalf("server_version = %q, want 7.4.1", res.ServerVersion)
	}
	if res.LatencyMS < 0 {
		t.Fatalf("latency = %d", res.LatencyMS)
	}

	// SELECT 帧必须带 redis_db（G19：逻辑库由连接选项决定，SELECT 不在白名单）。
	seenSelect := false
	for _, cmd := range fake.receivedCommands() {
		if strings.ToUpper(cmd[0]) == "SELECT" && len(cmd) > 1 && cmd[1] == "5" {
			seenSelect = true
		}
	}
	if !seenSelect {
		t.Fatal("connection must SELECT database 5 (redis_db passthrough)")
	}
}

// TestSignatureIncludesInstanceAndRedisDB 确保 instance/redis_db 变化时
// 连接池签名变化（池重建的前提）。
func TestSignatureIncludesInstanceAndRedisDB(t *testing.T) {
	base := store.Database{ID: "d1", Engine: store.EngineMSSQL, Host: "h", Port: 1433,
		DatabaseName: "db", Username: "u"}
	if signature(base, "pw") == signature(func() store.Database {
		d := base
		d.InstanceName = "SQLEXPRESS"
		d.Port = 0
		return d
	}(), "pw") {
		t.Fatal("instance_name change must change the pool signature")
	}
	redisBase := store.Database{ID: "r1", Engine: store.EngineRedis, Host: "h", Port: 6379}
	if signature(redisBase, "pw") == signature(func() store.Database {
		d := redisBase
		d.RedisDB = 9
		return d
	}(), "pw") {
		t.Fatal("redis_db change must change the pool signature")
	}
}
