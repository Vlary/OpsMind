// Redis 命令白名单（W2，CONTRACT.md 5.3）。
//
// 只放行契约写死的只读命令集合：首个词（大小写不敏感）必须命中白名单，
// 复合命令（OBJECT/MEMORY/CONFIG/CLIENT/SLOWLOG/LATENCY）还要求子命令
// 精确匹配。最多一条命令：出现 `|` 或任何位置嵌入 MULTI/EXEC/SUBSCRIBE
// 一律拒绝。KEYS 属 O(N) 阻塞命令，契约裁决允许（UI 提示改用 SCAN，
// 服务端不拦）。一切写命令与未知命令 allowed=false，
// RuleID=REDIS_NOT_WHITELISTED；白名单命中 RuleID=REDIS_READONLY。
package sqlguard

import (
	"errors"
	"strings"
)

// Redis 规则 ID（CONTRACT.md 5.3）。
const (
	RuleRedisReadonly       = "REDIS_READONLY"
	RuleRedisNotWhitelisted = "REDIS_NOT_WHITELISTED"
)

// redisReadOnlyCommands 是只读白名单（契约 5.3 逐字）。
var redisReadOnlyCommands = map[string]bool{
	// 键空间
	"GET": true, "MGET": true, "STRLEN": true, "EXISTS": true, "TYPE": true,
	"TTL": true, "PTTL": true, "GETRANGE": true, "SUBSTR": true,
	"RANDOMKEY": true, "SCAN": true, "DBSIZE": true, "KEYS": true,
	// 哈希
	"HGET": true, "HGETALL": true, "HMGET": true, "HKEYS": true,
	"HLEN": true, "HVALS": true, "HEXISTS": true, "HSCAN": true,
	// 列表
	"LINDEX": true, "LLEN": true, "LRANGE": true,
	// 集合
	"SCARD": true, "SISMEMBER": true, "SMEMBERS": true,
	"SRANDMEMBER": true, "SSCAN": true,
	// 有序集合
	"ZCARD": true, "ZCOUNT": true, "ZLEXCOUNT": true, "ZRANGE": true,
	"ZRANGEBYLEX": true, "ZRANGEBYSCORE": true, "ZRANK": true,
	"ZREVRANGE": true, "ZREVRANGEBYSCORE": true, "ZREVRANK": true,
	"ZSCORE": true, "ZSCAN": true,
	// 位
	"BITCOUNT": true,
	// 服务器（OBJECT/MEMORY/CONFIG/CLIENT/SLOWLOG/LATENCY 的子命令
	// 校验见 redisSubcommands）
	"INFO": true, "PING": true, "ECHO": true, "TIME": true,
	"OBJECT": true, "MEMORY": true, "CONFIG": true, "CLIENT": true,
	"SLOWLOG": true, "LATENCY": true,
}

// redisSubcommands 是复合命令唯一允许的子命令（契约 5.3：CONFIG 只允许
// GET；解析为 CONFIG+SET 时拒绝；OBJECT 只允许 ENCODING；等等）。
var redisSubcommands = map[string]string{
	"OBJECT":  "ENCODING",
	"MEMORY":  "USAGE",
	"CONFIG":  "GET",
	"CLIENT":  "GETNAME",
	"SLOWLOG": "GET",
	"LATENCY": "HISTORY",
}

// SplitRedisCommand 按空格切词，保留引号（单/双）内的空格并剥除引号，
// redis-cli 语义。守卫与 dbproxy 执行器共用这一份实现，确保白名单
// 校验看到的词序与实际发送给服务器的 RESP 参数完全一致——两处各自
// 实现切词会引入"校验词序 ≠ 执行词序"的绕过面。
func SplitRedisCommand(line string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	started := false

	flush := func() {
		if started {
			tokens = append(tokens, cur.String())
			cur.Reset()
			started = false
		}
	}

	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			} else {
				cur.WriteByte(c)
			}
		case inDouble:
			if c == '"' {
				inDouble = false
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' && !started:
			inSingle, started = true, true
		case c == '"' && !started:
			inDouble, started = true, true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	if inSingle || inDouble {
		return tokens, errors.New("unterminated quoted argument")
	}
	flush()
	return tokens, nil
}

// checkRedis 对单条 Redis 命令文本做白名单校验（Check 的 redis 分支）。
func checkRedis(line string) Result {
	cmds, err := SplitRedisCommand(line)
	if err != nil {
		return reject(KindUnknown, RuleInvalid, err.Error(), "")
	}
	if len(cmds) == 0 {
		return reject(KindUnknown, RuleInvalid, "command is empty", "")
	}
	normalized := strings.Join(cmds, " ")

	for _, raw := range cmds {
		// 契约：禁止 `|`/管道。RESP 数组本身没有管道语义，这里是
		// 保守防线（宁可误拦 key 含 | 的查询，不放行任何管道形态）。
		if strings.ContainsRune(raw, '|') {
			return reject(KindUnknown, RuleRedisNotWhitelisted, "pipe character is not allowed", normalized)
		}
		switch strings.ToUpper(raw) {
		case "MULTI", "EXEC", "SUBSCRIBE":
			return reject(KindUnknown, RuleRedisNotWhitelisted, "transaction/pubsub command is not allowed", normalized)
		}
	}

	name := strings.ToUpper(cmds[0])
	if sub, ok := redisSubcommands[name]; ok {
		if len(cmds) < 2 || strings.ToUpper(cmds[1]) != sub {
			return reject(KindUnknown, RuleRedisNotWhitelisted,
				"only "+name+" "+sub+" is allowed", normalized)
		}
	}
	if !redisReadOnlyCommands[name] {
		return reject(KindUnknown, RuleRedisNotWhitelisted,
			"command is not in the read-only whitelist", normalized)
	}
	// CONFIG GET 的响应会把服务端配置原样回显：requirepass/masterauth 等
	// 参数会明文返回 Redis 密码（评审发现，凭据暴露面超出「只读数据」）。
	// 失败关闭：敏感参数精确拒绝；含通配符（* / ?）的参数无法静态证明
	// 不会命中敏感项，同样拒绝。
	if name == "CONFIG" {
		for _, arg := range cmds[1:] {
			lower := strings.ToLower(arg)
			if strings.ContainsAny(arg, "*?") {
				return reject(KindUnknown, RuleRedisNotWhitelisted,
					"CONFIG GET with wildcard patterns is not allowed", normalized)
			}
			if configGetBlockedParams[lower] {
				return reject(KindUnknown, RuleRedisNotWhitelisted,
					"CONFIG GET must not expose server credentials", normalized)
			}
		}
	}
	return allowed(KindSelect, RuleRedisReadonly, "read-only command", normalized)
}

// configGetBlockedParams 是 CONFIG GET 禁止查询的参数：其值是服务端
// 凭据，明文回显即泄漏。
var configGetBlockedParams = map[string]bool{
	"requirepass":         true,
	"masterauth":          true,
	"masteruser":          true,
	"tls-key-file-pass":   true,
	"tls-client-key-pass": true,
}
