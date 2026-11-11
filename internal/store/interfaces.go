package store

import (
	"context"
	"encoding/json"
	"time"
)

// Store 是持久层的唯一入口；具体实现由 internal/store/postgres 提供。
type Store interface {
	Ping(ctx context.Context) error
	Close()
	WithTx(ctx context.Context, fn func(Store) error) error

	Users() UserStore
	Sessions() SessionStore
	Servers() ServerStore
	Databases() DatabaseStore
	Providers() ProviderStore
	Conversations() ConversationStore
	Messages() MessageStore
	ToolCalls() ToolCallStore
	Approvals() ApprovalStore
	AuditLogs() AuditStore
	Settings() SettingStore
	TerminalSessions() TerminalSessionStore         // W3
	RdpSessions() RdpSessionStore                   // W4
	Metrics() MetricStore                           // 五期 M1
	AlertRules() AlertRuleStore                     // 五期 M1
	AlertEvents() AlertEventStore                   // 五期 M1
	NotificationChannels() NotificationChannelStore // 五期 M1
	DiagnosisSessions() DiagnosisSessionStore       // 五期 M2
	InspectionJobs() InspectionJobStore             // 五期 M2
	InspectionReports() InspectionReportStore       // 五期 M2
	BatchJobs() BatchJobStore                       // 五期 M3
}

// MetricStore 是指标仓储（五期 M1）。BatchInsert 单批 >500 行返回 ErrInvalid
// （契约 11 节防 OOM）；UPSERT latest 在 BatchInsert 内部同一事务完成。
type MetricStore interface {
	BatchInsert(ctx context.Context, items []InsertMetricParams) error
	Latest(ctx context.Context) ([]ServerMetricLatest, error)
	LatestByServer(ctx context.Context, serverID string) (ServerMetricLatest, error)
	Window(ctx context.Context, f MetricWindow) ([]ServerMetric, error) // created_at ASC
	DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error)  // 保留策略
}

// AlertRuleStore 是告警规则仓储（五期 M1）。
type AlertRuleStore interface {
	List(ctx context.Context) ([]AlertRule, error)
	Get(ctx context.Context, id string) (AlertRule, error)
	Create(ctx context.Context, p CreateAlertRuleParams) (AlertRule, error)
	Update(ctx context.Context, id string, p UpdateAlertRuleParams) (AlertRule, error)
	Delete(ctx context.Context, id string) error
}

// AlertEventStore 是告警事件仓储（五期 M1）。Create 依赖部分唯一索引
// alert_events_open_uniq：并发重复插入由索引兜底（冲突返回 ErrConflict，
// 评估器忽略即可）。
type AlertEventStore interface {
	Create(ctx context.Context, rule AlertRule, serverID string, value float64, message string, firedAt time.Time) (AlertEvent, error)
	List(ctx context.Context, f AlertEventFilter) (items []AlertEvent, hasMore bool, err error)
	Get(ctx context.Context, id int64) (AlertEvent, error)
	Resolve(ctx context.Context, id int64, at time.Time) (AlertEvent, error)                // UPDATE WHERE status='firing'；0 行 ErrConflict
	SetNotifyStatus(ctx context.Context, id int64, status NotifyStatus, at time.Time) error // 只更新 last_notified_at/notify_status
	CountFiring(ctx context.Context) (int64, error)
	CountFiringByServer(ctx context.Context, serverID string) (int64, error)
}

// NotificationChannelStore 是通知渠道仓储（五期 M1）。加解密不在 store
// ——handler 持 cipher，store 只存取密文与 hint。
type NotificationChannelStore interface {
	List(ctx context.Context) ([]NotificationChannel, error)
	Get(ctx context.Context, id string) (NotificationChannel, error)
	Create(ctx context.Context, p CreateNotificationChannelParams) (NotificationChannel, error)
	Update(ctx context.Context, id string, p UpdateNotificationChannelParams) (NotificationChannel, error)
	Delete(ctx context.Context, id string) error
}

// DiagnosisSessionStore 是诊断会话仓储（五期 M2）。UpdateStatus 为 CAS：
// UPDATE ... WHERE id=$1 AND (status=$2 OR $2=”) RETURNING *；0 行返回
// ErrConflict。
type DiagnosisSessionStore interface {
	Create(ctx context.Context, p CreateDiagnosisParams) (DiagnosisSession, error)
	Get(ctx context.Context, id string) (DiagnosisSession, error)
	List(ctx context.Context, status DiagnosisStatus, serverID string, limit int) ([]DiagnosisSession, error)
	UpdateStatus(ctx context.Context, p UpdateDiagnosisStatusParams) (DiagnosisSession, error)
	ExpireStale(ctx context.Context, now time.Time) (int64, error) // awaiting_fix 且 expires_at<now → expired
}

// InspectionJobStore 是巡检任务仓储（五期 M2）。
type InspectionJobStore interface {
	List(ctx context.Context) ([]InspectionJob, error)
	Get(ctx context.Context, id string) (InspectionJob, error)
	Create(ctx context.Context, p CreateInspectionJobParams) (InspectionJob, error)
	Update(ctx context.Context, id string, p UpdateInspectionJobParams) (InspectionJob, error)
	Delete(ctx context.Context, id string) error
	MarkRun(ctx context.Context, id string, lastRunAt, nextRunAt time.Time) error
	ListDue(ctx context.Context, now time.Time) ([]InspectionJob, error) // enabled AND next_run_at<=now
}

// InspectionReportStore 是巡检报告仓储（五期 M2）。
type InspectionReportStore interface {
	Create(ctx context.Context, p CreateInspectionReportParams) (InspectionReport, error)
	Get(ctx context.Context, id string) (InspectionReport, []InspectionReportItem, error)
	List(ctx context.Context, f InspectionReportFilter) ([]InspectionReport, error)
	CreateItem(ctx context.Context, p CreateInspectionItemParams) (InspectionReportItem, error)
	PreviousScores(ctx context.Context, jobID string, before time.Time) (map[string]int, error) // 同 job 上一份报告 per-server 评分（趋势对比）
}

// BatchJobStore 是批量任务仓储（五期 M3）。job 状态归并（pending→running→
// done/partial/failed/cancelled）由 batchx 在每次 FinishResult 后重算
// （COUNT 聚合，无竞态——单写者）。ByApproval 0 行返回 ErrNotFound
// （非 batch 审批不调用）。
type BatchJobStore interface {
	Create(ctx context.Context, p CreateBatchJobParams) (BatchJob, error)
	Get(ctx context.Context, id string) (BatchJob, []BatchResult, error)
	List(ctx context.Context, limit int) ([]BatchJob, error)
	SetStatus(ctx context.Context, id string, status BatchStatus, at time.Time) error
	CreateResult(ctx context.Context, p CreateBatchResultParams) (BatchResult, error)
	FinishResult(ctx context.Context, p FinishBatchResultParams) (BatchResult, error)
	ByApproval(ctx context.Context, approvalID string) (BatchResult, error) // decide handler 回填入口
}

// UserStore 是用户仓储。
type UserStore interface {
	Count(ctx context.Context) (int64, error)
	Create(ctx context.Context, p CreateUserParams) (User, error)
	ByUsername(ctx context.Context, username string) (User, error)
	ByID(ctx context.Context, id string) (User, error)
	// UsernamesByIDs 批量按 id 查用户名（审计页装配 actor_name 用）；
	// 非法 uuid 的 id 直接忽略；查不到的 id 不出现在结果里。
	UsernamesByIDs(ctx context.Context, ids []string) (map[string]string, error)
	UpdatePassword(ctx context.Context, id string, p UpdatePasswordParams) error
}

// SessionStore 是会话仓储。
type SessionStore interface {
	Create(ctx context.Context, p CreateSessionParams) (Session, error)
	ByTokenHash(ctx context.Context, tokenHash []byte) (Session, error)
	Touch(ctx context.Context, id string, at time.Time) error
	Delete(ctx context.Context, id string) error
	DeleteByUser(ctx context.Context, userID string) error
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
}

// ServerStore 是服务器仓储。
type ServerStore interface {
	List(ctx context.Context) ([]Server, error)
	Get(ctx context.Context, id string) (Server, error)
	Create(ctx context.Context, p CreateServerParams) (Server, error)
	Update(ctx context.Context, id string, p UpdateServerParams) (Server, error)
	Delete(ctx context.Context, id string) error
	UpdateHostKey(ctx context.Context, id, fingerprint, publicKey string, at time.Time) (Server, error)
	// UpdateContainerRuntime 写回容器运行时探测结果（四期 C1，契约 2.9 约定 1）。
	// runtime 合法值 ''/none/docker/podman/containerd；network_device 只允许 ''
	// （应用保证，违反返回 ErrInvalid）。
	UpdateContainerRuntime(ctx context.Context, id string, runtime ContainerRuntime, at time.Time) (Server, error)
}

// DatabaseStore 是外部数据库仓储。
type DatabaseStore interface {
	List(ctx context.Context) ([]Database, error)
	Get(ctx context.Context, id string) (Database, error)
	Create(ctx context.Context, p CreateDatabaseParams) (Database, error)
	Update(ctx context.Context, id string, p UpdateDatabaseParams) (Database, error)
	Delete(ctx context.Context, id string) error
}

// ProviderStore 是供应商仓储。
type ProviderStore interface {
	List(ctx context.Context) ([]Provider, error)
	Get(ctx context.Context, id string) (Provider, error)
	Create(ctx context.Context, p CreateProviderParams) (Provider, error)
	Update(ctx context.Context, id string, p UpdateProviderParams) (Provider, error)
	Delete(ctx context.Context, id string) error
}

// ConversationStore 是会话（对话）仓储。
type ConversationStore interface {
	ListPage(ctx context.Context, cursor ConversationListCursor, limit int) (items []Conversation, hasMore bool, err error)
	Get(ctx context.Context, id string) (Conversation, error)
	Create(ctx context.Context, p CreateConversationParams) (Conversation, error)
	UpdateTitle(ctx context.Context, id, title string, at time.Time) (Conversation, error)
	BindModel(ctx context.Context, id, providerID, model string, at time.Time) (Conversation, error)
	Touch(ctx context.Context, id string, at time.Time) error
	Delete(ctx context.Context, id string) error
	// SetApprovalMode 设置/清除会话级审批门限覆盖（六期 BE-P，契约 3.4）：
	// mode=nil 清除覆盖（跟随全局）；PATCH /api/conversations/:id 的
	// approval_mode 分支唯一写入口。mode 合法性由 DB CHECK 兜底。
	SetApprovalMode(ctx context.Context, id string, mode *ApprovalMode, at time.Time) (Conversation, error)
	// ListReplayCatalog 是回放目录聚合查询（八期 BE-R，契约 2.12 约定 4 /
	// 5.19.1）。排序 last_message_at DESC, id DESC；多查一条判定 hasMore；
	// 游标语义与 ListPage 完全一致。计数经 LATERAL 子查询一次取回（禁止 N+1）。
	ListReplayCatalog(ctx context.Context, p ListReplayCatalogParams) ([]ReplayCatalogItem, bool, error)
}

// MessageStore 是消息仓储。
type MessageStore interface {
	Create(ctx context.Context, p CreateMessageParams) (Message, error)
	ListPage(ctx context.Context, conversationID string, beforeSeq int64, limit int) (items []Message, hasMore bool, err error)
}

// ToolCallStore 是工具调用仓储。
type ToolCallStore interface {
	Create(ctx context.Context, p CreateToolCallParams) (ToolCall, error)
	// SetStatus 更新工具调用状态；非失败终态 errorMessage 传空串。
	SetStatus(ctx context.Context, id string, status ToolCallStatus, errorCode, errorMessage string) (ToolCall, error)
	Finish(ctx context.Context, p FinishToolCallParams) (ToolCall, error)
	ListPage(ctx context.Context, conversationID string, beforeSeq int64, limit int) (items []ToolCall, hasMore bool, err error)
	ByID(ctx context.Context, id string) (ToolCall, error)
}

// ApprovalStore 是审批仓储。
type ApprovalStore interface {
	Create(ctx context.Context, p CreateApprovalParams) (Approval, error)
	Get(ctx context.Context, id string) (Approval, error)
	List(ctx context.Context, conversationID string, status ApprovalStatus, limit int) ([]Approval, error)
	// Decide 签名由多参数改为结构体（DecideApprovalParams，六期 BE-P 契约
	// 3.3/3.4）——同一 UPDATE 落库 status/decided_* 与 edited_command/
	// edited_risk_level/edited_rule_id（改写的重分类在 Service 层完成，
	// store 只落值）。语义不变：UPDATE ... WHERE id=$1 AND status='pending'
	// RETURNING *；0 行时不存在返回 ErrNotFound、非 pending 返回 ErrConflict。
	Decide(ctx context.Context, p DecideApprovalParams) (Approval, error)
	Expire(ctx context.Context, now time.Time) (int64, error)
}

// AuditStore 是只追加审计仓储。
type AuditStore interface {
	Append(ctx context.Context, p CreateAuditParams) error
	List(ctx context.Context, f AuditFilter) (items []AuditLog, hasMore bool, err error)
	LastHash(ctx context.Context) (string, error)
}

// SettingStore 是键值配置仓储。
type SettingStore interface {
	Get(ctx context.Context, key string) (Setting, error)
	Set(ctx context.Context, key string, value json.RawMessage, at time.Time) (Setting, error)
	List(ctx context.Context) ([]Setting, error)
}
