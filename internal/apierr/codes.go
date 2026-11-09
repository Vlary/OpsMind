// Package apierr 是错误码与统一 JSON 错误输出的唯一实现。
package apierr

// 错误码（CONTRACT.md 9.6）。不得自行新增或改名。
const (
	AuthRequired       = "AUTH_REQUIRED"
	AuthInvalid        = "AUTH_INVALID"
	AuthRateLimited    = "AUTH_RATE_LIMITED"
	OriginForbidden    = "ORIGIN_FORBIDDEN"
	ValidationFailed   = "VALIDATION_FAILED"
	NotFound           = "NOT_FOUND"
	Conflict           = "CONFLICT"
	Internal           = "INTERNAL"
	ServiceUnavailable = "SERVICE_UNAVAILABLE"

	TooManyStreams = "TOO_MANY_STREAMS"
	StreamActive   = "STREAM_ACTIVE"

	ConversationNotFound    = "CONVERSATION_NOT_FOUND"
	ConversationModelLocked = "CONVERSATION_MODEL_LOCKED"
	NoProviderConfigured    = "NO_PROVIDER_CONFIGURED"
	ProviderNotFound        = "PROVIDER_NOT_FOUND"
	ProviderTestFailed      = "PROVIDER_TEST_FAILED"
	ProviderModelsFailed    = "PROVIDER_MODELS_FAILED"

	ServerNotFound    = "SERVER_NOT_FOUND"
	ServerTestFailed  = "SERVER_TEST_FAILED"
	SSHHostKeyChanged = "SSH_HOST_KEY_CHANGED"
	SSHTimeout        = "SSH_TIMEOUT"
	SSHExecFailed     = "SSH_EXEC_FAILED"
	CommandTooLarge   = "COMMAND_TOO_LARGE"
	CommandInvalid    = "COMMAND_INVALID"
	CommandForbidden  = "COMMAND_FORBIDDEN"

	DatabaseNotFound   = "DATABASE_NOT_FOUND"
	DatabaseTestFailed = "DATABASE_TEST_FAILED"
	SQLNotReadOnly     = "SQL_NOT_READONLY"
	SQLTimeout         = "SQL_TIMEOUT"
	SQLQueryFailed     = "SQL_QUERY_FAILED"

	ApprovalNotFound   = "APPROVAL_NOT_FOUND"
	ApprovalNotPending = "APPROVAL_NOT_PENDING"
	ApprovalDenied     = "APPROVAL_DENIED"
	ApprovalTimeout    = "APPROVAL_TIMEOUT"
	// ApprovalCancelled 表示审批因回答流中断被连带取消（六期打磨 sprint 3）。
	// 只作为工具结果/tool_calls.error_code 的终态原因，不映射 HTTP 状态码
	//（用户不会再对一条已随流断开的审批发起请求）；与 SSE 通道码
	// APPROVAL_STREAM_CANCELLED 区分——后者描述响应流本身。
	ApprovalCancelled = "APPROVAL_CANCELLED"

	ToolNotFound       = "TOOL_NOT_FOUND"
	ToolArgsInvalid    = "TOOL_ARGS_INVALID"
	AgentMaxIterations = "AGENT_MAX_ITERATIONS"
	LLMUpstreamError   = "LLM_UPSTREAM_ERROR"
	LLMTimeout         = "LLM_TIMEOUT"
	LLMOutputTruncated = "LLM_OUTPUT_TRUNCATED"
	LLMStreamError     = "LLM_STREAM_ERROR"

	AuditWriteFailed = "AUDIT_WRITE_FAILED"

	// 仅用于 SSE，不映射 HTTP 状态码。
	StreamTimeout           = "STREAM_TIMEOUT"
	ServerShuttingDown      = "SERVER_SHUTTING_DOWN"
	ApprovalStreamCancelled = "APPROVAL_STREAM_CANCELLED"
	ClientDisconnected      = "CLIENT_DISCONNECTED"

	// W3：终端会话与录制（CONTRACT.md 9.6）。
	TermSessionNotFound  = "TERM_SESSION_NOT_FOUND"
	TermSessionExists    = "TERM_SESSION_EXISTS"
	TermSessionLimit     = "TERM_SESSION_LIMIT"
	TermConnectFailed    = "TERM_CONNECT_FAILED"
	TermUnsupported      = "TERM_UNSUPPORTED"
	TermWSProtocol       = "TERM_WS_PROTOCOL"
	RecordingNotFound    = "RECORDING_NOT_FOUND"
	RecordingUnavailable = "RECORDING_UNAVAILABLE"

	// W4：RDP 网关（CONTRACT.md 9.6）。
	RdpSessionNotFound = "RDP_SESSION_NOT_FOUND"
	RdpSessionExists   = "RDP_SESSION_EXISTS"
	RdpSessionLimit    = "RDP_SESSION_LIMIT"
	RdpConnectFailed   = "RDP_CONNECT_FAILED"
	RdpUnsupported     = "RDP_UNSUPPORTED"
	RdpNoCredentials   = "RDP_NO_CREDENTIALS"
	GuacdUnavailable   = "GUACD_UNAVAILABLE"

	// N1：网络设备 CLI（CONTRACT.md 9.6）。
	NetCLILoginFailed = "NETCLI_LOGIN_FAILED" // 网络设备登录失败（凭据错误/提示符未出现）
	NetCLIExecFailed  = "NETCLI_EXEC_FAILED"  // 网络设备命令执行失败（expect 超时/通道断开/错误指示）

	// C1 四期：容器管理（CONTRACT.md 9.6）。
	ContainerUnsupported = "CONTAINER_UNSUPPORTED" // 目标服务器无受支持的容器管理面（network_device、runtime none/''/containerd）
	ContainerNotFound    = "CONTAINER_NOT_FOUND"   // 容器不存在（inspect 空数组/logs No such container）
	ContainerCLIFailed   = "CONTAINER_CLI_FAILED"  // 容器 CLI 执行失败（daemon 停/命令 exit≠0）

	// 五期 M1：监控告警（CONTRACT.md 9.6）。
	AlertRuleNotFound           = "ALERT_RULE_NOT_FOUND"           // 告警规则不存在
	AlertEventNotFound          = "ALERT_EVENT_NOT_FOUND"          // 告警事件不存在
	NotificationChannelNotFound = "NOTIFICATION_CHANNEL_NOT_FOUND" // 通知渠道不存在
	NotificationConfigInvalid   = "NOTIFICATION_CONFIG_INVALID"    // 渠道配置 schema 校验失败
	NotificationTestFailed      = "NOTIFICATION_TEST_FAILED"       // 渠道测试投递失败

	// 五期 M2：AI 自诊断 / 定时巡检（CONTRACT.md 9.6）。
	DiagnosisSessionNotFound = "DIAGNOSIS_SESSION_NOT_FOUND" // 诊断会话不存在
	DiagnosisBusy            = "DIAGNOSIS_BUSY"              // 诊断并发/队列满
	InspectionJobNotFound    = "INSPECTION_JOB_NOT_FOUND"    // 巡检任务不存在
	InspectionReportNotFound = "INSPECTION_REPORT_NOT_FOUND" // 巡检报告不存在
	CronInvalid              = "CRON_INVALID"                // cron 表达式非法

	// 五期 M3：批量操作（CONTRACT.md 9.6）。
	BatchJobNotFound    = "BATCH_JOB_NOT_FOUND"    // 批量任务不存在
	BatchTooManyTargets = "BATCH_TOO_MANY_TARGETS" // 批量目标超过 50 台
)
