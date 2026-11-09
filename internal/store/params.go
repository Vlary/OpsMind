package store

import (
	"encoding/json"
	"time"
)

// CreateUserParams 是创建用户参数。
type CreateUserParams struct {
	Username     string
	PasswordHash string
}

// UpdatePasswordParams 是更新密码参数。
type UpdatePasswordParams struct {
	PasswordHash string
	UpdatedAt    time.Time
}

// CreateSessionParams 是创建会话参数。
type CreateSessionParams struct {
	UserID    string
	TokenHash []byte
	ExpiresAt time.Time
	IP        string
	UserAgent string
	CreatedAt time.Time
}

// ServerCredentials 是服务器的加密凭据。
type ServerCredentials struct {
	AuthMethod          AuthMethod
	PasswordEncrypted   string
	PrivateKeyEncrypted string
	PassphraseEncrypted string
}

// CreateServerParams 是创建服务器参数。
type CreateServerParams struct {
	Name                  string
	OS                    ServerOS    // "" => linux；network_device 恒 network
	DeviceKind            DeviceKind  // N1："" => server
	NetdevBrand           NetdevBrand // N1：仅 network_device 必填
	AccessProto           AccessProto // N1："" => ssh
	Host                  string
	Port                  int
	WinRMPort             int // 0 => 5985
	Username              string
	Credentials           ServerCredentials
	EnableSecretEncrypted string // N1：网络设备 enable/super 密码（cryptox 密文）；空=未配置
	Tags                  []string
	AutoExecEnabled       bool
}

// UpdateServerParams 是更新服务器参数；nil 表示不修改。
type UpdateServerParams struct {
	Name        *string
	OS          *ServerOS
	DeviceKind  *DeviceKind  // N1
	NetdevBrand *NetdevBrand // N1
	AccessProto *AccessProto // N1
	Host        *string
	Port        *int
	WinRMPort   *int
	Username    *string
	Credentials *ServerCredentials
	// EnableSecretEncrypted：nil=不变；非 nil=替换（空串=清空，契约 2.6
	// 提供即替换、空串表示清空——语义同 password）。
	EnableSecretEncrypted *string // N1
	Tags                  *[]string
	AutoExecEnabled       *bool
}

// DatabaseCredentials 是数据库的加密密码。
type DatabaseCredentials struct {
	PasswordEncrypted string
}

// CreateDatabaseParams 是创建数据库参数。
type CreateDatabaseParams struct {
	Name         string
	Engine       DatabaseEngine
	Host         string
	Port         int
	InstanceName string // 仅 mssql
	DatabaseName string
	Username     string
	Credentials  DatabaseCredentials
	RedisDB      int    // 仅 redis
	AuthDB       string // N2：仅 mongodb；空串 = 驱动默认 admin
	SQLitePath   string
	SSLMode      SSLMode
}

// UpdateDatabaseParams 是更新数据库参数；nil 表示不修改。
type UpdateDatabaseParams struct {
	Name         *string
	Host         *string
	Port         *int
	InstanceName *string
	DatabaseName *string
	Username     *string
	Credentials  *DatabaseCredentials // nil=不变；非 nil=替换密码（空字符串表示清空）
	RedisDB      *int
	AuthDB       *string // N2：仅 mongodb；空串 = 驱动默认 admin
	SQLitePath   *string
	SSLMode      *SSLMode
}

// ProviderAPIKey 是供应商加密后的 API Key 与提示。
type ProviderAPIKey struct {
	Encrypted string
	Hint      string
}

// CreateProviderParams 是创建供应商参数。
type CreateProviderParams struct {
	Name         string
	Preset       ProviderPreset
	Kind         ProviderKind
	BaseURL      string
	APIKey       *ProviderAPIKey
	DefaultModel string
	Models       []string
}

// UpdateProviderParams 是更新供应商参数；nil 表示不修改。
type UpdateProviderParams struct {
	Name         *string
	Preset       *ProviderPreset
	Kind         *ProviderKind
	BaseURL      *string
	APIKey       *ProviderAPIKey
	DefaultModel *string
	Models       *[]string
}

// CreateConversationParams 是创建会话参数。
type CreateConversationParams struct {
	Title string
}

// ConversationListCursor 是会话列表游标分页位置：上一页末条会话的
// 排序键（last_message_at DESC, id DESC）。ID 为空表示第一页。
type ConversationListCursor struct {
	LastMessageAt time.Time
	ID            string
}

// CreateMessageParams 是写消息参数。
type CreateMessageParams struct {
	ConversationID string
	TurnID         string
	Role           MessageRole
	Content        string
	Reasoning      string
	// ReasoningDurationMS 为该轮 reasoning 首/尾 delta 的服务端毫秒差；
	// nil=无 reasoning，0=有 reasoning 但不足 1ms。
	ReasoningDurationMS *int64
	Status              MessageStatus
	ErrorCode           string
	Model               string
	PromptTokens        int
	CompletionTokens    int
	CreatedAt           time.Time
}

// CreateToolCallParams 是创建工具调用参数。
type CreateToolCallParams struct {
	ConversationID string
	TurnID         string
	ProviderCallID string
	Name           string
	Arguments      json.RawMessage
	RiskLevel      string
	Status         ToolCallStatus
	StartedAt      time.Time
	// ApprovalMode 是命令执行类工具判定时的有效门限快照（六期打磨
	// sprint 1，迁移 00010）；其余工具传空串（列 NULL）。
	ApprovalMode ApprovalMode
}

// FinishToolCallParams 是结束工具调用参数。
type FinishToolCallParams struct {
	ID           string
	Status       ToolCallStatus
	Result       json.RawMessage
	ErrorCode    string
	ErrorMessage string
	FinishedAt   time.Time
}

// CreateAuditParams 是追加审计参数。
type CreateAuditParams struct {
	CreatedAt      time.Time
	ActorType      string
	ActorID        string
	Action         string
	ResourceType   string
	ResourceID     string
	RequestID      string
	ConversationID *string
	ServerID       *string
	IP             string
	Detail         json.RawMessage
	PrevHash       string
	Hash           string
}

// CreateApprovalParams 是创建审批参数。
type CreateApprovalParams struct {
	// C1（四期）：Source 取 "chat"|"console"；console 时 ConversationID/ToolCallID 为 nil。
	// RuleID 四期加入（chat 路径取 risk.Decision.RuleID；console 容器操作恒
	// "CONTAINER_CHANGE"；空串沿用历史缺省）。
	ConversationID *string
	ToolCallID     *string
	Source         string
	ServerID       *string
	ServerName     string
	Command        string
	RiskLevel      string
	RuleID         string
	Reason         string
	// ApprovalMode 是创建时有效门限快照（六期 BE-P，契约 2.11 约定 1）；
	// 空串按 'standard' 落库（store 行为约定）。
	ApprovalMode string
	RequestID    string
	ExpiresAt    time.Time
	CreatedAt    time.Time
}

// DecideApprovalParams 是 decide 参数（六期 BE-P 结构体化，原多参数签名
// 废弃——契约 3.3）。EditedCommand 空串表示未改写；改写的重分类在
// approvals.Service.Decide 内完成（2.11 约定 3），EditedRiskLevel/
// EditedRuleID 由 Service 算出后随状态同一 UPDATE 落库——store 只落值。
type DecideApprovalParams struct {
	ID              string
	Status          ApprovalStatus // approved|denied|expired
	DecidedBy       string
	Note            string
	EditedCommand   string
	EditedRiskLevel string // Service 内部分类结果；未改写为空串
	EditedRuleID    string
	DecidedAt       time.Time
}

// AuditFilter 是审计列表过滤条件。
type AuditFilter struct {
	BeforeID       int64
	Limit          int
	Action         string
	ResourceType   string
	ResourceID     string
	ConversationID string
	ServerID       string
	FromTime       time.Time // 五期 M3：audit_search 工具的时间下界（零值=不过滤）；REST /api/audit 不变
	ToTime         time.Time // 五期 M3：时间上界（零值=不过滤）
}

// ---------- 五期 M1（监控告警）----------

// InsertMetricParams 是指标写入（collector 批量，BatchInsert 每批 ≤500）。
type InsertMetricParams struct {
	ServerID  string
	CreatedAt time.Time
	CPU       float64
	Mem       float64
	Disk      float64
	Load1     float64
	NetIn     int64
	NetOut    int64
}

// MetricWindow 是窗口查询条件。
type MetricWindow struct {
	ServerID string // ""=全部（overview 聚合不使用，仅单机）
	From     time.Time
	To       time.Time
	Limit    int // 硬上限 1440（契约 11 节）；窗口查询按 created_at ASC 返回
}

// CreateAlertRuleParams 是创建告警规则参数。
type CreateAlertRuleParams struct {
	Name            string
	Metric          AlertMetric
	Comparator      AlertComparator
	Threshold       float64
	DurationWindows int
	CooldownSeconds int
	Severity        AlertSeverity
	SilentUntil     *time.Time
	NotifyChannelID *string
	Enabled         bool
}

// UpdateAlertRuleParams 是更新告警规则参数（nil=不变）。
type UpdateAlertRuleParams struct {
	Name               *string
	Metric             *AlertMetric
	Comparator         *AlertComparator
	Threshold          *float64
	DurationWindows    *int
	CooldownSeconds    *int
	Severity           *AlertSeverity
	SilentUntil        *time.Time // 修改静默截止
	ClearSilentUntil   bool       // true=清空静默（与 SilentUntil 互斥，同时给出返回 ErrInvalid）
	NotifyChannelID    *string    // 修改绑定渠道
	ClearNotifyChannel bool       // true=解绑渠道（与 NotifyChannelID 互斥）
	Enabled            *bool
}

// AlertEventFilter 是事件列表过滤条件。
type AlertEventFilter struct {
	Status   AlertEventStatus // ""=不过滤
	Severity AlertSeverity    // ""=不过滤
	ServerID string           // ""=不过滤
	BeforeID int64            // 0=最新一页
	Limit    int              // 1..200
}

// CreateNotificationChannelParams 是创建通知渠道参数；ConfigJSON 为明文配置
// JSON（结构见 monitor.NotificationConfig），store 实现前由 handler/cryptox
// 加密——store 只存取密文与 hint。
type CreateNotificationChannelParams struct {
	Name       string
	Kind       NotificationKind
	ConfigJSON string
	ConfigHint string
}

// UpdateNotificationChannelParams 是更新通知渠道参数。
type UpdateNotificationChannelParams struct {
	Name       *string
	Kind       *NotificationKind
	ConfigJSON *string // nil=不变；非 nil=整体替换（空串表示清空——不允许，返回 ErrInvalid）
	ConfigHint *string
}

// ---------- 五期 M2（AI 自诊断 / 定时巡检）----------

// CreateDiagnosisParams 是诊断会话创建。Trigger 由调用方定（alert/manual/chat）。
type CreateDiagnosisParams struct {
	ServerID       string
	AlertEventID   *int64
	ConversationID *string
	Trigger        DiagnosisTrigger
	ExpiresAt      time.Time
}

// UpdateDiagnosisStatusParams 是状态迁移（engine 专用；UpdateStatus 用 CAS
// 语义，见契约 3.4 行为约定）。
type UpdateDiagnosisStatusParams struct {
	ID        string
	From      DiagnosisStatus // 期望当前状态（CAS）；空串=不检查
	To        DiagnosisStatus
	Report    json.RawMessage // To=report/awaiting_fix 时写报告；nil=不改
	ErrorCode string
	Timestamp time.Time // 写 started_at/finished_at 的依据
}

// CreateInspectionJobParams 是巡检任务创建。
type CreateInspectionJobParams struct {
	Name            string
	Cron            string
	TargetTags      []string
	Template        InspectionTemplate
	NotifyEnabled   bool
	NotifyChannelID *string
	Enabled         bool
	NextRunAt       *time.Time // 由 handler 按 cron 计算
}

// UpdateInspectionJobParams 是巡检任务更新（指针字段 nil=不改）。
type UpdateInspectionJobParams struct {
	Name               *string
	Cron               *string // 变更时 handler 重算 next_run_at
	TargetTags         *[]string
	Template           *InspectionTemplate
	NotifyEnabled      *bool
	NotifyChannelID    *string
	ClearNotifyChannel bool
	Enabled            *bool
}

// CreateInspectionReportParams 是巡检报告创建。
type CreateInspectionReportParams struct {
	JobID        *string
	Trigger      InspectionTrigger
	ServerCount  int
	AvgScore     int
	AnomalyCount int
	StartedAt    time.Time
	FinishedAt   *time.Time
}

// CreateInspectionItemParams 是单台巡检明细创建。
type CreateInspectionItemParams struct {
	ReportID     string
	ServerID     string
	Score        int
	AnomalyCount int
	Anomalies    json.RawMessage
	RawOutput    string // 已截断
	// Command 是模板展开后的命令清单（换行分隔；六期打磨 sprint 1）。
	Command    string
	Truncated  bool
	DurationMS int64
	ErrorCode  string
}

// InspectionReportFilter 是报告列表过滤（契约 11 节 #16：limit 1..200）。
type InspectionReportFilter struct {
	JobID string // ""=全部
	Limit int    // 1..200
}

// ---------- 五期 M3（批量操作）----------

// CreateBatchJobParams 是批量 job 创建（server_ids 上限 50，契约 11 节）。
type CreateBatchJobParams struct {
	Description    string
	Command        string
	ServerIDs      []string
	Source         BatchSource
	ConversationID *string
	CreatedBy      *string
}

// CreateBatchResultParams 是批量单台结果创建（risk_level 为逐台分类快照）。
// ApprovalID 是契约 3.3 的必要补充：审批台建 result 时即写入 approval_id，
// decide handler 才能经 ByApproval 反查回填（BatchResult.ApprovalID 落列）。
type CreateBatchResultParams struct {
	BatchJobID string
	ServerID   string
	RiskLevel  string
	RuleID     string
	Status     BatchResultStatus
	ApprovalID *string
	// ErrorCode 同为必要补充：skipped 台（SERVER_NOT_FOUND/
	// COMMAND_FORBIDDEN）无执行段，创建时即落错误码（G36 断言）。
	ErrorCode string
}

// FinishBatchResultParams 是审批执行后回填（decide handler 经 batchx 调用）。
type FinishBatchResultParams struct {
	ID         string
	ApprovalID string // 按 approval_id 定位也行；ID 直达
	Status     BatchResultStatus
	ExitCode   *int
	Output     string
	Truncated  bool
	ErrorCode  string
	DurationMS int64
	FinishedAt time.Time
}
