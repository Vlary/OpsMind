// Package offline 实现主机侧离线恢复子命令（契约 12.3 第 11/25 条 /
// 12.5 / README「单管理员」）：
//
//	aiops admin reset      重置指定管理员：置 must_change_password=true、
//	                       吊销其全部 auth session 并收敛其待处理授权面、
//	                       写 admin.offline_reset 审计
//	aiops break-glass      一次性批准一条 pending 审批：置 approved +
//	                       break_glass=true、写 admin.break_glass 审计
//
// 这是唯一绕过 HTTP 鉴权的恢复路径，因此只依赖主机文件系统/数据库权限
// （AIOPS_DATABASE_URL），不监听端口、不经过任何网络身份。审计动作按契约
// actor_type=system，detail 含 reason/approval_id/host_user/tty。
package offline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"strings"
	"time"

	"aiops/internal/audit"
	"aiops/internal/rbacops"
	"aiops/internal/store"
)

// 审计动作名（契约 9.7）。
const (
	ActionOfflineReset = "admin.offline_reset"
	ActionBreakGlass   = "admin.break_glass"
)

// AuditDetail 返回离线操作的审计 detail（契约 9.7：reason/approval_id?/
// host_user/tty）。
func AuditDetail(reason, approvalID string) map[string]any {
	detail := map[string]any{
		"reason":    reason,
		"host_user": hostUser(),
		"tty":       tty(),
	}
	if approvalID != "" {
		detail["approval_id"] = approvalID
	}
	return detail
}

// ResetAdmin 执行 `aiops admin reset --user <username>`。
//
// 语义：置 must_change_password=true、吊销全部 auth session、收敛该用户
// 的 pending 审批 / awaiting_approval tool_call / 未消费 grant / batch 台，
// 并在同一事务内写 admin.offline_reset 审计（outbox）。
func ResetAdmin(ctx context.Context, st store.Store, username, reason string, now time.Time) (store.User, error) {
	if strings.TrimSpace(username) == "" {
		return store.User{}, errors.New("offline: username is required")
	}
	u, err := st.Users().ByUsername(ctx, username)
	if err != nil {
		return store.User{}, fmt.Errorf("offline: load user %q: %w", username, err)
	}
	event := audit.Event{
		ActorType:    "system",
		ActorID:      "system",
		Action:       ActionOfflineReset,
		ResourceType: "user",
		ResourceID:   u.ID,
		Detail:       AuditDetail(reason, ""),
	}
	err = st.WithTx(ctx, func(tx store.Store) error {
		if err := tx.Users().SetMustChangePassword(ctx, u.ID, true, now); err != nil {
			return err
		}
		if err := rbacops.RevokeSessionsAndAuthorizations(ctx, tx, u.ID, now); err != nil {
			return err
		}
		return enqueueAudit(ctx, tx, event, now)
	})
	if err != nil {
		return store.User{}, err
	}
	u.MustChangePassword = true
	return u, nil
}

// BreakGlassApprove 执行
// `aiops break-glass --action approve --approval-id <id> --reason <r> --confirm`。
//
// 只允许 --action approve（v1 不做离线拒绝）；审批必须仍为 pending。
// 批准与 admin.break_glass 审计同事务提交。
func BreakGlassApprove(ctx context.Context, st store.Store, approvalID, reason string, now time.Time) (store.Approval, error) {
	if strings.TrimSpace(approvalID) == "" {
		return store.Approval{}, errors.New("offline: approval id is required")
	}
	if strings.TrimSpace(reason) == "" {
		return store.Approval{}, errors.New("offline: reason is required")
	}
	bg := store.BreakGlassStoreOf(st)
	if bg == nil {
		return store.Approval{}, errors.New("offline: store does not support break-glass")
	}
	var ap store.Approval
	err := st.WithTx(ctx, func(tx store.Store) error {
		txBG := store.BreakGlassStoreOf(tx)
		if txBG == nil {
			return errors.New("offline: transaction does not support break-glass")
		}
		approved, err := txBG.ApproveBreakGlass(ctx, approvalID, reason, now)
		if err != nil {
			return err
		}
		ap = approved
		return enqueueAudit(ctx, tx, audit.Event{
			ActorType:      "system",
			ActorID:        "system",
			Action:         ActionBreakGlass,
			ResourceType:   "approval",
			ResourceID:     approved.ID,
			ConversationID: approved.ConversationID,
			ServerID:       approved.ServerID,
			Detail:         AuditDetail(reason, approved.ID),
		}, now)
	})
	if err != nil {
		return store.Approval{}, err
	}
	return ap, nil
}

// enqueueAudit 在业务事务内把离线操作审计直接追加到 audit_logs 哈希链
// （audit.AppendOnce）。
//
// 不能用 audit_outbox：离线 CLI 不启动 worker，写 outbox 会让审计在
// audit_logs 里一直缺席——评审实测正是「执行后 audit_logs 计数为 0」。
// 直接追加仍取链锁、读链尾、按库内规范形态复算，链保持连续。
func enqueueAudit(ctx context.Context, tx store.Store, e audit.Event, now time.Time) error {
	return audit.AppendOnce(ctx, tx, e, now)
}

// hostUser 返回执行命令的 OS 用户（审计证据）。
func hostUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// tty 返回执行命令的终端设备；cron/容器内为空串。
func tty() string {
	for _, fd := range []string{"/dev/stdin", "/dev/tty"} {
		if link, err := os.Readlink(fd); err == nil && link != "" {
			return link
		}
	}
	return ""
}

// NewLogger 返回离线子命令的最小 logger（单行 JSON，便于运维重定向）。
func NewLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
