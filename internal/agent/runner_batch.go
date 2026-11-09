// runner_batch.go 实现五期 M3 的 batch_execute AI 工具（契约 7.1/7.2/
// 2.10 约定 5）：对话是批量操作的主入口——「重启所有 web 服务器」→ 模型
// 调本工具 → batchx 逐台分类（safe+auto_exec 直跑、其余逐台 source=batch
// 审批）→ 本工具立即返回分派概要，不在 chat 流内等待审批（用户在 /batch
// 页或审批列表逐台 decide；进度走 GET /api/batch/jobs/:id 轮询）。
//
// agent 不 import batchx（契约 2.3 依赖表）：经 BatchDispatcher 接口由
// main 组装时注入（同 SetNetCLIExecutor 模式；G37 fake batchx 即实现此接口）。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"aiops/internal/store"
)

// BatchDispatcher 是批量编排引擎的注入接口（batchx.Service 实现；
// 方法集只需 Dispatch——概要计数由本包读 store 装配）。
type BatchDispatcher interface {
	Dispatch(ctx context.Context, p store.CreateBatchJobParams) (store.BatchJob, error)
}

// batchExecuteMaxTargets 是单次批量目标上限（契约 7.1/11 节写死 50）。
const batchExecuteMaxTargets = 50

// SetBatchExecutor 注入批量编排引擎（main 组装时调用；nil 表示功能未装配，
// 工具调用返回 INTERNAL）。
func (r *Runner) SetBatchExecutor(b BatchDispatcher) { r.batch = b }

// runBatchExecute 实现 batch_execute：校验参数 → Dispatch → 读回逐台
// results 装配状态计数概要（契约 7.1 结果 schema）。
func (r *Runner) runBatchExecute(ctx context.Context, conv store.Conversation, rec store.ToolCall, in RunInput, raw json.RawMessage) toolOutcome {
	var args batchExecuteArgs
	if err := decodeStrictArgs(raw, &args); err != nil {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid tool arguments"}, Status: store.ToolFailed}
	}
	if len(args.ServerIDs) == 0 {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "server_ids is required"}, Status: store.ToolFailed}
	}
	if len(args.ServerIDs) > batchExecuteMaxTargets {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "server_ids exceeds 50 targets"}, Status: store.ToolFailed}
	}
	if args.Command == "" {
		return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "command is required"}, Status: store.ToolFailed}
	}
	if len(args.Command) > MaxCommandBytes {
		return toolOutcome{Err: &ToolError{Code: codeCommandInvalid, Message: "command is invalid or too large"}, Status: store.ToolFailed}
	}
	if r.batch == nil {
		return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "batch executor is not configured"}, Status: store.ToolFailed}
	}

	job, err := r.batch.Dispatch(ctx, store.CreateBatchJobParams{
		Description:    args.Description,
		Command:        args.Command,
		ServerIDs:      args.ServerIDs,
		Source:         store.BatchSourceChat,
		ConversationID: &conv.ID,
		CreatedBy:      &in.UserID,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalid):
			return toolOutcome{Err: &ToolError{Code: codeToolArgsInvalid, Message: "invalid batch arguments"}, Status: store.ToolFailed}
		default:
			return toolOutcome{Err: &ToolError{Code: codeInternal, Message: "batch dispatch failed"}, Status: store.ToolFailed}
		}
	}

	// 读回逐台结果装配计数概要（≤50 行有界）；读取失败时退回 job 摘要。
	summary := map[string]any{
		"batch_job_id": job.ID,
		"status":       string(job.Status),
		"total":        job.TotalCount,
	}
	if _, results, err := r.st.BatchJobs().Get(ctx, job.ID); err == nil {
		counts := map[string]int{}
		failed := []string{}
		for _, res := range results {
			counts[string(res.Status)]++
			if res.Status == store.BatchResultFailed && len(failed) < 10 {
				// 失败原因摘要（任务：成功 N 台/失败 M 台+失败原因摘要）。
				failed = append(failed, res.ServerName+"("+res.ErrorCode+")")
			}
		}
		for status, n := range counts {
			summary[status] = n
		}
		if len(failed) > 0 {
			summary["failed_servers"] = failed
		}
	}
	summary["note"] = "已创建 " + itoa(summary["awaiting_approval"]) + " 条审批，等待用户在批量页或审批列表决定；进度可用 GET /api/batch/jobs/" + job.ID + " 查询"

	payload, err := json.Marshal(summary)
	if err != nil {
		return toolOutcome{Fatal: err}
	}
	return toolOutcome{Result: payload, Status: store.ToolSucceeded}
}

func itoa(v any) string {
	if n, ok := v.(int); ok {
		return strconv.Itoa(n)
	}
	return "0"
}
