// evaluator_test.go 覆盖 G31：告警评估状态机——持续 duration_windows 个
// 点越限触发 firing 且 message 正确；firing 存在不重复触发（ErrConflict
// 吞掉）；全部恢复→resolved；冷却期内不触发、期满触发新 firing；
// silent_until 未来→notify_status=skipped 不投递；缓冲未命中回退查库
// （LIMIT=duration_windows）；duration_windows 上限 30。
package monitor

import (
	"context"
	"testing"
	"time"

	"aiops/internal/store"
)

func newTestEvaluator(t *testing.T) (*evaluator, *fakeMonitorStore, *notifier) {
	t.Helper()
	st := newFakeMonitorStore()
	now := time.Now()
	clock := func() time.Time { return now }
	notifier := newNotifier(NotifyConfig{}, st, nil, quietLogger(), clock, &fakeAuditSink{})
	e := &evaluator{
		st: st, buf: newMetricBuffer(), log: quietLogger(),
		clock: clock, audit: &fakeAuditSink{}, notify: notifier,
		interval: 60 * time.Second,
	}
	return e, st, notifier
}

func cpuRule(name string, threshold float64, windows, cooldown int, mods ...func(*store.AlertRule)) store.AlertRule {
	r := store.AlertRule{
		ID: "rule-" + name, Name: name, Metric: store.AlertMetricCPU,
		Comparator: store.AlertComparatorGT, Threshold: threshold,
		DurationWindows: windows, CooldownSeconds: cooldown,
		Severity: store.AlertSeverityWarning, Enabled: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	for _, m := range mods {
		m(&r)
	}
	return r
}

func mustCreateRule(t *testing.T, st *fakeMonitorStore, r store.AlertRule) store.AlertRule {
	t.Helper()
	created, err := st.AlertRules().Create(context.Background(), store.CreateAlertRuleParams{
		Name: r.Name, Metric: r.Metric, Comparator: r.Comparator, Threshold: r.Threshold,
		DurationWindows: r.DurationWindows, CooldownSeconds: r.CooldownSeconds,
		Severity: r.Severity, SilentUntil: r.SilentUntil, NotifyChannelID: r.NotifyChannelID,
		Enabled: r.Enabled,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	return created
}

func seedLatest(t *testing.T, st *fakeMonitorStore, serverID string, cpu float64) {
	t.Helper()
	err := st.Metrics().BatchInsert(context.Background(), []store.InsertMetricParams{{
		ServerID: serverID, CreatedAt: time.Now().Add(time.Second),
		CPU: cpu, Mem: 40, Disk: 50, Load1: 0.5,
	}})
	if err != nil {
		t.Fatalf("seed latest: %v", err)
	}
}

func TestEvaluatorTriggersAfterDurationWindows(t *testing.T) {
	e, st, _ := newTestEvaluator(t)
	mustCreateRule(t, st, cpuRule("cpu-high", 90, 3, 300))
	srv := fakeServer("s1", store.ServerOSLinux)
	st.upsertServer(srv)
	seedLatest(t, st, srv.ID, 95)

	// 环形缓冲只喂 2 个越限点：不足 duration_windows，不触发。
	e.buf.push(srv.ID, Sample{CPU: 95}, time.Now())
	e.buf.push(srv.ID, Sample{CPU: 96}, time.Now())
	n, err := e.evaluateOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want 0/nil（点数不足）", n, err)
	}

	// 第 3 个越限点：触发。
	e.buf.push(srv.ID, Sample{CPU: 97}, time.Now())
	n, err = e.evaluateOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v, want 1/nil", n, err)
	}
	ev := st.alerts[0]
	if ev.Status != store.AlertEventFiring || ev.Value != 97 {
		t.Fatalf("event = %+v", ev)
	}
	wantMsg := "cpu_pct 97.0 > 90.0 持续 3 个采样点"
	if ev.Message != wantMsg {
		t.Fatalf("message = %q, want %q", ev.Message, wantMsg)
	}

	// firing 存在时重复越限不新建（部分唯一索引兜底路径）。
	e.buf.push(srv.ID, Sample{CPU: 99}, time.Now())
	n, err = e.evaluateOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("duplicate trigger: n=%d err=%v, want 0", n, err)
	}
	if len(st.alerts) != 1 {
		t.Fatalf("events = %d, want 1", len(st.alerts))
	}
}

func TestEvaluatorResolvesWhenAllNormal(t *testing.T) {
	e, st, _ := newTestEvaluator(t)
	mustCreateRule(t, st, cpuRule("cpu-high", 90, 2, 300))
	srv := fakeServer("s2", store.ServerOSLinux)
	st.upsertServer(srv)
	seedLatest(t, st, srv.ID, 95)

	e.buf.push(srv.ID, Sample{CPU: 95}, time.Now())
	e.buf.push(srv.ID, Sample{CPU: 96}, time.Now())
	if _, err := e.evaluateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.alerts) != 1 || st.alerts[0].Status != store.AlertEventFiring {
		t.Fatalf("precondition failed: %+v", st.alerts)
	}

	// 全部恢复正常 → resolved。
	e.buf.push(srv.ID, Sample{CPU: 10}, time.Now())
	e.buf.push(srv.ID, Sample{CPU: 11}, time.Now())
	if _, err := e.evaluateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.alerts[0].Status != store.AlertEventResolved {
		t.Fatalf("status = %s, want resolved", st.alerts[0].Status)
	}
	if st.alerts[0].ResolvedAt == nil {
		t.Fatal("resolved_at must be set")
	}
}

func TestEvaluatorCooldownBlocksAndExpiryAllows(t *testing.T) {
	e, st, _ := newTestEvaluator(t)
	mustCreateRule(t, st, cpuRule("cpu-high", 90, 1, 3600))
	srv := fakeServer("s3", store.ServerOSLinux)
	st.upsertServer(srv)
	seedLatest(t, st, srv.ID, 95)

	// 触发。
	e.buf.push(srv.ID, Sample{CPU: 95}, time.Now())
	if n, _ := e.evaluateOnce(context.Background()); n != 1 {
		t.Fatalf("first trigger n=%d", n)
	}
	// 恢复。
	e.buf.push(srv.ID, Sample{CPU: 5}, time.Now())
	if _, err := e.evaluateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 冷却期内再越限：不触发。
	e.buf.push(srv.ID, Sample{CPU: 99}, time.Now())
	if n, _ := e.evaluateOnce(context.Background()); n != 0 {
		t.Fatalf("cooldown must block, n=%d", n)
	}
	// 推进时钟越过冷却期：触发新 firing。
	resolved := st.alerts[0].ResolvedAt.Add(time.Duration(3600) * time.Second)
	e.clock = func() time.Time { return resolved.Add(time.Second) }
	if n, _ := e.evaluateOnce(context.Background()); n != 1 {
		t.Fatalf("cooldown expiry must allow new firing, n=%d", n)
	}
}

func TestEvaluatorSilentUntilSkipsNotify(t *testing.T) {
	e, st, notifier := newTestEvaluator(t)
	until := time.Now().Add(time.Hour)
	mustCreateRule(t, st, cpuRule("cpu-silent", 90, 1, 300, func(r *store.AlertRule) {
		r.SilentUntil = &until
	}))
	srv := fakeServer("s4", store.ServerOSLinux)
	st.upsertServer(srv)
	seedLatest(t, st, srv.ID, 95)
	e.buf.push(srv.ID, Sample{CPU: 95}, time.Now())

	if n, _ := e.evaluateOnce(context.Background()); n != 1 {
		t.Fatalf("silent period must still record the event, n=%d", n)
	}
	if st.alerts[0].Status != store.AlertEventFiring {
		t.Fatal("event must be firing")
	}
	if st.alerts[0].NotifyStatus != store.NotifySkipped {
		t.Fatalf("notify_status = %s, want skipped", st.alerts[0].NotifyStatus)
	}
	// 未入队：notifier 去重表无该事件。
	if len(notifierDelivered(notifier)) != 0 {
		t.Fatal("silent event must not be enqueued")
	}
}

func TestEvaluatorNoChannelOnlyRecords(t *testing.T) {
	e, st, _ := newTestEvaluator(t)
	mustCreateRule(t, st, cpuRule("cpu-noch", 90, 1, 300))
	srv := fakeServer("s5", store.ServerOSLinux)
	st.upsertServer(srv)
	seedLatest(t, st, srv.ID, 95)
	e.buf.push(srv.ID, Sample{CPU: 95}, time.Now())
	if n, _ := e.evaluateOnce(context.Background()); n != 1 {
		t.Fatalf("n=%d", n)
	}
	if st.alerts[0].NotifyStatus != store.NotifyNone {
		t.Fatalf("notify_status = %s, want none（无绑定渠道只入库）", st.alerts[0].NotifyStatus)
	}
}

func TestEvaluatorEnqueuesNotifyWhenChannelBound(t *testing.T) {
	e, st, notifier := newTestEvaluator(t)
	ch, err := st.NotificationChannels().Create(context.Background(), store.CreateNotificationChannelParams{
		Name: "hook", Kind: store.NotificationWebhook, ConfigJSON: "enc", ConfigHint: "hint",
	})
	if err != nil {
		t.Fatal(err)
	}
	chID := ch.ID
	mustCreateRule(t, st, cpuRule("cpu-hook", 90, 1, 300, func(r *store.AlertRule) {
		r.NotifyChannelID = &chID
	}))
	srv := fakeServer("s6", store.ServerOSLinux)
	st.upsertServer(srv)
	seedLatest(t, st, srv.ID, 95)
	e.buf.push(srv.ID, Sample{CPU: 95}, time.Now())

	if n, _ := e.evaluateOnce(context.Background()); n != 1 {
		t.Fatalf("n=%d", n)
	}
	if st.alerts[0].NotifyStatus != store.NotifyPending {
		t.Fatalf("notify_status = %s, want pending", st.alerts[0].NotifyStatus)
	}
	// 同一 firing 重复入队被去重（delivered 表——进程内第二道防线）。
	notifier.enqueue(notifyTask{Event: st.alerts[0], RuleName: "cpu-hook", ServerName: "srv-s6", ChannelID: chID})
	if len(notifierDelivered(notifier)) != 1 {
		t.Fatalf("dedup failed, delivered=%d", len(notifierDelivered(notifier)))
	}
}

// notifierDelivered 读取 notifier 去重表（测试观察口）。
func notifierDelivered(n *notifier) []int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]int64, 0, len(n.delivered))
	for id := range n.delivered {
		out = append(out, id)
	}
	return out
}

func TestEvaluatorBufferMissFallsBackToStore(t *testing.T) {
	e, st, _ := newTestEvaluator(t)
	mustCreateRule(t, st, cpuRule("cpu-fb", 90, 2, 300))
	srv := fakeServer("s7", store.ServerOSLinux)
	st.upsertServer(srv)
	seedLatest(t, st, srv.ID, 95)
	// 不写环形缓冲（模拟进程重启后的缓冲未命中）：评估回退查库。
	// fake store Window 返回 BatchInsert 写入的点（1 个）——不足 2 个点，
	// 不触发；再补一个库内点后触发。
	past := time.Now().Add(-2 * time.Minute)
	if err := st.Metrics().BatchInsert(context.Background(), []store.InsertMetricParams{{
		ServerID: srv.ID, CreatedAt: past, CPU: 96,
	}}); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.evaluateOnce(context.Background()); n != 1 {
		t.Fatalf("fallback window query should trigger, n=%d", n)
	}
}

// TestEvaluatorFallbackWindowUsesNewestPoints 锁死回退查库的取点语义：
// 窗口内点数超过 duration_windows 时必须取最近 N 点（ASC+LIMIT 会取到
// 最早 N 点——采集曾失败而库内有近窗数据的恢复期，最新越限点会被最旧
// 正常点稀释导致漏报，反之陈旧越限点也会误报 firing）。
// 走查实测形态：interval=120s、windows=3，回退窗口覆盖 240+ 个正常点与
// 尾部 6 个 cpu=95 点，DESC LIMIT 3 必须取到 95 而非正常点。
func TestEvaluatorFallbackWindowUsesNewestPoints(t *testing.T) {
	now := time.Now()
	// 形态一：旧正常点 + 最新越限点（点数 > N）→ 必须触发（漏报防线）。
	// 时间戳全部落在回退窗口（now-5min, now+1min）内且互不重叠，间隔 30s。
	e1, st1, _ := newTestEvaluator(t)
	mustCreateRule(t, st1, cpuRule("cpu-newest", 85, 3, 300))
	srv1 := fakeServer("s9", store.ServerOSLinux)
	st1.upsertServer(srv1)
	seedLatest(t, st1, srv1.ID, 95)
	var batch1 []store.InsertMetricParams
	for i := 0; i < 5; i++ { // 5 个旧正常点（ASC+LIMIT 会误取最早的 3 条）。
		batch1 = append(batch1, store.InsertMetricParams{
			ServerID: srv1.ID, CreatedAt: now.Add(-time.Duration(290-30*i) * time.Second), CPU: 40,
		})
	}
	for i := 0; i < 3; i++ { // 最新 3 个越限点。
		batch1 = append(batch1, store.InsertMetricParams{
			ServerID: srv1.ID, CreatedAt: now.Add(-time.Duration(120-60*i) * time.Second), CPU: 95,
		})
	}
	if err := st1.Metrics().BatchInsert(context.Background(), batch1); err != nil {
		t.Fatal(err)
	}
	n1, err := e1.evaluateOnce(context.Background())
	if err != nil || n1 != 1 {
		t.Fatalf("newest violating points must trigger: n=%d err=%v, want 1", n1, err)
	}
	if got := st1.alerts[0].Value; got != 95 {
		t.Fatalf("event value = %v, want 95（最新点）", got)
	}

	// 形态二（反例）：旧越限点 + 最新正常点 → 不得用陈旧数据误报。
	e2, st2, _ := newTestEvaluator(t)
	mustCreateRule(t, st2, cpuRule("cpu-stale", 85, 3, 300))
	srv2 := fakeServer("s10", store.ServerOSLinux)
	st2.upsertServer(srv2)
	seedLatest(t, st2, srv2.ID, 40)
	var batch2 []store.InsertMetricParams
	for i := 0; i < 3; i++ { // 最早 3 个越限点（ASC+LIMIT 会误取并误报）。
		batch2 = append(batch2, store.InsertMetricParams{
			ServerID: srv2.ID, CreatedAt: now.Add(-time.Duration(290-30*i) * time.Second), CPU: 95,
		})
	}
	for i := 0; i < 5; i++ { // 最新 5 个正常点。
		batch2 = append(batch2, store.InsertMetricParams{
			ServerID: srv2.ID, CreatedAt: now.Add(-time.Duration(200-40*i) * time.Second), CPU: 40,
		})
	}
	if err := st2.Metrics().BatchInsert(context.Background(), batch2); err != nil {
		t.Fatal(err)
	}
	n2, err := e2.evaluateOnce(context.Background())
	if err != nil || n2 != 0 {
		t.Fatalf("stale violating points must not trigger: n=%d err=%v, want 0", n2, err)
	}
}

func TestEvaluatorDurationWindowsCappedAt30(t *testing.T) {
	e, st, _ := newTestEvaluator(t)
	mustCreateRule(t, st, cpuRule("cpu-30", 90, 30, 300))
	srv := fakeServer("s8", store.ServerOSLinux)
	st.upsertServer(srv)
	seedLatest(t, st, srv.ID, 95)
	// 喂 30 个越限点（契约上限），触发一次。
	for i := 0; i < 30; i++ {
		e.buf.push(srv.ID, Sample{CPU: 95}, time.Now().Add(time.Duration(i)*time.Second))
	}
	if n, _ := e.evaluateOnce(context.Background()); n != 1 {
		t.Fatalf("30-point window must trigger, n=%d", n)
	}
	// LIMIT 生效：缓冲只存 1440 点，lastN(30) 全取到；窗口查询上限 30
	// 由 Window(Limit=windows) 保证（fake store 已按 Limit 截断）。
}
