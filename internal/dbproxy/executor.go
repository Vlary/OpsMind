// Package dbproxy 是外部数据库连接池与只读查询执行器。
//
// 三道闸保证只读：sqlguard 分类（调用方 + 执行器内再次检查）、只读事务/
// query_only 连接、以及数据库驱动本身不支持多语句（MySQL multiStatements=false）。
// 行数与序列化字节上限分别为 1000 与 1MB，超限截断但继续消费结果集。
package dbproxy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"aiops/internal/sqlguard"
	"aiops/internal/store"
)

// 默认与上限（CONTRACT.md 5.4 / 7.2）。
const (
	defaultQueryTimeout = 15 * time.Second
	maxQueryTimeout     = 60 * time.Second
	defaultMaxRows      = 1000
	maxMaxRows          = 1000
	defaultMaxBytes     = int64(1048576)
	maxMaxBytes         = int64(1048576)
)

var (
	// ErrNotReadOnly 表示 SQL 未通过只读守卫。
	ErrNotReadOnly = errors.New("dbproxy: query is not read-only")
	// ErrTimeout 表示查询超时（映射 HTTP SQL_TIMEOUT）。
	ErrTimeout = errors.New("dbproxy: query timed out")
	// ErrPoolClosed 表示执行器已关闭。
	ErrPoolClosed = errors.New("dbproxy: executor closed")
)

// NotReadOnlyError 携带守卫结论，供审计记录 rule_id。
type NotReadOnlyError struct {
	Result sqlguard.Result
}

func (e *NotReadOnlyError) Error() string {
	if e.Result.RuleID == "" {
		return ErrNotReadOnly.Error()
	}
	return ErrNotReadOnly.Error() + ": " + e.Result.RuleID
}

func (e *NotReadOnlyError) Unwrap() error { return ErrNotReadOnly }

// Column 是结果列元数据。
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// TestResult 是连接测试结果。
type TestResult struct {
	LatencyMS     int64  `json:"latency_ms"`
	ServerVersion string `json:"server_version"`
}

// QueryRequest 是只读查询请求。
type QueryRequest struct {
	SQL      string
	Timeout  time.Duration
	MaxRows  int
	MaxBytes int64
}

// QueryResult 是查询结果。
type QueryResult struct {
	Columns    []Column `json:"columns"`
	Rows       [][]any  `json:"rows"`
	RowCount   int      `json:"row_count"`
	Truncated  bool     `json:"truncated"`
	DurationMS int64    `json:"duration_ms"`

	// bytes 是累计的 JSON 序列化字节数，用于 MaxBytes 截断。
	bytes int64
}

// Executor 是外部数据库执行器接口。
type Executor interface {
	Test(ctx context.Context, db store.Database, password string) (TestResult, error)
	Query(ctx context.Context, db store.Database, password string, req QueryRequest) (QueryResult, error)
	Invalidate(databaseID string)
	Close() error
}

// Config 是连接池配置。
type Config struct {
	MaxConnsPerDatabase int
	MaxConnsGlobal      int
	IdleTimeout         time.Duration
	DialTimeout         time.Duration
}

// backend 是单数据库的连接池抽象。
type backend interface {
	Version(ctx context.Context) (string, error)
	Query(ctx context.Context, sql string, maxRows int, maxBytes int64) (QueryResult, error)
	Close()
}

// ExecutorPool 实现 Executor。
type ExecutorPool struct {
	cfg Config
	log *slog.Logger
	sem *semaphore.Weighted

	mu     sync.Mutex
	pools  map[string]*poolEntry
	closed bool
}

type poolEntry struct {
	signature string
	backend   backend
}

// New 构造执行器池并应用契约默认值。
func New(cfg Config, log *slog.Logger) *ExecutorPool {
	if cfg.MaxConnsPerDatabase <= 0 {
		cfg.MaxConnsPerDatabase = 4
	}
	if cfg.MaxConnsGlobal <= 0 {
		cfg.MaxConnsGlobal = 64
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &ExecutorPool{
		cfg:   cfg,
		log:   log,
		sem:   semaphore.NewWeighted(int64(cfg.MaxConnsGlobal)),
		pools: make(map[string]*poolEntry),
	}
}

// Test 建立连接并读取数据库版本。
func (p *ExecutorPool) Test(ctx context.Context, db store.Database, password string) (TestResult, error) {
	b, err := p.backendFor(db, password)
	if err != nil {
		return TestResult{}, err
	}
	if err := p.sem.Acquire(ctx, 1); err != nil {
		return TestResult{}, err
	}
	defer p.sem.Release(1)

	start := time.Now()
	version, err := b.Version(ctx)
	if err != nil {
		return TestResult{}, mapContextError(ctx, err)
	}
	return TestResult{
		LatencyMS:     time.Since(start).Milliseconds(),
		ServerVersion: version,
	}, nil
}

// Query 在只读事务中执行一条 SELECT。
func (p *ExecutorPool) Query(ctx context.Context, db store.Database, password string, req QueryRequest) (QueryResult, error) {
	// 第三道闸：执行器自身再次校验，不信任调用方。
	guard, _ := sqlguard.Check(sqlguard.Engine(db.Engine), req.SQL)
	if !guard.Allowed {
		return QueryResult{}, &NotReadOnlyError{Result: guard}
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultQueryTimeout
	}
	if timeout > maxQueryTimeout {
		timeout = maxQueryTimeout
	}
	maxRows := req.MaxRows
	if maxRows <= 0 {
		maxRows = defaultMaxRows
	}
	if maxRows > maxMaxRows {
		maxRows = maxMaxRows
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if maxBytes > maxMaxBytes {
		maxBytes = maxMaxBytes
	}

	b, err := p.backendFor(db, password)
	if err != nil {
		return QueryResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := p.sem.Acquire(ctx, 1); err != nil {
		return QueryResult{}, mapContextError(ctx, err)
	}
	defer p.sem.Release(1)

	start := time.Now()
	result, err := b.Query(ctx, req.SQL, maxRows, maxBytes)
	if err != nil {
		return result, mapContextError(ctx, err)
	}
	result.DurationMS = time.Since(start).Milliseconds()
	if result.Columns == nil {
		result.Columns = []Column{}
	}
	if result.Rows == nil {
		result.Rows = [][]any{}
	}
	result.RowCount = len(result.Rows)
	return result, nil
}

// Invalidate 关闭该数据库的连接池。
func (p *ExecutorPool) Invalidate(databaseID string) {
	p.mu.Lock()
	entry := p.pools[databaseID]
	delete(p.pools, databaseID)
	p.mu.Unlock()
	if entry != nil {
		entry.backend.Close()
	}
}

// Close 关闭所有连接池。
func (p *ExecutorPool) Close() error {
	p.mu.Lock()
	p.closed = true
	pools := p.pools
	p.pools = make(map[string]*poolEntry)
	p.mu.Unlock()

	for _, e := range pools {
		e.backend.Close()
	}
	return nil
}

// backendFor 返回数据库对应的后端；配置变化时重建。
func (p *ExecutorPool) backendFor(db store.Database, password string) (backend, error) {
	sig := signature(db, password)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	entry, ok := p.pools[db.ID]
	if ok && entry.signature == sig {
		p.mu.Unlock()
		return entry.backend, nil
	}
	var stale *poolEntry
	if ok {
		stale = entry
	}
	p.mu.Unlock()

	if stale != nil {
		stale.backend.Close()
	}

	b, err := p.open(db, password)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		b.Close()
		return nil, ErrPoolClosed
	}
	// 并发打开同一数据库时以先到者为准。
	if existing, ok := p.pools[db.ID]; ok && existing.signature == sig {
		p.mu.Unlock()
		b.Close()
		return existing.backend, nil
	}
	p.pools[db.ID] = &poolEntry{signature: sig, backend: b}
	p.mu.Unlock()
	return b, nil
}

func (p *ExecutorPool) open(db store.Database, password string) (backend, error) {
	switch db.Engine {
	case store.EnginePostgres:
		return newPostgresBackend(p.cfg, db, password)
	case store.EngineMySQL:
		return newMySQLBackend(p.cfg, db, password)
	case store.EngineSQLite:
		return newSQLiteBackend(p.cfg, db)
	case store.EngineMSSQL:
		return newMSSQLBackend(p.cfg, db, password)
	case store.EngineRedis:
		return newRedisBackend(p.cfg, db, password)
	case store.EngineMongoDB:
		return newMongoBackend(p.cfg, db, password, p.log)
	default:
		return nil, fmt.Errorf("dbproxy: unsupported engine %q", db.Engine)
	}
}

// signature 包含密码摘要，确保凭据变化后旧池不会被复用。
// W2：instance_name（mssql 命名实例）与 redis_db（redis 逻辑库）同样
// 参与签名——二者变化必须重建连接池。
// N2：auth_db（mongodb AuthSource）同理。
func signature(db store.Database, password string) string {
	sum := sha256.Sum256([]byte(password))
	return strings.Join([]string{
		string(db.Engine), db.Host, fmt.Sprint(db.Port), db.InstanceName, db.DatabaseName,
		db.Username, db.SQLitePath, fmt.Sprint(db.RedisDB), db.AuthDB, string(db.SSLMode), hex.EncodeToString(sum[:]),
	}, "|")
}

// mapContextError 把超时/取消统一映射为 ErrTimeout（取消保持原样）。
func mapContextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	return err
}

// scanSQLRows 把 database/sql 结果集转换为统一的结果结构。
func scanSQLRows(rows *sql.Rows, maxRows int, maxBytes int64) (QueryResult, error) {
	cols, err := rows.Columns()
	if err != nil {
		return QueryResult{}, err
	}
	types, _ := rows.ColumnTypes()

	result := QueryResult{Columns: make([]Column, len(cols)), Rows: [][]any{}}
	for i, name := range cols {
		typ := ""
		if i < len(types) {
			typ = types[i].DatabaseTypeName()
		}
		result.Columns[i] = Column{Name: name, Type: typ}
	}

	for rows.Next() {
		if len(result.Rows) >= maxRows {
			result.Truncated = true
			break
		}
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return result, err
		}
		row := make([]any, len(cols))
		for i, v := range raw {
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
	return result, nil
}

// appendRow 在行数/字节上限内追加一行；返回 false 表示已截断。
func appendRow(result *QueryResult, row []any, maxBytes int64) bool {
	b, err := json.Marshal(row)
	if err != nil {
		// 理论上 normalizeValue 已保证可序列化；兜底为字符串。
		safe := make([]any, len(row))
		for i, v := range row {
			safe[i] = fmt.Sprint(v)
		}
		b, err = json.Marshal(safe)
		if err != nil {
			return false
		}
		row = safe
	}
	if result.bytes+int64(len(b)) > maxBytes {
		return false
	}
	result.bytes += int64(len(b))
	result.Rows = append(result.Rows, row)
	return true
}

// normalizeValue 把驱动返回的 Go 类型转换为 JSON 友好值。
func normalizeValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return x
	case json.RawMessage:
		return string(x)
	}

	if dv, ok := v.(driver.Valuer); ok {
		if val, err := dv.Value(); err == nil {
			if _, isSelf := val.(driver.Valuer); !isSelf && val != nil {
				return normalizeValue(val)
			}
		}
	}
	if s, ok := v.(fmt.Stringer); ok {
		return s.String()
	}
	return fmt.Sprint(v)
}

var _ Executor = (*ExecutorPool)(nil)
