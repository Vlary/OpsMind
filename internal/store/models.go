// Package store 定义领域模型、参数、仓储接口与错误。
//
// 本包由 CONTRACT.md 第 3 节逐字规定，骨架阶段只放类型定义，不包含任何 SQL
// 或实现逻辑。实现者（BE1）不得在本包中 import 其他 internal 包。
package store

import (
	"encoding/json"
	"time"
)

// AuthMethod 是服务器认证方式。password_ntlm/password_basic 为二期 WinRM
// 通道专用（契约 3.1：仅 os=windows 合法，密码同存 password_enc）。
type AuthMethod string

const (
	AuthMethodPassword      AuthMethod = "password"
	AuthMethodPrivateKey    AuthMethod = "private_key"
	AuthMethodPasswordNTLM  AuthMethod = "password_ntlm"  // W1：WinRM NTLM（工作组默认协商）
	AuthMethodPasswordBasic AuthMethod = "password_basic" // W1：WinRM Basic（明文，建议 HTTPS）
)

// ServerOS 是服务器操作系统，决定执行通道路由（契约 2.7 双轨 / 2.8）。
// N1：network 仅用于 device_kind=network_device（网络设备风险分册路由）。
type ServerOS string

const (
	ServerOSLinux   ServerOS = "linux"
	ServerOSWindows ServerOS = "windows"
	ServerOSNetwork ServerOS = "network" // N1：仅 network_device
)

// DeviceKind 是资产形态（契约 3.1，N1）。server=主机（linux/windows）；
// network_device=网络设备（交换机/路由器/防火墙，CLI 行协议）。
type DeviceKind string

const (
	DeviceKindServer        DeviceKind = "server"
	DeviceKindNetworkDevice DeviceKind = "network_device"
)

// ContainerRuntime 是容器运行时探测结果（契约 3.1，四期 C1）。
// 空串=未探测（缺省）；network_device 恒空串。containerd 仅探测展示，
// 不参与容器管理面（契约 2.9 约定 2）。
type ContainerRuntime string

const (
	ContainerRuntimeNone       ContainerRuntime = ""     // 未探测
	ContainerRuntimeAbsent     ContainerRuntime = "none" // 已探测，无运行时
	ContainerRuntimeDocker     ContainerRuntime = "docker"
	ContainerRuntimePodman     ContainerRuntime = "podman"
	ContainerRuntimeContainerd ContainerRuntime = "containerd" // 仅探测展示，管理面不支持
)

// NetdevBrand 是网络设备厂商（契约 3.1，N1）——风险分册（risk.NetBrand）
// 与 netdev profile 的路由键。
type NetdevBrand string

const (
	NetdevBrandCisco   NetdevBrand = "cisco"
	NetdevBrandHuawei  NetdevBrand = "huawei"
	NetdevBrandH3C     NetdevBrand = "h3c"
	NetdevBrandJunos   NetdevBrand = "junos"
	NetdevBrandGeneric NetdevBrand = "generic"
)

// AccessProto 是网络设备接入协议（契约 3.1，N1）。telnet 为明文，创建需
// 显式确认（confirm_telnet_cleartext，契约 2.6）。
type AccessProto string

const (
	AccessProtoSSH    AccessProto = "ssh"
	AccessProtoTelnet AccessProto = "telnet"
)

// DatabaseEngine 是外部数据库类型。
type DatabaseEngine string

const (
	EnginePostgres DatabaseEngine = "postgres"
	EngineMySQL    DatabaseEngine = "mysql"
	EngineSQLite   DatabaseEngine = "sqlite"
	EngineMSSQL    DatabaseEngine = "mssql"   // W2：SQL Server（go-mssqldb）
	EngineRedis    DatabaseEngine = "redis"   // W2：Redis（go-redis/v9，命令白名单）
	EngineMongoDB  DatabaseEngine = "mongodb" // N2：MongoDB（mongo-driver v1.17.4，JSON 命令文档守卫）
)

// SSLMode 是外部数据库 TLS 模式。
type SSLMode string

const (
	SSLModeDisable    SSLMode = "disable"
	SSLModePrefer     SSLMode = "prefer"
	SSLModeRequire    SSLMode = "require"
	SSLModeVerifyCA   SSLMode = "verify-ca"
	SSLModeVerifyFull SSLMode = "verify-full"
)

// ProviderKind 是 LLM 供应商协议类型。
type ProviderKind string

const (
	ProviderOpenAICompatible ProviderKind = "openai_compatible"
	ProviderAnthropic        ProviderKind = "anthropic"
)

// ProviderPreset 是内置供应商预设。
type ProviderPreset string

const (
	PresetOpenAI    ProviderPreset = "openai"
	PresetDeepSeek  ProviderPreset = "deepseek"
	PresetDashScope ProviderPreset = "dashscope"
	PresetMoonshot  ProviderPreset = "moonshot"
	PresetZhipu     ProviderPreset = "zhipu"
	PresetAnthropic ProviderPreset = "anthropic"
	PresetGemini    ProviderPreset = "gemini"
	PresetOllama    ProviderPreset = "ollama"
	PresetCustom    ProviderPreset = "custom"
)

// MessageRole 是消息角色。
type MessageRole string

const (
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
)

// MessageStatus 是消息终态。
type MessageStatus string

const (
	MessageComplete MessageStatus = "complete"
	MessageError    MessageStatus = "error"
)

// ToolCallStatus 是工具调用状态。
type ToolCallStatus string

const (
	ToolStarted          ToolCallStatus = "started"
	ToolAwaitingApproval ToolCallStatus = "awaiting_approval"
	ToolSucceeded        ToolCallStatus = "succeeded"
	ToolFailed           ToolCallStatus = "failed"
	ToolDenied           ToolCallStatus = "denied"
	ToolExpired          ToolCallStatus = "expired"
	ToolAwaitingUser     ToolCallStatus = "awaiting_user"
)

// ApprovalStatus 是审批状态。
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalDenied   ApprovalStatus = "denied"
	ApprovalExpired  ApprovalStatus = "expired"
)

// User 是唯一的 admin 账号。
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Session 是服务端会话；token 只存 SHA-256。
type Session struct {
	ID         string    `json:"-"`
	UserID     string    `json:"-"`
	TokenHash  []byte    `json:"-"`
	ExpiresAt  time.Time `json:"-"`
	LastSeenAt time.Time `json:"-"`
	IP         string    `json:"-"`
	UserAgent  string    `json:"-"`
	CreatedAt  time.Time `json:"-"`
}

// Server 是 SSH/WinRM 服务器或网络设备资产（三期起 device_kind 区分）。
type Server struct {
	ID                    string           `json:"id"`
	Name                  string           `json:"name"`
	DeviceKind            DeviceKind       `json:"device_kind"` // N1：server | network_device
	OS                    ServerOS         `json:"os"`
	NetdevBrand           NetdevBrand      `json:"netdev_brand"` // N1：仅 network_device；server 恒 ""
	AccessProto           AccessProto      `json:"access_proto"` // N1：network_device: ssh|telnet；server 恒 "ssh"
	Host                  string           `json:"host"`
	Port                  int              `json:"port"`
	WinRMPort             int              `json:"winrm_port"`
	Username              string           `json:"username"`
	AuthMethod            AuthMethod       `json:"auth_method"`
	PasswordEncrypted     string           `json:"-"`
	PrivateKeyEncrypted   string           `json:"-"`
	PassphraseEncrypted   string           `json:"-"`
	EnableSecretEncrypted string           `json:"-"` // N1：网络设备 enable/super 密码；server 恒空
	HasPassword           bool             `json:"has_password"`
	HasPrivateKey         bool             `json:"has_private_key"`
	HasEnableSecret       bool             `json:"has_enable_secret"` // N1：派生只读；server 恒 false
	ContainerRuntime      ContainerRuntime `json:"container_runtime"` // C1（四期）：''|none|docker|podman|containerd；探测写回（契约 2.9 约定 1），不可由创建/更新请求设置；network_device 恒 ''
	Tags                  []string         `json:"tags"`
	AutoExecEnabled       bool             `json:"auto_exec_enabled"`
	HostKeyFingerprint    string           `json:"host_key_fingerprint"`
	HostKeyPublic         string           `json:"-"`
	HostKeyVerifiedAt     *time.Time       `json:"host_key_verified_at"`
	CreatedAt             time.Time        `json:"created_at"`
	UpdatedAt             time.Time        `json:"updated_at"`
}

// Database 是外部数据库资产。
type Database struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	Engine            DatabaseEngine `json:"engine"`
	Host              string         `json:"host"`
	Port              int            `json:"port"`
	InstanceName      string         `json:"instance_name"` // 仅 mssql；与 port 互斥
	DatabaseName      string         `json:"database_name"`
	Username          string         `json:"username"`
	PasswordEncrypted string         `json:"-"`
	HasPassword       bool           `json:"has_password"`
	RedisDB           int            `json:"redis_db"` // 仅 redis
	AuthDB            string         `json:"auth_db"`  // N2：仅 mongodb；空串 = 驱动默认 admin（AuthSource）
	SQLitePath        string         `json:"sqlite_path"`
	SSLMode           SSLMode        `json:"ssl_mode"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

// Provider 是 LLM 供应商配置。
type Provider struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Preset          ProviderPreset `json:"preset"`
	Kind            ProviderKind   `json:"kind"`
	BaseURL         string         `json:"base_url"`
	APIKeyEncrypted string         `json:"-"`
	APIKeyHint      string         `json:"-"`
	APIKeyMasked    string         `json:"api_key_masked"`
	HasAPIKey       bool           `json:"has_api_key"`
	DefaultModel    string         `json:"default_model"`
	Models          []string       `json:"models"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// Conversation 是一次对话会话。
type Conversation struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	ProviderID *string `json:"provider_id"`
	Model      string  `json:"model"`
	// ApprovalMode 是会话级审批门限覆盖（六期 BE-P，契约 2.11 约定 1）：
	// nil=跟随全局（conversations.approval_mode NULL）。
	ApprovalMode *ApprovalMode `json:"approval_mode"`
	// EffectiveApprovalMode 是派生字段（覆盖 > 全局缺省 standard）。
	// store 读方法恒空串，由 httpapi handler 读取 settings 后统一填充
	//（契约 3.4 六期行为约定）；前端不得依赖 store 层值。
	EffectiveApprovalMode ApprovalMode `json:"effective_approval_mode"`
	CreatedAt             time.Time    `json:"created_at"`
	UpdatedAt             time.Time    `json:"updated_at"`
	LastMessageAt         time.Time    `json:"last_message_at"`
}

// ---------- 六期（BE-P）----------

// ApprovalMode 是审批门限四档（契约 2.11 约定 1/2 的策略矩阵输入）。
// standard=一至五期现行行为（全局缺省）；有效门限=会话覆盖 > 全局缺省
// （approvals.EffectiveMode）。approvals.approval_mode 是创建时快照。
type ApprovalMode string

const (
	ApprovalModeAll      ApprovalMode = "all"      // 全部命令审批（含只读）
	ApprovalModeStandard ApprovalMode = "standard" // 危险命令审批、只读免审（现行默认）
	ApprovalModeHigh     ApprovalMode = "high"     // 仅高危审批（warning 级 needs_approval 也免审，7.3E）
	ApprovalModeNone     ApprovalMode = "none"     // 免审批、全部自动执行（仍必须落审计）
)

// ValidApprovalMode 判断门限值是否合法四档之一。
func ValidApprovalMode(m ApprovalMode) bool {
	switch m {
	case ApprovalModeAll, ApprovalModeStandard, ApprovalModeHigh, ApprovalModeNone:
		return true
	}
	return false
}

// Message 是会话中的一条消息。
type Message struct {
	ID             string      `json:"id"`
	Seq            int64       `json:"seq"`
	ConversationID string      `json:"conversation_id"`
	TurnID         string      `json:"turn_id"`
	Role           MessageRole `json:"role"`
	Content        string      `json:"content"`
	Reasoning      string      `json:"reasoning"`
	// ReasoningDurationMS 是该轮 reasoning 首/尾 delta 的服务端时间差（毫秒，
	// 六期打磨 sprint 2）。nil=无 reasoning 或 00009 之前的旧行无计时数据；
	// 0=有 reasoning 但不足 1ms。
	ReasoningDurationMS *int64        `json:"reasoning_duration_ms"`
	Status              MessageStatus `json:"status"`
	ErrorCode           string        `json:"error_code"`
	Model               string        `json:"model"`
	PromptTokens        int           `json:"prompt_tokens"`
	CompletionTokens    int           `json:"completion_tokens"`
	CreatedAt           time.Time     `json:"created_at"`
}

// ToolCall 是一次工具调用。
type ToolCall struct {
	ID             string          `json:"id"`
	Seq            int64           `json:"seq"`
	ConversationID string          `json:"conversation_id"`
	TurnID         string          `json:"turn_id"`
	ProviderCallID string          `json:"provider_call_id"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	Result         json.RawMessage `json:"result"`
	RiskLevel      string          `json:"risk_level"`
	Status         ToolCallStatus  `json:"status"`
	ErrorCode      string          `json:"error_code"`
	ErrorMessage   string          `json:"error_message"`
	// ApprovalMode 是命令执行类工具判定时的有效门限快照（六期打磨
	// sprint 1，迁移 00010）。空串=非命令执行类工具，或 00010 之前的旧行
	// 未记录（前端对空串不渲染门限徽标，语义如实不伪造）。
	ApprovalMode string     `json:"approval_mode"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}

// Approval 是一次命令审批。
type Approval struct {
	ID string `json:"id"`
	// C1（四期）：source=console（容器页等控制台发起）时两者均为 nil；
	// source=chat（模型工具调用）恒非 nil——agent 侧调用不变。
	ConversationID *string        `json:"conversation_id"`
	ToolCallID     *string        `json:"tool_call_id"`
	Source         string         `json:"source"` // chat|console（四期，缺省 chat）
	ServerID       *string        `json:"server_id"`
	ServerName     string         `json:"server_name"`
	Command        string         `json:"command"`
	RiskLevel      string         `json:"risk_level"`
	RuleID         string         `json:"rule_id"`
	Reason         string         `json:"reason"`
	Status         ApprovalStatus `json:"status"`
	ApprovalMode   ApprovalMode   `json:"approval_mode"` // BE-P（六期）：创建时有效门限快照；00008 前历史行等价 standard
	RequestedAt    time.Time      `json:"requested_at"`
	ExpiresAt      time.Time      `json:"expires_at"`
	DecidedAt      *time.Time     `json:"decided_at"`
	DecidedBy      *string        `json:"decided_by"`
	Note           string         `json:"note"`
	// BE-P（六期）：decide 时用户改写。空串=未改写；执行与展示用改写后命令
	//（edited_command != "" ? edited_command : command，回放/审计展示/UI 同源）。
	// 原 Command/RiskLevel/RuleID/Reason 是创建时事实，改写不回写。
	EditedCommand   string `json:"edited_command"`
	EditedRiskLevel string `json:"edited_risk_level"` // ''|safe|needs_approval（forbidden 在 decide 即被拒，不落库）
	EditedRuleID    string `json:"edited_rule_id"`
}

// AuditLog 是只追加审计记录。
type AuditLog struct {
	ID             int64           `json:"id"`
	CreatedAt      time.Time       `json:"created_at"`
	ActorType      string          `json:"actor_type"`
	ActorID        string          `json:"actor_id"`
	Action         string          `json:"action"`
	ResourceType   string          `json:"resource_type"`
	ResourceID     string          `json:"resource_id"`
	RequestID      string          `json:"request_id"`
	ConversationID *string         `json:"conversation_id"`
	ServerID       *string         `json:"server_id"`
	IP             string          `json:"ip"`
	Detail         json.RawMessage `json:"detail"`
	PrevHash       string          `json:"prev_hash"`
	Hash           string          `json:"hash"`
}

// Setting 是键值配置。
type Setting struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// ---------- 五期 M1（监控告警）----------

// AlertMetric 是告警指标名（alert_rules.metric，与 server_metrics 列一一对应）。
type AlertMetric string

const (
	AlertMetricCPU    AlertMetric = "cpu_pct"
	AlertMetricMem    AlertMetric = "mem_pct"
	AlertMetricDisk   AlertMetric = "disk_pct"
	AlertMetricLoad1  AlertMetric = "load_1"
	AlertMetricNetIn  AlertMetric = "net_in_bps"
	AlertMetricNetOut AlertMetric = "net_out_bps"
)

// AlertComparator 是比较符。gt=高于阈值触发；lt=低于阈值触发。
type AlertComparator string

const (
	AlertComparatorGT AlertComparator = "gt"
	AlertComparatorLT AlertComparator = "lt"
)

// AlertSeverity 是等级（规则与事件共用同一枚举）。
type AlertSeverity string

const (
	AlertSeverityInfo     AlertSeverity = "info"
	AlertSeverityWarning  AlertSeverity = "warning"
	AlertSeverityCritical AlertSeverity = "critical"
)

// AlertEventStatus 是事件状态。firing 至 resolved 单向。
type AlertEventStatus string

const (
	AlertEventFiring   AlertEventStatus = "firing"
	AlertEventResolved AlertEventStatus = "resolved"
)

// NotifyStatus 是通知投递状态。
type NotifyStatus string

const (
	NotifyNone    NotifyStatus = "none"    // 未配置渠道/未到投递
	NotifyPending NotifyStatus = "pending" // 已入队
	NotifySent    NotifyStatus = "sent"
	NotifyFailed  NotifyStatus = "failed"  // 重试耗尽
	NotifySkipped NotifyStatus = "skipped" // 静默期
)

// NotificationKind 是通知渠道类型（凭据加密存 notification_channels.config_enc）。
type NotificationKind string

const (
	NotificationWebhook  NotificationKind = "webhook"
	NotificationFeishu   NotificationKind = "feishu"
	NotificationDingtalk NotificationKind = "dingtalk"
	NotificationEmail    NotificationKind = "email"
)

// ServerMetric 是单条指标点（server_metrics 行 / 时间序列 API 元素同形）。
type ServerMetric struct {
	ServerID  string    `json:"server_id"`
	CreatedAt time.Time `json:"created_at"`
	CPU       float64   `json:"cpu_pct"`
	Mem       float64   `json:"mem_pct"`
	Disk      float64   `json:"disk_pct"`
	Load1     float64   `json:"load_1"`
	NetIn     int64     `json:"net_in_bps"`
	NetOut    int64     `json:"net_out_bps"`
}

// ServerMetricLatest 是每台最新值（server_metrics_latest 行）。overview 与
// 告警评估用；ServerName 由 JOIN servers.name 填充（删除后空串）。
type ServerMetricLatest struct {
	ServerID   string    `json:"server_id"`
	ServerName string    `json:"server_name"`
	CreatedAt  time.Time `json:"created_at"`
	CPU        float64   `json:"cpu_pct"`
	Mem        float64   `json:"mem_pct"`
	Disk       float64   `json:"disk_pct"`
	Load1      float64   `json:"load_1"`
	NetIn      int64     `json:"net_in_bps"`
	NetOut     int64     `json:"net_out_bps"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// AlertRule 是告警规则。
type AlertRule struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Metric          AlertMetric     `json:"metric"`
	Comparator      AlertComparator `json:"comparator"`
	Threshold       float64         `json:"threshold"`
	DurationWindows int             `json:"duration_windows"` // 1..30 个采集点
	CooldownSeconds int             `json:"cooldown_seconds"` // 60..86400
	Severity        AlertSeverity   `json:"severity"`
	SilentUntil     *time.Time      `json:"silent_until"`
	NotifyChannelID *string         `json:"notify_channel_id"`
	Enabled         bool            `json:"enabled"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// NotificationChannel 是通知渠道。ConfigEncrypted 不出 API；ConfigHint 为
// 非敏感掩码提示。
type NotificationChannel struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Kind            NotificationKind `json:"kind"`
	ConfigEncrypted string           `json:"-"`
	ConfigHint      string           `json:"config_hint"`
	HasConfig       bool             `json:"has_config"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// AlertEvent 是告警事件。ServerName/RuleName/RuleSeverity 由 JOIN 填充；
// Metric/Severity 为触发时快照。
type AlertEvent struct {
	ID             int64            `json:"id"`
	RuleID         string           `json:"rule_id"`
	RuleName       string           `json:"rule_name"` // JOIN alert_rules.name
	ServerID       string           `json:"server_id"`
	ServerName     string           `json:"server_name"` // JOIN servers.name
	Metric         AlertMetric      `json:"metric"`      // JOIN alert_rules.metric
	Status         AlertEventStatus `json:"status"`
	Severity       AlertSeverity    `json:"severity"`
	Value          float64          `json:"value"`
	Message        string           `json:"message"`
	FiredAt        time.Time        `json:"fired_at"`
	ResolvedAt     *time.Time       `json:"resolved_at"`
	LastNotifiedAt *time.Time       `json:"last_notified_at"`
	NotifyStatus   NotifyStatus     `json:"notify_status"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

// ---------- 五期 M2（AI 自诊断 / 定时巡检）----------

// DiagnosisStatus 是诊断会话状态机
// pending→diagnosing→report→awaiting_fix→done|expired；failed 为执行错误
// 终态（契约 2.10 约定 3）。
type DiagnosisStatus string

const (
	DiagnosisPending     DiagnosisStatus = "pending"
	DiagnosisDiagnosing  DiagnosisStatus = "diagnosing"
	DiagnosisReport      DiagnosisStatus = "report"
	DiagnosisAwaitingFix DiagnosisStatus = "awaiting_fix"
	DiagnosisDone        DiagnosisStatus = "done"
	DiagnosisExpired     DiagnosisStatus = "expired"
	DiagnosisFailed      DiagnosisStatus = "failed"
)

// DiagnosisTrigger 是诊断触发来源。
type DiagnosisTrigger string

const (
	DiagnosisTriggerAlert  DiagnosisTrigger = "alert"  // 告警自动（settings 开关）
	DiagnosisTriggerManual DiagnosisTrigger = "manual" // 页面
	DiagnosisTriggerChat   DiagnosisTrigger = "chat"   // 对话 run_diagnosis 工具
)

// InspectionTemplate 是巡检模板（命令清单与评分规则见契约 7.7）。
type InspectionTemplate string

const (
	InspectionBasic InspectionTemplate = "basic"
	InspectionFull  InspectionTemplate = "full"
)

// InspectionTrigger 是巡检触发来源。
type InspectionTrigger string

const (
	InspectionTriggerCron   InspectionTrigger = "cron"
	InspectionTriggerManual InspectionTrigger = "manual"
	InspectionTriggerChat   InspectionTrigger = "chat"
)

// DiagnosisSession 是诊断会话。Report 见契约 5.16 DiagnosisReport schema。
type DiagnosisSession struct {
	ID             string           `json:"id"`
	AlertEventID   *int64           `json:"alert_event_id"`
	ServerID       string           `json:"server_id"`
	ServerName     string           `json:"server_name"` // JOIN
	ConversationID *string          `json:"conversation_id"`
	Trigger        DiagnosisTrigger `json:"trigger"`
	Status         DiagnosisStatus  `json:"status"`
	Report         json.RawMessage  `json:"report"` // null 或 DiagnosisReport 对象
	ErrorCode      string           `json:"error_code"`
	StartedAt      *time.Time       `json:"started_at"`
	FinishedAt     *time.Time       `json:"finished_at"`
	ExpiresAt      time.Time        `json:"expires_at"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

// InspectionJob 是巡检任务。
type InspectionJob struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	Cron            string             `json:"cron"`        // 五段，UTC
	TargetTags      []string           `json:"target_tags"` // 空=全部 device_kind=server
	Template        InspectionTemplate `json:"template"`
	NotifyEnabled   bool               `json:"notify_enabled"`
	NotifyChannelID *string            `json:"notify_channel_id"`
	Enabled         bool               `json:"enabled"`
	LastRunAt       *time.Time         `json:"last_run_at"`
	NextRunAt       *time.Time         `json:"next_run_at"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

// InspectionReport 是一次巡检运行。Items 由 Get 装配。
type InspectionReport struct {
	ID           string            `json:"id"`
	JobID        *string           `json:"job_id"`   // null=即时巡检（无 job）
	JobName      string            `json:"job_name"` // JOIN；即时巡检为空串
	Trigger      InspectionTrigger `json:"trigger"`
	ServerCount  int               `json:"server_count"`
	AvgScore     int               `json:"avg_score"`
	AnomalyCount int               `json:"anomaly_count"`
	StartedAt    time.Time         `json:"started_at"`
	FinishedAt   *time.Time        `json:"finished_at"`
	CreatedAt    time.Time         `json:"created_at"`
}

// InspectionReportItem 是单台巡检明细。Anomalies 为 [{item,detail,severity}]
// 数组（见契约 7.7）。
type InspectionReportItem struct {
	ID           int64           `json:"id"`
	ReportID     string          `json:"report_id"`
	ServerID     string          `json:"server_id"`
	ServerName   string          `json:"server_name"` // JOIN
	Score        int             `json:"score"`
	AnomalyCount int             `json:"anomaly_count"`
	Anomalies    json.RawMessage `json:"anomalies"`
	RawOutput    string          `json:"raw_output"` // 截断后
	// Command 是模板展开后实际执行的命令清单（换行分隔，按执行顺序；
	// 六期打磨 sprint 1，迁移 00011）。空串=00011 之前的旧行未记录。
	Command    string    `json:"command"`
	Truncated  bool      `json:"truncated"`
	DurationMS int64     `json:"duration_ms"`
	ErrorCode  string    `json:"error_code"`
	CreatedAt  time.Time `json:"created_at"`
}

// ---------- 五期 M3（批量操作）----------

// BatchStatus 是批量 job 状态（契约 3.1）。pending=已建未分派；running=
// 分派中；全部台终态后 done（无失败）或 partial（含失败/拒绝/跳过）；
// failed=编排错误；cancelled=取消。
type BatchStatus string

const (
	BatchPending   BatchStatus = "pending"
	BatchRunning   BatchStatus = "running"
	BatchPartial   BatchStatus = "partial"
	BatchDone      BatchStatus = "done"
	BatchFailed    BatchStatus = "failed"
	BatchCancelled BatchStatus = "cancelled"
)

// BatchResultStatus 是单台结果状态（契约 3.1）。awaiting_approval→
// executed|failed|denied|expired；forbidden 与服务器不存在 → skipped；
// 未处理 → cancelled（契约 2.10 约定 5）。
type BatchResultStatus string

const (
	BatchResultPending          BatchResultStatus = "pending"
	BatchResultAwaitingApproval BatchResultStatus = "awaiting_approval"
	BatchResultExecuted         BatchResultStatus = "executed"
	BatchResultFailed           BatchResultStatus = "failed"
	BatchResultDenied           BatchResultStatus = "denied"
	BatchResultExpired          BatchResultStatus = "expired"
	BatchResultSkipped          BatchResultStatus = "skipped"
	BatchResultCancelled        BatchResultStatus = "cancelled"
)

// BatchSource 是批量 job 来源。
type BatchSource string

const (
	BatchSourceConsole BatchSource = "console"
	BatchSourceChat    BatchSource = "chat"
)

// BatchJob 是批量 job（契约 3.2）。TotalCount/DoneCount 由 batch_results
// 聚合派生（store 查询时 COUNT，不落列）；Results 由 Get/List 装配
// （List 摘要、Get 全量）。
type BatchJob struct {
	ID             string      `json:"id"`
	Description    string      `json:"description"` // 自然语言原文
	Command        string      `json:"command"`
	ServerIDs      []string    `json:"server_ids"` // 1..50
	Source         BatchSource `json:"source"`
	ConversationID *string     `json:"conversation_id"`
	Status         BatchStatus `json:"status"`
	TotalCount     int         `json:"total_count"`
	DoneCount      int         `json:"done_count"` // 终态台数（含 skipped/denied 等）
	FinishedAt     *time.Time  `json:"finished_at"`
	CreatedBy      *string     `json:"created_by"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

// BatchResult 是单台结果（契约 3.2）。ServerName JOIN 填充；服务器删除后
// 空串。Output 截断发生在 batchx（256KB），store 不二次截断。
type BatchResult struct {
	ID         string            `json:"id"`
	BatchJobID string            `json:"batch_job_id"`
	ServerID   string            `json:"server_id"`
	ServerName string            `json:"server_name"` // JOIN；服务器删除后空串
	RiskLevel  string            `json:"risk_level"`  // safe|needs_approval|forbidden
	RuleID     string            `json:"rule_id"`
	ApprovalID *string           `json:"approval_id"`
	Status     BatchResultStatus `json:"status"`
	// EditedCommand 是该台审批的用户改写命令（六期打磨 sprint 5，契约 2.11
	// 约定 3）：查询时经 batch_results.approval_id 关联 approvals 读出；
	// 空串=未改写或无审批（safe 直执行）。批量详情据此显示「实际执行：
	// <改写命令>」——此前已执行行只显示 job 的原文命令，用户看不出真正
	// 跑的是什么（API 侧 output/approval.edited_command 才是真相）。
	EditedCommand string    `json:"edited_command"`
	ExitCode      *int      `json:"exit_code"`
	Output        string    `json:"output"` // 截断后
	Truncated     bool      `json:"truncated"`
	ErrorCode     string    `json:"error_code"`
	DurationMS    int64     `json:"duration_ms"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ---------- 八期（BE-R 回放目录）----------

// ReplayCatalogItem 是回放目录单行（契约 2.12 约定 4 / 5.19.1）。计数由
// ListReplayCatalog 的 LATERAL 聚合派生（不落列、不由前端拼装）。
// ActionCount 只统计动作类工具（agent.ReplayActionTools，7.8）——含
// failed/denied 的调用（它们是「发生过的动作事实」）。
type ReplayCatalogItem struct {
	ConversationID string    `json:"conversation_id"`
	Title          string    `json:"title"`
	LastMessageAt  time.Time `json:"last_message_at"`
	ToolCount      int       `json:"tool_count"`
	ActionCount    int       `json:"action_count"`
	ApprovalCount  int       `json:"approval_count"`
	HasActions     bool      `json:"has_actions"` // ActionCount > 0
	Truncated      bool      `json:"truncated"`   // ToolCount+ApprovalCount > 20000
}
