package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"aiops/internal/store"
	"aiops/internal/store/postgres"
)

// 第 10 轮 high 回归（真实 Postgres）：采集间隔 30s 时 24h 窗口有 2880 点，
// GET /api/servers/:id/metrics?minutes=1440 必须全部返回且 truncated=false。
// 修复前 store 的 LIMIT 硬上限是 1440，接口只回最近 12h，页面把其余 12h
// 误标成「无采样」。
//
// 与 internal/store/postgres / audit_verify 集成测试共用同一测试库与
// advisory lock，避免并行 TRUNCATE。
func TestServerMetricsThirtySecondFullDayPostgresIntegration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 复用 audit_verify 的库锁（同库串行）。
	db := acquireAuditVerifyDBLock(t)

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := postgres.Open(ctx, pgAuditVerifyDSN(), log)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	truncate := func() {
		if _, err := db.Exec(`TRUNCATE servers RESTART IDENTITY CASCADE`); err != nil {
			t.Fatalf("truncate servers: %v", err)
		}
	}
	truncate()
	t.Cleanup(truncate)

	srv, err := st.Servers().Create(ctx, store.CreateServerParams{
		Name: "it-fast-01", OS: store.ServerOSLinux, Host: "127.0.0.1", Port: 22,
		Username: "root",
		Credentials: store.ServerCredentials{
			AuthMethod:        store.AuthMethodPassword,
			PasswordEncrypted: "v1:test",
		},
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	now := time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)
	base := now.Add(-time.Duration(store.MetricWindowLimitMax-1) * 30 * time.Second)
	// 2880 点 × 30s，末点=now；按 store 单批 ≤500 写入。
	batch := make([]store.InsertMetricParams, 0, 500)
	for index := 0; index < store.MetricWindowLimitMax; index++ {
		batch = append(batch, store.InsertMetricParams{
			ServerID:  srv.ID,
			CreatedAt: base.Add(time.Duration(index) * 30 * time.Second),
			CPU:       float64(index % 100), Mem: 40, Disk: 50, Load1: 0.5,
			NetIn: 1024, NetOut: 2048,
		})
		if len(batch) == cap(batch) {
			if err := st.Metrics().BatchInsert(ctx, batch); err != nil {
				t.Fatalf("batch insert: %v", err)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := st.Metrics().BatchInsert(ctx, batch); err != nil {
			t.Fatalf("batch insert tail: %v", err)
		}
	}

	d := &Deps{Store: st, Clock: func() time.Time { return now }}
	r := gin.New()
	protected := r.Group("/api")
	protected.Use(withTestAdminIdentity())
	d.RegisterMonitorRoutes(protected)

	req := httptest.NewRequest(http.MethodGet, "/api/servers/"+srv.ID+"/metrics?minutes=1440", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var res metricsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Points) != store.MetricWindowLimitMax {
		t.Fatalf("points = %d, want %d", len(res.Points), store.MetricWindowLimitMax)
	}
	if res.Truncated {
		t.Fatal("truncated = true with a complete 30s/24h window")
	}
	first, _ := res.Points[0]["created_at"].(string)
	last, _ := res.Points[len(res.Points)-1]["created_at"].(string)
	if first != base.Format(time.RFC3339) || last != now.Format(time.RFC3339) {
		t.Fatalf("range = %s..%s, want %s..%s", first, last,
			base.Format(time.RFC3339), now.Format(time.RFC3339))
	}
}
