package sqlguard

import (
	"strings"
)

// lexSQL 剥离注释，把字符串与引用标识符替换为占位符（masked），
// 同时保留一个仅压缩空白的 normalized 版本用于展示/审计。
//
// 语句计数基于 masked 版本，因此字符串、引号标识符与注释里的分号不会
// 被误判为多语句。
type lexed struct {
	normalized string
	masked     string
	stmts      []string // masked 版本按 ; 切分后的非空语句
}

type lexError struct{ msg string }

func (e *lexError) Error() string { return e.msg }

// mysqlDashComment 判断 MySQL 的 `--` 是否构成注释：后随空白或控制字符
// （或行尾）才算，`--1` / `--+` 等不是注释。
func mysqlDashComment(sql string, next int) bool {
	return next >= len(sql) || sql[next] <= ' '
}

func lexSQL(sql string, engine Engine) (lexed, error) {
	var nb, mb strings.Builder
	nb.Grow(len(sql))
	mb.Grow(len(sql))
	pendingSpace := false

	flushSpace := func() {
		if pendingSpace && nb.Len() > 0 {
			nb.WriteByte(' ')
			mb.WriteByte(' ')
		}
		pendingSpace = false
	}
	write := func(c byte) {
		flushSpace()
		nb.WriteByte(c)
		mb.WriteByte(c)
	}
	writeString := func(s string) {
		flushSpace()
		nb.WriteString(s)
		mb.WriteByte('?')
	}

	i := 0
	for i < len(sql) {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			pendingSpace = true
			i++

		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			// MySQL 规定 `--` 后必须跟空白才算注释；`--1` 是 SQL 片段而不是
			// 注释，整体剥除会漏看其后的 INTO OUTFILE 等关键字。
			if engine == EngineMySQL && !mysqlDashComment(sql, i+2) {
				write(c)
				i++
				continue
			}
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			pendingSpace = true

		case c == '#' && engine == EngineMySQL:
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			pendingSpace = true

		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			// MySQL 会解析并执行 `/*! ... */` 中的内容；不能当普通注释剥除。
			if engine == EngineMySQL && i+2 < len(sql) && sql[i+2] == '!' {
				return lexed{normalized: strings.TrimSpace(nb.String())}, &lexError{"MySQL executable comments are not allowed"}
			}
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return lexed{normalized: strings.TrimSpace(nb.String())}, &lexError{"unterminated block comment"}
			}
			i += 2 + end + 2
			pendingSpace = true

		case c == '\'':
			raw, next, err := scanQuoted(sql, i, '\'', engine == EngineMySQL)
			if err != nil {
				return lexed{normalized: strings.TrimSpace(nb.String())}, err
			}
			writeString(raw)
			i = next

		case c == '"':
			raw, next, err := scanQuoted(sql, i, '"', engine == EngineMySQL)
			if err != nil {
				return lexed{normalized: strings.TrimSpace(nb.String())}, err
			}
			writeString(raw)
			i = next

		case c == '`' && (engine == EngineMySQL || engine == EngineSQLite):
			raw, next, err := scanQuoted(sql, i, '`', false)
			if err != nil {
				return lexed{normalized: strings.TrimSpace(nb.String())}, err
			}
			writeString(raw)
			i = next

		case c == '[' && engine == EngineSQLite:
			end := strings.IndexByte(sql[i+1:], ']')
			if end < 0 {
				return lexed{normalized: strings.TrimSpace(nb.String())}, &lexError{"unterminated quoted identifier"}
			}
			writeString(sql[i : i+1+end+1])
			i += 1 + end + 1

		case c == '$' && engine == EnginePostgres:
			tag, ok := dollarTag(sql[i:])
			if !ok {
				write(c)
				i++
				continue
			}
			rest := sql[i+len(tag):]
			end := strings.Index(rest, tag)
			if end < 0 {
				return lexed{normalized: strings.TrimSpace(nb.String())}, &lexError{"unterminated dollar-quoted string"}
			}
			writeString(sql[i : i+len(tag)+end+len(tag)])
			i += len(tag) + end + len(tag)

		case c == ';':
			write(c)
			i++

		default:
			write(c)
			i++
		}
	}

	normalized := strings.TrimSpace(nb.String())
	masked := strings.TrimSpace(mb.String())
	parts := strings.Split(masked, ";")
	stmts := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			stmts = append(stmts, t)
		}
	}
	return lexed{normalized: normalized, masked: masked, stmts: stmts}, nil
}

// scanQuoted 扫描引号包围的片段，返回原文（含引号）与结束位置。
// 仅 MySQL 识别反斜杠转义；单引号内的 ” 与双引号内的 "" 都是转义。
func scanQuoted(s string, start int, quote byte, backslash bool) (string, int, error) {
	i := start + 1
	for i < len(s) {
		if backslash && s[i] == '\\' && i+1 < len(s) {
			i += 2
			continue
		}
		if s[i] == quote {
			if i+1 < len(s) && s[i+1] == quote {
				i += 2
				continue
			}
			return s[start : i+1], i + 1, nil
		}
		i++
	}
	return "", len(s), &lexError{"unterminated quoted literal"}
}

// dollarTag 识别 $tag$ 形式的 PostgreSQL 美元引号。
func dollarTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	j := 1
	for j < len(s) && isIdentPartByte(s[j]) && s[j] != '$' {
		j++
	}
	if j < len(s) && s[j] == '$' {
		return s[:j+1], true
	}
	return "", false
}

func isIdentStartByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isIdentPartByte(c byte) bool {
	return isIdentStartByte(c) || c >= '0' && c <= '9' || c == '$'
}

type sqlToken struct {
	upper string
	depth int
}

// tokenize 提取 masked SQL 中的标识符/关键字及其括号深度。
// 数值、运算符与占位符被忽略。
func tokenize(masked string) []sqlToken {
	var toks []sqlToken
	depth := 0
	for i := 0; i < len(masked); {
		c := masked[i]
		switch {
		case c == '(':
			depth++
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			i++
		case isIdentStartByte(c):
			j := i + 1
			for j < len(masked) && isIdentPartByte(masked[j]) {
				j++
			}
			toks = append(toks, sqlToken{upper: strings.ToUpper(masked[i:j]), depth: depth})
			i = j
		default:
			i++
		}
	}
	return toks
}

// checkLexical 对 MySQL/SQLite 做词法只读分类。
func checkLexical(engine Engine, lex lexed) Result {
	_ = engine
	if len(lex.stmts) == 0 {
		return reject(KindUnknown, RuleInvalid, "SQL contains no statement", lex.normalized)
	}
	if len(lex.stmts) > 1 {
		return reject(KindMultiple, RuleMultiple, "multiple statements are not allowed", lex.normalized)
	}

	stmt := lex.stmts[0]
	toks := tokenize(stmt)
	if len(toks) == 0 {
		return reject(KindUnknown, RuleInvalid, "SQL contains no statement", lex.normalized)
	}
	first := toks[0].upper

	if res, rejected := scanDangerous(lex.normalized, toks, first); rejected {
		return res
	}

	switch first {
	case "SELECT":
		return allowed(KindSelect, RuleRead, "read-only SELECT", lex.normalized)
	case "WITH":
		final, ok := withFinalKeyword(toks)
		if !ok {
			return reject(KindUnknown, RuleInvalid, "WITH must end in SELECT", lex.normalized)
		}
		if final != "SELECT" && final != "TABLE" {
			return reject(kindForKeyword(final), RuleDML, "WITH must end in SELECT", lex.normalized)
		}
		return allowed(KindWithSelect, RuleRead, "read-only WITH...SELECT", lex.normalized)
	case "EXPLAIN":
		kind, rule, ok := checkExplainTokens(toks)
		if !ok {
			return reject(kind, rule, "EXPLAIN must target a SELECT", lex.normalized)
		}
		return allowed(KindExplain, RuleRead, "read-only EXPLAIN", lex.normalized)
	default:
		kind := kindForKeyword(first)
		rule := ruleForKind(kind)
		return reject(kind, rule, "statement is not read-only", lex.normalized)
	}
}

// scanDangerous 扫描整条语句（含子查询）中的写/危险关键字。
func scanDangerous(normalized string, toks []sqlToken, first string) (Result, bool) {
	for i, t := range toks {
		switch t.upper {
		case "INSERT":
			return reject(KindInsert, RuleDML, "INSERT is not read-only", normalized), true
		case "UPDATE":
			if i > 0 && toks[i-1].upper == "FOR" {
				return reject(KindSelect, RuleLocking, "FOR UPDATE is not read-only", normalized), true
			}
			return reject(KindUpdate, RuleDML, "UPDATE is not read-only", normalized), true
		case "DELETE":
			return reject(KindDelete, RuleDML, "DELETE is not read-only", normalized), true
		case "REPLACE", "MERGE", "UPSERT":
			return reject(KindUpdate, RuleDML, "write statement is not read-only", normalized), true
		case "CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME", "GRANT", "REVOKE", "COMMENT":
			return reject(KindDDL, RuleDDL, "DDL is not read-only", normalized), true
		case "ATTACH", "DETACH", "VACUUM", "REINDEX", "PRAGMA":
			return reject(KindDDL, RuleDDL, "SQLite maintenance statement is not read-only", normalized), true
		case "ANALYZE":
			if first == "EXPLAIN" {
				return reject(KindExplain, RuleExplainAnalyze, "EXPLAIN ANALYZE executes the query", normalized), true
			}
			return reject(KindDDL, RuleDDL, "ANALYZE is not read-only", normalized), true
		case "CALL", "EXEC", "EXECUTE":
			return reject(KindCall, RuleCall, "stored procedure call is not read-only", normalized), true
		case "COPY":
			return reject(KindCopy, RuleCopy, "COPY is not read-only", normalized), true
		case "LOAD":
			return reject(KindCopy, RuleCopy, "LOAD DATA/LOAD_FILE is not read-only", normalized), true
		case "LOAD_FILE":
			// tokenize 把 LOAD_FILE 视为单个标识符；它是 MySQL 读服务器任意
			// 文件的函数，必须独立于 LOAD 拦截。
			return reject(KindCopy, RuleCopy, "LOAD_FILE reads server files", normalized), true
		case "LOAD_EXTENSION":
			// SQLite load_extension() 可加载任意共享库执行代码；裸标识符即
			// 调用（引号包裹的列名已被 lex mask 成 '?'，不会命中此分支）。
			return reject(KindCall, RuleDangerousFn, "load_extension is not allowed", normalized), true
		case "SLEEP", "BENCHMARK":
			// MySQL SLEEP()/BENCHMARK() 是纯耗时型 DoS 向量，只读 SELECT
			// 也不应放行；裸标识符即调用（引号包裹的列名已被 lex mask）。
			return reject(KindCall, RuleDangerousFn, "time-based function is not allowed", normalized), true
		case "BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE":
			return reject(KindTCL, RuleTCL, "transaction control is not read-only", normalized), true
		case "START":
			if i+1 < len(toks) && toks[i+1].upper == "TRANSACTION" {
				return reject(KindTCL, RuleTCL, "transaction control is not read-only", normalized), true
			}
		case "INTO":
			return reject(KindDDL, RuleInto, "SELECT INTO / INTO OUTFILE is not allowed", normalized), true
		case "FOR":
			if i+1 < len(toks) {
				switch toks[i+1].upper {
				case "UPDATE", "SHARE", "NO":
					return reject(KindSelect, RuleLocking, "row locking clause is not read-only", normalized), true
				}
			}
		case "LOCK":
			if i+2 < len(toks) && toks[i+1].upper == "IN" && toks[i+2].upper == "SHARE" {
				return reject(KindSelect, RuleLocking, "LOCK IN SHARE MODE is not read-only", normalized), true
			}
		}
	}
	return Result{}, false
}

// withFinalKeyword 返回 WITH 查询在顶层最终执行的关键字。
func withFinalKeyword(toks []sqlToken) (string, bool) {
	for i := 1; i < len(toks); i++ {
		if toks[i].depth != 0 {
			continue
		}
		switch toks[i].upper {
		case "SELECT", "INSERT", "UPDATE", "DELETE", "VALUES", "TABLE", "MERGE":
			return toks[i].upper, true
		}
	}
	return "", false
}

// checkExplainTokens 校验 EXPLAIN 的目标必须是 SELECT。
func checkExplainTokens(toks []sqlToken) (Kind, string, bool) {
	for i := 1; i < len(toks); i++ {
		switch toks[i].upper {
		case "ANALYZE":
			return KindExplain, RuleExplainAnalyze, false
		case "SELECT":
			return KindExplain, RuleRead, true
		case "WITH":
			for j := i + 1; j < len(toks); j++ {
				if toks[j].depth == 0 && toks[j].upper == "SELECT" {
					return KindExplain, RuleRead, true
				}
			}
			return KindUnknown, RuleInvalid, false
		case "INSERT", "UPDATE", "DELETE", "VALUES", "CREATE", "ALTER", "DROP", "TABLE":
			return kindForKeyword(toks[i].upper), RuleDML, false
		}
	}
	return KindUnknown, RuleInvalid, false
}

func kindForKeyword(kw string) Kind {
	switch kw {
	case "SELECT", "TABLE":
		return KindSelect
	case "WITH":
		return KindWithSelect
	case "EXPLAIN":
		return KindExplain
	case "INSERT":
		return KindInsert
	case "UPDATE":
		return KindUpdate
	case "DELETE":
		return KindDelete
	case "CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME", "GRANT", "REVOKE":
		return KindDDL
	case "COPY", "LOAD":
		return KindCopy
	case "CALL", "EXEC", "EXECUTE":
		return KindCall
	case "BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE", "START":
		return KindTCL
	default:
		return KindUnknown
	}
}

func ruleForKind(kind Kind) string {
	switch kind {
	case KindDDL:
		return RuleDDL
	case KindInsert, KindUpdate, KindDelete:
		return RuleDML
	case KindCopy:
		return RuleCopy
	case KindCall:
		return RuleCall
	case KindTCL:
		return RuleTCL
	default:
		return RuleInvalid
	}
}
