package sqlguard

import (
	"strings"

	"github.com/auxten/postgresql-parser/pkg/sql/parser"
	"github.com/auxten/postgresql-parser/pkg/sql/sem/tree"
)

// checkPostgres 用 AST 做 PostgreSQL 只读分类。
//
// 语句数必须为 1；仅 Select（含 WITH...SELECT）与非 ANALYZE 的 Explain 允许。
// dangerousPGFunctions 是只读事务无法约束其副作用的函数：
// 会话控制、文件读取/写入、大对象、dblink、复制与序列推进等。
var dangerousPGFunctions = map[string]bool{
	"pg_read_file":                        true,
	"pg_read_binary_file":                 true,
	"pg_stat_file":                        true,
	"pg_ls_dir":                           true,
	"pg_ls_logdir":                        true,
	"pg_ls_waldir":                        true,
	"pg_ls_tmpdir":                        true,
	"pg_ls_archive_statusdir":             true,
	"pg_terminate_backend":                true,
	"pg_cancel_backend":                   true,
	"pg_reload_conf":                      true,
	"pg_rotate_logfile":                   true,
	"pg_promote":                          true,
	"pg_switch_wal":                       true,
	"pg_create_restore_point":             true,
	"pg_drop_replication_slot":            true,
	"pg_create_physical_replication_slot": true,
	"pg_create_logical_replication_slot":  true,
	"pg_replication_origin_advance":       true,
	"pg_logical_emit_message":             true,
	"pg_backup_start":                     true,
	"pg_backup_stop":                      true,
	"pg_export_snapshot":                  true,
	"lo_import":                           true,
	"lo_export":                           true,
	"lo_unlink":                           true,
	"lo_create":                           true,
	"lo_from_bytea":                       true,
	"lo_put":                              true,
	"dblink":                              true,
	"dblink_exec":                         true,
	"dblink_connect":                      true,
	"dblink_connect_u":                    true,
	"dblink_disconnect":                   true,
	"set_config":                          true,
	"pg_notify":                           true,
	"pg_advisory_lock":                    true,
	"pg_advisory_lock_shared":             true,
	"pg_advisory_xact_lock":               true,
	"pg_advisory_xact_lock_shared":        true,
	"pg_advisory_unlock":                  true,
	"pg_advisory_unlock_all":              true,
	"nextval":                             true,
	"setval":                              true,
	"pg_sleep":                            true,
	"pg_sleep_for":                        true,
	"pg_sleep_until":                      true,
}

// dangerousPGFunctionCall 在规范化文本中查找危险函数调用。
// 要求函数名后（允许空白与引用标识符的结束双引号）紧跟 `(`，避免把普通
// 字符串字面量当成函数调用。
func dangerousPGFunctionCall(text string) (string, bool) {
	for i := 0; i < len(text); i++ {
		if !isIdentStartByte(text[i]) {
			continue
		}
		j := i + 1
		for j < len(text) && isIdentPartByte(text[j]) {
			j++
		}
		name := strings.ToLower(text[i:j])
		if dangerousPGFunctions[name] {
			k := j
			if k < len(text) && text[k] == '"' {
				k++
			}
			for k < len(text) && (text[k] == ' ' || text[k] == '\t' || text[k] == '\n' || text[k] == '\r') {
				k++
			}
			if k < len(text) && text[k] == '(' {
				return name, true
			}
		}
		i = j - 1
	}
	return "", false
}

func checkPostgres(sql string, lex lexed) Result {
	if fn, bad := dangerousPGFunctionCall(lex.normalized); bad {
		return reject(KindCall, RuleDangerousFn, "function "+fn+" is not read-only", lex.normalized)
	}
	stmts, err := parser.Parse(sql)
	if err != nil {
		// SELECT ... INTO 在该解析器里是语法错误，先按词法给出更准确的规则。
		if hasSelectInto(lex) {
			return reject(KindDDL, RuleInto, "SELECT INTO is not allowed", lex.normalized)
		}
		// 解析器不支持的只读形式（例如 EXPLAIN VERBOSE）回退到保守的词法分类；
		// 词法分类同样只放行 SELECT/WITH...SELECT/EXPLAIN。
		if res := checkLexical(EnginePostgres, lex); res.Allowed {
			return res
		}
		kind, rule := classifyByFirstKeyword(lex)
		return reject(kind, rule, "unable to parse SQL: "+firstLine(err.Error()), lex.normalized)
	}
	if len(stmts) != 1 {
		return reject(KindMultiple, RuleMultiple, "multiple statements are not allowed", lex.normalized)
	}

	switch ast := stmts[0].AST.(type) {
	case *tree.Select:
		if res, ok := checkSelectAST(ast, 0); !ok {
			res.Normalized = lex.normalized
			return res
		}
		if hasSelectInto(lex) {
			return reject(KindDDL, RuleInto, "SELECT INTO is not allowed", lex.normalized)
		}
		kind := KindSelect
		if ast.With != nil {
			kind = KindWithSelect
		}
		return allowed(kind, RuleRead, "read-only SELECT", lex.normalized)

	case *tree.Explain:
		if ast.ExplainOptions.Flags[tree.ExplainFlagAnalyze] {
			return reject(KindExplain, RuleExplainAnalyze, "EXPLAIN ANALYZE executes the query", lex.normalized)
		}
		inner, ok := ast.Statement.(*tree.Select)
		if !ok {
			kind := classifyASTKind(ast.Statement)
			return reject(kind, ruleForKind(kind), "EXPLAIN must target a SELECT", lex.normalized)
		}
		if res, ok := checkSelectAST(inner, 0); !ok {
			res.Normalized = lex.normalized
			return res
		}
		return allowed(KindExplain, RuleRead, "read-only EXPLAIN", lex.normalized)

	case *tree.ExplainAnalyzeDebug:
		return reject(KindExplain, RuleExplainAnalyze, "EXPLAIN ANALYZE executes the query", lex.normalized)

	default:
		kind := classifyASTKind(ast)
		return reject(kind, ruleForKind(kind), "statement is not read-only", lex.normalized)
	}
}

// checkSelectAST 递归校验 SELECT 结构：锁定子句、CTE 内部语句、集合操作。
func checkSelectAST(sel *tree.Select, depth int) (Result, bool) {
	if sel == nil {
		return reject(KindUnknown, RuleInvalid, "empty SELECT", ""), false
	}
	if depth > 16 {
		return reject(KindUnknown, RuleInvalid, "SELECT nesting too deep", ""), false
	}
	if len(sel.Locking) > 0 {
		return reject(KindSelect, RuleLocking, "FOR UPDATE/SHARE is not read-only", ""), false
	}
	if sel.With != nil {
		for _, cte := range sel.With.CTEList {
			inner, ok := cte.Stmt.(*tree.Select)
			if !ok {
				kind := classifyASTKind(cte.Stmt)
				return reject(kind, RuleDML, "data-modifying CTE is not allowed", ""), false
			}
			if res, ok := checkSelectAST(inner, depth+1); !ok {
				return res, false
			}
		}
	}
	switch s := sel.Select.(type) {
	case *tree.SelectClause:
		// FROM 子查询（含 JOIN 两侧、别名/括号包裹）与表达式子查询
		// （WHERE/投影/HAVING 中的 IN (SELECT …)、EXISTS 等）内部同样会
		// 执行 SELECT：藏在这些位置的 FOR UPDATE/FOR SHARE 不得因外层
		// 未加锁而放行（评审实测 "SELECT * FROM (SELECT 1 FOR UPDATE) sub"
		// 与 WHERE IN 子查询形态均曾判 allowed）。
		for _, te := range s.From.Tables {
			if res, ok := checkTableExpr(te, depth+1); !ok {
				return res, false
			}
		}
		for _, se := range s.Exprs {
			if res, ok := checkExprSubqueries(se.Expr, depth+1); !ok {
				return res, false
			}
		}
		if s.Where != nil {
			if res, ok := checkExprSubqueries(s.Where.Expr, depth+1); !ok {
				return res, false
			}
		}
		if s.Having != nil {
			if res, ok := checkExprSubqueries(s.Having.Expr, depth+1); !ok {
				return res, false
			}
		}
		return Result{}, true
	case *tree.ParenSelect:
		return checkSelectAST(s.Select, depth+1)
	case *tree.UnionClause:
		if res, ok := checkSelectAST(s.Left, depth+1); !ok {
			return res, false
		}
		return checkSelectAST(s.Right, depth+1)
	case *tree.ValuesClause:
		return reject(KindUnknown, RuleInvalid, "VALUES is not a read-only query", ""), false
	default:
		return reject(KindUnknown, RuleInvalid, "unsupported SELECT form", ""), false
	}
}

// checkTableExpr 递归校验 FROM 项：子查询、别名/括号包裹与 JOIN 两侧
// 都要下钻，锁定子句藏在任意一层都拒绝。
func checkTableExpr(te tree.TableExpr, depth int) (Result, bool) {
	if depth > 16 {
		return reject(KindUnknown, RuleInvalid, "SELECT nesting too deep", ""), false
	}
	switch t := te.(type) {
	case *tree.TableName, *tree.TableRef, *tree.RowsFromExpr:
		return Result{}, true
	case *tree.Subquery:
		return checkNestedSelect(t.Select, depth+1)
	case *tree.AliasedTableExpr:
		return checkTableExpr(t.Expr, depth+1)
	case *tree.ParenTableExpr:
		return checkTableExpr(t.Expr, depth+1)
	case *tree.JoinTableExpr:
		if res, ok := checkTableExpr(t.Left, depth+1); !ok {
			return res, false
		}
		return checkTableExpr(t.Right, depth+1)
	default:
		// StatementSource 等未知来源：失败关闭。
		return reject(KindUnknown, RuleInvalid, "unsupported FROM item", ""), false
	}
}

// checkNestedSelect 校验 Subquery.Select 承载的嵌套 SELECT。
// SelectStatement 接口不含 *Select（解析器产出 ParenSelect/SelectClause/
// UnionClause/ValuesClause 四种）；裸形态包一层 Select 复用统一校验
// （Locking/With 字段都在 Select 层）。
func checkNestedSelect(stmt tree.SelectStatement, depth int) (Result, bool) {
	switch stmt.(type) {
	case *tree.ParenSelect:
		return checkSelectAST(stmt.(*tree.ParenSelect).Select, depth+1)
	case *tree.SelectClause, *tree.UnionClause:
		return checkSelectAST(&tree.Select{Select: stmt}, depth+1)
	case *tree.ValuesClause:
		return reject(KindUnknown, RuleInvalid, "VALUES is not a read-only query", ""), false
	default:
		return reject(KindUnknown, RuleInvalid, "unsupported nested SELECT form", ""), false
	}
}

// subqueryCollector 收集表达式子树中的标量子查询（不修改表达式）。
type subqueryCollector struct {
	subs []*tree.Subquery
}

func (c *subqueryCollector) VisitPre(e tree.Expr) (bool, tree.Expr) {
	if sq, ok := e.(*tree.Subquery); ok {
		c.subs = append(c.subs, sq)
		return false, e // 内部由 checkSelectAST 递归，不在此遍历
	}
	return true, e
}

func (c *subqueryCollector) VisitPost(e tree.Expr) tree.Expr { return e }

// checkExprSubqueries 遍历表达式子树，对其中的标量子查询递归校验。
func checkExprSubqueries(expr tree.Expr, depth int) (Result, bool) {
	if expr == nil {
		return Result{}, true
	}
	if depth > 16 {
		return reject(KindUnknown, RuleInvalid, "SELECT nesting too deep", ""), false
	}
	var c subqueryCollector
	tree.WalkExprConst(&c, expr)
	for _, sq := range c.subs {
		if res, ok := checkNestedSelect(sq.Select, depth+1); !ok {
			return res, false
		}
	}
	return Result{}, true
}

// classifyASTKind 把非 SELECT 的 AST 映射到 Kind。
func classifyASTKind(ast tree.Statement) Kind {
	switch ast.(type) {
	case *tree.Insert:
		return KindInsert
	case *tree.Update:
		return KindUpdate
	case *tree.Delete:
		return KindDelete
	case *tree.CopyFrom:
		return KindCopy
	case *tree.Truncate,
		*tree.CreateTable, *tree.CreateView, *tree.CreateDatabase, *tree.CreateIndex,
		*tree.CreateSequence, *tree.CreateRole,
		*tree.AlterTable, *tree.AlterIndex, *tree.AlterSequence, *tree.AlterRole,
		*tree.DropTable, *tree.DropView, *tree.DropDatabase, *tree.DropIndex,
		*tree.DropSequence, *tree.DropRole,
		*tree.RenameTable, *tree.RenameDatabase, *tree.RenameColumn, *tree.RenameIndex,
		*tree.Grant, *tree.GrantRole, *tree.Revoke, *tree.RevokeRole,
		*tree.CommentOnTable, *tree.CommentOnColumn, *tree.CommentOnDatabase, *tree.CommentOnIndex:
		return KindDDL
	case *tree.BeginTransaction, *tree.CommitTransaction, *tree.RollbackTransaction,
		*tree.Savepoint, *tree.ReleaseSavepoint, *tree.RollbackToSavepoint, *tree.SetTransaction:
		return KindTCL
	case *tree.Explain, *tree.ExplainAnalyzeDebug:
		return KindExplain
	case *tree.Select:
		return KindSelect
	default:
		return KindUnknown
	}
}

// hasSelectInto 判断 masked 语句是否是 SELECT/WITH 且含 INTO。
func hasSelectInto(lex lexed) bool {
	if len(lex.stmts) != 1 {
		return false
	}
	toks := tokenize(lex.stmts[0])
	if len(toks) == 0 {
		return false
	}
	switch toks[0].upper {
	case "SELECT", "WITH", "TABLE":
	default:
		return false
	}
	for _, t := range toks {
		if t.upper == "INTO" {
			return true
		}
	}
	return false
}

func classifyByFirstKeyword(lex lexed) (Kind, string) {
	if len(lex.stmts) == 0 {
		return KindUnknown, RuleInvalid
	}
	toks := tokenize(lex.stmts[0])
	if len(toks) == 0 {
		return KindUnknown, RuleInvalid
	}
	kind := kindForKeyword(toks[0].upper)
	return kind, ruleForKind(kind)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
