package sqlguard

import (
	"strings"
	"testing"
)

// 公共 API 级：词法与 AST 的未覆盖分支。

func TestLexicalEngineSpecificQuoteErrors(t *testing.T) {
	cases := []struct {
		name   string
		engine Engine
		sql    string
		msg    string
	}{
		// PostgreSQL 双引号标识符未闭合。
		{"pg unterminated dquote", EnginePostgres, `SELECT "abc`, "unterminated quoted literal"},
		// PostgreSQL 美元引号未闭合。
		{"pg unterminated dollar", EnginePostgres, "SELECT $tag$abc", "unterminated dollar-quoted string"},
		// SQLite 方括号标识符未闭合。
		{"sqlite unterminated bracket", EngineSQLite, "SELECT [abc", "unterminated quoted identifier"},
		// MySQL 单引号字符串未闭合。
		{"mysql unterminated quote", EngineMySQL, "SELECT 'abc", "unterminated quoted literal"},
		// MySQL 可执行注释被整体拒绝。
		{"mysql exec comment", EngineMySQL, "SELECT /*+ hint */ 1 /*! DELETE */", "MySQL executable comments"},
		// 块注释未闭合。
		{"unterminated block comment", EngineMySQL, "SELECT /* 1", "unterminated block comment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Check(tc.engine, tc.sql)
			if err != nil {
				t.Fatalf("Check error: %v", err)
			}
			if res.Allowed {
				t.Fatalf("Check(%q) allowed, want rejected", tc.sql)
			}
			if res.RuleID != RuleInvalid {
				t.Fatalf("rule = %s, want %s (reason=%s)", res.RuleID, RuleInvalid, res.Reason)
			}
			if !strings.Contains(res.Reason, tc.msg) {
				t.Fatalf("reason = %q, want containing %q", res.Reason, tc.msg)
			}
		})
	}
}

func TestLexicalStatementEdgeClassification(t *testing.T) {
	cases := []struct {
		name   string
		engine Engine
		sql    string
		kind   Kind
		rule   string
	}{
		// 无可识别 token 的语句。
		{"no tokens", EngineMySQL, "???", KindUnknown, RuleInvalid},
		// WITH 无顶层最终关键字。
		{"with dangling", EngineMySQL, "WITH x AS (SELECT 1)", KindUnknown, RuleInvalid},
		// WITH 最终落地 VALUES（非 SELECT/TABLE）。
		{"with values", EngineMySQL, "WITH x AS (SELECT 1) VALUES (1)", KindUnknown, RuleDML},
		// 未知首关键字。
		{"show tables", EngineMySQL, "SHOW TABLES", KindUnknown, RuleInvalid},
		// FOR UPDATE 锁定子句（词法路径）。
		{"for update", EngineMySQL, "SELECT * FROM t FOR UPDATE", KindSelect, RuleLocking},
		{"for share", EngineMySQL, "SELECT * FROM t FOR SHARE", KindSelect, RuleLocking},
		// 存储过程调用。
		{"call proc", EngineMySQL, "CALL do_work(1)", KindCall, RuleCall},
		// EXPLAIN 目标非 SELECT（词法路径：WITH 内无顶层 SELECT）。
		{"explain with no select", EngineMySQL, "EXPLAIN WITH x AS (SELECT 1)", KindUnknown, RuleInvalid},
		// EXPLAIN 后只跟标识符，无 SELECT。
		{"explain bare", EngineMySQL, "EXPLAIN t", KindUnknown, RuleInvalid},
		// SQLite PRAGMA 维护语句。
		{"pragma", EngineSQLite, "PRAGMA table_info(t)", KindDDL, RuleDDL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Check(tc.engine, tc.sql)
			if err != nil {
				t.Fatalf("Check error: %v", err)
			}
			if res.Allowed {
				t.Fatalf("Check(%q) allowed, want rejected", tc.sql)
			}
			if res.Kind != tc.kind {
				t.Fatalf("kind = %s, want %s (reason=%s)", res.Kind, tc.kind, res.Reason)
			}
			if res.RuleID != tc.rule {
				t.Fatalf("rule = %s, want %s", res.RuleID, tc.rule)
			}
		})
	}
}

func TestPostgresNestedSelectForms(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		kind Kind
		rule string
	}{
		// 子查询裸 SelectClause 内的锁定子句。
		{"locking in bare subquery", "SELECT * FROM (SELECT 1 FOR UPDATE) sub", KindSelect, RuleLocking},
		// 子查询 VALUES 形态。
		{"values in subquery", "SELECT * FROM (VALUES (1)) t", KindUnknown, RuleInvalid},
		// JOIN 一侧子查询带锁定。
		{"locking in join side", "SELECT * FROM (SELECT 1 FOR UPDATE) a JOIN t2 ON true", KindSelect, RuleLocking},
		// HAVING 中的标量子查询带锁定。
		{"locking in having subquery", "SELECT 1 HAVING 1 IN (SELECT 1 FOR UPDATE)", KindSelect, RuleLocking},
		// CTE 内部锁定。
		{"locking in cte", "WITH x AS (SELECT 1 FOR UPDATE) SELECT * FROM x", KindSelect, RuleLocking},
		// UNION 右侧锁定。
		{"locking in union right", "SELECT 1 UNION SELECT 2 FOR UPDATE", KindSelect, RuleLocking},
		// 顶层 VALUES 语句。
		{"top level values", "VALUES (1)", KindUnknown, RuleInvalid},
		// EXPLAIN 非 SELECT 目标。
		{"explain insert", "EXPLAIN INSERT INTO t VALUES (1)", KindInsert, RuleDML},
		// EXPLAIN 内部 SELECT 带锁定。
		{"explain locking", "EXPLAIN SELECT * FROM t FOR UPDATE", KindSelect, RuleLocking},
		// SELECT ... INTO（解析器不支持，按词法给出 RuleInto）。
		{"select into", "SELECT * INTO new_table FROM t", KindDDL, RuleInto},
		// COPY 语句。
		{"copy from stdin", "COPY t (a) FROM STDIN", KindCopy, RuleCopy},
		// 事务控制。
		{"begin", "BEGIN", KindTCL, RuleTCL},
		{"commit", "COMMIT", KindTCL, RuleTCL},
		// 带引号的危险函数名。
		{"quoted dangerous fn", `SELECT "pg_sleep"(1)`, KindCall, RuleDangerousFn},
		// 危险函数名与括号间有空白。
		{"spaced dangerous fn", "SELECT pg_sleep (1)", KindCall, RuleDangerousFn},
		// 危险函数仅作为列名出现（无括号调用）不误伤普通查询。
		// 危险函数仅作列名（无调用括号）不误伤只读查询——移出拒绝表见下方单独断言。
		// 括号包裹的 FROM 项（ParenTableExpr 下钻）。
		{"paren table expr", "SELECT * FROM ((SELECT 1 FOR UPDATE)) t", KindSelect, RuleLocking},
		// EXPLAIN ANALYZE DEBUG 变体。
		{"explain analyze debug", "EXPLAIN ANALYZE (DEBUG) SELECT 1", KindExplain, RuleExplainAnalyze},
		// 无法解析且首关键字未知。
		{"unparseable unknown", "FOO BAR", KindUnknown, RuleInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Check(EnginePostgres, tc.sql)
			if err != nil {
				t.Fatalf("Check error: %v", err)
			}
			if res.Allowed {
				t.Fatalf("Check(%q) allowed, want rejected", tc.sql)
			}
			if res.Kind != tc.kind || res.RuleID != tc.rule {
				t.Fatalf("Check(%q) = %s/%s, want %s/%s (reason=%s)", tc.sql, res.Kind, res.RuleID, tc.kind, tc.rule, res.Reason)
			}
		})
	}
}

// 危险函数名仅作列名出现（无括号调用）时不得误伤只读查询。
func TestPostgresDangerousNameAsColumnAllowed(t *testing.T) {
	res, err := Check(EnginePostgres, "SELECT pg_sleep FROM t")
	if err != nil {
		t.Fatalf("Check error: %v", err)
	}
	if !res.Allowed || res.Kind != KindSelect || res.RuleID != RuleRead {
		t.Fatalf("res = %+v, want allowed read-only select", res)
	}
}

// PG 语句解析失败但词法判只读时回退放行（EXPLAIN VERBOSE 一类）。
func TestPostgresFallbackLexicalAllowed(t *testing.T) {
	res, err := Check(EnginePostgres, "EXPLAIN VERBOSE SELECT * FROM users")
	if err != nil {
		t.Fatalf("Check error: %v", err)
	}
	if !res.Allowed || res.Kind != KindExplain || res.RuleID != RuleRead {
		t.Fatalf("res = %+v, want allowed explain", res)
	}
}

// WITH 最终落地 TABLE：只读形态允许（词法与 AST 一致）。
func TestPostgresWithTableAllowed(t *testing.T) {
	res, err := Check(EnginePostgres, "WITH x AS (SELECT 1) TABLE x")
	if err != nil {
		t.Fatalf("Check error: %v", err)
	}
	if !res.Allowed || res.RuleID != RuleRead {
		t.Fatalf("res = %+v, want allowed read", res)
	}
}

// dollarTag / kindForKeyword / firstLine 的边界直测。
func TestHelperEdgeFunctions(t *testing.T) {
	if tag, ok := dollarTag("$"); ok || tag != "" {
		t.Errorf(`dollarTag("$") = %q,%v, want "",false`, tag, ok)
	}
	if tag, ok := dollarTag("x"); ok || tag != "" {
		t.Errorf(`dollarTag("x") = %q,%v, want "",false`, tag, ok)
	}
	if tag, ok := dollarTag("$$"); !ok || tag != "$$" {
		t.Errorf(`dollarTag("$$") = %q,%v, want "$$",true`, tag, ok)
	}
	if tag, ok := dollarTag("$tag$"); !ok || tag != "$tag$" {
		t.Errorf(`dollarTag("$tag$") = %q,%v, want "$tag$",true`, tag, ok)
	}
	// $1 参数占位不是美元引号。
	if _, ok := dollarTag("$1"); ok {
		t.Error(`dollarTag("$1") = true, want false (bind param)`)
	}

	kindWant := map[string]Kind{
		"SELECT": KindSelect, "TABLE": KindSelect, "WITH": KindWithSelect,
		"EXPLAIN": KindExplain, "INSERT": KindInsert, "UPDATE": KindUpdate,
		"DELETE": KindDelete, "CREATE": KindDDL, "DROP": KindDDL,
		"COPY": KindCopy, "LOAD": KindCopy, "CALL": KindCall,
		"BEGIN": KindTCL, "COMMIT": KindTCL, "WHATEVER": KindUnknown,
	}
	for kw, want := range kindWant {
		if got := kindForKeyword(kw); got != want {
			t.Errorf("kindForKeyword(%q) = %s, want %s", kw, got, want)
		}
	}

	if got := firstLine("a\nb\nc"); got != "a" {
		t.Errorf("firstLine multi = %q, want a", got)
	}
	if got := firstLine("only"); got != "only" {
		t.Errorf("firstLine single = %q, want only", got)
	}
}

// checkExplainTokens 的分支直测：ANALYZE / 目标动词 / WITH 内 SELECT。
func TestCheckExplainTokensBranches(t *testing.T) {
	mk := func(ss ...string) []sqlToken {
		out := make([]sqlToken, len(ss))
		for i, s := range ss {
			out[i] = sqlToken{upper: s}
		}
		return out
	}
	cases := []struct {
		toks    []sqlToken
		kind    Kind
		rule    string
		allowed bool
	}{
		{mk("EXPLAIN", "ANALYZE"), KindExplain, RuleExplainAnalyze, false},
		{mk("EXPLAIN", "SELECT"), KindExplain, RuleRead, true},
		{mk("EXPLAIN", "WITH", "SELECT"), KindExplain, RuleRead, true},          // WITH 内顶层 SELECT
		{mk("EXPLAIN", "WITH", "SELECT", "UNION"), KindExplain, RuleRead, true}, // 深度过滤后仍允许
		{mk("EXPLAIN", "WITH", "1"), KindUnknown, RuleInvalid, false},           // WITH 内无 SELECT
		{mk("EXPLAIN", "INSERT"), KindInsert, RuleDML, false},
		{mk("EXPLAIN", "TABLE"), KindSelect, RuleDML, false},
		{mk("EXPLAIN", "x"), KindUnknown, RuleInvalid, false},
	}
	for _, tc := range cases {
		kind, rule, ok := checkExplainTokens(tc.toks)
		if ok != tc.allowed || kind != tc.kind || rule != tc.rule {
			t.Errorf("checkExplainTokens(%v) = %s/%s/%v, want %s/%s/%v",
				tc.toks, kind, rule, ok, tc.kind, tc.rule, tc.allowed)
		}
	}
}

// hasSelectInto 对非单语句 / 非查询首关键字的防御。
func TestHasSelectIntoDefensive(t *testing.T) {
	lexMulti, _ := lexSQL("SELECT 1; SELECT INTO t", EnginePostgres)
	if hasSelectInto(lexMulti) {
		t.Error("multi-statement must be false")
	}
	lexOther, _ := lexSQL("BEGIN INTO", EnginePostgres)
	if hasSelectInto(lexOther) {
		t.Error("non-query first keyword must be false")
	}
	lexTokless, _ := lexSQL("???", EnginePostgres)
	if hasSelectInto(lexTokless) {
		t.Error("tokenless statement must be false")
	}
	lexInto, _ := lexSQL("SELECT * INTO t FROM x", EnginePostgres)
	if !hasSelectInto(lexInto) {
		t.Error("SELECT INTO must be detected")
	}
}

// classifyByFirstKeyword 的防御分支。
func TestClassifyByFirstKeywordDefensive(t *testing.T) {
	lexEmpty, _ := lexSQL("-- comment only", EnginePostgres)
	if k, r := classifyByFirstKeyword(lexEmpty); k != KindUnknown || r != RuleInvalid {
		t.Errorf("empty stmts = %s/%s, want unknown/invalid", k, r)
	}
	lexTokless, _ := lexSQL("???", EnginePostgres)
	if k, r := classifyByFirstKeyword(lexTokless); k != KindUnknown || r != RuleInvalid {
		t.Errorf("tokenless = %s/%s, want unknown/invalid", k, r)
	}
	lexSel, _ := lexSQL("SELECT 1", EnginePostgres)
	if k, r := classifyByFirstKeyword(lexSel); k != KindSelect || r != RuleInvalid {
		t.Errorf("select = %s/%s, want select/invalid (read-only rule maps later)", k, r)
	}
}

// MySQL 的 `--1` 不是注释；`-- x` 是注释。
func TestMySQLDashComment(t *testing.T) {
	if !mysqlDashComment("SELECT 1 -- ", 11) {
		t.Error("dash comment at EOF should be a comment")
	}
	if !mysqlDashComment("SELECT 1 -- x", 11) {
		t.Error("dash followed by space should be a comment")
	}
	if mysqlDashComment("SELECT 1 --1", 11) {
		t.Error("--1 must not be treated as a comment")
	}

	// --1 整体保留：语句中的 INTO OUTFILE 不得被注释剥除遮蔽。
	res, err := Check(EngineMySQL, "SELECT --1 INTO OUTFILE '/tmp/x' FROM t")
	if err != nil {
		t.Fatalf("Check error: %v", err)
	}
	if res.Allowed || res.RuleID != RuleInto {
		t.Fatalf("res = %s/%s, want rejected INTO (got reason=%s)", res.Kind, res.RuleID, res.Reason)
	}
}

// tokenize 的括号深度跟踪与右括号下限保护。
func TestTokenizeDepth(t *testing.T) {
	toks := tokenize("SELECT f(a, b) FROM (SELECT g(c)) d")
	if len(toks) == 0 {
		t.Fatal("tokenize returned no tokens")
	}
	// 多余的右括号不会让 depth 变负。
	toks = tokenize(") ) SELECT")
	if len(toks) != 1 || toks[0].upper != "SELECT" || toks[0].depth != 0 {
		t.Fatalf("tokenize(unbalanced) = %+v", toks)
	}
}
