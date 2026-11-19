package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"aiops/internal/audit"
	"aiops/internal/auth"
	"aiops/internal/authz"
	"aiops/internal/store"
	"aiops/internal/store/postgres"
)

// 走查第 4 轮（high）回归：真实 Postgres 下 GET /api/audit/verify 必须对
// 「全新库 + prev_hash 全链自洽」返回 200。此前的实现用纳秒时间戳算哈希，
// timestamptz 只存微秒，校验侧复算必然不一致，稳定 409 AUDIT_CHAIN_BROKEN。
//
// 集成测试与 internal/store/postgres 包共用同一个测试库，因此复用该包
// TestMain 的 advisory lock（migrate_test.go 的 0x4f50534d），避免两个测试
// 二进制并行 TRUNCATE/写入同一张表。
const pgAuditVerifyLockID int64 = 0x4f50534d

func pgAuditVerifyDSN() string {
	if v := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL")); v != "" {
		return v
	}
	return "postgres://aiops:aiops@127.0.0.1:65432/aiops?sslmode=disable"
}

// acquireAuditVerifyDBLock 获取测试库独占锁；集成测试在库不可达时必须失败
// 而不是 skip（与 internal/store/postgres 的约定一致）。
func acquireAuditVerifyDBLock(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", pgAuditVerifyDSN())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("test database unreachable (%s): %v", pgAuditVerifyDSN(), err)
	}
	if _, err := db.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, pgAuditVerifyLockID); err != nil {
		_ = db.Close()
		t.Fatalf("acquire test database lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`SELECT pg_advisory_unlock($1)`, pgAuditVerifyLockID)
		_ = db.Close()
	})
	return db
}

func truncateAuditVerifyTables(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`TRUNCATE audit_logs, audit_outbox, sessions, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate test tables: %v", err)
	}
}

func TestAuditVerifyPostgresIntegration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := acquireAuditVerifyDBLock(t)

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// 必须先 Open 再 TRUNCATE：Open 才执行内嵌 goose Up。CI 与任何全新库上
	// 表还不存在，先 TRUNCATE 会以 SQLSTATE 42P01（relation does not exist）
	// 直接失败——本测试此前只在已迁移过的开发库上跑过，所以没暴露。
	st, err := postgres.Open(ctx, pgAuditVerifyDSN(), log)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	truncateAuditVerifyTables(t, db)
	t.Cleanup(func() { truncateAuditVerifyTables(t, db) })

	passwordHash, err := bcrypt.GenerateFromPassword([]byte("it-audit-verify"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Users().Create(ctx, store.CreateUserParams{
		Username: "it-auditor", PasswordHash: string(passwordHash),
		Role: store.RoleAdmin, Enabled: true,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// 时钟带纳秒：正是走查里触发误报的输入（修复后由 Prepare 截断）。
	clock := func() time.Time {
		return time.Date(2026, 11, 12, 9, 8, 7, 123456789, time.UTC)
	}
	writer := audit.NewWriter(st, log, clock)
	writer.Start(ctx)
	if err := writer.Enqueue(ctx, audit.Event{
		ActorType: "user", ActorID: "it-user", Action: "it.event.one",
		ResourceType: "test", ResourceID: "r1", Detail: map[string]any{"i": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Enqueue(ctx, audit.Event{
		ActorType: "user", ActorID: "it-user", Action: "it.event.two",
		ResourceType: "test", ResourceID: "r2", Detail: map[string]any{"i": 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Enqueue(ctx, audit.Event{
		ActorType: "user", ActorID: "it-user", Action: "it.event.three",
		ResourceType: "test", ResourceID: "r3", Detail: map[string]any{"i": 3},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(ctx); err != nil {
		t.Fatalf("flush audit: %v", err)
	}

	// 事务内审计走 audit_outbox：payload 里的 detail 在 worker 追加前同样要
	// 经 jsonb 规范化（生产主路径，与 writeBatch 是两段独立代码）。
	payload, err := audit.OutboxPayload(audit.Event{
		ActorType: "user", ActorID: "it-user", Action: "it.outbox.event",
		ResourceType: "test", ResourceID: "r4", Detail: map[string]any{"i": 4},
	}, clock())
	if err != nil {
		t.Fatalf("outbox payload: %v", err)
	}
	outbox := store.OutboxOf(st)
	if outbox == nil {
		t.Fatal("postgres store does not implement audit_outbox")
	}
	if err := outbox.EnqueueAuditOutbox(ctx, payload, clock()); err != nil {
		t.Fatalf("enqueue outbox: %v", err)
	}
	// outbox 由 worker 每 batchInterval 扫描一次；有界轮询等待落链。
	var outboxRows int
	for attempt := 0; attempt < 50; attempt++ {
		if err := db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action = 'it.outbox.event'`).Scan(&outboxRows); err != nil {
			t.Fatalf("count outbox event: %v", err)
		}
		if outboxRows == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if outboxRows != 1 {
		t.Fatalf("outbox event rows = %d, want 1", outboxRows)
	}

	// 数据库确实只保留微秒（证明修复有必要，而不是换了口径）。
	var storedCreatedAt time.Time
	if err := db.QueryRow(`SELECT created_at FROM audit_logs ORDER BY id LIMIT 1`).Scan(&storedCreatedAt); err != nil {
		t.Fatalf("read stored created_at: %v", err)
	}
	if got := storedCreatedAt.Nanosecond(); got != 123456000 {
		t.Fatalf("stored created_at nanos = %d, want microsecond precision 123456000", got)
	}

	authSvc := auth.NewService(st, auth.Config{SessionTTL: time.Hour})
	login, err := authSvc.Login(ctx, "it-auditor", "it-audit-verify", "127.0.0.1", "integration-test")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	deps := &Deps{Log: log, Store: st, Auth: authSvc, Audit: writer, Clock: clock}
	router := gin.New()
	api := router.Group("/api")
	api.Use(deps.requireAuth())
	// 与生产同一守卫路径（servers.go:66）。
	api.GET("/audit/verify", authz.RequirePermission(authz.PermAudit, deps.authzDenied), deps.handleVerifyAudit)

	doVerify := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/audit/verify"+query, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: login.Token})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// 1) 真实 Postgres、真实链路：必须 200 且完整复算（legacy=0）。
	//    3 条直写 + 1 条 outbox 事务审计。
	rec := doVerify("")
	if rec.Code != http.StatusOK {
		t.Fatalf("verify fresh chain = %d %s", rec.Code, rec.Body.String())
	}
	var okResp struct {
		OK       bool   `json:"ok"`
		Checked  int    `json:"checked"`
		Legacy   int    `json:"legacy"`
		HeadID   int64  `json:"head_id"`
		HeadHash string `json:"head_hash"`
		BrokenAt *int64 `json:"broken_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &okResp); err != nil {
		t.Fatalf("decode verify response: %v (%s)", err, rec.Body.String())
	}
	if !okResp.OK || okResp.Checked != 4 || okResp.Legacy != 0 || okResp.HeadID != 4 || okResp.BrokenAt != nil {
		t.Fatalf("verify response = %+v", okResp)
	}

	// 停掉 writer，避免 audit.verify 行在下面手工插入假行时并发追加，
	// 干扰断点定位。后续请求不再写审计（本用例只验证校验侧）。
	closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := writer.Close(closeCtx); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	cancel()
	deps.Audit = nil

	var lastHash string
	if err := db.QueryRow(`SELECT hash FROM audit_logs ORDER BY id DESC LIMIT 1`).Scan(&lastHash); err != nil {
		t.Fatalf("read last hash: %v", err)
	}

	// 2) 00014 前存量行：纳秒哈希不可复算，只校验链接。手工插入 legacy 行
	//    （哈希故意错误），链接正确时必须 200 且 legacy=1。
	var legacyID int64
	if err := db.QueryRow(`
		INSERT INTO audit_logs (actor_type, action, prev_hash, hash, legacy_hash)
		VALUES ('system', 'it.legacy', $1, $2, true)
		RETURNING id`, lastHash, strings.Repeat("c", 64)).Scan(&legacyID); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	rec = doVerify("?from_id=" + strconv.FormatInt(legacyID, 10) + "&to_id=" + strconv.FormatInt(legacyID, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("verify legacy row = %d %s", rec.Code, rec.Body.String())
	}
	var legacyResp struct {
		OK      bool  `json:"ok"`
		Checked int   `json:"checked"`
		Legacy  int   `json:"legacy"`
		HeadID  int64 `json:"head_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &legacyResp); err != nil {
		t.Fatal(err)
	}
	if !legacyResp.OK || legacyResp.Checked != 1 || legacyResp.Legacy != 1 || legacyResp.HeadID != legacyID {
		t.Fatalf("legacy verify response = %+v", legacyResp)
	}

	// 3) 链断裂：新建行 prev_hash 与真实前驱不符 → 409，且 error.details
	//    带断点 id（审计页据此展示）。
	var brokenID int64
	if err := db.QueryRow(`
		INSERT INTO audit_logs (actor_type, action, prev_hash, hash, legacy_hash)
		VALUES ('system', 'it.broken', $1, $2, false)
		RETURNING id`, strings.Repeat("f", 64), strings.Repeat("d", 64)).Scan(&brokenID); err != nil {
		t.Fatalf("insert broken row: %v", err)
	}
	rec = doVerify("?from_id=" + strconv.FormatInt(brokenID, 10) + "&to_id=" + strconv.FormatInt(brokenID, 10))
	if rec.Code != http.StatusConflict {
		t.Fatalf("verify broken row = %d %s, want 409", rec.Code, rec.Body.String())
	}
	var errResp struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode broken response: %v (%s)", err, rec.Body.String())
	}
	if errResp.Error.Code != "AUDIT_CHAIN_BROKEN" {
		t.Fatalf("broken code = %q", errResp.Error.Code)
	}
	if got, ok := errResp.Error.Details["broken_at"].(float64); !ok || int64(got) != brokenID {
		t.Fatalf("broken details = %+v, want broken_at=%d", errResp.Error.Details, brokenID)
	}
	if got, ok := errResp.Error.Details["checked"].(float64); !ok || int(got) != 1 {
		t.Fatalf("broken details checked = %+v", errResp.Error.Details)
	}
}
