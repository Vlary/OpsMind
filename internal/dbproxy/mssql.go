// SQL Server 执行器（W2，CONTRACT.md 5.4/7.6，驱动 go-mssqldb）。
//
// 只读保障：契约裁决 mssql 不开显式事务（守卫已限定单条 SELECT；
// SET LOCK_TIMEOUT 等会话参数会引入隐式事务，一律不设），超时全部由
// context + DSN connection timeout 承担。只读语义依赖三道闸中的前两道：
// 调用方守卫与 ExecutorPool.Query 内的二次分类（executor.go）。
package dbproxy

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	_ "github.com/microsoft/go-mssqldb" // 注册 sqlserver 驱动

	"aiops/internal/store"
)

// mssqlBackend 是 SQL Server 连接池（database/sql + go-mssqldb）。
type mssqlBackend struct {
	db *sql.DB
}

func newMSSQLBackend(cfg Config, db store.Database, password string) (*mssqlBackend, error) {
	if db.Host == "" || db.DatabaseName == "" || db.Username == "" {
		return nil, fmt.Errorf("dbproxy: mssql requires host, database_name and username")
	}
	if password == "" {
		return nil, fmt.Errorf("dbproxy: mssql requires a password")
	}
	instance := strings.TrimSpace(db.InstanceName)
	if instance != "" && db.Port > 0 {
		return nil, fmt.Errorf("dbproxy: mssql instance_name and port are mutually exclusive")
	}
	dsn, err := mssqlDSN(cfg, db, password)
	if err != nil {
		return nil, err
	}

	sqlDB, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, fmt.Errorf("dbproxy: mssql open: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.MaxConnsPerDatabase)
	sqlDB.SetMaxIdleConns(cfg.MaxConnsPerDatabase)
	sqlDB.SetConnMaxIdleTime(cfg.IdleTimeout)
	return &mssqlBackend{db: sqlDB}, nil
}

// mssqlDSN 构造 go-mssqldb URL DSN。
//
// 契约 5.4 的 DSN 语义经 URL 形式表达：instance_name 非空时
// server=<host>\<instance>（port=0 走 SQL Server Browser 解析），
// 否则 server=<host>;port=<port>。密码经 userinfo 由 net/url 百分号
// 转义，避免分号 DSN 的转义歧义（; } " 等字符在密码中安全）。
func mssqlDSN(cfg Config, db store.Database, password string) (string, error) {
	instance := strings.TrimSpace(db.InstanceName)
	u := &url.URL{
		Scheme: "sqlserver",
		User:   url.UserPassword(db.Username, password),
	}
	if instance != "" {
		u.Host = db.Host
		u.Path = "/" + instance
	} else {
		port := db.Port
		if port <= 0 {
			port = 1433 // 契约 4.4：端口直连默认 1433（handler 侧兜底的防线）
		}
		u.Host = net.JoinHostPort(db.Host, strconv.Itoa(port))
	}
	q := u.Query()
	q.Set("database", db.DatabaseName)
	q.Set("encrypt", mssqlEncrypt(db.SSLMode))
	if db.SSLMode == store.SSLModeRequire {
		q.Set("trustservercertificate", "true")
	}
	if secs := int(cfg.DialTimeout.Seconds()); secs > 0 {
		q.Set("connection timeout", strconv.Itoa(secs))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// mssqlEncrypt 按契约 5.4 映射 ssl_mode → encrypt 参数：
// disable/prefer→false（SQL Server 无机会加密，prefer 降级为不加密）、
// require→true + TrustServerCertificate、verify-ca/verify-full→true
// （系统信任链校验）。
func mssqlEncrypt(mode store.SSLMode) string {
	switch mode {
	case store.SSLModeRequire, store.SSLModeVerifyCA, store.SSLModeVerifyFull:
		return "true"
	default:
		return "false"
	}
}

// Version 读取 SELECT @@VERSION（契约 5.4：Test 探针）。
func (b *mssqlBackend) Version(ctx context.Context) (string, error) {
	var version string
	if err := b.db.QueryRowContext(ctx, "SELECT @@VERSION").Scan(&version); err != nil {
		return "", err
	}
	return version, nil
}

// Query 执行单条只读 SELECT（无显式事务，超时由 ctx 控制）。
// 列类型使用 go-mssqldb 返回的数据库类型名（nvarchar、int 等），
// 由共享的 scanSQLRows 从 ColumnTypes 提取。
func (b *mssqlBackend) Query(ctx context.Context, sqlText string, maxRows int, maxBytes int64) (QueryResult, error) {
	rows, err := b.db.QueryContext(ctx, sqlText)
	if err != nil {
		return QueryResult{}, mapContextError(ctx, err)
	}
	defer rows.Close()
	result, err := scanSQLRows(rows, maxRows, maxBytes)
	if err != nil {
		// database/sql 的游标超时由 ctx 取消传播；统一映射 ErrTimeout。
		return result, mapContextError(ctx, err)
	}
	return result, nil
}

func (b *mssqlBackend) Close() {
	if b.db != nil {
		_ = b.db.Close()
	}
}
