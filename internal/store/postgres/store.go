// Package postgres 是 store 接口的 PostgreSQL/pgx 实现。
//
// 所有 SQL 只出现在本包与 internal/store/migrations 中。所有查询都带 context，
// 底层错误统一映射为 store.ErrNotFound / ErrConflict / ErrInvalid。
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"

	"aiops/internal/store"
	"aiops/internal/store/migrations"
)

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store 实现 store.Store。事务内的子 store 共享同一个 pgx.Tx。
type Store struct {
	pool *pgxpool.Pool
	q    querier
	tx   pgx.Tx
	dsn  string
}

var _ store.Store = (*Store)(nil)

// Open 打开 pgxpool、执行 embed 的 goose Up，返回实现了 store.Store 的句柄。
func Open(ctx context.Context, databaseURL string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse database url: %w", err)
	}
	cfg.MaxConns = 20
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: open pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	s := &Store{pool: pool, q: pool, dsn: databaseURL}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	log.Info("postgres ready")
	return s, nil
}

// Migrate 执行内嵌的 goose 迁移；重复执行是幂等的。
func (s *Store) Migrate(ctx context.Context) error {
	db, err := sql.Open("pgx", s.dsn)
	if err != nil {
		return fmt.Errorf("postgres: open migration connection: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("postgres: set goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("postgres: migrate: %w", err)
	}
	return nil
}

// Ping 检查主库连通性。
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Close 关闭连接池；事务句柄只回滚自身。
func (s *Store) Close() {
	if s.tx != nil {
		_ = s.tx.Rollback(context.Background())
		return
	}
	if s.pool != nil {
		s.pool.Close()
	}
}

// WithTx 在单层事务中执行 fn；回调中的子 store 绑定同一事务。
func (s *Store) WithTx(ctx context.Context, fn func(store.Store) error) error {
	if s.tx != nil {
		return store.ErrTxUnsupported
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	child := &Store{pool: s.pool, q: tx, tx: tx, dsn: s.dsn}
	if err := fn(child); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

func (s *Store) Users() store.UserStore                       { return userStore{q: s.q} }
func (s *Store) Sessions() store.SessionStore                 { return sessionStore{q: s.q} }
func (s *Store) Servers() store.ServerStore                   { return serverStore{q: s.q} }
func (s *Store) Databases() store.DatabaseStore               { return databaseStore{q: s.q} }
func (s *Store) Providers() store.ProviderStore               { return providerStore{q: s.q} }
func (s *Store) Conversations() store.ConversationStore       { return conversationStore{q: s.q} }
func (s *Store) Messages() store.MessageStore                 { return messageStore{q: s.q} }
func (s *Store) ToolCalls() store.ToolCallStore               { return toolCallStore{q: s.q} }
func (s *Store) Approvals() store.ApprovalStore               { return approvalStore{q: s.q} }
func (s *Store) AuditLogs() store.AuditStore                  { return auditStore{q: s.q} }
func (s *Store) Settings() store.SettingStore                 { return settingStore{q: s.q} }
func (s *Store) TerminalSessions() store.TerminalSessionStore { return terminalSessionStore{q: s.q} }
func (s *Store) Metrics() store.MetricStore                   { return metricStore{q: s.q} }     // 五期 M1
func (s *Store) AlertRules() store.AlertRuleStore             { return alertRuleStore{q: s.q} }  // 五期 M1
func (s *Store) AlertEvents() store.AlertEventStore           { return alertEventStore{q: s.q} } // 五期 M1
func (s *Store) NotificationChannels() store.NotificationChannelStore { // 五期 M1
	return notificationChannelStore{q: s.q}
}
func (s *Store) DiagnosisSessions() store.DiagnosisSessionStore { // 五期 M2
	return diagnosisSessionStore{q: s.q}
}
func (s *Store) InspectionJobs() store.InspectionJobStore { // 五期 M2
	return inspectionJobStore{q: s.q}
}
func (s *Store) InspectionReports() store.InspectionReportStore { // 五期 M2
	return inspectionReportStore{q: s.q}
}
func (s *Store) BatchJobs() store.BatchJobStore { return batchJobStore{q: s.q} } // 五期 M3

// mapError 把 pgx/PostgreSQL 错误映射为 store 的哨兵错误。
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return store.ErrConflict
		case "23503", "23514", "22001", "22P02", "22007", "22008":
			// FK/CHECK/字符串长度/UUID/时间格式：调用参数不合法。
			return store.ErrInvalid
		}
	}
	return err
}
