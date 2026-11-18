package auth

import (
	"context"
	"time"

	"aiops/internal/audit"
	"aiops/internal/store"
)

// emitAuditTx 在业务事务内写 audit_outbox（契约 12.3 第 19 条）。
//
// 返回 false 表示存储未实现 outbox（内存 fake/骨架）：调用方应在事务
// 提交后回落到进程内异步 audit.Enqueue，行为与旧路径一致。返回 true 时
// 审计已与改密/启用等业务行同提交，客户端断开或进程崩溃都不会丢。
func emitAuditTx(ctx context.Context, tx store.Store, e audit.Event, now time.Time) (bool, error) {
	outbox := store.OutboxOf(tx)
	if outbox == nil {
		return false, nil
	}
	payload, err := audit.OutboxPayload(e, now)
	if err != nil {
		return true, err
	}
	if err := outbox.EnqueueAuditOutbox(ctx, payload, now); err != nil {
		return true, err
	}
	return true, nil
}
