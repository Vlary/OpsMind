// revoke.go 是 HTTP 层的吊销编排（契约 12.3 第 12/13/15/22 条）。
//
// 数据库侧的收敛（审批/grant/batch/tool_call）在调用方业务事务内经
// internal/rbacops 完成；本文件负责事务之外、进程内资源的收敛：
// 已建立的终端/RDP WebSocket、SSE 流，以及仍在等待审批的发起方。
package httpapi

import (
	"context"
	"time"

	"aiops/internal/rbacops"
	"aiops/internal/store"
)

// revokePendingInTx 在同一事务内收敛目标用户的遗留授权面。
func revokePendingInTx(ctx context.Context, tx store.Store, userID string, now time.Time) error {
	return rbacops.RevokePending(ctx, tx, userID, now)
}

// revokeAfterCommit 处理不属于数据库事务的收敛：断开该用户已建立的
// 终端/RDP WebSocket（按用户）与 SSE 流，并唤醒仍在等待审批的发起方。
func (d *Deps) revokeAfterCommit(ctx context.Context, userID string) {
	d.revokeLiveInteractive(ctx, userID)
	if d.Streams != nil {
		d.Streams.CancelByUser(userID)
	}
	if d.Approvals != nil {
		d.Approvals.NotifyRevoked(ctx, userID)
	}
}

// closeInteractiveByAuthSession 关闭某个 auth session 名下的全部交互连接
// （logout 专用：只断开当前这次登录建立的 shell/SSE，不影响该用户其它
// 设备上的会话）。
func (d *Deps) closeInteractiveByAuthSession(ctx context.Context, authSessionID string) {
	if authSessionID == "" {
		return
	}
	if d.Term != nil {
		d.Term.CloseByAuthSession(ctx, authSessionID, "client_closed")
	}
	if d.RDP != nil {
		d.RDP.CloseByAuthSession(ctx, authSessionID, "client_closed")
	}
	if d.Streams != nil {
		d.Streams.CancelByAuthSession(authSessionID)
	}
}
