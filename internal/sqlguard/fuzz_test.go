package sqlguard

import (
	"strings"
	"testing"

	"aiops/internal/risk"
)

// FuzzCheck 对 Check 做模糊测试：同一份输入同时派生 postgres/mysql/sqlite
// 三种引擎，核心不变式：
//  1. 任何输入绝不 panic，字段合法、合法方言不返回业务错误；
//  2. 任何引擎下多语句不允许；
//  3. MySQL 下任何形式（含 /*! */ 可执行注释、-- 后无空格变体）的
//     INTO OUTFILE / LOAD_FILE / 写语句不允许；
//  4. PG 下 pg_terminate_backend、pg_read_file、EXPLAIN ANALYZE 不允许。
func FuzzCheck(f *testing.F) {
	seeds := []string{
		// MySQL INTO OUTFILE / LOAD_FILE 变体（上一轮修复的绕过手法）。
		"SELECT * INTO OUTFILE '/tmp/x' FROM t",
		"SELECT 1 /*!50000 INTO OUTFILE '/tmp/pwn' */",
		"SELECT 1 /*! , SLEEP(5) */",
		"SELECT 1 --1 INTO OUTFILE '/tmp/pwn'",
		"SELECT 1 -- INTO OUTFILE '/tmp/pwn'\nSELECT 2",
		"SELECT LOAD_FILE('/etc/passwd')",
		"select load_file('/etc/passwd')",
		"SELECT `load_file` FROM t",
		// 多语句。
		"SELECT 1; SELECT 2",
		"SELECT * FROM t WHERE a = ';' ; DROP TABLE t",
		"SELECT 1; DELETE FROM t",
		"SELECT 1\n;DROP TABLE t",
		// PG 危险函数与 EXPLAIN ANALYZE。
		"SELECT pg_terminate_backend(1234)",
		"SELECT pg_read_file('/etc/passwd')",
		"SELECT pg_catalog.pg_read_file('/etc/passwd')",
		`SELECT "pg_terminate_backend"(1234)`,
		"EXPLAIN ANALYZE SELECT 1",
		"EXPLAIN (ANALYZE, BUFFERS) SELECT 1",
		"SELECT nextval('users_id_seq')",
		// 写语句。
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"DROP TABLE t",
		"CREATE TABLE t (id int)",
		"ALTER TABLE t ADD c int",
		"TRUNCATE t",
		"REPLACE INTO t VALUES (1)",
		"LOAD DATA INFILE 'x' INTO TABLE t",
		"CALL p()",
		"BEGIN",
		"START TRANSACTION",
		"GRANT SELECT ON t TO u",
		"ANALYZE TABLE t",
		"WITH x AS (DELETE FROM t) SELECT 1",
		"SELECT * FROM t FOR UPDATE",
		"SELECT * INTO t2 FROM t",
		// 危险函数（MySQL/SQLite 词法侧，评审修复回归）。
		"SELECT SLEEP(10)",
		"SELECT BENCHMARK(1000000, MD5('x'))",
		"SELECT load_extension('/x')",
		// 合法只读（防止检测器误报）。
		"SELECT 1",
		"SELECT * FROM users WHERE id = 1",
		"WITH x AS (SELECT 1 AS n) SELECT * FROM x",
		"WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM t WHERE n<10) SELECT * FROM t",
		"EXPLAIN SELECT 1",
		"EXPLAIN FORMAT=JSON SELECT * FROM t",
		"EXPLAIN QUERY PLAN SELECT * FROM t",
		"SELECT 'DROP TABLE users' AS label",
		"SELECT 'pg_read_file' AS fn",
		"SELECT $$a;b$$ AS body",
		"SELECT 1 -- DROP TABLE users\n",
		"SELECT /* DELETE FROM users */ 1",
		"SELECT 1 # comment ; DROP TABLE t",
		"SELECT * FROM `delete`",
		"SELECT * FROM [delete]",
		`SELECT "update" FROM t`,
		"SELECT 'a''b' FROM t",
		"SELECT * FROM t WHERE a = 'x\\' -- not a comment'",
		"SELECT * FROM t WHERE name = 'delete'",
		"SELECT updated_at FROM t",
		"select 1;\n",
		// 非法 / 未闭合形态。
		"",
		"/* unterminated",
		"SELECT 'unterminated",
		"SELECT `unterminated",
		// 内存红线引爆样本：多语句 / 嵌套括号 / 可执行注释 / -- 变体轰炸，
		// 词法剥离与 PG 解析不得出现输入驱动的巨量分配（回归种子锁死）。
		strings.Repeat("SELECT 1;", 100),
		strings.Repeat("((", 1000) + "SELECT 1" + strings.Repeat("))", 1000),
		strings.Repeat("/*! SELECT 1 */", 50),
		strings.Repeat("--1 INTO OUTFILE 'x'", 50),
		"SELECT LOAD_FILE(" + strings.Repeat("(", 500) + "'/etc/passwd'" + strings.Repeat(")", 500) + ")",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		// 内存红线：超过 4KB 的输入直接跳过，防止 fuzz worker 在超长
		// 输入上放大分配（Check 自身仍有 64KB 硬上限兜底）。
		if len(raw) > 4096 {
			t.Skip()
		}
		sql := string(raw)

		for _, eng := range []Engine{EnginePostgres, EngineMySQL, EngineSQLite} {
			lex, lexErr := lexSQL(sql, eng)
			res, err := Check(eng, sql)
			if err != nil {
				t.Fatalf("Check(%s, %q) returned error %v; supported engines must express rejection via Result", eng, sql, err)
			}

			// 不变式 1：字段合法。
			if res.RuleID == "" || res.Reason == "" {
				t.Fatalf("Check(%s, %q) incomplete result: %+v", eng, sql, res)
			}
			if res.Allowed {
				switch res.Kind {
				case KindSelect, KindWithSelect, KindExplain:
				default:
					t.Fatalf("Check(%s, %q) allowed with kind %q", eng, sql, res.Kind)
				}
				if res.RiskLevel != risk.LevelSafe {
					t.Fatalf("Check(%s, %q) allowed with risk %q", eng, sql, res.RiskLevel)
				}
			}

			// 不变式 2：任何引擎多语句不允许。
			if lexErr == nil && len(lex.stmts) > 1 && res.Allowed {
				t.Fatalf("Check(%s, %q) allowed %d statements", eng, sql, len(lex.stmts))
			}

			// 不变式 3：MySQL 写语句 / INTO OUTFILE / LOAD_FILE 任何形式不允许。
			if eng == EngineMySQL {
				if kw := matchMySQLWriteKeyword(mysqlSkeleton(sql)); kw != "" && res.Allowed {
					t.Fatalf("Check(mysql, %q) allowed despite %s", sql, kw)
				}
			}

			// 不变式 4：PG 危险函数与 EXPLAIN ANALYZE 不允许。
			if eng == EnginePostgres && lexErr == nil {
				if pgDangerMasked(lex.masked) && res.Allowed {
					t.Fatalf("Check(postgres, %q) allowed despite dangerous function/EXPLAIN ANALYZE", sql)
				}
			}
		}
	})
}

// ---------------------------------------------------------------------------
// 独立检测器：不复用 checkLexical/checkPostgres 的判定路径，只共享 lexSQL
// 的“语句切分”产物（多语句不变式即 lexed.stmts 的契约）。关键字检测基于
// 独立实现的 MySQL 骨架剥离，能暴露剥离器差异导致的放行。
// ---------------------------------------------------------------------------

// mysqlSkeleton 独立剥离 MySQL 方言的注释与引号内容，返回仅含代码骨架的文本。
//   - '...'、"..."（含反斜杠与双写转义）、`...` 的内容替换为空格；
//   - # 与 "-- "（后随空白/行尾）注释剥除；
//   - "--x" 后无空格的变体保留（不是注释）；
//   - /*! ... */ 可执行注释整体保留（其内容会被执行）。
func mysqlSkeleton(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	i := 0
	for i < len(sql) {
		c := sql[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			next := scanQuotedLite(sql, i, c, true)
			if next < 0 {
				// 未闭合：保留原文，让关键字检测更保守。
				b.WriteByte(c)
				i++
				continue
			}
			b.WriteByte(' ')
			i = next
		case c == '#':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			if i+2 >= len(sql) || sql[i+2] <= ' ' {
				for i < len(sql) && sql[i] != '\n' {
					i++
				}
			} else {
				b.WriteString("--")
				i += 2
			}
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			if i+2 < len(sql) && sql[i+2] == '!' {
				// 可执行注释：内容保留。
				b.WriteString("/*!")
				i += 3
				continue
			}
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				i = len(sql)
			} else {
				b.WriteByte(' ')
				i += 2 + end + 2
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// scanQuotedLite 扫描引号片段，返回结束下标（不含）；backslash 启用
// MySQL 反斜杠转义；双写引号是转义。未闭合返回 -1。
func scanQuotedLite(s string, start int, quote byte, backslash bool) int {
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
			return i + 1
		}
		i++
	}
	return -1
}

// mysqlWriteKeywords 是绝不允许出现在 MySQL 只读查询骨架中的词。
// 危险函数（SLEEP/BENCHMARK/LOAD_EXTENSION）以裸标识符出现在骨架即命中；
// 引号包裹的同名列已被骨架剥离，不会误报。
// OUTFILE/DUMPFILE 单独不是危险词（MySQL 非保留字，可作列名，如
// `SELECT outfile FROM t`），危险形态是 INTO OUTFILE / INTO DUMPFILE
// 短语；INTO 本身已单独列为绝不允许词。
var mysqlWriteKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "REPLACE", "MERGE", "UPSERT",
	"CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME", "GRANT", "REVOKE",
	"COMMENT", "CALL", "EXEC", "EXECUTE", "COPY", "LOAD",
	"INTO", "INTO OUTFILE", "INTO DUMPFILE", "LOAD_FILE",
	"ATTACH", "DETACH", "VACUUM", "REINDEX", "PRAGMA", "ANALYZE",
	"BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE",
	"SLEEP", "BENCHMARK", "LOAD_EXTENSION",
}

func isIdentByteM(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// matchMySQLWriteKeyword 在骨架中做词边界匹配，命中返回该词。
func matchMySQLWriteKeyword(skeleton string) string {
	up := strings.ToUpper(skeleton)
	for _, kw := range mysqlWriteKeywords {
		for i := 0; i+len(kw) <= len(up); i++ {
			if up[i:i+len(kw)] != kw {
				continue
			}
			if i > 0 && isIdentByteM(up[i-1]) {
				continue
			}
			if i+len(kw) < len(up) && isIdentByteM(up[i+len(kw)]) {
				continue
			}
			return kw
		}
	}
	return ""
}

// pgDangerMasked 在 PG 掩码文本（字符串已替换为 ?）中检测危险函数调用与
// EXPLAIN ANALYZE，命中返回描述。
func pgDangerMasked(masked string) bool {
	// 危险函数：pg_terminate_backend / pg_read_file 后紧跟 '('。
	lower := strings.ToLower(masked)
	for _, fn := range []string{"pg_terminate_backend", "pg_read_file"} {
		for i := 0; i+len(fn) <= len(lower); i++ {
			if lower[i:i+len(fn)] != fn {
				continue
			}
			if i > 0 && isIdentByteM(lower[i-1]) {
				continue
			}
			if i+len(fn) < len(lower) && isIdentByteM(lower[i+len(fn)]) {
				continue
			}
			k := i + len(fn)
			for k < len(lower) && (lower[k] == ' ' || lower[k] == '\t' || lower[k] == '\n' || lower[k] == '\r') {
				k++
			}
			if k < len(lower) && lower[k] == '(' {
				return true
			}
		}
	}

	// EXPLAIN ... ANALYZE：语句以 EXPLAIN 开头且出现 ANALYZE 词。
	toks := tokenize(masked)
	if len(toks) > 1 && toks[0].upper == "EXPLAIN" {
		for _, t := range toks[1:] {
			if t.upper == "ANALYZE" {
				return true
			}
		}
	}
	return false
}
