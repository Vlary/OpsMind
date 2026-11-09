// Redis 执行器（W2，CONTRACT.md 5.4/7.6，驱动 github.com/redis/go-redis/v9）。
//
// 参数按 RESP 数组传递（client.Do(ctx, args...)），绝不拼接命令字符串；
// 切词与 sqlguard 共用同一份 SplitRedisCommand，保证白名单校验的词序
// 与实际执行的词序一致。结果映射（契约 5.4 写死）：单值回复 →
// [value] 一行；数组回复 → [index,value]；SCAN/HSCAN/SSCAN/ZSCAN →
// [cursor,key]（HSCAN 三列 cursor,field,value）；INFO 按行拆分为 [line]；
// 整数/状态回复字符串化；二进制安全回复 base64。
package dbproxy

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"aiops/internal/sqlguard"
	"aiops/internal/store"
)

// redisBackend 是 go-redis 客户端（单节点，池化）。
type redisBackend struct {
	client *redis.Client
}

func newRedisBackend(cfg Config, db store.Database, password string) (*redisBackend, error) {
	if db.Host == "" {
		return nil, fmt.Errorf("dbproxy: redis requires host")
	}
	if db.Port < 1 || db.Port > 65535 {
		return nil, fmt.Errorf("dbproxy: redis requires a valid port")
	}
	if db.RedisDB < 0 || db.RedisDB > 1023 {
		return nil, fmt.Errorf("dbproxy: redis_db must be between 0 and 1023")
	}

	opts := &redis.Options{
		Addr:            net.JoinHostPort(db.Host, strconv.Itoa(db.Port)),
		DB:              db.RedisDB,
		Username:        db.Username,
		Password:        password,
		DialTimeout:     cfg.DialTimeout,
		ReadTimeout:     maxQueryTimeout + 5*time.Second,
		WriteTimeout:    cfg.DialTimeout,
		PoolSize:        cfg.MaxConnsPerDatabase,
		MaxIdleConns:    cfg.MaxConnsPerDatabase,
		ConnMaxIdleTime: cfg.IdleTimeout,
	}
	if tlsCfg := redisTLSConfig(db.SSLMode); tlsCfg != nil {
		opts.TLSConfig = tlsCfg
	}
	return &redisBackend{client: redis.NewClient(opts)}, nil
}

// redisTLSConfig 按契约 5.4 映射 ssl_mode：disable/prefer→无 TLS，
// require→TLS 跳过证书校验，verify-ca/verify-full→TLS 系统校验。
func redisTLSConfig(mode store.SSLMode) *tls.Config {
	switch mode {
	case store.SSLModeRequire:
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // 契约语义：require 即跳过证书校验
	case store.SSLModeVerifyCA, store.SSLModeVerifyFull:
		return &tls.Config{MinVersion: tls.VersionTLS12}
	default:
		return nil
	}
}

// Version 执行 PING + INFO server，返回 redis_version（契约 5.4：Test 探针）。
func (b *redisBackend) Version(ctx context.Context) (string, error) {
	if err := b.client.Ping(ctx).Err(); err != nil {
		return "", err
	}
	info, err := b.client.Info(ctx, "server").Result()
	if err != nil {
		return "", err
	}
	version, ok := parseRedisVersion(info)
	if !ok {
		return "", fmt.Errorf("dbproxy: redis info missing redis_version")
	}
	return version, nil
}

// parseRedisVersion 从 INFO 输出中提取 redis_version。
func parseRedisVersion(info string) (string, bool) {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "redis_version:"); ok {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// Query 执行单条白名单命令（RESP 数组传参）并按契约映射结果。
func (b *redisBackend) Query(ctx context.Context, sqlText string, maxRows int, maxBytes int64) (QueryResult, error) {
	args, err := sqlguard.SplitRedisCommand(sqlText)
	if err != nil {
		return QueryResult{}, err
	}
	if len(args) == 0 {
		return QueryResult{}, fmt.Errorf("dbproxy: empty redis command")
	}

	doArgs := make([]any, len(args))
	for i, a := range args {
		doArgs[i] = a
	}
	val, err := b.client.Do(ctx, doArgs...).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return QueryResult{}, err
	}
	return mapRedisResult(args, val, err, maxRows, maxBytes)
}

// mapRedisResult 把 go-redis 回复映射为统一 QueryResult（契约 5.4 写死）。
// err 仅可能是 redis.Nil（单值空回复，如 GET 不存在的 key → 一行 null）。
func mapRedisResult(args []string, val any, cmdErr error, maxRows int, maxBytes int64) (QueryResult, error) {
	name := strings.ToUpper(args[0])
	switch name {
	case "SCAN", "SSCAN", "ZSCAN", "HSCAN":
		return mapRedisScan(name, val, maxRows, maxBytes)
	case "INFO":
		return mapRedisInfo(val, maxRows, maxBytes)
	}

	if cmdErr != nil { // redis.Nil：单值空回复
		result := QueryResult{Columns: []Column{{Name: "value", Type: "text"}}}
		result.Rows = [][]any{{nil}}
		result.RowCount = 1
		return result, nil
	}

	switch val.(type) {
	case []any:
		// 数组回复（MGET/KEYS/HGETALL/ZRANGE/SMEMBERS/CONFIG GET 等）：
		// columns=[index,value]，每元素一行。
		items := val.([]any)
		result := QueryResult{
			Columns: []Column{{Name: "index", Type: "text"}, {Name: "value", Type: "text"}},
		}
		for i, item := range items {
			if len(result.Rows) >= maxRows {
				result.Truncated = true
				break
			}
			// 数组元素中的 null（如 MGET 不存在的 key）保持 JSON null，
			// 与单值空回复的 [null] 行一致。
			var cell any
			if item != nil {
				cell = redisValueString(item)
			}
			row := []any{strconv.Itoa(i), cell}
			if !appendRow(&result, row, maxBytes) {
				result.Truncated = true
				break
			}
		}
		result.RowCount = len(result.Rows)
		return result, nil

	default:
		// 单值回复（string/int64/status）：columns=[value]，一行。
		result := QueryResult{Columns: []Column{{Name: "value", Type: "text"}}}
		if len(result.Rows) < maxRows {
			appendRow(&result, []any{redisValueString(val)}, maxBytes)
		}
		result.RowCount = len(result.Rows)
		return result, nil
	}
}

// mapRedisScan 映射 SCAN 家族：columns=[cursor,key]；HSCAN 为
// [cursor,field,value] 三列（契约 5.4）。
func mapRedisScan(name string, val any, maxRows int, maxBytes int64) (QueryResult, error) {
	parts, ok := val.([]any)
	if !ok || len(parts) < 2 {
		return QueryResult{}, fmt.Errorf("dbproxy: unexpected %s reply", name)
	}
	cursor := redisValueString(parts[0])
	items, _ := parts[1].([]any)

	var result QueryResult
	if name == "HSCAN" {
		result = QueryResult{Columns: []Column{
			{Name: "cursor", Type: "text"}, {Name: "field", Type: "text"}, {Name: "value", Type: "text"},
		}}
		for i := 0; i+1 < len(items); i += 2 {
			if len(result.Rows) >= maxRows {
				result.Truncated = true
				break
			}
			row := []any{cursor, redisValueString(items[i]), redisValueString(items[i+1])}
			if !appendRow(&result, row, maxBytes) {
				result.Truncated = true
				break
			}
		}
	} else {
		result = QueryResult{Columns: []Column{
			{Name: "cursor", Type: "text"}, {Name: "key", Type: "text"},
		}}
		for _, item := range items {
			if len(result.Rows) >= maxRows {
				result.Truncated = true
				break
			}
			row := []any{cursor, redisValueString(item)}
			if !appendRow(&result, row, maxBytes) {
				result.Truncated = true
				break
			}
		}
	}
	result.RowCount = len(result.Rows)
	return result, nil
}

// mapRedisInfo 把 INFO 文本按行拆分为 columns=[line] 多行。
func mapRedisInfo(val any, maxRows int, maxBytes int64) (QueryResult, error) {
	s, ok := val.(string)
	if !ok {
		return QueryResult{}, fmt.Errorf("dbproxy: unexpected INFO reply")
	}
	result := QueryResult{Columns: []Column{{Name: "line", Type: "text"}}}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(result.Rows) >= maxRows {
			result.Truncated = true
			break
		}
		if !appendRow(&result, []any{line}, maxBytes) {
			result.Truncated = true
			break
		}
	}
	result.RowCount = len(result.Rows)
	return result, nil
}

// redisValueString 把回复值字符串化；二进制安全回复（含控制字符或
// 非 UTF-8 字节）base64 编码（契约 5.4）。
func redisValueString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		if isBinaryRedisString(x) {
			return base64.StdEncoding.EncodeToString([]byte(x))
		}
		return x
	case []byte:
		if isBinaryRedisString(string(x)) {
			return base64.StdEncoding.EncodeToString(x)
		}
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

// isBinaryRedisString 判定是否需要 base64：非 UTF-8 或含控制字符
// （除 \t \n \r）或 DEL。
func isBinaryRedisString(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r == 0x7f || (r < 0x20 && r != '\t' && r != '\n' && r != '\r') {
			return true
		}
	}
	return false
}

func (b *redisBackend) Close() {
	if b.client != nil {
		_ = b.client.Close()
	}
}
