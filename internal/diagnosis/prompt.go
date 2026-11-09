// prompt.go 装配诊断 mini-loop 的系统提示词与首轮上下文（契约 5.16）：
// 告警事件（若有）+ 最新指标 + 最近 1h 指标 + 目标 Server 元数据。
// 诊断协议写死：模型每轮输出严格 JSON——{"action":"probe","command":...}
// 请求执行一条只读命令，或 {"action":"report","report":{...}} 产出报告。
package diagnosis

import (
	"context"
	"fmt"
	"strings"
	"time"

	"aiops/internal/store"
)

// contextLimits 是上下文装配的防 OOM 上限（写死）：
//   - 最近 1h 指标最多取 60 点（60s 粒度下恰为 1h；store LIMIT 60）
//   - 装配文本上限 32KB（超出截断——上下文不是证据正文）
const (
	metricWindowMinutes = 60
	metricWindowPoints  = 60
	contextTextMaxBytes = 32768
)

// diagnosisSystemPrompt 是 mini-loop 的系统提示词（协议与硬性行为写死；
// 风险闸是代码强制的，不写在提示词里——契约 2.10 约定 3）。
func diagnosisSystemPrompt(srv store.Server) string {
	osLabel := string(srv.OS)
	if srv.DeviceKind == store.DeviceKindNetworkDevice {
		osLabel = "network (" + string(srv.NetdevBrand) + ")"
	}
	return "你是 OpsMind 的服务器故障诊断引擎，正在对单台资产做只读诊断。\n" +
		"目标资产：name=" + srv.Name + " os=" + osLabel + " host=" + srv.Host + "\n" +
		"协议（每轮必须且只能输出一个 JSON 对象，不要输出其他文本）：\n" +
		`1. 需要更多证据时输出：{"action":"probe","command":"<一条只读命令>"}` + "\n" +
		`2. 证据足够时输出最终报告：{"action":"report","report":{...}}` + "\n" +
		"报告 report 的 schema（严格 JSON）：\n" +
		`{"summary":"一段结论","severity":"info|warning|critical","root_cause":"原因分析",` +
		`"evidence":[{"source":"command|metric|alert|log","command":"...","output_excerpt":"输出摘录","note":"说明"}],` +
		`"suggestions":[{"command":"修复命令","reason":"理由"}],"confidence":"low|medium|high"}` + "\n" +
		"硬性行为：\n" +
		"- 只给出只读采集命令（如 ps/free/df/cat/journalctl 查询）；写操作与重启类命令只会被引擎拒绝。\n" +
		"- 每轮命令被拒绝时，改写为等价的只读形式继续，不要重复同一条命令。\n" +
		"- 建议修复命令（suggestions）面向人工审批执行，可以包含需要审批的命令，但禁止破坏性命令。\n" +
		"- 证据摘录只保留与结论相关的行。\n" +
		"- 结论必须基于实际拿到的命令输出或指标，不得臆测。"
}

// buildFirstUserMessage 装配首轮 user 消息：告警（若有）+ 最新指标 +
// 最近 1h 指标摘要 + 资产元数据。任何装配失败只省略该段（诊断不因缺
// 指标停摆）。
func buildFirstUserMessage(ctx context.Context, st store.Store, sess store.DiagnosisSession,
	srv store.Server, now time.Time) string {
	var b strings.Builder
	b.WriteString("诊断目标与背景信息如下：\n")
	fmt.Fprintf(&b, "server: id=%s name=%s os=%s host=%s:%d tags=%v\n",
		srv.ID, srv.Name, srv.OS, srv.Host, srv.Port, srv.Tags)

	if sess.AlertEventID != nil {
		if ev, err := st.AlertEvents().Get(ctx, *sess.AlertEventID); err == nil {
			fmt.Fprintf(&b, "触发告警: id=%d rule=%s metric=%s severity=%s value=%.2f fired_at=%s message=%s\n",
				ev.ID, ev.RuleName, ev.Metric, ev.Severity, ev.Value,
				ev.FiredAt.Format(time.RFC3339), ev.Message)
		}
	}
	if m, err := st.Metrics().LatestByServer(ctx, srv.ID); err == nil {
		fmt.Fprintf(&b, "最新指标: cpu=%.1f%% mem=%.1f%% disk=%.1f%% load_1=%.2f net_in=%dBps net_out=%dBps at=%s\n",
			m.CPU, m.Mem, m.Disk, m.Load1, m.NetIn, m.NetOut, m.CreatedAt.Format(time.RFC3339))
	}
	if points, err := st.Metrics().Window(ctx, store.MetricWindow{
		ServerID: srv.ID, From: now.Add(-metricWindowMinutes * time.Minute),
		To: now, Limit: metricWindowPoints,
	}); err == nil && len(points) > 0 {
		var cpuSum, memSum float64
		var cpuMax, memMax float64
		for _, p := range points {
			cpuSum += p.CPU
			memSum += p.Mem
			if p.CPU > cpuMax {
				cpuMax = p.CPU
			}
			if p.Mem > memMax {
				memMax = p.Mem
			}
		}
		fmt.Fprintf(&b, "近1h指标(%d点): cpu_avg=%.1f%% cpu_max=%.1f%% mem_avg=%.1f%% mem_max=%.1f%%\n",
			len(points), cpuSum/float64(len(points)), cpuMax, memSum/float64(len(points)), memMax)
	}
	b.WriteString("开始诊断。先输出第一轮 JSON。")

	msg := b.String()
	if len(msg) > contextTextMaxBytes {
		msg = truncateBytes(msg, contextTextMaxBytes)
	}
	return msg
}
