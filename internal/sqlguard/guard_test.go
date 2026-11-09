package sqlguard

import (
	"strings"
	"testing"
)

func TestCheckPostgresAllowed(t *testing.T) {
	cases := []string{
		"SELECT 1",
		"SELECT * FROM users WHERE id = 1",
		"select id, name from users order by id limit 10",
		"WITH x AS (SELECT 1 AS n) SELECT * FROM x",
		"WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM t WHERE n<10) SELECT * FROM t",
		"EXPLAIN SELECT 1",
		"EXPLAIN VERBOSE SELECT * FROM users",
		"SELECT 'DROP TABLE users' AS label",
		"SELECT 'pg_read_file' AS fn",
		"SELECT $$a;b$$ AS body",
		"SELECT $1::int",
		"SELECT 1 -- DROP TABLE users\n",
		"SELECT /* DELETE FROM users */ 1",
		"SELECT 1;",
		"SELECT a FROM t UNION SELECT b FROM u",
		// 无锁定子句的嵌套子查询不得因递归检查被误伤。
		"SELECT * FROM (SELECT 1) x",
		"SELECT id FROM t WHERE id IN (SELECT id FROM t2)",
		"SELECT (SELECT 1) AS x FROM t WHERE y IN (SELECT 2)",
		"SELECT * FROM a JOIN (SELECT 1) b ON true",
	}
	for _, sql := range cases {
		res, err := Check(EnginePostgres, sql)
		if err != nil {
			t.Fatalf("Check(%q) error: %v", sql, err)
		}
		if !res.Allowed {
			t.Fatalf("Check(%q) = %+v, want allowed", sql, res)
		}
		if res.RiskLevel != "safe" {
			t.Fatalf("Check(%q).RiskLevel = %q, want safe", sql, res.RiskLevel)
		}
	}
}

func TestCheckPostgresRejected(t *testing.T) {
	cases := []struct {
		sql  string
		kind Kind
		rule string
	}{
		{"EXPLAIN ANALYZE SELECT 1", KindExplain, RuleExplainAnalyze},
		{"SELECT 1; SELECT 2", KindMultiple, RuleMultiple},
		{"INSERT INTO t VALUES (1)", KindInsert, RuleDML},
		{"UPDATE t SET a = 1", KindUpdate, RuleDML},
		{"DELETE FROM t", KindDelete, RuleDML},
		{"CREATE TABLE t (id int)", KindDDL, RuleDDL},
		{"DROP TABLE t", KindDDL, RuleDDL},
		{"ALTER TABLE t ADD COLUMN a int", KindDDL, RuleDDL},
		{"TRUNCATE t", KindDDL, RuleDDL},
		{"GRANT SELECT ON t TO u", KindDDL, RuleDDL},
		{"COPY t FROM STDIN", KindCopy, RuleCopy},
		{"CALL p()", KindCall, RuleCall},
		{"SELECT * FROM t FOR UPDATE", KindSelect, RuleLocking},
		{"SELECT * FROM t FOR SHARE", KindSelect, RuleLocking},
		// 嵌套位置的锁定子句同样拒绝（评审发现：SelectClause 曾不递归
		// FROM/表达式子查询，"SELECT * FROM (SELECT 1 FOR UPDATE) sub"
		// 曾判 allowed）。
		{"SELECT * FROM (SELECT 1 FOR UPDATE) sub", KindSelect, RuleLocking},
		{"SELECT * FROM (SELECT id FROM t FOR UPDATE) x LIMIT 1", KindSelect, RuleLocking},
		{"SELECT 1 FROM t WHERE id IN (SELECT id FROM t2 FOR UPDATE)", KindSelect, RuleLocking},
		{"SELECT * FROM a JOIN (SELECT 1 FOR SHARE) b ON true", KindSelect, RuleLocking},
		{"SELECT * FROM (SELECT * FROM (SELECT 1 FOR UPDATE) d) e", KindSelect, RuleLocking},
		{"SELECT EXISTS (SELECT 1 FROM t FOR UPDATE)", KindSelect, RuleLocking},
		{"SELECT (SELECT max(id) FROM t FOR UPDATE) FROM a", KindSelect, RuleLocking},
		{"SELECT * INTO t2 FROM t", KindDDL, RuleInto},
		{"WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x", KindDelete, RuleDML},
		{"BEGIN", KindTCL, RuleTCL},
		{"COMMIT", KindTCL, RuleTCL},
		{"VALUES (1)", KindUnknown, RuleInvalid},
		{"SELECT pg_terminate_backend(1234)", KindCall, RuleDangerousFn},
		{"SELECT pg_read_file('/etc/passwd')", KindCall, RuleDangerousFn},
		{"SELECT pg_catalog.pg_read_file('/etc/passwd')", KindCall, RuleDangerousFn},
		{`SELECT "pg_terminate_backend"(1234)`, KindCall, RuleDangerousFn},
		{"SELECT nextval('users_id_seq')", KindCall, RuleDangerousFn},
	}
	for _, tc := range cases {
		res, err := Check(EnginePostgres, tc.sql)
		if err != nil {
			t.Fatalf("Check(%q) error: %v", tc.sql, err)
		}
		if res.Allowed {
			t.Fatalf("Check(%q) allowed, want rejected", tc.sql)
		}
		if res.Kind != tc.kind {
			t.Fatalf("Check(%q).Kind = %q (%s), want %q", tc.sql, res.Kind, res.RuleID, tc.kind)
		}
		if res.RuleID != tc.rule {
			t.Fatalf("Check(%q).RuleID = %q (%s), want %q", tc.sql, res.RuleID, res.Reason, tc.rule)
		}
		if res.RiskLevel != "needs_approval" {
			t.Fatalf("Check(%q).RiskLevel = %q, want needs_approval", tc.sql, res.RiskLevel)
		}
	}
}

func TestCheckLexicalAllowed(t *testing.T) {
	cases := []struct {
		engine Engine
		sql    string
	}{
		{EngineMySQL, "SELECT * FROM users"},
		{EngineMySQL, "select id from t where name = 'delete'"},
		{EngineMySQL, "SELECT * FROM t /* INSERT INTO x */ WHERE 1 = 1"},
		{EngineMySQL, "SELECT * FROM t -- DELETE FROM x"},
		{EngineMySQL, "SELECT * FROM `delete`"},
		{EngineMySQL, "SELECT * FROM t WHERE id IN (1, 2)"},
		{EngineMySQL, "WITH x AS (SELECT 1) SELECT * FROM x"},
		{EngineMySQL, "EXPLAIN SELECT * FROM t"},
		{EngineMySQL, "EXPLAIN FORMAT=JSON SELECT * FROM t"},
		{EngineMySQL, "SELECT * FROM t WHERE a = 'FOR UPDATE'"},
		{EngineMySQL, "SELECT * FROM t WHERE a = 'x\\' -- not a comment'"},
		{EngineMySQL, "SELECT 1 # comment ; DROP TABLE t"},
		{EngineMySQL, "select 1;\n"},
		// 与危险函数同名的列名必须用引号包裹（词法层已 mask 成 '?'），不误伤。
		{EngineMySQL, "SELECT `sleep` AS wait_sec FROM t"},
		{EngineMySQL, "SELECT `benchmark` FROM t"},
		{EngineSQLite, `SELECT "load_extension" AS col FROM t`},
		{EngineSQLite, "SELECT * FROM users"},
		{EngineSQLite, `SELECT "update" FROM t`},
		{EngineSQLite, "SELECT 'a''b' FROM t"},
		{EngineSQLite, "EXPLAIN QUERY PLAN SELECT * FROM t"},
		{EngineSQLite, "EXPLAIN SELECT * FROM t"},
		{EngineSQLite, "WITH x AS (SELECT 1) SELECT * FROM x"},
		{EngineSQLite, "SELECT * FROM [delete]"},
	}
	for _, tc := range cases {
		res, err := Check(tc.engine, tc.sql)
		if err != nil {
			t.Fatalf("Check(%s, %q) error: %v", tc.engine, tc.sql, err)
		}
		if !res.Allowed {
			t.Fatalf("Check(%s, %q) = %+v, want allowed", tc.engine, tc.sql, res)
		}
	}
}

func TestCheckLexicalRejected(t *testing.T) {
	cases := []struct {
		engine Engine
		sql    string
		kind   Kind
		rule   string
	}{
		{EngineMySQL, "SELECT 1; DELETE FROM t", KindMultiple, RuleMultiple},
		{EngineMySQL, "SELECT * FROM t FOR UPDATE", KindSelect, RuleLocking},
		{EngineMySQL, "SELECT * FROM t LOCK IN SHARE MODE", KindSelect, RuleLocking},
		{EngineMySQL, "SELECT * INTO OUTFILE '/tmp/x' FROM t", KindDDL, RuleInto},
		{EngineMySQL, "INSERT INTO t VALUES (1)", KindInsert, RuleDML},
		{EngineMySQL, "UPDATE t SET a = 1", KindUpdate, RuleDML},
		{EngineMySQL, "DELETE FROM t", KindDelete, RuleDML},
		{EngineMySQL, "REPLACE INTO t VALUES (1)", KindUpdate, RuleDML},
		{EngineMySQL, "CREATE TABLE t (id int)", KindDDL, RuleDDL},
		{EngineMySQL, "DROP TABLE t", KindDDL, RuleDDL},
		{EngineMySQL, "TRUNCATE TABLE t", KindDDL, RuleDDL},
		{EngineMySQL, "ALTER TABLE t ADD c int", KindDDL, RuleDDL},
		{EngineMySQL, "LOAD DATA INFILE 'x' INTO TABLE t", KindCopy, RuleCopy},
		{EngineMySQL, "CALL p()", KindCall, RuleCall},
		{EngineMySQL, "BEGIN", KindTCL, RuleTCL},
		{EngineMySQL, "START TRANSACTION", KindTCL, RuleTCL},
		{EngineMySQL, "EXPLAIN ANALYZE SELECT * FROM t", KindExplain, RuleExplainAnalyze},
		{EngineMySQL, "ANALYZE TABLE t", KindDDL, RuleDDL},
		{EngineMySQL, "WITH x AS (DELETE FROM t) SELECT 1", KindDelete, RuleDML},
		{EngineMySQL, "SELECT * FROM t WHERE a = ';' ; DROP TABLE t", KindMultiple, RuleMultiple},
		{EngineMySQL, "SELECT 1 /*! , SLEEP(5) */", KindUnknown, RuleInvalid},
		{EngineMySQL, "SELECT 1 /*!50000 INTO OUTFILE '/tmp/pwn' */", KindUnknown, RuleInvalid},
		{EngineMySQL, "SELECT 1 --1 INTO OUTFILE '/tmp/pwn'", KindDDL, RuleInto},
		// 耗时型/加载型危险函数：只读 SELECT 也不放行（评审修复回归）。
		{EngineMySQL, "SELECT SLEEP(10)", KindCall, RuleDangerousFn},
		{EngineMySQL, "SELECT * FROM t WHERE a = 1 AND SLEEP(5)", KindCall, RuleDangerousFn},
		{EngineMySQL, "SELECT BENCHMARK(1000000, MD5('x'))", KindCall, RuleDangerousFn},
		{EngineMySQL, "select sleep(3) from t", KindCall, RuleDangerousFn},
		{EngineSQLite, "SELECT load_extension('/x')", KindCall, RuleDangerousFn},
		{EngineSQLite, `SELECT LOAD_EXTENSION('\\evil.so') FROM t`, KindCall, RuleDangerousFn},
		{EngineSQLite, "PRAGMA foreign_keys = ON", KindDDL, RuleDDL},
		{EngineSQLite, "ATTACH DATABASE 'x.db' AS y", KindDDL, RuleDDL},
		{EngineSQLite, "VACUUM", KindDDL, RuleDDL},
		{EngineSQLite, "SELECT * FROM t; DROP TABLE t", KindMultiple, RuleMultiple},
		{EngineSQLite, `SELECT * FROM t WHERE a = 'x\'; DROP TABLE t`, KindMultiple, RuleMultiple},
		{EngineSQLite, "WITH x AS (UPDATE t SET a=1) SELECT 1", KindUpdate, RuleDML},
	}
	for _, tc := range cases {
		res, err := Check(tc.engine, tc.sql)
		if err != nil {
			t.Fatalf("Check(%s, %q) error: %v", tc.engine, tc.sql, err)
		}
		if res.Allowed {
			t.Fatalf("Check(%s, %q) allowed, want rejected", tc.engine, tc.sql)
		}
		if res.Kind != tc.kind {
			t.Fatalf("Check(%s, %q).Kind = %q (%s), want %q", tc.engine, tc.sql, res.Kind, res.RuleID, tc.kind)
		}
		if res.RuleID != tc.rule {
			t.Fatalf("Check(%s, %q).RuleID = %q (%s), want %q", tc.engine, tc.sql, res.RuleID, res.Reason, tc.rule)
		}
	}
}

func TestCheckInvalidInput(t *testing.T) {
	cases := []struct {
		engine Engine
		sql    string
	}{
		{EnginePostgres, ""},
		{EnginePostgres, strings.Repeat("a", MaxSQLBytes+1)},
		{EnginePostgres, "SELECT 1\x00"},
		{EngineMySQL, ""},
		{EngineMySQL, "/* unterminated"},
		{EngineMySQL, "SELECT 'unterminated"},
		{EngineSQLite, "SELECT `unterminated"},
	}
	for _, tc := range cases {
		res, err := Check(tc.engine, tc.sql)
		if err != nil {
			t.Fatalf("Check(%s, %q) error: %v", tc.engine, truncate(tc.sql), err)
		}
		if res.Allowed {
			t.Fatalf("Check(%s, %q) allowed, want rejected", tc.engine, truncate(tc.sql))
		}
		if res.RuleID != RuleInvalid && res.RuleID != RuleMultiple {
			t.Fatalf("Check(%s, %q).RuleID = %q, want %q", tc.engine, truncate(tc.sql), res.RuleID, RuleInvalid)
		}
	}
}

func TestCheckUnsupportedEngine(t *testing.T) {
	_, err := Check(Engine("oracle"), "SELECT 1")
	if err == nil {
		t.Fatal("expected ErrUnsupportedEngine")
	}
}

func TestNormalizedSQL(t *testing.T) {
	res, err := Check(EngineMySQL, "SELECT   *\nFROM t   -- trailing\nWHERE id = 1")
	if err != nil || !res.Allowed {
		t.Fatalf("Check failed: %+v %v", res, err)
	}
	if res.Normalized != "SELECT * FROM t WHERE id = 1" {
		t.Fatalf("Normalized = %q", res.Normalized)
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
