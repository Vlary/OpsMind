// Package rbacops 是账号授权面收敛的共享实现：停用/降级/吊销会话/重置密码
// 与主机侧离线恢复（aiops admin reset）都必须走同一套收敛逻辑（契约 12.3
// 第 12/15/22 条）。
//
// 包内只依赖 store 与 audit，不 import httpapi/cmd：HTTP handler 在自己的
// 业务事务内调用，CLI 子命令在自己的事务内调用，保证「用户行 + 会话 +
// 审批 + grant + batch 结果 + 审计」同提交或同回滚。
//
// 走查第 3 轮 high：收敛动作此前只 UPDATE 不写审计，审计页只有
// approval.created / terminal.grant_requested，审计员无法证明这些授权是
// 被系统作废还是丢失。现在按受影响行在同一事务内写 audit_outbox。
package rbacops

import (
	"context"
	"time"

	"aiops/internal/audit"
	"aiops/internal/store"
)

// NoteUserRevoked 是账号吊销收敛写入 approvals.note 的原因标记。
const NoteUserRevoked = "rbac:user_revoked"

// 收敛动作名（契约 9.7；前端审计动作筛选已登记 approval.expired 与
// terminal.grant_expired）。
const (
	ActionApprovalExpired = "approval.expired"
	ActionGrantExpired    = "terminal.grant_expired"
)

// auditSink 是收敛审计的写入方式：
//
//   - outboxSink：业务事务内写 audit_outbox，由独立 worker 追加哈希链
//     （HTTP 服务进程路径；与业务行同提交）；
//   - directSink：事务内直接追加 audit_logs 哈希链（主机侧离线 CLI 路径，
//     进程无常驻 worker，命令返回时审计必须已可查——契约 12.3 第 19 条）。
type auditSink interface {
	emit(ctx context.Context, st store.Store, ev audit.Event, now time.Time) error
}

type outboxSink struct{}

func (outboxSink) emit(ctx context.Context, st store.Store, ev audit.Event, now time.Time) error {
	outbox := store.OutboxOf(st)
	if outbox == nil {
		// 未实现 outbox 的存储（内存 fake/骨架）：调用方在事务提交后由
		// audit.Writer 补写，这里跳过不影响业务收敛。
		return nil
	}
	payload, err := audit.OutboxPayload(ev, now)
	if err != nil {
		return err
	}
	return outbox.EnqueueAuditOutbox(ctx, payload, now)
}

type directSink struct{}

func (directSink) emit(ctx context.Context, st store.Store, ev audit.Event, now time.Time) error {
	if st.AuditLogs() == nil {
		return nil
	}
	return audit.AppendOnce(ctx, st, ev, now)
}

// RevokePending 在同一事务内收敛目标用户遗留的授权面：
//
//   - 该用户全部 pending 审批 → expired（note=rbac:user_revoked），关联的
//     awaiting_approval tool_call → expired/APPROVAL_CANCELLED；
//   - 该用户已批准但未消费的 terminal grant → expired（re-enable 后旧批准
//     不得再开会话）；
//   - 该用户创建的 batch job 中 awaiting_approval 的台 → cancelled 并重算
//     job 状态；
//   - 每个被作废的审批/授权各写一行审计（actor=system，detail 带
//     reason=rbac:user_revoked 与审批/命令/grant 信息），与业务行同事务。
//
// 审计走 audit_outbox（HTTP 服务进程有常驻 worker）；主机侧离线 CLI 用
// RevokePendingDirectAudit（无常驻 worker，需在事务内直接追加哈希链）。
//
// 未实现可选扩展的存储（内存 fake/骨架）按能力降级：至少收敛 pending 的
// terminal grant，其余由各自的过期扫描兜底。
func RevokePending(ctx context.Context, st store.Store, userID string, now time.Time) error {
	return revokePending(ctx, st, userID, now, outboxSink{})
}

// RevokePendingDirectAudit 与 RevokePending 同语义，但审计在事务内直接追加
// audit_logs 哈希链（供主机侧 aiops admin reset 使用：CLI 没有常驻 worker，
// 写 outbox 会让收敛审计在审计页缺席到下次启动）。
func RevokePendingDirectAudit(ctx context.Context, st store.Store, userID string, now time.Time) error {
	return revokePending(ctx, st, userID, now, directSink{})
}

func revokePending(ctx context.Context, st store.Store, userID string, now time.Time, sink auditSink) error {
	if st == nil || userID == "" {
		return nil
	}
	if revs := store.RevokeStoreOf(st); revs != nil {
		expiredApprovals, err := revs.ExpirePendingByUser(ctx, userID, NoteUserRevoked, now)
		if err != nil {
			return err
		}
		// 按来源路由审计动作：终端/RDP 授权统一记 terminal.grant_expired
		//（与批准侧的 terminal.grant_approved / 消费侧的
		// terminal.grant_consumed 同一生命周期），其余审批记
		// approval.expired。两类事件各自完整，不重复记账。
		terminalPending, otherPending := splitTerminal(expiredApprovals)
		if err := emitApprovalExpired(ctx, st, otherPending, NoteUserRevoked, now, sink); err != nil {
			return err
		}
		if err := emitGrantExpired(ctx, st, terminalPending, NoteUserRevoked, now, sink); err != nil {
			return err
		}
		expiredGrants, err := revs.ExpireApprovedGrantsByUser(ctx, userID, NoteUserRevoked, now)
		if err != nil {
			return err
		}
		if err := emitGrantExpired(ctx, st, expiredGrants, NoteUserRevoked, now, sink); err != nil {
			return err
		}
	} else if grants := store.GrantStoreOf(st); grants != nil {
		expiredGrants, err := grants.ExpirePendingGrantsByUser(ctx, userID, now)
		if err != nil {
			return err
		}
		if err := emitGrantExpired(ctx, st, expiredGrants, NoteUserRevoked, now, sink); err != nil {
			return err
		}
	}
	if batch := store.BatchRevokeOf(st); batch != nil {
		if _, err := batch.CancelAwaitingByUser(ctx, userID, now); err != nil {
			return err
		}
	}
	return nil
}

// RevokeSessionsAndAuthorizations 在调用方事务内吊销该用户全部 auth session
// 并收敛授权面（供离线 admin reset 使用；HTTP 路径已有同样的语句）。
//
// 离线 CLI 无常驻 worker，因此收敛审计直接追加哈希链（契约 12.3 第 19 条
// 「命令返回时审计已可查」）。
func RevokeSessionsAndAuthorizations(ctx context.Context, st store.Store, userID string, now time.Time) error {
	if err := st.Sessions().DeleteByUser(ctx, userID); err != nil {
		return err
	}
	return RevokePendingDirectAudit(ctx, st, userID, now)
}

// splitTerminal 按 source 拆分「终端/RDP 授权」与其它审批（审计动作不同）。
func splitTerminal(rows []store.Approval) (terminal, other []store.Approval) {
	for _, ap := range rows {
		if ap.Source == "terminal" {
			terminal = append(terminal, ap)
			continue
		}
		other = append(other, ap)
	}
	return terminal, other
}

// emitApprovalExpired 为每条被系统作废的审批写一行 approval.expired。
// 写入方式由 sink 决定（outbox 或直接追加哈希链）。
func emitApprovalExpired(ctx context.Context, st store.Store, rows []store.Approval, reason string, now time.Time, sink auditSink) error {
	for _, ap := range rows {
		ev := audit.Event{
			ActorType:      "system",
			ActorID:        "system",
			Action:         ActionApprovalExpired,
			ResourceType:   "approval",
			ResourceID:     ap.ID,
			ConversationID: ap.ConversationID,
			ServerID:       ap.ServerID,
			Detail: map[string]any{
				"approval_id":   ap.ID,
				"reason":        reason,
				"source":        ap.Source,
				"command":       ap.Command,
				"risk_level":    ap.RiskLevel,
				"requested_by":  ap.RequestedBy,
				"approval_mode": string(ap.ApprovalMode),
				"server_name":   ap.ServerName,
			},
		}
		if err := sink.emit(ctx, st, ev, now); err != nil {
			return err
		}
	}
	return nil
}

// emitGrantExpired 为每条被系统作废的终端/RDP 授权写一行
// terminal.grant_expired（契约 9.7 动作名）。
func emitGrantExpired(ctx context.Context, st store.Store, rows []store.Approval, reason string, now time.Time, sink auditSink) error {
	for _, ap := range rows {
		ev := audit.Event{
			ActorType:      "system",
			ActorID:        "system",
			Action:         ActionGrantExpired,
			ResourceType:   "approval",
			ResourceID:     ap.ID,
			ConversationID: ap.ConversationID,
			ServerID:       ap.ServerID,
			Detail: map[string]any{
				"approval_id":  ap.ID,
				"reason":       reason,
				"grant_kind":   derefOrEmpty(ap.GrantKind),
				"server_id":    derefOrEmpty(ap.ServerID),
				"requested_by": ap.RequestedBy,
			},
		}
		if ap.GrantExpiresAt != nil {
			ev.Detail["grant_expires_at"] = ap.GrantExpiresAt.UTC().Format(time.RFC3339)
		}
		if err := sink.emit(ctx, st, ev, now); err != nil {
			return err
		}
	}
	return nil
}

func derefOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
