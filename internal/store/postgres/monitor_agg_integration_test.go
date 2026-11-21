// M1 图表聚合（五期本轮）的 PostgreSQL 集成测试：/api/monitor/trend、
// /api/alert-events/stats、/api/alert-events 的 fired 范围过滤、
// /api/inspection/trend 与 overview 的 firing 分级聚合。真实 PG
// （TEST_DATABASE_URL；包级 TestMain 持有 advisory lock）。
package postgres

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"aiops/internal/store"
)

// cleanMonitorAggTables 清空聚合测试涉及的表（servers 级联其余表）。
func cleanMonitorAggTables(t *testing.T) {
	t.Helper()
	_, err := testDB.pool.Exec(context.Background(), `
		TRUNCATE server_metrics, server_metrics_latest, alert_events, alert_rules,
		         diagnosis_sessions, inspection_report_items, inspection_reports,
		         inspection_jobs, servers
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func mustAggServer(t *testing.T, name string) store.Server {
	t.Helper()
	s, err := testDB.Servers().Create(context.Background(), store.CreateServerParams{
		Name: name, OS: store.ServerOSLinux, Host: "127.0.0.1", Port: 22,
		Username: "root",
		Credentials: store.ServerCredentials{
			AuthMethod:        store.AuthMethodPassword,
			PasswordEncrypted: "v1:test",
		},
	})
	if err != nil {
		t.Fatalf("create server %s: %v", name, err)
	}
	return s
}

func mustAggRule(t *testing.T, name string, severity store.AlertSeverity) store.AlertRule {
	t.Helper()
	r, err := testDB.AlertRules().Create(context.Background(), store.CreateAlertRuleParams{
		Name: name, Metric: store.AlertMetricCPU, Comparator: store.AlertComparatorGT,
		Threshold: 90, DurationWindows: 3, CooldownSeconds: 300, Severity: severity, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create rule %s: %v", name, err)
	}
	return r
}

// insertEvent 直接落一条事件（绕过 Create 的 firing 唯一约束，便于构造
// 同 rule/server 的多条历史与 resolved 行）。
func insertEvent(t *testing.T, ruleID, serverID string, status store.AlertEventStatus,
	severity store.AlertSeverity, firedAt time.Time) {
	t.Helper()
	_, err := testDB.pool.Exec(context.Background(), `
		INSERT INTO alert_events (rule_id, server_id, status, severity, value, message,
			fired_at, resolved_at, notify_status)
		VALUES ($1::uuid, $2::uuid, $3::text, $4::text, 95, 'm', $5::timestamptz,
			CASE WHEN $3::text = 'resolved' THEN $5::timestamptz ELSE NULL END, 'none')`,
		ruleID, serverID, string(status), string(severity), utc(firedAt))
	if err != nil {
		t.Fatalf("insert alert event: %v", err)
	}
}

// TestAlertEventsFiredIndexMigrated：00016 的专用索引存在（纯 fired_at 范围
// 扫描；00007 的 status 前导列索引覆盖不了）。
func TestAlertEventsFiredIndexMigrated(t *testing.T) {
	var n int
	if err := testDB.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'public' AND indexname = 'alert_events_fired_idx'`).Scan(&n); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if n != 1 {
		t.Fatal("alert_events_fired_idx is missing (migration 00016 not applied)")
	}
}

// TestMetricStoreTrend：date_bin 分桶、p95/avg/max、台数/样本数；上界不含；
// 空窗口返回空切片而非错误。
func TestMetricStoreTrend(t *testing.T) {
	cleanMonitorAggTables(t)
	ctx := context.Background()
	srvA := mustAggServer(t, "agg-a")
	srvB := mustAggServer(t, "agg-b")
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	if err := testDB.Metrics().BatchInsert(ctx, []store.InsertMetricParams{
		{ServerID: srvA.ID, CreatedAt: from.Add(10 * time.Minute), CPU: 10, Mem: 20},
		{ServerID: srvB.ID, CreatedAt: from.Add(20 * time.Minute), CPU: 30, Mem: 40},
		{ServerID: srvA.ID, CreatedAt: from.Add(5*time.Hour + 30*time.Minute), CPU: 50, Mem: 60},
		// 上界（To）不含：不能进任何桶。
		{ServerID: srvB.ID, CreatedAt: to, CPU: 99, Mem: 99},
	}); err != nil {
		t.Fatalf("insert metrics: %v", err)
	}
	points, err := testDB.Metrics().Trend(ctx, store.MetricTrendQuery{
		From: from, To: to, BucketSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("trend: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("points = %d, want 2 (%+v)", len(points), points)
	}
	p := points[0]
	if !p.BucketStart.Equal(from) {
		t.Fatalf("bucket[0] = %s, want %s", p.BucketStart, from)
	}
	if p.Servers != 2 || p.Samples != 2 {
		t.Fatalf("servers/samples = %d/%d", p.Servers, p.Samples)
	}
	// 走查第 17 轮 low：首末采样时刻取实际样本 min/max，而非桶边界。
	if !p.FirstSample.Equal(from.Add(10*time.Minute)) || !p.LastSample.Equal(from.Add(20*time.Minute)) {
		t.Fatalf("first/last sample = %s/%s, want %s/%s", p.FirstSample, p.LastSample,
			from.Add(10*time.Minute), from.Add(20*time.Minute))
	}
	if p.CPUAvg != 20 || p.CPUMax != 30 || p.MemAvg != 30 || p.MemMax != 40 {
		t.Fatalf("aggregates = %+v", p)
	}
	// percentile_cont(0.95) 对 [10,30] 线性插值 = 29。
	if math.Abs(p.CPUP95-29) > 1e-6 {
		t.Fatalf("cpu_p95 = %v, want 29", p.CPUP95)
	}
	second := points[1]
	if !second.BucketStart.Equal(from.Add(5 * time.Hour)) {
		t.Fatalf("bucket[1] = %s", second.BucketStart)
	}
	if second.Samples != 1 || second.CPUP95 != 50 || second.MemP95 != 60 {
		t.Fatalf("bucket[1] = %+v", second)
	}

	// 空窗口：空切片、无错误（handler 负责补 null 桶）。
	empty, err := testDB.Metrics().Trend(ctx, store.MetricTrendQuery{
		From: from.Add(48 * time.Hour), To: from.Add(72 * time.Hour), BucketSeconds: 3600,
	})
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty trend = %v, %v", empty, err)
	}
	// 非法桶宽 / 反向窗口：ErrInvalid。
	for _, q := range []store.MetricTrendQuery{
		{From: from, To: to, BucketSeconds: 0},
		{From: to, To: from, BucketSeconds: 3600},
	} {
		if _, err := testDB.Metrics().Trend(ctx, q); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("invalid query %+v err = %v, want ErrInvalid", q, err)
		}
	}
}

// TestMetricStoreTrendClampsFutureSamples：走查第 16 轮 medium——Trend 的
// To 是当前桶结束（未来时刻），时钟超前机器写入的未来样本会让 P95/峰值
// 冲到 100%。Now 非零时 SQL 上界钳到服务端当前时刻；零值保持旧行为。
func TestMetricStoreTrendClampsFutureSamples(t *testing.T) {
	cleanMonitorAggTables(t)
	ctx := context.Background()
	srv := mustAggServer(t, "agg-future")
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(2 * time.Hour)
	now := from.Add(40 * time.Minute)
	if err := testDB.Metrics().BatchInsert(ctx, []store.InsertMetricParams{
		{ServerID: srv.ID, CreatedAt: from.Add(10 * time.Minute), CPU: 10, Mem: 20},
		{ServerID: srv.ID, CreatedAt: now, CPU: 30, Mem: 40},
		// 同一桶内的未来样本（now+10min）：不钳制会被算成峰值 100。
		{ServerID: srv.ID, CreatedAt: now.Add(10 * time.Minute), CPU: 100, Mem: 100},
	}); err != nil {
		t.Fatalf("insert metrics: %v", err)
	}
	clamped, err := testDB.Metrics().Trend(ctx, store.MetricTrendQuery{
		From: from, To: to, BucketSeconds: 3600, Now: now,
	})
	if err != nil {
		t.Fatalf("trend clamped: %v", err)
	}
	if len(clamped) != 1 {
		t.Fatalf("clamped points = %d, want 1 (%+v)", len(clamped), clamped)
	}
	p := clamped[0]
	if p.Samples != 2 || p.Servers != 1 {
		t.Fatalf("clamped samples/servers = %d/%d, want 2/1", p.Samples, p.Servers)
	}
	if p.CPUMax != 30 || math.Abs(p.CPUAvg-20) > 1e-6 {
		t.Fatalf("clamped aggregates = %+v, want max 30 avg 20", p)
	}
	// 未来样本被排除后，last_sample 必须停在 now，而不是 now+10min。
	if !p.FirstSample.Equal(from.Add(10*time.Minute)) || !p.LastSample.Equal(now) {
		t.Fatalf("clamped first/last sample = %s/%s, want %s/%s", p.FirstSample, p.LastSample,
			from.Add(10*time.Minute), now)
	}
	// Now 为零：不钳制，保持旧行为（仅用于离线聚合/单测）。
	unclamped, err := testDB.Metrics().Trend(ctx, store.MetricTrendQuery{
		From: from, To: to, BucketSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("trend unclamped: %v", err)
	}
	if len(unclamped) != 1 || unclamped[0].Samples != 3 || unclamped[0].CPUMax != 100 {
		t.Fatalf("unclamped = %+v, want 3 samples max 100", unclamped)
	}
}

// TestAlertEventStoreCountFiringBySeverityNoCap：单等级 >1000 必须精确
// （旧 overview 实现按 3×5 页×200 统计，1001 条只会记 1000）。
func TestAlertEventStoreCountFiringBySeverityNoCap(t *testing.T) {
	cleanMonitorAggTables(t)
	ctx := context.Background()
	srv := mustAggServer(t, "firing-a")
	if _, err := testDB.pool.Exec(ctx, `
		INSERT INTO alert_rules (name, metric, comparator, threshold, severity)
		SELECT 'bulk-' || g, 'cpu_pct', 'gt', 90, 'warning'
		FROM generate_series(1, 1001) g`); err != nil {
		t.Fatalf("seed rules: %v", err)
	}
	if _, err := testDB.pool.Exec(ctx, `
		INSERT INTO alert_events (rule_id, server_id, status, severity, value, fired_at, notify_status)
		SELECT id, $1::uuid, 'firing', 'warning', 95, now(), 'none'
		FROM alert_rules WHERE name LIKE 'bulk-%'`, srv.ID); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	critRule := mustAggRule(t, "crit-rule", store.AlertSeverityCritical)
	if _, err := testDB.AlertEvents().Create(ctx, critRule, srv.ID, 99, "m", time.Now().UTC()); err != nil {
		t.Fatalf("create critical: %v", err)
	}
	// resolved info：不计入 firing。
	infoRule := mustAggRule(t, "info-rule", store.AlertSeverityInfo)
	ev, err := testDB.AlertEvents().Create(ctx, infoRule, srv.ID, 50, "m", time.Now().UTC())
	if err != nil {
		t.Fatalf("create info: %v", err)
	}
	if _, err := testDB.AlertEvents().Resolve(ctx, ev.ID, time.Now().UTC()); err != nil {
		t.Fatalf("resolve info: %v", err)
	}

	counts, err := testDB.AlertEvents().CountFiringBySeverity(ctx)
	if err != nil {
		t.Fatalf("count by severity: %v", err)
	}
	if counts.Warning != 1001 || counts.Critical != 1 || counts.Info != 0 {
		t.Fatalf("counts = %+v", counts)
	}
	total, err := testDB.AlertEvents().CountFiring(ctx)
	if err != nil || total != 1002 {
		t.Fatalf("total = %d err=%v", total, err)
	}
}

// TestAlertEventStoreStats：两条聚合 SQL——分级时间轴 + top rules 排序。
func TestAlertEventStoreStats(t *testing.T) {
	cleanMonitorAggTables(t)
	ctx := context.Background()
	srv1 := mustAggServer(t, "stats-1")
	srv2 := mustAggServer(t, "stats-2")
	srv3 := mustAggServer(t, "stats-3")
	ruleA := mustAggRule(t, "stats-a", store.AlertSeverityWarning)
	ruleB := mustAggRule(t, "stats-b", store.AlertSeverityCritical)
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	insertEvent(t, ruleA.ID, srv1.ID, store.AlertEventFiring, store.AlertSeverityWarning, from.Add(10*time.Minute))
	insertEvent(t, ruleA.ID, srv2.ID, store.AlertEventResolved, store.AlertSeverityWarning, from.Add(20*time.Minute))
	insertEvent(t, ruleB.ID, srv1.ID, store.AlertEventFiring, store.AlertSeverityCritical, from.Add(12*time.Hour))
	insertEvent(t, ruleB.ID, srv3.ID, store.AlertEventResolved, store.AlertSeverityCritical, from.Add(12*time.Hour+30*time.Minute))

	stats, err := testDB.AlertEvents().Stats(ctx, store.AlertEventStatsQuery{
		From: from, To: to, BucketSeconds: 3600, TopRules: 5,
	})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Total != 4 || stats.DistinctRules != 2 {
		t.Fatalf("total/distinct = %d/%d", stats.Total, stats.DistinctRules)
	}
	if len(stats.Timeline) != 2 {
		t.Fatalf("timeline = %+v", stats.Timeline)
	}
	if !stats.Timeline[0].BucketStart.Equal(from) || stats.Timeline[0].Warning != 2 ||
		stats.Timeline[0].Info != 0 || stats.Timeline[0].Critical != 0 {
		t.Fatalf("timeline[0] = %+v", stats.Timeline[0])
	}
	if !stats.Timeline[1].BucketStart.Equal(from.Add(12*time.Hour)) || stats.Timeline[1].Critical != 2 {
		t.Fatalf("timeline[1] = %+v", stats.Timeline[1])
	}
	if len(stats.TopRules) != 2 {
		t.Fatalf("top rules = %+v", stats.TopRules)
	}
	// 并列 count=2，按 last_fired_at DESC：ruleB(12:30) 在前。
	if stats.TopRules[0].RuleID != ruleB.ID || stats.TopRules[0].Severity != store.AlertSeverityCritical ||
		stats.TopRules[0].Count != 2 || stats.TopRules[0].ServerCount != 2 {
		t.Fatalf("top[0] = %+v", stats.TopRules[0])
	}
	if stats.TopRules[1].RuleID != ruleA.ID || stats.TopRules[1].RuleName != "stats-a" {
		t.Fatalf("top[1] = %+v", stats.TopRules[1])
	}

	// 空窗口：Total=0、Timeline 为空、TopRules 空（handler 补零时间轴）。
	empty, err := testDB.AlertEvents().Stats(ctx, store.AlertEventStatsQuery{
		From: from.Add(48 * time.Hour), To: from.Add(72 * time.Hour), BucketSeconds: 3600, TopRules: 5,
	})
	if err != nil || empty.Total != 0 || len(empty.Timeline) != 0 || len(empty.TopRules) != 0 {
		t.Fatalf("empty stats = %+v err=%v", empty, err)
	}
	// top_rules 越界：ErrInvalid。
	if _, err := testDB.AlertEvents().Stats(ctx, store.AlertEventStatsQuery{
		From: from, To: to, BucketSeconds: 3600, TopRules: 21,
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("top_rules=21 err = %v, want ErrInvalid", err)
	}
}

// TestAlertEventStoreListFiredFilters：rule_id + fired_at 范围过滤与既有
// 游标/limit 叠加（fired_at DESC, id DESC 不变）。
func TestAlertEventStoreListFiredFilters(t *testing.T) {
	cleanMonitorAggTables(t)
	ctx := context.Background()
	srv := mustAggServer(t, "list-a")
	ruleA := mustAggRule(t, "list-a", store.AlertSeverityWarning)
	ruleB := mustAggRule(t, "list-b", store.AlertSeverityWarning)
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	bucketEnd := base.Add(time.Hour)
	lastInstant := bucketEnd.Add(-500 * time.Microsecond)
	insertEvent(t, ruleA.ID, srv.ID, store.AlertEventResolved, store.AlertSeverityWarning, base)
	insertEvent(t, ruleA.ID, srv.ID, store.AlertEventResolved, store.AlertSeverityWarning, lastInstant)
	// 恰在桶边界的事件属于下一桶（半开区间 [after, before)）。
	insertEvent(t, ruleA.ID, srv.ID, store.AlertEventResolved, store.AlertSeverityWarning, bucketEnd)
	insertEvent(t, ruleB.ID, srv.ID, store.AlertEventResolved, store.AlertSeverityWarning, base.Add(2*time.Hour))

	after := base
	before := bucketEnd
	items, hasMore, err := testDB.AlertEvents().List(ctx, store.AlertEventFilter{
		RuleID: ruleA.ID, FiredAfter: &after, FiredBefore: &before, Limit: 50,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if hasMore || len(items) != 2 {
		t.Fatalf("items=%d hasMore=%v", len(items), hasMore)
	}
	// 桶末 500µs 的事件在列表内，边界事件不在；按 fired_at DESC 排序。
	if !items[0].FiredAt.Equal(lastInstant) || !items[1].FiredAt.Equal(after) {
		t.Fatalf("order = %s, %s", items[0].FiredAt, items[1].FiredAt)
	}
	// 下一桶只含边界事件：相邻桶不重不漏。
	nextItems, _, err := testDB.AlertEvents().List(ctx, store.AlertEventFilter{
		RuleID: ruleA.ID, FiredAfter: &bucketEnd,
		FiredBefore: ptrTime(bucketEnd.Add(time.Hour)), Limit: 50,
	})
	if err != nil || len(nextItems) != 1 || !nextItems[0].FiredAt.Equal(bucketEnd) {
		t.Fatalf("next bucket = %d err=%v", len(nextItems), err)
	}
	// 全量：半开上界只排除 window 外的事件，不丢桶末事件。
	all, _, err := testDB.AlertEvents().List(ctx, store.AlertEventFilter{Limit: 50})
	if err != nil || len(all) != 4 {
		t.Fatalf("all = %d err=%v", len(all), err)
	}
	// 与 before_id 叠加：游标语义不变（不含游标行，只回到 base 那条）。
	older, _, err := testDB.AlertEvents().List(ctx, store.AlertEventFilter{
		BeforeID: items[0].ID, Limit: 50,
	})
	if err != nil || len(older) != 1 || !older[0].FiredAt.Equal(base) {
		t.Fatalf("before_id = %d err=%v", len(older), err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestAlertEventStoreStatsDrilldownConsistency：柱（stats 桶）与表（List
// 下钻）对桶末 500µs 的事件同口径——stats 的总数/桶计数等于下钻行数
// （走查第 13 轮 medium：旧闭区间上界减 1ms 会漏掉 …59.9995Z）。
func TestAlertEventStoreStatsDrilldownConsistency(t *testing.T) {
	cleanMonitorAggTables(t)
	ctx := context.Background()
	srv := mustAggServer(t, "bucket-end")
	rule := mustAggRule(t, "bucket-end-rule", store.AlertSeverityWarning)
	from := time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	// 桶 [11:00, 12:00)：一条普通、一条落在最后 500µs。
	insertEvent(t, rule.ID, srv.ID, store.AlertEventResolved, store.AlertSeverityWarning, from.Add(10*time.Minute))
	insertEvent(t, rule.ID, srv.ID, store.AlertEventResolved, store.AlertSeverityCritical, to.Add(-500*time.Microsecond))

	stats, err := testDB.AlertEvents().Stats(ctx, store.AlertEventStatsQuery{
		From: from, To: to, BucketSeconds: 3600, TopRules: 5,
	})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Total != 2 || len(stats.Timeline) != 1 {
		t.Fatalf("stats total/timeline = %d/%+v", stats.Total, stats.Timeline)
	}
	bucketTotal := stats.Timeline[0].Warning + stats.Timeline[0].Critical + stats.Timeline[0].Info
	if bucketTotal != stats.Total {
		t.Fatalf("bucket total = %d, stats.Total = %d", bucketTotal, stats.Total)
	}

	after, before := from, to
	items, _, err := testDB.AlertEvents().List(ctx, store.AlertEventFilter{
		FiredAfter: &after, FiredBefore: &before, Limit: 50,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if int64(len(items)) != bucketTotal {
		t.Fatalf("drilldown rows = %d, bucket count = %d", len(items), bucketTotal)
	}
	// 桶末 500µs 的 critical 事件确实出现在下钻结果里。
	var sawLast bool
	for _, item := range items {
		if item.FiredAt.Equal(to.Add(-500*time.Microsecond)) && item.Severity == store.AlertSeverityCritical {
			sawLast = true
		}
	}
	if !sawLast {
		t.Fatalf("bucket-end event missing from drilldown: %+v", items)
	}
}

// TestInspectionReportStoreTrend：DESC LIMIT n+1 取数、Go 反转升序、
// truncated=还有更早报告；job_id 过滤与即时巡检（job_id NULL）口径。
func TestInspectionReportStoreTrend(t *testing.T) {
	cleanMonitorAggTables(t)
	ctx := context.Background()
	job, err := testDB.InspectionJobs().Create(ctx, store.CreateInspectionJobParams{
		Name: "nightly", Cron: "0 2 * * *", Template: store.InspectionBasic, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	insertReport := func(jobID *string, createdAt time.Time, score int) string {
		t.Helper()
		var id string
		err := testDB.pool.QueryRow(ctx, `
			INSERT INTO inspection_reports (job_id, trigger, server_count, avg_score,
				anomaly_count, started_at, finished_at, created_at)
			VALUES ($1::uuid, 'cron', 2, $2, 1, $3, $3, $3)
			RETURNING id`, jobID, score, utc(createdAt)).Scan(&id)
		if err != nil {
			t.Fatalf("insert report: %v", err)
		}
		return id
	}
	base := time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC)
	r1 := insertReport(&job.ID, base, 70)
	r2 := insertReport(&job.ID, base.Add(24*time.Hour), 80)
	r3 := insertReport(&job.ID, base.Add(48*time.Hour), 90)
	r4 := insertReport(nil, base.Add(72*time.Hour), 60)

	points, truncated, err := testDB.InspectionReports().Trend(ctx, job.ID, 2)
	if err != nil {
		t.Fatalf("trend: %v", err)
	}
	if !truncated || len(points) != 2 {
		t.Fatalf("points=%d truncated=%v, want 2/true", len(points), truncated)
	}
	if points[0].ReportID != r2 || points[1].ReportID != r3 {
		t.Fatalf("order = %+v, want [%s %s]", points, r2, r3)
	}
	if points[0].AvgScore != 80 || points[0].ServerCount != 2 ||
		points[0].AnomalyCount != 1 || points[0].Trigger != store.InspectionTriggerCron {
		t.Fatalf("point[0] = %+v", points[0])
	}

	points, truncated, err = testDB.InspectionReports().Trend(ctx, job.ID, 60)
	if err != nil || truncated || len(points) != 3 {
		t.Fatalf("all points=%d truncated=%v err=%v", len(points), truncated, err)
	}
	if points[0].ReportID != r1 || points[2].ReportID != r3 {
		t.Fatalf("ascending order = %+v", points)
	}

	// 不传 job：包含即时巡检（job_id NULL）的最新两份，升序。
	points, truncated, err = testDB.InspectionReports().Trend(ctx, "", 2)
	if err != nil || !truncated || len(points) != 2 {
		t.Fatalf("global points=%d truncated=%v err=%v", len(points), truncated, err)
	}
	if points[0].ReportID != r3 || points[1].ReportID != r4 {
		t.Fatalf("global order = %+v", points)
	}

	// 无报告：空切片、无错误；limit >60 返回 ErrInvalid。
	points, truncated, err = testDB.InspectionReports().Trend(ctx, uuid.NewString(), 30)
	if err != nil || truncated || len(points) != 0 {
		t.Fatalf("empty = %+v truncated=%v err=%v", points, truncated, err)
	}
	if _, _, err := testDB.InspectionReports().Trend(ctx, job.ID, 61); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("limit=61 err = %v, want ErrInvalid", err)
	}
}

// TestMetricStoreExcludesNetworkDevices：资产从 server 改成 network_device
// 后，历史指标既不能出现在 Latest / Trend / Window / LatestByServer，也要
// 在 Update 的同一语句里被清理（走查第 9 轮 medium）。
func TestMetricStoreExcludesNetworkDevices(t *testing.T) {
	cleanMonitorAggTables(t)
	ctx := context.Background()
	stillServer := mustAggServer(t, "still-server")
	sw, err := testDB.Servers().Create(ctx, store.CreateServerParams{
		Name: "net-switch-01", OS: store.ServerOSLinux, Host: "127.0.0.1", Port: 22,
		Username: "admin",
		Credentials: store.ServerCredentials{
			AuthMethod:        store.AuthMethodPassword,
			PasswordEncrypted: "v1:test",
		},
	})
	if err != nil {
		t.Fatalf("create switch: %v", err)
	}
	from := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	if err := testDB.Metrics().BatchInsert(ctx, []store.InsertMetricParams{
		{ServerID: stillServer.ID, CreatedAt: from.Add(10 * time.Minute), CPU: 10, Mem: 20},
		{ServerID: sw.ID, CreatedAt: from.Add(20 * time.Minute), CPU: 90, Mem: 80},
	}); err != nil {
		t.Fatalf("insert metrics: %v", err)
	}

	// 改类型前：两台都在 Latest 与 Trend 里（基线，证明后面的排除不是空测）。
	latest, err := testDB.Metrics().Latest(ctx)
	if err != nil || len(latest) != 2 {
		t.Fatalf("latest before = %+v err=%v, want 2", latest, err)
	}

	// 走查场景：server → network_device（ServerFormDrawer 允许编辑）。
	kind := store.DeviceKindNetworkDevice
	osNet := store.ServerOSNetwork
	brand := store.NetdevBrandCisco
	proto := store.AccessProtoSSH
	updated, err := testDB.Servers().Update(ctx, sw.ID, store.UpdateServerParams{
		DeviceKind: &kind, OS: &osNet, NetdevBrand: &brand, AccessProto: &proto,
	})
	if err != nil {
		t.Fatalf("switch to network_device: %v", err)
	}
	if updated.DeviceKind != store.DeviceKindNetworkDevice {
		t.Fatalf("device_kind = %q, want network_device", updated.DeviceKind)
	}

	// Update 同步清理残留：latest 与明细都不再有该资产的行。
	var latestRows, metricRows int
	if err := testDB.pool.QueryRow(ctx,
		`SELECT count(*) FROM server_metrics_latest WHERE server_id = $1::uuid`, sw.ID).Scan(&latestRows); err != nil {
		t.Fatalf("count latest: %v", err)
	}
	if err := testDB.pool.QueryRow(ctx,
		`SELECT count(*) FROM server_metrics WHERE server_id = $1::uuid`, sw.ID).Scan(&metricRows); err != nil {
		t.Fatalf("count metrics: %v", err)
	}
	if latestRows != 0 || metricRows != 0 {
		t.Fatalf("residual rows latest=%d metrics=%d, want 0/0", latestRows, metricRows)
	}

	// 读路径也不再返回它（即使绕过清理直接插入残留行也成立）。
	if _, err := testDB.pool.Exec(ctx, `
		INSERT INTO server_metrics_latest (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1)
		VALUES ($1::uuid, $2, 77, 66, 55, 1)`, sw.ID, utc(from.Add(30*time.Minute))); err != nil {
		t.Fatalf("reinsert residual latest: %v", err)
	}
	if _, err := testDB.pool.Exec(ctx, `
		INSERT INTO server_metrics (server_id, created_at, cpu_pct, mem_pct, disk_pct, load_1)
		VALUES ($1::uuid, $2, 77, 66, 55, 1)`, sw.ID, utc(from.Add(30*time.Minute))); err != nil {
		t.Fatalf("reinsert residual metric: %v", err)
	}

	latest, err = testDB.Metrics().Latest(ctx)
	if err != nil || len(latest) != 1 || latest[0].ServerID != stillServer.ID {
		t.Fatalf("latest after = %+v err=%v, want only still-server", latest, err)
	}
	if _, err := testDB.Metrics().LatestByServer(ctx, sw.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("latest by network device err = %v, want ErrNotFound", err)
	}
	window, err := testDB.Metrics().Window(ctx, store.MetricWindow{
		ServerID: sw.ID, From: from, To: to, Limit: 10,
	})
	if err != nil || len(window) != 0 {
		t.Fatalf("network device window = %+v err=%v, want empty", window, err)
	}
	allWindow, err := testDB.Metrics().Window(ctx, store.MetricWindow{From: from, To: to, Limit: 10})
	if err != nil || len(allWindow) != 1 || allWindow[0].ServerID != stillServer.ID {
		t.Fatalf("all window = %+v err=%v, want only still-server", allWindow, err)
	}
	points, err := testDB.Metrics().Trend(ctx, store.MetricTrendQuery{
		From: from, To: to, BucketSeconds: 3600,
	})
	if err != nil || len(points) != 1 {
		t.Fatalf("trend = %+v err=%v, want 1 bucket", points, err)
	}
	if points[0].Servers != 1 || points[0].Samples != 1 {
		t.Fatalf("trend servers/samples = %d/%d, want 1/1 (network device excluded)",
			points[0].Servers, points[0].Samples)
	}
	if points[0].CPUMax != 10 {
		t.Fatalf("trend cpu max = %v, want 10", points[0].CPUMax)
	}
}
