// policy.go 是六期 BE-P 审批门限统一策略的唯一实现（CONTRACT.md 2.11
// 约定 1/2、5.5）：四档矩阵 NeedsApproval 与有效门限解析 EffectiveMode。
// agent（ssh_exec/container_action）、batchx（逐台分派）、REST 容器
// action、诊断 fix 必须调用本函数，禁止内联重写。
package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"aiops/internal/risk"
	"aiops/internal/store"
)

// approvalModeSettingKey 是全局审批门限的 settings 键（契约 4.11/00008）。
const approvalModeSettingKey = "approval.mode"

// ErrInvalid 表示 decide 请求本身非法（deny 携带 edited_command、改写
// 超长/含 NUL、审批无 server_id）——httpapi 映射 400 VALIDATION_FAILED。
var ErrInvalid = errors.New("approvals: invalid decide request")

// ErrForbidden 表示改写后命令重新分类为 forbidden——httpapi 映射 403
// COMMAND_FORBIDDEN，审批保持 pending（契约 2.11 约定 3）。
var ErrForbidden = errors.New("approvals: edited command forbidden")

// EditedCommandMaxRunes 是 decide 改写命令的长度上限（契约 2.11 约定 3，
// DB CHECK length 0..2000 双重校验）。
const EditedCommandMaxRunes = 2000

// PolicyInput 是统一审批门限策略的输入（契约 5.5）。
//
// level=forbidden 的调用是契约违规（forbidden 不进审批，调用方必须先拒），
// NeedsApproval 按「审批」处理（保守方向）。severity 只在
// level==needs_approval 时参与判定（risk.SeverityOf 结果）。
type PolicyInput struct {
	Mode            store.ApprovalMode
	AutoExecEnabled bool
	Level           risk.Level
	Severity        risk.Severity // level==needs_approval 时必填（SeverityOf 结果）
}

// NeedsApproval 实现契约 2.11 约定 2 的写死矩阵：
//
//	条件                                | all | standard | high | none
//	safe, auto_exec=true                | 审批 | 直执行    | 直执行 | 直执行
//	safe, auto_exec=false               | 审批 | 审批      | 直执行 | 直执行
//	needs_approval(warning), auto=true  | 审批 | 审批      | 直执行 | 直执行
//	needs_approval(warning), auto=false | 审批 | 审批      | 审批   | 审批
//	needs_approval(high),   auto=true   | 审批 | 审批      | 审批   | 直执行
//	needs_approval(high),   auto=false  | 审批 | 审批      | 审批   | 审批
//
// standard 列=一至五期现行行为（runner.go/runner_container.go 的内联判定）。
// 服务器级硬保护：auto_exec_enabled=false 时非只读命令（needs_approval）
// 无论门限一律审批。门限单调性：all ≥ standard ≥ high ≥ none（审批面只缩
// 不涨）。未知门限值按 standard 保守处理。
func NeedsApproval(in PolicyInput) bool {
	if in.Level == risk.LevelForbidden {
		// forbidden 不进审批表（调用方必须先拒）；误入本函数按「审批」
		// 保守处理（契约 5.5 PolicyInput 注释）。
		return true
	}
	switch in.Mode {
	case store.ApprovalModeAll:
		// 全部命令审批（含只读）。
		return true
	case store.ApprovalModeHigh:
		if in.Level == risk.LevelSafe {
			// high 档：只读直执行（auto_exec 不约束只读——门限已高于开关）。
			return false
		}
		if in.Severity == risk.SeverityWarning && in.AutoExecEnabled {
			// warning 免审，但受服务器 auto_exec 硬保护。
			return false
		}
		return true
	case store.ApprovalModeNone:
		if in.Level == risk.LevelSafe {
			return false
		}
		// none 档：全部自动执行；auto_exec=false 仍是非只读的硬保护闸。
		return !in.AutoExecEnabled
	default:
		// standard（现行行为）：safe 且 auto_exec 直执行；safe 未开
		// auto_exec、needs_approval 一律审批。
		if in.Level == risk.LevelSafe && in.AutoExecEnabled {
			return false
		}
		return true
	}
}

// GlobalMode 读取全局审批门限 settings["approval.mode"]（JSON 字符串）；
// 键缺失/值非法一律兜底 standard（契约 2.11 约定 1——防手工删键/写坏值）。
// 每 turn/每请求至多调用一次并透传（契约 11 节第 18 条）。
func GlobalMode(ctx context.Context, st store.Store) (store.ApprovalMode, error) {
	if st == nil || st.Settings() == nil {
		// store 未接线 settings（骨架/部分单测）时兜底 standard。
		return store.ApprovalModeStandard, nil
	}
	setting, err := st.Settings().Get(ctx, approvalModeSettingKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.ApprovalModeStandard, nil
		}
		return store.ApprovalModeStandard, err
	}
	var raw string
	if err := json.Unmarshal(setting.Value, &raw); err != nil {
		return store.ApprovalModeStandard, nil
	}
	mode := store.ApprovalMode(strings.TrimSpace(raw))
	if !store.ValidApprovalMode(mode) {
		return store.ApprovalModeStandard, nil
	}
	return mode, nil
}

// ResolveMode 在已知全局门限下解析单会话的有效门限：覆盖非 nil 取覆盖，
// 否则取全局（契约 2.11 约定 1）。供 handler 装配
// Conversation.EffectiveApprovalMode 时对一批会话复用一次全局读。
func ResolveMode(conv store.Conversation, global store.ApprovalMode) store.ApprovalMode {
	if conv.ApprovalMode != nil && *conv.ApprovalMode != "" {
		if store.ValidApprovalMode(*conv.ApprovalMode) {
			return *conv.ApprovalMode
		}
	}
	if global == "" || !store.ValidApprovalMode(global) {
		return store.ApprovalModeStandard
	}
	return global
}

// EffectiveMode 解析有效门限：conv 覆盖非 nil 取覆盖；否则读
// settings["approval.mode"]，键缺失/值非法一律兜底 standard。httpapi/
// agent/batchx 在 turn/请求开始时调用一次并透传（契约 5.5）。
func EffectiveMode(ctx context.Context, st store.Store, conv store.Conversation) (store.ApprovalMode, error) {
	if conv.ApprovalMode != nil && *conv.ApprovalMode != "" {
		if store.ValidApprovalMode(*conv.ApprovalMode) {
			return *conv.ApprovalMode, nil
		}
	}
	return GlobalMode(ctx, st)
}
