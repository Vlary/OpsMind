package dbproxy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/go-sql-driver/mysql"

	"aiops/internal/store"
)

// mysqlBackend 是 MySQL/MariaDB 连接池（database/sql + go-sql-driver）。
type mysqlBackend struct {
	db *sql.DB
}

func newMySQLBackend(cfg Config, db store.Database, password string) (*mysqlBackend, error) {
	if db.Host == "" || db.DatabaseName == "" || db.Username == "" {
		return nil, fmt.Errorf("dbproxy: mysql requires host, database_name and username")
	}
	port := db.Port
	if port <= 0 {
		port = 3306
	}

	c := mysql.NewConfig()
	c.User = db.Username
	c.Passwd = password
	c.Net = "tcp"
	c.Addr = net.JoinHostPort(db.Host, fmt.Sprint(port))
	c.DBName = db.DatabaseName
	c.ParseTime = true
	c.MultiStatements = false // 单语句硬约束
	c.Timeout = cfg.DialTimeout
	c.ReadTimeout = maxQueryTimeout + 5*time.Second
	c.WriteTimeout = cfg.DialTimeout
	c.TLSConfig = mysqlTLSConfig(db.SSLMode)

	sqlDB, err := sql.Open("mysql", c.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("dbproxy: mysql open: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.MaxConnsPerDatabase)
	sqlDB.SetMaxIdleConns(cfg.MaxConnsPerDatabase)
	sqlDB.SetConnMaxIdleTime(cfg.IdleTimeout)
	return &mysqlBackend{db: sqlDB}, nil
}

// mysqlTLSConfig 按契约映射 ssl_mode → tls 参数。
func mysqlTLSConfig(mode store.SSLMode) string {
	switch mode {
	case store.SSLModeVerifyFull:
		return "true"
	case store.SSLModePrefer, store.SSLModeRequire, store.SSLModeVerifyCA:
		return "skip-verify"
	default:
		return "false"
	}
}

// Version 读取 VERSION()。
func (b *mysqlBackend) Version(ctx context.Context) (string, error) {
	var version string
	if err := b.db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return "", mapMySQLError(err)
	}
	return version, nil
}

// Query 在只读事务中执行一条 SELECT。
func (b *mysqlBackend) Query(ctx context.Context, sqlText string, maxRows int, maxBytes int64) (QueryResult, error) {
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return QueryResult{}, mapMySQLError(err)
	}
	defer conn.Close()

	// MAX_EXECUTION_TIME 仅 MySQL 5.7.8+ 支持；不支持时退化为 context 超时。
	_, _ = conn.ExecContext(ctx, fmt.Sprintf("SET SESSION MAX_EXECUTION_TIME=%d", deadlineMillis(ctx)))

	if _, err := conn.ExecContext(ctx, "START TRANSACTION READ ONLY"); err != nil {
		return QueryResult{}, mapMySQLError(err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		// SET SESSION 不随事务回滚；归还池化连接前必须复位，否则同一连接上
		// 后续 Test/Version 等查询会继承上一次设置的超时上限。
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SET SESSION MAX_EXECUTION_TIME=DEFAULT")
	}()

	rows, err := conn.QueryContext(ctx, sqlText)
	if err != nil {
		return QueryResult{}, mapMySQLError(err)
	}
	defer rows.Close()

	result, err := scanSQLRows(rows, maxRows, maxBytes)
	if err != nil {
		return result, mapMySQLError(err)
	}
	return result, nil
}

func (b *mysqlBackend) Close() {
	if b.db != nil {
		_ = b.db.Close()
	}
}

// mapMySQLError 把 MySQL 的执行超时错误映射为 ErrTimeout。
func mapMySQLError(err error) error {
	if err == nil {
		return nil
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 3024 { // ER_QUERY_TIMEOUT
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	return err
}
