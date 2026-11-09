// phase5_m1_integration_test.go 是五期 M1 的 store 集成测试（真实
// PostgreSQL，契约 9.2 G11/G30-G32 的持久层部分）：metrics 批量插入 +
// UPSERT latest 同事务、窗口查询 ASC/LIMIT 校验、保留清理；告警规则 CRUD
// 与唯一名冲突；事件创建/部分唯一索引冲突/Resolve/计数；渠道 CRUD 与
// 密文不回显；AuditFilter 时间过滤。
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"aiops/internal/store"
)

func cleanPhase5Tables(t *testing.T) {
	t.Helper()
	_, err := testDB.pool.Exec(context.Background(), `
		TRUNCATE servers, notification_channels, alert_rules, alert_events,
		         server_metrics, server_metrics_latest, audit_logs
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate phase5 tables: %v", err)
	}
}

func mustServerRow(t *testing.T) store.Server {
	t.Helper()
	srv, err := testDB.Servers().Create(context.Background(), store.CreateServerParams{
		Name: "m1-it", Host: "127.0.0.1", Port: 22, Username: "root",
		Credentials: store.ServerCredentials{
			AuthMethod:        store.AuthMethodPassword,
			PasswordEncrypted: "v1:test:dummy",
		},
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	return srv
}

func TestMetricsBatchInsertLatestWindow(t *testing.T) {
	cleanPhase5Tables(t)
	ctx := context.Background()
	srv := mustServerRow(t)

	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	var batch []store.InsertMetricParams
	for i := 0; i < 10; i++ {
		batch = append(batch, store.InsertMetricParams{
			ServerID: srv.ID, CreatedAt: base.Add(time.Duration(i) * time.Minute),
			CPU: float64(i), Mem: 40, Disk: 60, Load1: 0.5, NetIn: int64(100 * i), NetOut: int64(200 * i),
		})
	}
	if err := testDB.Metrics().BatchInsert(ctx, batch); err != nil {
		t.Fatalf("batch insert: %v", err)
	}
	// 超过 500 行拒绝（防 OOM 契约 11 节 #3）。
	var big []store.InsertMetricParams
	for i := 0; i < 501; i++ {
		big = append(big, store.InsertMetricParams{ServerID: srv.ID, CreatedAt: base, CPU: 1})
	}
	if err := testDB.Metrics().BatchInsert(ctx, big); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("batch >500 err = %v, want ErrInvalid", err)
	}

	// latest = 最后一个点。
	latest, err := testDB.Metrics().LatestByServer(ctx, srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.CPU != 9 {
		t.Fatalf("latest cpu = %v, want 9", latest.CPU)
	}
	// 窗口查询：ASC + Limit 截断取最新 N 个。
	points, err := testDB.Metrics().Window(ctx, store.MetricWindow{
		ServerID: srv.ID, From: base.Add(-time.Minute), To: base.Add(20 * time.Minute), Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 {
		t.Fatalf("window points = %d, want 3", len(points))
	}
	if !points[0].CreatedAt.Before(points[2].CreatedAt) || points[2].CPU != 9 {
		t.Fatalf("window order/limit wrong: %+v", points)
	}
	// Limit > 1440 拒绝。
	if _, err := testDB.Metrics().Window(ctx, store.MetricWindow{
		ServerID: srv.ID, Limit: 1441, From: base, To: base,
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("window limit 1441 err = %v, want ErrInvalid", err)
	}
	// CHECK 约束：cpu>100 拒绝（ErrInvalid）。
	if err := testDB.Metrics().BatchInsert(ctx, []store.InsertMetricParams{{
		ServerID: srv.ID, CreatedAt: base, CPU: 101,
	}}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("cpu=101 err = %v, want ErrInvalid", err)
	}
	// 保留清理。
	n, err := testDB.Metrics().DeleteBefore(ctx, base.Add(5*time.Minute))
	if err != nil || n != 5 {
		t.Fatalf("DeleteBefore n=%d err=%v, want 5", n, err)
	}
}

func TestAlertRulesCRUD(t *testing.T) {
	cleanPhase5Tables(t)
	ctx := context.Background()

	rule, err := testDB.AlertRules().Create(ctx, store.CreateAlertRuleParams{
		Name: "cpu-high", Metric: store.AlertMetricCPU, Comparator: store.AlertComparatorGT,
		Threshold: 90, DurationWindows: 3, CooldownSeconds: 300,
		Severity: store.AlertSeverityWarning, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if _, err := testDB.AlertRules().Create(ctx, store.CreateAlertRuleParams{
		Name: "cpu-high", Metric: store.AlertMetricCPU, Comparator: store.AlertComparatorGT,
		Threshold: 10, DurationWindows: 1, CooldownSeconds: 60, Severity: store.AlertSeverityWarning,
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate rule err = %v, want ErrConflict", err)
	}
	// 非法 duration_windows（表 CHECK）。
	if _, err := testDB.AlertRules().Create(ctx, store.CreateAlertRuleParams{
		Name: "bad", Metric: store.AlertMetricCPU, Comparator: store.AlertComparatorGT,
		Threshold: 10, DurationWindows: 31, CooldownSeconds: 60, Severity: store.AlertSeverityWarning,
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("windows=31 err = %v, want ErrInvalid", err)
	}
	// pct 阈值 >100 被 alert_rules_check 拒绝。
	if _, err := testDB.AlertRules().Create(ctx, store.CreateAlertRuleParams{
		Name: "bad2", Metric: store.AlertMetricMem, Comparator: store.AlertComparatorGT,
		Threshold: 101, DurationWindows: 1, CooldownSeconds: 60, Severity: store.AlertSeverityWarning,
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("mem>100 err = %v, want ErrInvalid", err)
	}
	// 六期打磨 sprint 2（走查 high）：采集器的「采集不可达」系统哨兵规则
	// 曾用 cpu_pct + threshold=101，被上面同一条 CHECK 拒绝（store.ErrInvalid）
	// ——告警永远创建失败、失联对用户完全静默。系统规则改用 load_1 +
	// 极大阈值后必须能落库（load_1 不受百分比 CHECK 约束）。
	if _, err := testDB.AlertRules().Create(ctx, store.CreateAlertRuleParams{
		Name: "monitor.collector.unreachable", Metric: store.AlertMetricLoad1,
		Comparator: store.AlertComparatorGT, Threshold: 1e12,
		DurationWindows: 1, CooldownSeconds: 60, Severity: store.AlertSeverityCritical,
		Enabled: false,
	}); err != nil {
		t.Fatalf("suspend sentinel rule must be creatable, err = %v", err)
	}
	// 收尾：清掉哨兵规则，让下面的 list 断言仍为 1 条。
	allRules, err := testDB.AlertRules().List(ctx)
	if err != nil {
		t.Fatalf("list after sentinel: %v", err)
	}
	for _, r := range allRules {
		if r.Name == "monitor.collector.unreachable" {
			if err := testDB.AlertRules().Delete(ctx, r.ID); err != nil {
				t.Fatalf("delete sentinel: %v", err)
			}
		}
	}

	disabled := false
	updated, err := testDB.AlertRules().Update(ctx, rule.ID, store.UpdateAlertRuleParams{Enabled: &disabled})
	if err != nil || updated.Enabled {
		t.Fatalf("update err=%v rule=%+v", err, updated)
	}
	rules, err := testDB.AlertRules().List(ctx)
	if err != nil || len(rules) != 1 {
		t.Fatalf("list = %d err=%v", len(rules), err)
	}
	if err := testDB.AlertRules().Delete(ctx, rule.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := testDB.AlertRules().Get(ctx, rule.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get deleted err = %v", err)
	}
}

func TestAlertEventsLifecycle(t *testing.T) {
	cleanPhase5Tables(t)
	ctx := context.Background()
	srv := mustServerRow(t)
	rule, err := testDB.AlertRules().Create(ctx, store.CreateAlertRuleParams{
		Name: "cpu-high", Metric: store.AlertMetricCPU, Comparator: store.AlertComparatorGT,
		Threshold: 90, Severity: store.AlertSeverityCritical, DurationWindows: 1, CooldownSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}

	ev, err := testDB.AlertEvents().Create(ctx, rule, srv.ID, 95.5, "msg", time.Now().UTC())
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	// 部分唯一索引：同 (rule,server) 的第二条 firing 冲突。
	if _, err := testDB.AlertEvents().Create(ctx, rule, srv.ID, 96, "msg2", time.Now().UTC()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate firing err = %v, want ErrConflict", err)
	}
	// resolved 后可再触发。
	resolved, err := testDB.AlertEvents().Resolve(ctx, ev.ID, time.Now().UTC())
	if err != nil || resolved.Status != store.AlertEventResolved || resolved.ResolvedAt == nil {
		t.Fatalf("resolve = %+v err=%v", resolved, err)
	}
	if _, err := testDB.AlertEvents().Resolve(ctx, ev.ID, time.Now().UTC()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("double resolve err = %v, want ErrConflict", err)
	}
	ev2, err := testDB.AlertEvents().Create(ctx, rule, srv.ID, 97, "msg3", time.Now().UTC())
	if err != nil {
		t.Fatalf("re-trigger after resolve: %v", err)
	}
	if err := testDB.AlertEvents().SetNotifyStatus(ctx, ev2.ID, store.NotifySent, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// 计数与过滤列表。
	if n, _ := testDB.AlertEvents().CountFiring(ctx); n != 1 {
		t.Fatalf("CountFiring = %d, want 1", n)
	}
	if n, _ := testDB.AlertEvents().CountFiringByServer(ctx, srv.ID); n != 1 {
		t.Fatalf("CountFiringByServer = %d", n)
	}
	items, hasMore, err := testDB.AlertEvents().List(ctx, store.AlertEventFilter{
		Status: store.AlertEventFiring, Severity: store.AlertSeverityCritical, Limit: 10,
	})
	if err != nil || hasMore || len(items) != 1 || items[0].RuleName != "cpu-high" {
		t.Fatalf("list = %+v hasMore=%v err=%v", items, hasMore, err)
	}
}

func TestNotificationChannelsCRUD(t *testing.T) {
	cleanPhase5Tables(t)
	ctx := context.Background()
	ch, err := testDB.NotificationChannels().Create(ctx, store.CreateNotificationChannelParams{
		Name: "hook", Kind: store.NotificationWebhook, ConfigJSON: "v1:enc:blob", ConfigHint: "h",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ch.HasConfig || ch.ConfigEncrypted == "" {
		t.Fatal("HasConfig must derive from non-empty ciphertext")
	}
	if _, err := testDB.NotificationChannels().Create(ctx, store.CreateNotificationChannelParams{
		Name: "hook", Kind: store.NotificationWebhook, ConfigJSON: "x",
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate name err = %v, want ErrConflict", err)
	}
	got, err := testDB.NotificationChannels().Get(ctx, ch.ID)
	if err != nil || got.ConfigEncrypted != "v1:enc:blob" {
		t.Fatalf("get = %+v err=%v", got, err)
	}
	// 非法 kind（表 CHECK）。
	if _, err := testDB.NotificationChannels().Create(ctx, store.CreateNotificationChannelParams{
		Name: "bad", Kind: store.NotificationKind("sms"), ConfigJSON: "x",
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("kind=sms err = %v, want ErrInvalid", err)
	}
	// 空串 config 替换被拒（契约 3.3）。
	empty := ""
	if _, err := testDB.NotificationChannels().Update(ctx, ch.ID, store.UpdateNotificationChannelParams{
		ConfigJSON: &empty,
	}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("empty config err = %v, want ErrInvalid", err)
	}
	if err := testDB.NotificationChannels().Delete(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAuditListTimeFilter(t *testing.T) {
	cleanPhase5Tables(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// audit_logs 要求 64 位十六进制哈希链；直接给满足 CHECK 的定长哈希
	//（本测试只验证时间过滤，不重算链）。
	h := func(i int) string { return strings.Repeat(fmt.Sprintf("%d", i%10), 64) }
	for i := 0; i < 3; i++ {
		if err := testDB.AuditLogs().Append(ctx, store.CreateAuditParams{
			CreatedAt: now.Add(time.Duration(i) * time.Hour), ActorType: "system",
			Action: "metric.collect", ResourceType: "monitor", Detail: []byte("{}"),
			PrevHash: h(i), Hash: h(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 下界：只留最近 2 条。
	items, _, err := testDB.AuditLogs().List(ctx, store.AuditFilter{
		FromTime: now.Add(30 * time.Minute), Limit: 10,
	})
	if err != nil || len(items) != 2 {
		t.Fatalf("from filter = %d err=%v, want 2", len(items), err)
	}
	// 上界：只留最早 2 条。
	items, _, err = testDB.AuditLogs().List(ctx, store.AuditFilter{
		ToTime: now.Add(90 * time.Minute), Limit: 10,
	})
	if err != nil || len(items) != 2 {
		t.Fatalf("to filter = %d err=%v, want 2", len(items), err)
	}
	// 组合：空窗。
	items, _, err = testDB.AuditLogs().List(ctx, store.AuditFilter{
		FromTime: now.Add(4 * time.Hour), ToTime: now.Add(5 * time.Hour), Limit: 10,
	})
	if err != nil || len(items) != 0 {
		t.Fatalf("empty window = %d err=%v", len(items), err)
	}
}
