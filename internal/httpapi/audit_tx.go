package httpapi

import (
	"context"

	"github.com/gin-gonic/gin"

	"aiops/internal/audit"
	"aiops/internal/store"
)

// auditInTx 在业务事务内写 audit_outbox（契约 12.3 第 19 条）：业务行与
// 审计载荷同提交，客户端断开/队列满都不会丢审计。
//
// 返回 false 表示该存储未实现 outbox（内存 fake/骨架）——调用方应在事务
// 提交后回落到进程内异步 audit.Enqueue，行为与旧路径一致。
func (d *Deps) auditInTx(ctx context.Context, tx store.Store, e audit.Event) (bool, error) {
	outbox := store.OutboxOf(tx)
	if outbox == nil {
		return false, nil
	}
	payload, err := audit.OutboxPayload(e, d.Clock())
	if err != nil {
		return true, err
	}
	if err := outbox.EnqueueAuditOutbox(ctx, payload, d.Clock()); err != nil {
		return true, err
	}
	return true, nil
}

// auditInTxCtx 是 auditInTx 的 gin 版本：在写 outbox 前补齐 IP 与 request_id
// （评审 medium：事务内审计此前只填业务字段，审计页「来源 IP」「request_id」
// 两列对用户管理事件恒为空，无法与请求日志关联）。
func (d *Deps) auditInTxCtx(c *gin.Context, tx store.Store, e audit.Event) (bool, error) {
	if e.RequestID == "" {
		e.RequestID = requestIDOf(c)
	}
	if e.IP == "" {
		e.IP = d.requestIP(c)
	}
	return d.auditInTx(c.Request.Context(), tx, e)
}

// revokeLiveInteractive 关闭该用户已建立的终端/RDP WebSocket（停用/降级/
// 重置密码/吊销会话/改密后必须立即断开，评审发现：此前只删 auth session，
// 已建立的交互 shell 仍可继续执行）。
func (d *Deps) revokeLiveInteractive(ctx context.Context, userID string) {
	if d.Term != nil {
		d.Term.CloseByUser(ctx, userID, "error")
	}
	if d.RDP != nil {
		d.RDP.CloseByUser(ctx, userID, "error")
	}
}
