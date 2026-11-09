// runner_diagnosis.go 实现五期 M2 的三个 AI 工具（契约 7.1/7.2/G37）：
//   - run_diagnosis        发起诊断会话并等待报告（轮询至终态或 120s 上限）
//   - run_inspection       立即巡检（1..50 台，basic/full）并返回评分摘要
//   - get_inspection_report 查询报告（report_id 或最新 limit 份）
//
// agent 不 import diagnosis/inspection（契约 2.3 包边界白名单）：经
// AttachPhase5 注入的最小接口调用（main 组装时传引擎/管理器实例）。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"aiops/internal/store"
)

// DiagnosisStarter 是诊断引擎的最小接口（diagnosis.Engine 满足）。
type DiagnosisStarter interface {
	Create(ctx context.Context, p store.CreateDiagnosisParams) (store.DiagnosisSession, error)
}

// InspectionRunner 是巡检执行器的最小接口（inspection.Manager 满足）。
type InspectionRunner interface {
	Run(ctx context.Context, jobID *string, trigger store.InspectionTrigger,
		serverIDs []string, template store.InspectionTemplate) (store.InspectionReport, error)
}

// 工具侧写死的上限（契约 7.2：run_diagnosis 工具侧 120s 轮询上限；
// run_inspection 目标 1..50；get_inspection_report limit ≤20）。
const (
	diagnosisToolWaitTimeout = 120 * time.Second
	diagnosisPollInterval    = time.Second
	inspectionToolMaxTargets = 50
	inspectionReportMaxLimit = 20
)

// AttachPhase5 注入五期 M2 引擎（main 组装：runner.AttachPhase5(diagEngine, inspectMgr)）。
// 未注入时 run_diagnosis/run_inspection 返回工具错误（get_inspection_report
// 为纯 store 查询，不依赖注入）。
func (r *Runner) AttachPhase5(diagnosis DiagnosisStarter, inspection InspectionRunner) {
	r.phase5Diagnosis = diagnosis
	r.phase5Inspection = inspection
}

// diagnosisReportView 是报告 JSON 的透传视图（report 字段原样回给模型）。
type diagnosisReportView struct {
	Summary     string          `json:"summary"`
	Severity    string          `json:"severity"`
	RootCause   string          `json:"root_cause"`
	Evidence    json.RawMessage `json:"evidence"`
	Suggestions json.RawMessage `json:"suggestions"`
	Confidence  string          `json:"confidence"`
}

// runRunDiagnosis 实现 run_diagnosis 工具（契约 7.2：恒 safe、无审批——
// 工具本身只创建会话并等待；引擎内部的 safe 闸是唯一执行闸）。
func (r *Runner) runRunDiagnosis(ctx context.Context, conv store.Conversation, raw json.RawMessage) toolOutcome {
	if r.phase5Diagnosis == nil {
		return toolOutcome{Err: &ToolError{Code: "SERVICE_UNAVAILABLE", Message: "diagnosis engine not attached"}, Status: store.ToolFailed}
	}
	var args runDiagnosisArgs
	if err := decodeStrictArgs(raw, &args); err != nil {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid tool arguments"}, Status: store.ToolFailed}
	}
	if args.ServerID == "" {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "server_id is required"}, Status: store.ToolFailed}
	}
	convID := conv.ID
	params := store.CreateDiagnosisParams{
		ServerID:       args.ServerID,
		ConversationID: &convID,
		Trigger:        store.DiagnosisTriggerChat,
	}
	if args.AlertEventID != nil {
		params.AlertEventID = args.AlertEventID
	}
	sess, err := r.phase5Diagnosis.Create(ctx, params)
	if err != nil {
		if err.Error() == errDiagnosisBusyMessage {
			// diagnosis.ErrBusy 的文本在契约 5.16 逐字写死（agent 不 import
			// diagnosis，按哨兵文本比对）。
			return toolOutcome{Err: &ToolError{Code: "DIAGNOSIS_BUSY", Message: "diagnosis queue limit reached"}, Status: store.ToolFailed}
		}
		if errors.Is(err, store.ErrNotFound) {
			return toolOutcome{Err: &ToolError{Code: "SERVER_NOT_FOUND", Message: "server not found"}, Status: store.ToolFailed}
		}
		return toolOutcome{Fatal: err}
	}

	// 轮询至终态或 120s（契约 7.2；每秒一次，共 ≤120 次——终止条件）。
	deadline := time.Now().Add(diagnosisToolWaitTimeout)
	for {
		cur, err := r.st.DiagnosisSessions().Get(ctx, sess.ID)
		if err != nil {
			return toolOutcome{Fatal: err}
		}
		if isDiagnosisTerminal(cur.Status) {
			return diagnosisToolResult(cur)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return diagnosisToolResult(cur) // 超时返回当前状态（契约 7.2）
		}
		select {
		case <-ctx.Done():
			return diagnosisToolResult(cur)
		case <-time.After(diagnosisPollInterval):
		}
	}
}

// errDiagnosisBusyMessage 是 diagnosis.ErrBusy 的哨兵文本（契约 5.16 逐字
// 写死；agent 不 import diagnosis，按文本比对）。
const errDiagnosisBusyMessage = "diagnosis: queue limit reached"

func isDiagnosisTerminal(s store.DiagnosisStatus) bool {
	switch s {
	case store.DiagnosisAwaitingFix, store.DiagnosisDone, store.DiagnosisExpired, store.DiagnosisFailed:
		return true
	}
	return false
}

// diagnosisToolResult 构造 7.1 契约的返回 JSON。
func diagnosisToolResult(sess store.DiagnosisSession) toolOutcome {
	out := map[string]any{
		"diagnosis_id":     sess.ID,
		"server_id":        sess.ServerID,
		"server_name":      sess.ServerName,
		"status":           string(sess.Status),
		"evidence_count":   0,
		"suggestion_count": 0,
	}
	if len(sess.Report) > 0 {
		var rep diagnosisReportView
		if err := json.Unmarshal(sess.Report, &rep); err == nil {
			out["summary"] = rep.Summary
			out["severity"] = rep.Severity
			out["report"] = sess.Report
			if ev := countJSONArray(rep.Evidence); ev >= 0 {
				out["evidence_count"] = ev
			}
			if sg := countJSONArray(rep.Suggestions); sg >= 0 {
				out["suggestion_count"] = sg
			}
		}
	}
	if sess.ErrorCode != "" {
		out["error_code"] = sess.ErrorCode
	}
	res, _ := json.Marshal(out)
	return toolOutcome{Result: res}
}

func countJSONArray(raw json.RawMessage) int {
	if len(raw) == 0 {
		return -1
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return -1
	}
	return len(arr)
}

// runRunInspection 实现 run_inspection 工具（契约 7.2：目标 1..50，超限
// TOOL_ARGS_INVALID；巡检不用 LLM，同步返回评分摘要）。
func (r *Runner) runRunInspection(ctx context.Context, raw json.RawMessage) toolOutcome {
	if r.phase5Inspection == nil {
		return toolOutcome{Err: &ToolError{Code: "SERVICE_UNAVAILABLE", Message: "inspection manager not attached"}, Status: store.ToolFailed}
	}
	var args runInspectionArgs
	if err := decodeStrictArgs(raw, &args); err != nil {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid tool arguments"}, Status: store.ToolFailed}
	}
	if len(args.ServerIDs) < 1 || len(args.ServerIDs) > inspectionToolMaxTargets {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "server_ids must contain 1..50 items"}, Status: store.ToolFailed}
	}
	template := store.InspectionBasic
	if args.Template != nil && *args.Template != "" {
		switch store.InspectionTemplate(*args.Template) {
		case store.InspectionBasic, store.InspectionFull:
			template = store.InspectionTemplate(*args.Template)
		default:
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "template must be basic or full"}, Status: store.ToolFailed}
		}
	}
	report, err := r.phase5Inspection.Run(ctx, nil, store.InspectionTriggerChat, args.ServerIDs, template)
	if err != nil {
		return toolOutcome{Fatal: err}
	}
	// items 摘要（server_name/score/anomalies/duration——契约 7.1 返回例）。
	items := []map[string]any{}
	if _, list, gerr := r.st.InspectionReports().Get(ctx, report.ID); gerr == nil {
		for _, it := range list {
			items = append(items, map[string]any{
				"server_id":     it.ServerID,
				"server_name":   it.ServerName,
				"score":         it.Score,
				"anomaly_count": it.AnomalyCount,
				"anomalies":     it.Anomalies,
				"duration_ms":   it.DurationMS,
			})
		}
	}
	res, _ := json.Marshal(map[string]any{
		"report_id":     report.ID,
		"trigger":       string(report.Trigger),
		"template":      string(template),
		"server_count":  report.ServerCount,
		"avg_score":     report.AvgScore,
		"anomaly_count": report.AnomalyCount,
		"items":         items,
	})
	return toolOutcome{Result: res}
}

// runGetInspectionReport 实现 get_inspection_report 工具（report_id 与
// limit 二选一，都给以 report_id 为准——契约 7.1）。
func (r *Runner) runGetInspectionReport(ctx context.Context, raw json.RawMessage) toolOutcome {
	var args getInspectionReportArgs
	if err := decodeStrictArgs(raw, &args); err != nil {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid tool arguments"}, Status: store.ToolFailed}
	}
	if args.ReportID != nil && *args.ReportID != "" {
		report, items, err := r.st.InspectionReports().Get(ctx, *args.ReportID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return toolOutcome{Err: &ToolError{Code: "INSPECTION_REPORT_NOT_FOUND", Message: "report not found"}, Status: store.ToolFailed}
			}
			return toolOutcome{Fatal: err}
		}
		prev := map[string]int{}
		if report.JobID != nil {
			if m, err := r.st.InspectionReports().PreviousScores(ctx, *report.JobID, report.StartedAt); err == nil {
				prev = m
			}
		}
		outItems := make([]map[string]any, 0, len(items))
		for _, it := range items {
			outItems = append(outItems, map[string]any{
				"server_id": it.ServerID, "server_name": it.ServerName,
				"score": it.Score, "anomaly_count": it.AnomalyCount,
				"anomalies": it.Anomalies, "raw_output_truncated": it.Truncated,
				"previous_score": prev[it.ServerID],
			})
		}
		res, _ := json.Marshal(map[string]any{
			"report": map[string]any{
				"report_id": report.ID, "job_id": report.JobID,
				"trigger": string(report.Trigger), "server_count": report.ServerCount,
				"avg_score": report.AvgScore, "anomaly_count": report.AnomalyCount,
				"started_at": report.StartedAt, "finished_at": report.FinishedAt,
			},
			"items": outItems,
		})
		return toolOutcome{Result: res}
	}
	limit := 5
	if args.Limit != nil {
		if *args.Limit < 1 || *args.Limit > inspectionReportMaxLimit {
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "limit must be between 1 and 20"}, Status: store.ToolFailed}
		}
		limit = *args.Limit
	}
	reports, err := r.st.InspectionReports().List(ctx, store.InspectionReportFilter{Limit: limit})
	if err != nil {
		return toolOutcome{Fatal: err}
	}
	if reports == nil {
		reports = []store.InspectionReport{}
	}
	res, _ := json.Marshal(map[string]any{"reports": reports})
	return toolOutcome{Result: res}
}
