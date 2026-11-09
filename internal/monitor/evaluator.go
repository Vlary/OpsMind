// evaluator.go 实现五期 M1 的告警评估引擎（契约 2.10 约定 2/5.15/11 节）：
// tick 驱动（默认 30s，采集轮后立即追加一轮），对每条 enabled 规则 × 每台
// 有最新指标的资产，取进程内环形缓冲最近 duration_windows 个点（≤30；缓冲
// 未命中回退查库 Window，LIMIT=duration_windows）评估：
//   - 全部点满足比较条件且无 firing → 创建 firing（部分唯一索引兜底并发，
//     ErrConflict 吞掉）；冷却期内（resolved_at+cooldown > now）不触发；
//     静默期（silent_until > now）事件照常入库但 notify_status=skipped。
//   - firing 存在且全部点恢复正常 → resolved。
//   - firing 创建时按规则绑定渠道投递通知（渠道为空只入库不投递）。
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"aiops/internal/store"
)

// 评估查询上限（契约 11 节 #4：firing 列表与冷却回溯都走索引且有界）。
const (
	// firingScanPages 是每轮 firing 快照的最大翻页数（200/页 → 上限 1000）。
	firingScanPages = 5
	// cooldownScanPages 是冷却回溯（按 server 查 resolved 事件）的最大翻页数。
	cooldownScanPages = 3
	// evaluationHardLimit 是评估每轮处理的 (规则×服务器) 组合硬上限。
	evaluationHardLimit = 20000
)

// evaluator 是告警评估引擎。
type evaluator struct {
	st       store.Store
	buf      *metricBuffer
	log      *slog.Logger
	clock    func() time.Time
	audit    auditSink
	notify   *notifier
	interval time.Duration // 采集间隔（回退查库窗口宽度用）

	// startDiagnosis 由 main 组装时注入（M2 diagnosis.Engine 提供；
	// monitor 不 import diagnosis——契约 5.15）。critical 告警创建且
	// settings["monitor.auto_diagnose"]=true 时回调；nil 表示未装配。
	startDiagnosis func(ctx context.Context, ev store.AlertEvent)

	mu sync.Mutex // evaluateOnce 互斥（tick 与采集后触发可能并发）
}

// severityOrder 返回等级排序权重（critical 最高）。
func severityOrder(s store.AlertSeverity) int {
	switch s {
	case store.AlertSeverityCritical:
		return 3
	case store.AlertSeverityWarning:
		return 2
	default:
		return 1
	}
}

// metricValue 从 Sample 取规则指标值。
func metricValue(m store.AlertMetric, s Sample) float64 {
	switch m {
	case store.AlertMetricCPU:
		return s.CPU
	case store.AlertMetricMem:
		return s.Mem
	case store.AlertMetricDisk:
		return s.Disk
	case store.AlertMetricLoad1:
		return s.Load1
	case store.AlertMetricNetIn:
		return float64(s.NetIn)
	case store.AlertMetricNetOut:
		return float64(s.NetOut)
	default:
		return 0
	}
}

// violates 判断单点是否越限。
func violates(rule store.AlertRule, v float64) bool {
	if rule.Comparator == store.AlertComparatorLT {
		return v < rule.Threshold
	}
	return v > rule.Threshold
}

// evaluateOnce 跑一轮评估，返回新触发的 firing 数。
func (e *evaluator) evaluateOnce(ctx context.Context) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	rules, err := e.st.AlertRules().List(ctx)
	if err != nil {
		return 0, err
	}
	enabled := make([]store.AlertRule, 0, len(rules))
	for _, r := range rules {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}
	if len(enabled) == 0 {
		return 0, nil
	}

	latest, err := e.st.Metrics().Latest(ctx)
	if err != nil {
		return 0, err
	}
	if len(latest) == 0 {
		return 0, nil
	}

	servers, err := e.st.Servers().List(ctx)
	if err != nil {
		return 0, err
	}
	names := make(map[string]string, len(servers))
	for _, s := range servers {
		names[s.ID] = s.Name
	}

	// firing 快照：分页拉取，上限 firingScanPages×200（契约 11 节 #4）。
	firing := make(map[string]store.AlertEvent, 64)
	before := int64(0)
	for page := 0; page < firingScanPages; page++ {
		items, hasMore, err := e.st.AlertEvents().List(ctx, store.AlertEventFilter{
			Status: store.AlertEventFiring, BeforeID: before, Limit: 200,
		})
		if err != nil {
			return 0, err
		}
		for _, ev := range items {
			firing[ev.RuleID+"\x00"+ev.ServerID] = ev
		}
		if !hasMore || len(items) == 0 {
			break
		}
		before = items[len(items)-1].ID
	}

	now := e.clock()
	triggered := 0
	combos := 0
	for _, rule := range enabled {
		for _, lat := range latest {
			combos++
			if combos > evaluationHardLimit { // 终止条件（防 OOM 铁律）
				e.log.Warn("monitor_evaluate_combos_capped", "limit", evaluationHardLimit)
				return triggered, nil
			}
			e.evaluatePair(ctx, rule, lat, names, firing, now, &triggered)
		}
	}
	return triggered, nil
}

// evaluatePair 评估单条 (规则×服务器)。
func (e *evaluator) evaluatePair(ctx context.Context, rule store.AlertRule,
	lat store.ServerMetricLatest, names map[string]string,
	firing map[string]store.AlertEvent, now time.Time, triggered *int) {

	key := rule.ID + "\x00" + lat.ServerID
	windows := rule.DurationWindows
	if windows < 1 {
		windows = 1
	}
	if windows > 30 {
		windows = 30 // 契约上限（表 CHECK 1..30）
	}

	points, stamps, ok := e.buf.lastN(lat.ServerID, windows)
	freshness := time.Duration(windows+2) * e.collectInterval()
	if ok && len(stamps) > 0 {
		// 缓冲命中仍需校验新鲜度：采集暂停（连续失败暂停 10 分钟）期间
		// 缓冲内容不变而评估照跑，旧采样点不得触发新 firing（五期评审
		// 发现）；陈旧则与未命中同路径（回退查库带时间窗，天然过滤）。
		if newest := stamps[len(stamps)-1]; now.Sub(newest) > freshness {
			ok = false
		}
	}
	if !ok || len(points) < windows {
		// 缓冲未命中/点数不足：回退查库（LIMIT=duration_windows）。
		from := now.Add(-time.Duration(windows+2) * e.collectInterval())
		ms, err := e.st.Metrics().Window(ctx, store.MetricWindow{
			ServerID: lat.ServerID, From: from, To: now.Add(time.Minute), Limit: windows,
		})
		if err != nil {
			e.log.Warn("monitor_evaluate_window_failed", "server_id", lat.ServerID, "error", err.Error())
			return
		}
		if len(ms) >= windows {
			points = make([]Sample, len(ms))
			for i, m := range ms {
				points[i] = Sample{CPU: m.CPU, Mem: m.Mem, Disk: m.Disk, Load1: m.Load1, NetIn: m.NetIn, NetOut: m.NetOut}
			}
		}
	}
	if len(points) < windows {
		return // 点数不足：不触发也不恢复（观测尚未覆盖持续窗口）
	}

	allViolating := true
	allNormal := true
	for _, p := range points {
		v := metricValue(rule.Metric, p)
		if violates(rule, v) {
			allNormal = false
		} else {
			allViolating = false
		}
	}

	if allViolating {
		if _, exists := firing[key]; exists {
			return // firing 唯一（契约 5.15）
		}
		// 冷却：最近一次 resolved + cooldown > now 则不触发（回溯有界）。
		if e.inCooldown(ctx, rule, lat.ServerID, now) {
			return
		}
		last := points[len(points)-1]
		value := metricValue(rule.Metric, last)
		cmp := ">"
		if rule.Comparator == store.AlertComparatorLT {
			cmp = "<"
		}
		msg := fmt.Sprintf("%s %.1f %s %.1f 持续 %d 个采样点",
			string(rule.Metric), value, cmp, rule.Threshold, windows)
		ev, err := e.st.AlertEvents().Create(ctx, rule, lat.ServerID, value, msg, now)
		if err != nil {
			if !errors.Is(err, store.ErrConflict) {
				e.log.Warn("monitor_alert_create_failed", "rule", rule.Name,
					"server_id", lat.ServerID, "error", err.Error())
			}
			return // 并发重复触发由唯一索引兜底，吞掉（契约 3.4）
		}
		*triggered++
		e.writeAlertAudit(ctx, "alert.triggered", ev)

		// 静默：事件照常入库但 skipped、不投递（契约 2.10 约定 2）。
		if rule.SilentUntil != nil && rule.SilentUntil.After(now) {
			_ = e.st.AlertEvents().SetNotifyStatus(ctx, ev.ID, store.NotifySkipped, now)
			return
		}
		// 无绑定渠道：只入库不投递（notify_status=none）。
		if rule.NotifyChannelID == nil {
			return
		}
		_ = e.st.AlertEvents().SetNotifyStatus(ctx, ev.ID, store.NotifyPending, now)
		e.notify.enqueue(notifyTask{
			Event: ev, RuleName: rule.Name, ServerName: names[lat.ServerID],
			ChannelID: *rule.NotifyChannelID,
		})
		// critical + 自动诊断开关 → 注入回调（M2 装配；nil 则跳过）。
		if rule.Severity == store.AlertSeverityCritical && e.autoDiagnoseEnabled(ctx) && e.startDiagnosis != nil {
			go func(ev store.AlertEvent) {
				defer func() {
					if r := recover(); r != nil {
						e.log.Warn("monitor_auto_diagnose_panic", "recover", r)
					}
				}()
				diagCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				e.startDiagnosis(diagCtx, ev)
			}(ev)
		}
		return
	}

	if allNormal {
		if ev, exists := firing[key]; exists {
			if resolved, err := e.st.AlertEvents().Resolve(ctx, ev.ID, now); err == nil {
				e.writeAlertAudit(ctx, "alert.resolved", resolved)
			} else if !errors.Is(err, store.ErrConflict) {
				e.log.Warn("monitor_alert_resolve_failed", "event_id", ev.ID, "error", err.Error())
			}
		}
	}
}

// inCooldown 回溯该 (规则×服务器) 最近一次 resolved 事件判断冷却。
// 翻页上限 cooldownScanPages×200；超出未找到视为不在冷却期（有界查询）。
func (e *evaluator) inCooldown(ctx context.Context, rule store.AlertRule, serverID string, now time.Time) bool {
	before := int64(0)
	for page := 0; page < cooldownScanPages; page++ {
		items, hasMore, err := e.st.AlertEvents().List(ctx, store.AlertEventFilter{
			Status: store.AlertEventResolved, ServerID: serverID, BeforeID: before, Limit: 200,
		})
		if err != nil {
			return false // 查询失败不阻塞触发（宁多触发不漏触发）
		}
		for _, ev := range items {
			if ev.RuleID != rule.ID || ev.ResolvedAt == nil {
				continue
			}
			return now.Before(ev.ResolvedAt.Add(time.Duration(rule.CooldownSeconds) * time.Second))
		}
		if !hasMore || len(items) == 0 {
			return false
		}
		before = items[len(items)-1].ID
	}
	return false
}

// autoDiagnoseEnabled 读 settings["monitor.auto_diagnose"]（JSON 布尔，
// 缺省 false；读取失败按 false——失败关闭）。
func (e *evaluator) autoDiagnoseEnabled(ctx context.Context) bool {
	setting, err := e.st.Settings().Get(ctx, "monitor.auto_diagnose")
	if err != nil {
		return false
	}
	var on bool
	if err := json.Unmarshal(setting.Value, &on); err != nil {
		return false
	}
	return on
}

// collectInterval 返回采集间隔（回退查库窗口宽度用）；未注入按缺省 60s。
func (e *evaluator) collectInterval() time.Duration {
	if e.interval <= 0 {
		return 60 * time.Second
	}
	return e.interval
}
