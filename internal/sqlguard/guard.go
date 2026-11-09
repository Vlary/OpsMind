// Package sqlguard 对 SQL 做单语句/只读分类，不建立数据库连接。
//
// PostgreSQL 使用 AST（github.com/auxten/postgresql-parser）判定语句类型与
// 锁定子句；MySQL/SQLite 在剥离字符串、标识符与注释后做词法分类。
// 只允许单条 SELECT、WITH...SELECT、EXPLAIN（非 ANALYZE）。
package sqlguard

import (
	"errors"
	"strings"

	"aiops/internal/risk"
)

// MaxSQLBytes 是单条 SQL 的硬上限（CONTRACT.md 2.6）。
const MaxSQLBytes = 65536

// ErrUnsupportedEngine 表示未知的 SQL 方言。
var ErrUnsupportedEngine = errors.New("sqlguard: unsupported engine")

// Engine 是 SQL 方言。
type Engine string

const (
	EnginePostgres Engine = "postgres"
	EngineMySQL    Engine = "mysql"
	EngineSQLite   Engine = "sqlite"
	EngineMSSQL    Engine = "mssql"   // W2
	EngineRedis    Engine = "redis"   // W2
	EngineMongoDB  Engine = "mongodb" // N2
)

// Kind 是语句分类。
type Kind string

const (
	KindSelect     Kind = "select"
	KindWithSelect Kind = "with_select"
	KindExplain    Kind = "explain"
	KindInsert     Kind = "insert"
	KindUpdate     Kind = "update"
	KindDelete     Kind = "delete"
	KindDDL        Kind = "ddl"
	KindCopy       Kind = "copy"
	KindCall       Kind = "call"
	KindTCL        Kind = "tcl"
	KindMultiple   Kind = "multiple"
	KindUnknown    Kind = "unknown"
)

// Result 是守卫结论。
type Result struct {
	Allowed    bool       `json:"allowed"`
	Kind       Kind       `json:"kind"`
	RiskLevel  risk.Level `json:"risk_level"`
	RuleID     string     `json:"rule_id"`
	Reason     string     `json:"reason"`
	Normalized string     `json:"normalized_sql"`
}

// Rule ID（稳定标识，审计与测试引用）。
const (
	RuleRead           = "SQL_GUARD_READ"
	RuleInvalid        = "SQL_GUARD_INVALID"
	RuleMultiple       = "SQL_GUARD_MULTI"
	RuleDDL            = "SQL_GUARD_DDL"
	RuleDML            = "SQL_GUARD_DML"
	RuleCopy           = "SQL_GUARD_COPY"
	RuleCall           = "SQL_GUARD_CALL"
	RuleTCL            = "SQL_GUARD_TCL"
	RuleLocking        = "SQL_GUARD_LOCKING"
	RuleInto           = "SQL_GUARD_INTO"
	RuleExplainAnalyze = "SQL_GUARD_EXPLAIN_ANALYZE"
	RuleDangerousFn    = "SQL_GUARD_DANGEROUS_FUNCTION"
)

// Check 只做分类，不连接数据库。
//
// 空输入、超过 65536 字节、含 NUL、解析失败一律 allowed=false。
// 仅 SELECT、WITH...SELECT、EXPLAIN（非 ANALYZE）可能 allowed=true。
// 对合法方言 Check 不返回业务错误；拒绝以 Result 表达，便于调用方统一映射
// SQL_NOT_READONLY。
func Check(engine Engine, sql string) (Result, error) {
	// N2：mongodb 的输入是单条 JSON 命令文档而非 SQL——JSON 的引号
	// （\" 转义）与 SQL 词法语义不同，lexSQL 会误判未闭合字符串，
	// 必须在通用空/超长/NUL 检查与词法之前分流（契约 5.3：这些拒绝
	// 的 RuleID 为 MONGO_NOT_JSON）。
	if engine == EngineMongoDB {
		return checkMongo(sql), nil
	}
	if sql == "" {
		return reject(KindUnknown, RuleInvalid, "SQL is empty", ""), nil
	}
	if len(sql) > MaxSQLBytes {
		return reject(KindUnknown, RuleInvalid, "SQL exceeds 65536 bytes", ""), nil
	}
	if strings.IndexByte(sql, 0) >= 0 {
		return reject(KindUnknown, RuleInvalid, "SQL contains NUL byte", ""), nil
	}

	lex, lexErr := lexSQL(sql, engine)
	if lexErr != nil {
		return reject(KindUnknown, RuleInvalid, lexErr.Error(), lex.normalized), nil
	}

	switch engine {
	case EnginePostgres:
		return checkPostgres(sql, lex), nil
	case EngineMySQL, EngineSQLite:
		return checkLexical(engine, lex), nil
	case EngineMSSQL:
		// W2：T-SQL 用自带词法器（[bracket ident]/N'...' 剥离在
		// checkMSSQL 内完成），不依赖 lexSQL 的 MySQL/SQLite 词法。
		return checkMSSQL(sql), nil
	case EngineRedis:
		// W2：Redis 命令白名单，输入是命令文本而非 SQL。
		return checkRedis(sql), nil
	default:
		return reject(KindUnknown, RuleInvalid, "unsupported SQL engine", lex.normalized), ErrUnsupportedEngine
	}
}

func allowed(kind Kind, rule, reason, normalized string) Result {
	return Result{
		Allowed:    true,
		Kind:       kind,
		RiskLevel:  risk.LevelSafe,
		RuleID:     rule,
		Reason:     reason,
		Normalized: normalized,
	}
}

func reject(kind Kind, rule, reason, normalized string) Result {
	return Result{
		Allowed:    false,
		Kind:       kind,
		RiskLevel:  risk.LevelNeedsApproval,
		RuleID:     rule,
		Reason:     reason,
		Normalized: normalized,
	}
}
