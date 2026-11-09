// T-SQL 只读词法分类（W2，CONTRACT.md 5.3）。
//
// T-SQL 没有成熟的开源 Go parser（契约裁决：不引），因此与 MySQL/SQLite
// 一样做词法分类，但使用 T-SQL 专用词法器：剥离 '...' / N'...' 字符串、
// [bracket ident] 与 "quoted ident"、-- 行注释与可嵌套的 /* */ 块注释后，
// 只放行首关键字 SELECT 或 WITH（且必须能证明顶层最终语句是 SELECT）。
//
// 显式拒绝（契约 5.3 定稿）：SELECT ... INTO、FOR BROWSE / FOR UPDATE /
// FOR XML / FOR JSON PATH、OPENROWSET / OPENQUERY / OPENDATASOURCE /
// BULK INSERT、EXEC / EXECUTE / sp_* / xp_*、多语句（; 分隔）、
// BACKUP / RESTORE / SHUTDOWN / DBCC、DML / DDL / TCL。
// T-SQL 特有只读语法（TOP、OFFSET ... FETCH NEXT、WITH (NOLOCK) 等
// 表提示、FOR SYSTEM_TIME）不触发拒绝。
package sqlguard

import (
	"strings"
)

// lexTSQL 对 T-SQL 做剥离词法：normalized 用于展示（保留字符串原文、
// 压缩空白），masked 把字符串与引用标识符替换为 '?'，语句计数与关键
// 字扫描都基于 masked，因此字符串、方括号标识符与注释里的内容不会
// 被误判，也无法借注释拼接/绕过关键字。
//
// 嵌套 /* /* */ */ 是 T-SQL 语义（与 MySQL 不同），按深度计数剥除。
func lexTSQL(sql string) (lexed, error) {
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
	writeMasked := func(raw string) {
		flushSpace()
		nb.WriteString(raw)
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
			// T-SQL 中 -- 到行尾无条件是注释（不同于 MySQL 需后随空白）。
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			pendingSpace = true

		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			depth := 1
			i += 2
			for i < len(sql) && depth > 0 {
				if sql[i] == '/' && i+1 < len(sql) && sql[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if sql[i] == '*' && i+1 < len(sql) && sql[i+1] == '/' {
					depth--
					i += 2
					continue
				}
				i++
			}
			if depth > 0 {
				return lexed{normalized: strings.TrimSpace(nb.String())}, &lexError{"unterminated block comment"}
			}
			pendingSpace = true

		case c == '\'' || (c == 'N' || c == 'n') && i+1 < len(sql) && sql[i+1] == '\'':
			// N'...' 国别字符串：N 前缀不进入 token 流，字符串按 '' 转义扫描。
			start := i
			if c != '\'' {
				start = i + 1
			}
			raw, next, err := scanQuoted(sql, start, '\'', false)
			if err != nil {
				return lexed{normalized: strings.TrimSpace(nb.String())}, err
			}
			writeMasked(raw)
			i = next

		case c == '[':
			// [bracket ident]：]] 是转义的 ]。
			start := i + 1
			j := start
			for j < len(sql) {
				if sql[j] == ']' {
					if j+1 < len(sql) && sql[j+1] == ']' {
						j += 2
						continue
					}
					writeMasked(sql[i : j+1])
					i = j + 1
					break
				}
				j++
			}
			if j >= len(sql) {
				return lexed{normalized: strings.TrimSpace(nb.String())}, &lexError{"unterminated bracket identifier"}
			}

		case c == '"':
			// QUOTED_IDENTIFIER ON（驱动默认）下 "..." 是引用标识符。
			raw, next, err := scanQuoted(sql, i, '"', false)
			if err != nil {
				return lexed{normalized: strings.TrimSpace(nb.String())}, err
			}
			writeMasked(raw)
			i = next

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

// checkMSSQL 对单条 T-SQL 做只读分类（Check 的 mssql 分支）。
func checkMSSQL(sql string) Result {
	lex, err := lexTSQL(sql)
	if err != nil {
		return reject(KindUnknown, RuleInvalid, err.Error(), lex.normalized)
	}
	if len(lex.stmts) == 0 {
		return reject(KindUnknown, RuleInvalid, "SQL contains no statement", lex.normalized)
	}
	if len(lex.stmts) > 1 {
		return reject(KindMultiple, RuleMultiple, "multiple statements are not allowed", lex.normalized)
	}

	toks := tokenize(lex.stmts[0])
	if len(toks) == 0 {
		return reject(KindUnknown, RuleInvalid, "SQL contains no statement", lex.normalized)
	}

	if res, rejected := scanTSQLDangerous(lex.normalized, toks); rejected {
		return res
	}

	switch toks[0].upper {
	case "SELECT":
		return allowed(KindSelect, RuleRead, "read-only SELECT", lex.normalized)
	case "WITH":
		final, ok := withFinalKeyword(toks)
		if !ok || final != "SELECT" {
			return reject(kindForKeyword(final), RuleDML, "WITH must end in SELECT", lex.normalized)
		}
		return allowed(KindWithSelect, RuleRead, "read-only WITH...SELECT", lex.normalized)
	default:
		kind := kindForKeyword(toks[0].upper)
		return reject(kind, ruleForKind(kind), "statement is not read-only", lex.normalized)
	}
}

// scanTSQLDangerous 扫描整条语句（含子查询）中的写/危险关键字与
// T-SQL 专有的外发/执行向量。词法层已剥注释并 mask 字符串与标识符，
// 注释绕过（EXEC/**/xp_cmdshell）与字符串伪装（'DROP TABLE'）均无效。
func scanTSQLDangerous(normalized string, toks []sqlToken) (Result, bool) {
	for i, t := range toks {
		// 扩展过程（xp_cmdshell）与 sp_ 存储过程一律拒绝；三段名
		//（master.dbo.xp_cmdshell）经 tokenize 拆点后同样命中前缀。
		if strings.HasPrefix(t.upper, "XP_") || strings.HasPrefix(t.upper, "SP_") {
			return reject(KindCall, RuleCall, "stored procedure call is not read-only", normalized), true
		}
		switch t.upper {
		case "INSERT":
			return reject(KindInsert, RuleDML, "INSERT is not read-only", normalized), true
		case "UPDATE", "MERGE":
			return reject(KindUpdate, RuleDML, "write statement is not read-only", normalized), true
		case "DELETE":
			return reject(KindDelete, RuleDML, "DELETE is not read-only", normalized), true
		case "CREATE", "ALTER", "DROP", "TRUNCATE", "GRANT", "REVOKE", "DENY":
			return reject(KindDDL, RuleDDL, "DDL is not read-only", normalized), true
		case "BACKUP", "RESTORE", "SHUTDOWN", "DBCC":
			return reject(KindDDL, RuleDDL, "maintenance statement is not read-only", normalized), true
		case "BULK":
			return reject(KindCopy, RuleCopy, "BULK INSERT is not read-only", normalized), true
		case "OPENROWSET", "OPENQUERY", "OPENDATASOURCE", "OPENXML":
			return reject(KindCall, RuleDangerousFn, "external data access is not allowed", normalized), true
		case "EXEC", "EXECUTE":
			return reject(KindCall, RuleCall, "stored procedure call is not read-only", normalized), true
		case "WAITFOR":
			// WAITFOR DELAY/TIME 是时间盲注/DoS 向量（同类于 MySQL SLEEP）。
			return reject(KindCall, RuleDangerousFn, "WAITFOR is not allowed", normalized), true
		case "BEGIN", "COMMIT", "ROLLBACK":
			// 三者均为 T-SQL 保留字，作列名必须写成 [begin]（已 mask 不误拦）。
			// SAVE 不是保留字（SAVE TRANSACTION 由首关键字拦截），不扫。
			return reject(KindTCL, RuleTCL, "transaction control is not read-only", normalized), true
		case "INTO":
			// SELECT ... INTO 在 T-SQL 中建表；INSERT INTO 由 INSERT 拦截。
			return reject(KindDDL, RuleInto, "SELECT INTO is not allowed", normalized), true
		case "GO":
			// 客户端批分隔符（sqlcmd 语义）；一并拒绝避免批级拆分绕过。
			return reject(KindMultiple, RuleMultiple, "batch separator is not allowed", normalized), true
		case "FOR":
			if i+1 < len(toks) {
				switch toks[i+1].upper {
				case "UPDATE", "BROWSE", "XML", "JSON":
					// FOR XML/JSON 可把查询结果外发，保守拒绝（契约 5.3）。
					return reject(KindSelect, RuleLocking, "FOR clause is not allowed", normalized), true
				}
				// FOR SYSTEM_TIME（临时表查询）是只读语法，放行。
			}
		}
	}
	return Result{}, false
}
