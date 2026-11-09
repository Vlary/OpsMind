package dbproxy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"aiops/internal/store"
)

// pgBackend 是 PostgreSQL 连接池（pgxpool）。
type pgBackend struct {
	pool *pgxpool.Pool
}

func newPostgresBackend(cfg Config, db store.Database, password string) (*pgBackend, error) {
	if db.Host == "" || db.DatabaseName == "" || db.Username == "" {
		return nil, fmt.Errorf("dbproxy: postgres requires host, database_name and username")
	}
	port := db.Port
	if port <= 0 {
		port = 5432
	}
	sslmode := string(db.SSLMode)
	if sslmode == "" {
		sslmode = string(store.SSLModeDisable)
	}

	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(db.Username, password),
		Host:   net.JoinHostPort(db.Host, strconv.Itoa(port)),
		Path:   "/" + db.DatabaseName,
	}
	q := u.Query()
	q.Set("sslmode", sslmode)
	q.Set("connect_timeout", strconv.Itoa(int(cfg.DialTimeout.Seconds())))
	u.RawQuery = q.Encode()

	poolCfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		// 不包装底层错误：它可能包含带密码的连接串。
		return nil, fmt.Errorf("dbproxy: invalid postgres configuration")
	}
	poolCfg.MaxConns = int32(cfg.MaxConnsPerDatabase)
	poolCfg.MinConns = 0
	poolCfg.MaxConnIdleTime = cfg.IdleTimeout
	poolCfg.ConnConfig.ConnectTimeout = cfg.DialTimeout

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("dbproxy: failed to create postgres pool")
	}
	return &pgBackend{pool: pool}, nil
}

// Version 读取 server_version。
func (b *pgBackend) Version(ctx context.Context) (string, error) {
	var version string
	if err := b.pool.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		return "", err
	}
	return version, nil
}

// Query 在只读事务中执行一条 SELECT，并设置语句超时。
func (b *pgBackend) Query(ctx context.Context, sqlText string, maxRows int, maxBytes int64) (QueryResult, error) {
	start := time.Now()

	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return QueryResult{}, err
	}
	defer conn.Release()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return QueryResult{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	ms := deadlineMillis(ctx)
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms)); err != nil {
		return QueryResult{}, err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL idle_in_transaction_session_timeout = %d", ms)); err != nil {
		return QueryResult{}, err
	}

	rows, err := tx.Query(ctx, sqlText)
	if err != nil {
		return QueryResult{}, err
	}
	defer rows.Close()

	fds := rows.FieldDescriptions()
	typeMap := conn.Conn().TypeMap()
	result := QueryResult{
		Columns:    make([]Column, len(fds)),
		Rows:       [][]any{},
		DurationMS: time.Since(start).Milliseconds(),
	}
	for i, fd := range fds {
		typ := ""
		if t, ok := typeMap.TypeForOID(fd.DataTypeOID); ok && t != nil {
			typ = t.Name
		}
		if typ == "" {
			typ = "oid:" + strconv.FormatUint(uint64(fd.DataTypeOID), 10)
		}
		result.Columns[i] = Column{Name: string(fd.Name), Type: typ}
	}

	for rows.Next() {
		if len(result.Rows) >= maxRows {
			result.Truncated = true
			break
		}
		vals, err := rows.Values()
		if err != nil {
			return result, err
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			row[i] = normalizeValue(v)
		}
		if !appendRow(&result, row, maxBytes) {
			result.Truncated = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	result.RowCount = len(result.Rows)

	// 显式结束只读事务（失败不影响已读取的结果）。
	_ = tx.Commit(context.WithoutCancel(ctx))
	return result, nil
}

func (b *pgBackend) Close() {
	if b.pool != nil {
		b.pool.Close()
	}
}

// deadlineMillis 返回 context 剩余时间（毫秒），用于 statement_timeout。
func deadlineMillis(ctx context.Context) int64 {
	if d, ok := ctx.Deadline(); ok {
		ms := time.Until(d).Milliseconds()
		if ms < 1 {
			return 1
		}
		return ms
	}
	return defaultQueryTimeout.Milliseconds()
}
