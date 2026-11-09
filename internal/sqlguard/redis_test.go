package sqlguard

import (
	"reflect"
	"testing"
)

// G18（redis 部分，CONTRACT.md 9.2）：白名单内 GET/ZRANGE/INFO/
// CONFIG GET 通过；SET/DEL/FLUSHALL/CONFIG SET/MULTI/CLIENT KILL/
// DUMP/SHUTDOWN 及未知命令拒绝；KEYS 允许。G19：SELECT 不在白名单
//（逻辑库由连接 DB 参数决定）。

func TestCheckRedisAllowed(t *testing.T) {
	cases := []string{
		// 键空间
		"GET mykey",
		"get mykey",        // 命令大小写不敏感
		"GET 'quoted key'", // 引号内空格保留
		`GET "my key"`,
		"MGET a b c",
		"STRLEN mykey",
		"EXISTS mykey",
		"TYPE mykey",
		"TTL mykey",
		"PTTL mykey",
		"GETRANGE mykey 0 4",
		"SUBSTR mykey 0 4",
		"RANDOMKEY",
		"OBJECT ENCODING mykey",
		"MEMORY USAGE mykey",
		"SCAN 0",
		"SCAN 0 MATCH user:* COUNT 100",
		"DBSIZE",
		"KEYS user:*", // 契约：KEYS 允许（O(N) 由 UI 提示改用 SCAN，服务端不拦）
		// 哈希
		"HGET h f",
		"HGETALL h",
		"HMGET h f1 f2",
		"HKEYS h",
		"HLEN h",
		"HVALS h",
		"HEXISTS h f",
		"HSCAN h 0",
		// 列表
		"LINDEX l 0",
		"LLEN l",
		"LRANGE l 0 -1",
		// 集合
		"SCARD s",
		"SISMEMBER s x",
		"SMEMBERS s",
		"SRANDMEMBER s",
		"SSCAN s 0",
		// 有序集合
		"ZCARD z",
		"ZCOUNT z 0 10",
		"ZLEXCOUNT z - +",
		"ZRANGE z 0 -1",
		"ZRANGE z 0 -1 WITHSCORES",
		"ZRANGEBYLEX z - +",
		"ZRANGEBYSCORE z -inf +inf",
		"ZRANK z m",
		"ZREVRANGE z 0 -1",
		"ZREVRANGEBYSCORE z +inf -inf",
		"ZREVRANK z m",
		"ZSCORE z m",
		"ZSCAN z 0",
		// 位
		"BITCOUNT k",
		// 服务器
		"INFO",
		"INFO server",
		"PING",
		"ECHO hello",
		"TIME",
		"CLIENT GETNAME",
		"CONFIG GET maxmemory",
		"CONFIG GET save",
		"SLOWLOG GET 10",
		"LATENCY HISTORY event",
	}
	for _, cmd := range cases {
		res, err := Check(EngineRedis, cmd)
		if err != nil {
			t.Fatalf("Check(redis, %q) error: %v", cmd, err)
		}
		if !res.Allowed {
			t.Fatalf("Check(redis, %q) = {rule=%s reason=%s}, want allowed", cmd, res.RuleID, res.Reason)
		}
		if res.RuleID != RuleRedisReadonly {
			t.Fatalf("Check(redis, %q).RuleID = %q, want %q", cmd, res.RuleID, RuleRedisReadonly)
		}
		if res.RiskLevel != "safe" {
			t.Fatalf("Check(redis, %q).RiskLevel = %q, want safe", cmd, res.RiskLevel)
		}
	}
}

func TestCheckRedisRejected(t *testing.T) {
	cases := []struct {
		cmd  string
		rule string
	}{
		// 写命令（契约 5.3：一切写命令与未知命令 REDIS_NOT_WHITELISTED）。
		{"SET k v", RuleRedisNotWhitelisted},
		{"set k v", RuleRedisNotWhitelisted},
		{"DEL k", RuleRedisNotWhitelisted},
		{"EXPIRE k 60", RuleRedisNotWhitelisted},
		{"RENAME a b", RuleRedisNotWhitelisted},
		{"HSET h f v", RuleRedisNotWhitelisted},
		{"LPUSH l x", RuleRedisNotWhitelisted},
		{"SADD s x", RuleRedisNotWhitelisted},
		{"ZADD z 1 m", RuleRedisNotWhitelisted},
		{"INCR k", RuleRedisNotWhitelisted},
		{"APPEND k v", RuleRedisNotWhitelisted},
		{"FLUSHALL", RuleRedisNotWhitelisted},
		{"FLUSHDB", RuleRedisNotWhitelisted},
		{"DUMP k", RuleRedisNotWhitelisted},
		{"RESTORE k 0 x", RuleRedisNotWhitelisted},
		{"MIGRATE host 6379 k 0 1000", RuleRedisNotWhitelisted},
		{"DEBUG SLEEP 1", RuleRedisNotWhitelisted},
		{"SHUTDOWN", RuleRedisNotWhitelisted},
		{"SCRIPT LOAD 'return 1'", RuleRedisNotWhitelisted},
		{"WAIT 1 100", RuleRedisNotWhitelisted},
		{"SELECT 1", RuleRedisNotWhitelisted}, // G19：逻辑库由连接 DB 参数决定
		{"SUBSCRIBE ch", RuleRedisNotWhitelisted},
		{"UNSUBSCRIBE ch", RuleRedisNotWhitelisted},
		{"PSUBSCRIBE ch*", RuleRedisNotWhitelisted},
		{"AUTH secret", RuleRedisNotWhitelisted},
		{"ACL LIST", RuleRedisNotWhitelisted},
		{"COMMAND COUNT", RuleRedisNotWhitelisted},
		{"KEYS | rm -rf /", RuleRedisNotWhitelisted}, // 管道字符
		{"GET a|b", RuleRedisNotWhitelisted},
		// 复合命令的越权子命令。
		{"CONFIG SET maxmemory 100", RuleRedisNotWhitelisted},
		// CONFIG GET 凭据参数：响应明文回显服务端密码，必须拒绝（评审发现）。
		{"CONFIG GET requirepass", RuleRedisNotWhitelisted},
		{"CONFIG GET masterauth", RuleRedisNotWhitelisted},
		{"CONFIG GET requirepass masterauth", RuleRedisNotWhitelisted},
		{"CONFIG GET tls-key-file-pass", RuleRedisNotWhitelisted},
		// 通配形态无法静态证明不命中敏感参数，失败关闭拒绝。
		{"CONFIG GET '*'", RuleRedisNotWhitelisted},
		{"CONFIG GET *pass*", RuleRedisNotWhitelisted},
		{"CONFIG REWRITE", RuleRedisNotWhitelisted},
		{"CLIENT KILL ID 1", RuleRedisNotWhitelisted},
		{"CLIENT LIST", RuleRedisNotWhitelisted},
		{"CLIENT SETNAME x", RuleRedisNotWhitelisted},
		{"OBJECT FREQ k", RuleRedisNotWhitelisted},
		{"MEMORY DOCTOR", RuleRedisNotWhitelisted},
		{"SLOWLOG RESET", RuleRedisNotWhitelisted},
		{"LATENCY RESET", RuleRedisNotWhitelisted},
		{"OBJECT", RuleRedisNotWhitelisted}, // 缺子命令
		{"CONFIG", RuleRedisNotWhitelisted},
		// 内嵌 MULTI/EXEC/SUBSCRIBE（任何词位置）。
		{"GET MULTI", RuleRedisNotWhitelisted},
		{"MGET a EXEC b", RuleRedisNotWhitelisted},
		{"ECHO SUBSCRIBE", RuleRedisNotWhitelisted},
		{"MULTI", RuleRedisNotWhitelisted},
		{"EXEC", RuleRedisNotWhitelisted},
		// 未知命令。
		{"FOO bar", RuleRedisNotWhitelisted},
		{"TTLX k", RuleRedisNotWhitelisted},
		{"EVAL 'return 1' 0", RuleRedisNotWhitelisted},
		{"BITFIELD k", RuleRedisNotWhitelisted},
		{"GETDEL k", RuleRedisNotWhitelisted},
		{"GETEX k", RuleRedisNotWhitelisted},
		{"COPY a b", RuleRedisNotWhitelisted},
		{"MOVE k 1", RuleRedisNotWhitelisted},
		{"SWAPDB 0 1", RuleRedisNotWhitelisted},
		{"FAILOVER", RuleRedisNotWhitelisted},
		{"REPLICAOF NO ONE", RuleRedisNotWhitelisted},
		// 解析失败/空。
		{"", RuleInvalid},
		{"   ", RuleInvalid},
		{"GET 'unterminated", RuleInvalid},
		{`GET "unterminated`, RuleInvalid},
	}
	for _, tc := range cases {
		res, err := Check(EngineRedis, tc.cmd)
		if err != nil {
			t.Fatalf("Check(redis, %q) error: %v", tc.cmd, err)
		}
		if res.Allowed {
			t.Fatalf("Check(redis, %q) allowed, want rejected", tc.cmd)
		}
		if res.RuleID != tc.rule {
			t.Fatalf("Check(redis, %q).RuleID = %q (%s), want %q", tc.cmd, res.RuleID, res.Reason, tc.rule)
		}
		if res.RiskLevel != "needs_approval" {
			t.Fatalf("Check(redis, %q).RiskLevel = %q, want needs_approval", tc.cmd, res.RiskLevel)
		}
	}
}

// TestSplitRedisCommand 锁定守卫与执行器共用的切词语义。
func TestSplitRedisCommand(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"GET key", []string{"GET", "key"}, false},
		{"  GET   key  ", []string{"GET", "key"}, false},
		{"GET 'my key'", []string{"GET", "my key"}, false},
		{`GET "my key"`, []string{"GET", "my key"}, false},
		{"ZRANGE z 0 -1 WITHSCORES", []string{"ZRANGE", "z", "0", "-1", "WITHSCORES"}, false},
		{"SCAN 0 MATCH user:* COUNT 100", []string{"SCAN", "0", "MATCH", "user:*", "COUNT", "100"}, false},
		{"", nil, false},
		{"   ", nil, false},
		{`GET ""`, []string{"GET", ""}, false},         // 空字符串参数合法（空 key 名）
		{"GET 'a\tb'", []string{"GET", "a\tb"}, false}, // 引号内制表符保留
		{"GET 'unterminated", []string{"GET"}, true},
		{`GET "unterminated`, []string{"GET"}, true},
		// 引号只认 token 开头：中途出现的引号是字面字符。
		{"GET ab\"cd", []string{"GET", `ab"cd`}, false},
		{"ECHO it's", []string{"ECHO", "it's"}, false},
	}
	for _, tc := range cases {
		got, err := SplitRedisCommand(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("SplitRedisCommand(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("SplitRedisCommand(%q) error: %v", tc.in, err)
		}
		if len(tc.want) == 0 && len(got) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("SplitRedisCommand(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

// TestCheckRedisNULAndSize 前置校验由 Check 统一处理（空/NUL/64KB）。
func TestCheckRedisNULAndSize(t *testing.T) {
	res, err := Check(EngineRedis, "GET a\x00b")
	if err != nil || res.Allowed || res.RuleID != RuleInvalid {
		t.Fatalf("Check(redis, NUL) = %+v err=%v, want rejected RuleInvalid", res, err)
	}
	res, err = Check(EngineRedis, "GET "+string(make([]byte, 70000)))
	if err != nil || res.Allowed || res.RuleID != RuleInvalid {
		t.Fatalf("Check(redis, oversized) = %+v err=%v, want rejected RuleInvalid", res, err)
	}
}
