package sqlguard

import "testing"

// G18（mssql 部分，CONTRACT.md 9.2）：单 SELECT/WITH…SELECT 通过；
// SELECT ... INTO、FOR BROWSE、OPENROWSET、EXEC sp_、xp_cmdshell、
// 多语句、BACKUP 拒绝；T-SQL 特有只读语法（TOP/OFFSET FETCH/NOLOCK）
// 不误判。

func TestCheckMSSQLAllowed(t *testing.T) {
	cases := []string{
		"SELECT 1",
		"SELECT * FROM sys.databases",
		"SELECT TOP 10 name FROM sys.objects ORDER BY name",
		"select top (5) percent * from t",
		"SELECT name FROM dbo.[Order Details]",
		"SELECT * FROM [my table] WHERE [name] = N'x'",
		"SELECT N'中文' AS s",
		"SELECT name FROM sys.objects WITH (NOLOCK)",
		"SELECT * FROM t WITH (NOLOCK, READUNCOMMITTED)",
		"WITH x AS (SELECT 1 AS n) SELECT * FROM x",
		"WITH cte AS (SELECT id FROM a), cte2 AS (SELECT id FROM b) SELECT * FROM cte JOIN cte2 ON cte.id = cte2.id",
		// T-SQL 隐式递归 CTE（无 RECURSIVE 关键字）。
		"WITH t(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM t WHERE n < 10) SELECT * FROM t",
		// 分页语法（OFFSET/FETCH NEXT）。
		"SELECT * FROM t ORDER BY id OFFSET 10 ROWS FETCH NEXT 20 ROWS ONLY",
		// 临时表（#t）与变量（@x）不是危险 token。
		"SELECT * FROM #temp",
		"SELECT @x AS v, ##global AS g",
		// 字符串/注释里的关键字不得误判。
		"SELECT 'DROP TABLE users' AS s",
		"SELECT 'xp_cmdshell ''dir''' AS s",
		"SELECT 1 -- DROP TABLE t\n",
		"SELECT /* DELETE FROM users */ 1",
		"SELECT /* a /* nested */ comment */ 1",
		"SELECT ';' AS semi, N'a;b' AS nsemi",
		"SELECT DB_NAME() AS db, SUSER_SNAME() AS who",
		// 系统视图三段名（sys 侧无 sp_/xp_ 前缀）。
		"SELECT * FROM sys.dm_exec_sessions WHERE session_id = @@SPID",
		// FOR SYSTEM_TIME（临时表查询）是只读语法，放行。
		"SELECT * FROM t FOR SYSTEM_TIME AS OF '2026-01-01 00:00:00'",
		"SELECT 1;",
		// 多行与大小写混合。
		"select\n  a,\n  b\nfrom [T]\nwhere b = 'x'",
	}
	for _, sql := range cases {
		res, err := Check(EngineMSSQL, sql)
		if err != nil {
			t.Fatalf("Check(mssql, %q) error: %v", sql, err)
		}
		if !res.Allowed {
			t.Fatalf("Check(mssql, %q) = {kind=%s rule=%s reason=%s}, want allowed", sql, res.Kind, res.RuleID, res.Reason)
		}
		if res.RiskLevel != "safe" {
			t.Fatalf("Check(mssql, %q).RiskLevel = %q, want safe", sql, res.RiskLevel)
		}
	}
}

func TestCheckMSSQLRejected(t *testing.T) {
	cases := []struct {
		sql  string
		kind Kind
		rule string
	}{
		// DML
		{"INSERT INTO t VALUES (1)", KindInsert, RuleDML},
		{"UPDATE t SET a = 1", KindUpdate, RuleDML},
		{"DELETE FROM t", KindDelete, RuleDML},
		{"MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN UPDATE SET a = 1", KindUpdate, RuleDML},
		{"WITH x AS (SELECT 1) DELETE FROM t", KindDelete, RuleDML},
		{"WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x", KindInsert, RuleDML},
		{"WITH x AS (SELECT 1) UPDATE t SET a = 1", KindUpdate, RuleDML},
		{"WITH x AS (SELECT 1) MERGE INTO t USING x ON 1=1 WHEN MATCHED THEN DELETE", KindUpdate, RuleDML},
		// DDL
		{"CREATE TABLE t (id int)", KindDDL, RuleDDL},
		{"ALTER TABLE t ADD c int", KindDDL, RuleDDL},
		{"DROP TABLE t", KindDDL, RuleDDL},
		{"TRUNCATE TABLE t", KindDDL, RuleDDL},
		{"GRANT SELECT ON t TO u", KindDDL, RuleDDL},
		{"DENY SELECT ON t TO u", KindDDL, RuleDDL},
		{"CREATE INDEX ix ON t(c)", KindDDL, RuleDDL},
		// TCL
		{"BEGIN TRANSACTION", KindTCL, RuleTCL},
		{"BEGIN TRAN", KindTCL, RuleTCL},
		{"COMMIT", KindTCL, RuleTCL},
		{"ROLLBACK TRAN x", KindTCL, RuleTCL},
		// 存储过程 / 扩展过程
		{"EXEC sp_helpdb", KindCall, RuleCall},
		{"EXECUTE sp_who2", KindCall, RuleCall},
		{"EXEC xp_cmdshell 'dir'", KindCall, RuleCall},
		{"xp_cmdshell 'dir'", KindCall, RuleCall},
		{"EXEC master.dbo.xp_cmdshell 'dir'", KindCall, RuleCall},
		{"sp_executesql N'SELECT 1'", KindCall, RuleCall},
		{"SELECT * FROM sys.databases WHERE name IN (SELECT name FROM sp_x)", KindCall, RuleCall},
		// 外部数据访问
		{"SELECT * FROM OPENROWSET('SQLNCLI', 'Server=x;', 'SELECT 1')", KindCall, RuleDangerousFn},
		{"SELECT * FROM OPENQUERY(linked, 'SELECT 1')", KindCall, RuleDangerousFn},
		{"SELECT * FROM OPENDATASOURCE('SQLNCLI', 'x')...t", KindCall, RuleDangerousFn},
		{"SELECT * FROM OPENXML(@h, '/root')", KindCall, RuleDangerousFn},
		// BULK INSERT
		{"BULK INSERT t FROM 'x.csv'", KindCopy, RuleCopy},
		{"BULK INSERT t FROM 'x.csv' WITH (FIELDTERMINATOR = ',')", KindCopy, RuleCopy},
		// 维护命令
		{"BACKUP DATABASE master TO DISK = 'x.bak'", KindDDL, RuleDDL},
		{"BACKUP LOG m TO DISK = 'x.trn'", KindDDL, RuleDDL},
		{"RESTORE DATABASE m FROM DISK = 'x.bak'", KindDDL, RuleDDL},
		{"SHUTDOWN", KindDDL, RuleDDL},
		{"SHUTDOWN WITH NOWAIT", KindDDL, RuleDDL},
		{"DBCC CHECKDB('master')", KindDDL, RuleDDL},
		{"DBCC DROPCLEANBUFFERS", KindDDL, RuleDDL},
		// SELECT ... INTO / FOR 子句
		{"SELECT * INTO newt FROM t", KindDDL, RuleInto},
		{"SELECT a, b INTO t2 FROM t", KindDDL, RuleInto},
		{"SELECT * FROM t FOR UPDATE", KindSelect, RuleLocking},
		{"SELECT * FROM t FOR BROWSE", KindSelect, RuleLocking},
		{"SELECT * FROM t FOR XML PATH('row')", KindSelect, RuleLocking},
		{"SELECT * FROM t FOR XML AUTO", KindSelect, RuleLocking},
		{"SELECT * FROM t FOR JSON PATH", KindSelect, RuleLocking},
		{"SELECT * FROM t FOR JSON AUTO, WITHOUT_ARRAY_WRAPPER", KindSelect, RuleLocking},
		// 多语句与批分隔
		{"SELECT 1; SELECT 2", KindMultiple, RuleMultiple},
		{"SELECT 1; DROP TABLE t", KindMultiple, RuleMultiple},
		{"SELECT 1\n;DELETE FROM t", KindMultiple, RuleMultiple},
		{"SELECT 1 GO SELECT 2", KindMultiple, RuleMultiple},
		{"SELECT 1\ngo\nSELECT 2", KindMultiple, RuleMultiple},
		// 时间盲注
		{"WAITFOR DELAY '0:0:10'", KindCall, RuleDangerousFn},
		{"WAITFOR TIME '22:00'", KindCall, RuleDangerousFn},
		{"SELECT 1; WAITFOR DELAY '0:0:5'", KindMultiple, RuleMultiple},
		// 首关键字即非法
		{"USE master", KindUnknown, RuleInvalid},
		{"KILL 60", KindUnknown, RuleInvalid},
		{"EXEC ('SELECT 1')", KindCall, RuleCall},
		{"RECONFIGURE", KindUnknown, RuleInvalid},
		{"sp_configure 'show advanced options', 1", KindCall, RuleCall},
		// 非语句/解析失败
		{"", KindUnknown, RuleInvalid},
		{"   ", KindUnknown, RuleInvalid},
		{"; ;", KindUnknown, RuleInvalid},
		{"SELECT 'unterminated", KindUnknown, RuleInvalid},
		{"SELECT * FROM [unterminated", KindUnknown, RuleInvalid},
		{"SELECT /* unterminated", KindUnknown, RuleInvalid},
		{"SELECT \"unterminated", KindUnknown, RuleInvalid},
	}
	for _, tc := range cases {
		res, err := Check(EngineMSSQL, tc.sql)
		if err != nil {
			t.Fatalf("Check(mssql, %q) error: %v", tc.sql, err)
		}
		if res.Allowed {
			t.Fatalf("Check(mssql, %q) allowed, want rejected", tc.sql)
		}
		if res.Kind != tc.kind {
			t.Fatalf("Check(mssql, %q).Kind = %q (rule=%s), want %q", tc.sql, res.Kind, res.RuleID, tc.kind)
		}
		if res.RuleID != tc.rule {
			t.Fatalf("Check(mssql, %q).RuleID = %q (%s), want %q", tc.sql, res.RuleID, res.Reason, tc.rule)
		}
		if res.RiskLevel != "needs_approval" {
			t.Fatalf("Check(mssql, %q).RiskLevel = %q, want needs_approval", tc.sql, res.RiskLevel)
		}
	}
}

// TestCheckMSSQLBypass 覆盖契约 7.4 语义的绕过尝试：注释拼接、大小写
// 混淆、注释夹分号、嵌套注释藏语句、三段名调用扩展过程等，剥注释与
// mask 后必须仍被拦截；同时字符串/引用标识符内的危险词不得误判。
func TestCheckMSSQLBypass(t *testing.T) {
	mustReject := []string{
		// 注释打断 EXEC 与过程名：剥注释后 token 序列不变。
		"EXEC /**/ xp_cmdshell 'dir'",
		"EXEC/*x*/master.dbo.xp_cmdshell 'dir'",
		"eXeC Xp_CmDsHeLl 'dir'",
		// 注释夹分号绕过多语句计数：分号在注释之外即计数。
		"SELECT 1 /* x */ ; /* y */ SELECT 2",
		"SELECT 1 --\n; SELECT 2",
		// 嵌套块注释（T-SQL 语义）整段剥除后，尾随的分号语句仍拦。
		"SELECT /* first /* DROP TABLE t */ still comment */ 1; DROP TABLE t",
		// 三段名/两段名调用过程：tokenize 拆点后 sp_/xp_ 前缀仍命中。
		"EXEC dbo.sp_recompile 't'",
		"SELECT 1 FROM sys.databases; EXEC msdb.dbo.sp_start_job 'j'",
		// 大小写与空白混淆。
		"sElEcT 1 ; dRoP tAbLe t",
		"WITH\nx\nAS\n(SELECT 1)\nDELETE\nFROM\nt",
		"SELECT 1 gO SELECT 2",
		// WITH 最终语句为写操作（T-SQL 允许 WITH...DML）。
		"WITH x AS (SELECT 1) mErGe INTO t USING x ON 1=1 WHEN MATCHED THEN DELETE",
	}
	for _, sql := range mustReject {
		res, err := Check(EngineMSSQL, sql)
		if err != nil {
			t.Fatalf("Check(mssql, %q) error: %v", sql, err)
		}
		if res.Allowed {
			t.Fatalf("bypass attempt %q was allowed, want rejected", sql)
		}
	}

	// 字符串与引用标识符内的危险词是数据，不是代码：不得误判。
	mustAllow := []string{
		"SELECT [xp_cmdshell] FROM t WHERE c = 'EXEC sp_help'",
		`SELECT "DROP TABLE t" AS s`,
		"SELECT 'a /* not comment */ b' AS s",
		"SELECT 'a -- not comment' AS s",
		"SELECT N'; DROP TABLE t' AS s",
	}
	for _, sql := range mustAllow {
		res, err := Check(EngineMSSQL, sql)
		if err != nil {
			t.Fatalf("Check(mssql, %q) error: %v", sql, err)
		}
		if !res.Allowed {
			t.Fatalf("literal masking case %q rejected (%s/%s), want allowed", sql, res.Kind, res.RuleID)
		}
	}
}
