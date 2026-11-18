package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrGrantUnavailable 表示没有可消费的 terminal grant（不存在、未批准、
// 已消费、已过期、发起人/服务器不匹配）。httpapi 映射 403
// TERMINAL_APPROVAL_REQUIRED（契约 9.6）。
var ErrGrantUnavailable = errors.New("store: terminal grant unavailable")

// ErrGrantKindMismatch 表示 grant 类型与端点不匹配（term grant 用于 RDP 或
// 反之）。httpapi 映射 403 TERMINAL_GRANT_KIND_MISMATCH。
var ErrGrantKindMismatch = errors.New("store: terminal grant kind mismatch")

// ConsumeGrantParams 是单次消费 terminal grant 的参数（契约 12.3 第 12 条）。
// 消费必须是原子 CAS：approved + unconsumed + unexpired + requested_by=用户
// + server_id 匹配 + grant_kind 匹配，全部条件在一条 UPDATE 内判定。
type ConsumeGrantParams struct {
	ApprovalID string
	UserID     string
	ServerID   string
	GrantKind  string // term|rdp
	Now        time.Time
}

// ListAvailableGrantsParams 是「本人可用 JIT 授权」查询参数（契约 12.5：
// 发起人在打开终端/桌面之前先看到已批准、未消费、未过期的 grant）。
// 只按 requested_by 过滤——他人授权任何角色都不可见（audit:read 也没有
// 列表入口：授权是个人凭据，不是审计对象）。
type ListAvailableGrantsParams struct {
	UserID string
	// GrantKind 为空表示 term + rdp 都返回；否则只返回指定类型。
	GrantKind string
	Now       time.Time
	Limit     int
}

// ReleaseGrantParams 是回滚一次终端授权消费的参数（第 4 轮走查 medium）。
//
// 场景：handler 先消费 grant 再开终端/RDP 会话；Open 失败（同服务器已有
// 活动会话、会话数上限、网关不可用等）时授权不应被白白烧掉。回滚必须
// 精确匹配本次消费写入的 consumed_at，避免释放其它请求/后续消费。
type ReleaseGrantParams struct {
	ApprovalID string
	UserID     string
	ConsumedAt time.Time
}

// GrantStore 是 ApprovalStore 的可选扩展（只在具备 grant 列的存储实现）。
// 调用方用接口断言探测；缺失实现时按 grant 不可用失败关闭。
type GrantStore interface {
	ConsumeGrant(ctx context.Context, p ConsumeGrantParams) (Approval, error)
	// ReleaseGrant 回滚一次刚发生的消费（仅当仍是本次消费的 consumed_at），
	// 返回释放后的行。不属于该用户或已被再次消费返回 ErrGrantUnavailable。
	ReleaseGrant(ctx context.Context, p ReleaseGrantParams) (Approval, error)
	// ExpirePendingGrantsByUser 把该用户所有 pending 的 terminal grant 置
	// expired 并返回受影响的行走（停用/改角色/吊销售后一致性，契约 12.3
	// 第 12 条；返回行供调用方在同一事务内逐条写审计）。
	ExpirePendingGrantsByUser(ctx context.Context, userID string, now time.Time) ([]Approval, error)
	// ListAvailableGrants 返回该用户 approved + unconsumed + unexpired 的
	// terminal grant（有界 limit，最多 200）。发起人页面的「你有 N 条可用
	// 授权」入口用它，避免用户盲目尝试开会话。
	ListAvailableGrants(ctx context.Context, p ListAvailableGrantsParams) ([]Approval, error)
}

// GrantExpiryStore 是「已批准但已过 grant_expires_at 的终端授权」的收敛
// 能力（走查第 3 轮 medium：过期事实必须落库并写 terminal.grant_expired
// 审计，否则审计页「终端授权过期」恒空、前端只能临时推算）。
//
// 有界：单次最多返回 limit 行（调用方传 200），sweeper 每秒复扫。
type GrantExpiryStore interface {
	ExpireStaleGrants(ctx context.Context, now time.Time, limit int) ([]Approval, error)
}

// GrantExpiryStoreOf 返回审批仓储的过期 grant 收敛能力；未实现返回 nil。
func GrantExpiryStoreOf(st Store) GrantExpiryStore {
	if st == nil || st.Approvals() == nil {
		return nil
	}
	g, _ := st.Approvals().(GrantExpiryStore)
	return g
}

// AuditVerifyStore 是审计链校验的分页读能力（id ASC，afterID 之后）。
// 独立于 AuditStore：验证必须从最早一行向后读，不能把全表装进内存
// （防 OOM 铁律：单页有硬上限，调用方逐页推进）。
type AuditVerifyStore interface {
	ListAuditAsc(ctx context.Context, afterID int64, limit int) ([]AuditLog, error)
}

// VerifyStoreOf 返回审计仓储的升序分页能力；未实现返回 nil。
func VerifyStoreOf(st Store) AuditVerifyStore {
	if st == nil || st.AuditLogs() == nil {
		return nil
	}
	v, _ := st.AuditLogs().(AuditVerifyStore)
	return v
}

// AuditOutboxEntry 是一条待追加哈希链的审计 outbox 行。
type AuditOutboxEntry struct {
	ID        int64
	Payload   json.RawMessage
	CreatedAt time.Time
	Attempts  int
}

// AuditOutboxStore 是状态变更审计的持久化通道（契约 12.3 第 19 条）。
//
// 业务事务内 EnqueueAuditOutbox（与业务同提交）；独立 worker 在
// WithTx 内 LockAuditChain → LastHash → Append → ProcessAuditOutbox，
// 串行追加哈希链。接口是 Store 的可选扩展（内存 fake 可不实现）。
type AuditOutboxStore interface {
	EnqueueAuditOutbox(ctx context.Context, payload []byte, at time.Time) error
	PendingAuditOutbox(ctx context.Context, limit int) ([]AuditOutboxEntry, error)
	// ProcessAuditOutbox 在调用方事务内标记已处理（毫秒级，不阻塞）。
	ProcessAuditOutbox(ctx context.Context, ids []int64, at time.Time) error
	// LockAuditChain 取 pg_advisory_xact_lock(hashtext('audit.chain'))。
	LockAuditChain(ctx context.Context) error
}

// OutboxOf 返回 store 的 outbox 能力；未实现返回 nil（调用方回落到进程内
// 异步审计，保证骨架与单测仍可运行）。
func OutboxOf(st Store) AuditOutboxStore {
	if st == nil {
		return nil
	}
	outbox, _ := st.(AuditOutboxStore)
	return outbox
}

// GrantStoreOf 返回审批仓储的 grant 能力；未实现返回 nil。
func GrantStoreOf(st Store) GrantStore {
	if st == nil || st.Approvals() == nil {
		return nil
	}
	g, _ := st.Approvals().(GrantStore)
	return g
}

// RevokeStore 是审批仓储的可选扩展：账号停用/降级/吊销会话时把该用户
// 遗留的待处理审批与已批准未消费的 grant 一并收敛（契约 12.3 第 12/15/22
// 条）。ExpirePendingGrantsByUser 只覆盖 source=terminal 的 pending 行，
// 不覆盖命令类审批（chat/console/batch）与 approved-but-unconsumed grant。
//
// 两个方法都返回**受影响的行**（有界，单次最多 500 行）：调用方必须在同一
// 事务内逐行写 audit_outbox（approval.expired / terminal.grant_expired），
// 否则审计员无法证明这些授权是被系统作废还是丢失（走查第 3 轮 high）。
type RevokeStore interface {
	// ExpirePendingByUser 把该用户全部 pending 审批置 expired（note 记录
	// 原因），并把关联的 awaiting_approval tool_call 收敛为 expired/
	// APPROVAL_CANCELLED。返回过期的审批行。
	ExpirePendingByUser(ctx context.Context, userID, note string, now time.Time) ([]Approval, error)
	// ExpireApprovedGrantsByUser 把该用户已批准但未消费的 terminal grant
	// 置 expired（避免 re-enable 后旧授权仍可开会话）。返回受影响的行。
	ExpireApprovedGrantsByUser(ctx context.Context, userID, note string, now time.Time) ([]Approval, error)
}

// BreakGlassStore 是审批仓储的可选扩展：主机侧 break-glass 单次批准
// （契约 12.3 第 11 条）。只允许把 pending 审批置 approved 并置
// break_glass=true，不提供 HTTP 入口。
type BreakGlassStore interface {
	// ApproveBreakGlass 原子批准一条 pending 审批：status='approved'、
	// decided_by=NULL（主机侧无用户身份）、note 记录原因、break_glass=true。
	// 非 pending 返回 ErrConflict；不存在返回 ErrNotFound。
	ApproveBreakGlass(ctx context.Context, approvalID, note string, at time.Time) (Approval, error)
}

// BreakGlassStoreOf 返回审批仓储的 break-glass 能力；未实现返回 nil。
func BreakGlassStoreOf(st Store) BreakGlassStore {
	if st == nil || st.Approvals() == nil {
		return nil
	}
	b, _ := st.Approvals().(BreakGlassStore)
	return b
}

// RevokeStoreOf 返回审批仓储的吊销能力；未实现返回 nil。
func RevokeStoreOf(st Store) RevokeStore {
	if st == nil || st.Approvals() == nil {
		return nil
	}
	r, _ := st.Approvals().(RevokeStore)
	return r
}
