package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aiops/internal/llm"
	"aiops/internal/risk"
)

// 工具名固定 11 个。
const (
	ToolListServers         = "list_servers"
	ToolServerFacts         = "server_facts"
	ToolSSHExec             = "ssh_exec"
	ToolListDatabases       = "list_databases"
	ToolDBQuery             = "db_query"
	ToolAskUser             = "ask_user"
	ToolContainerList       = "container_list"        // C1 四期
	ToolContainerInspect    = "container_inspect"     // C1 四期
	ToolContainerAction     = "container_action"      // C1 四期
	ToolContainerLogs       = "container_logs"        // C1 四期
	ToolImageList           = "image_list"            // C1 四期
	ToolGetMetrics          = "get_metrics"           // M1 五期
	ToolListAlerts          = "list_alerts"           // M1 五期
	ToolAuditSearch         = "audit_search"          // M1/M3 五期
	ToolRunDiagnosis        = "run_diagnosis"         // M2 五期
	ToolRunInspection       = "run_inspection"        // M2 五期
	ToolGetInspectionReport = "get_inspection_report" // M2 五期
	ToolBatchExecute        = "batch_execute"         // M3 五期
	MaxCommandBytes         = 16384
	MaxSQLBytes             = 65536
	MaxOutputBytes          = 262144
	DefaultSSHTimeoutS      = 60
	MaxSSHTimeoutS          = 300
	// 容器 CLI 超时与探测超时（契约 7.2：探测 15s；CLI 60s）。
	ContainerCLITimeout  = 60 * time.Second
	ContainerDetectLimit = 15 * time.Second
	// 容器日志 tail 范围（契约 7.2：tail 1..1000，缺省 200）。
	ContainerLogsDefaultTail = 200
	ContainerLogsMaxTail     = 1000

	// 上下文构造时 tool_calls 的分页参数：每页条数与最多翻页数，
	// 上限防止极端长会话下查询与内存开销失控（见 buildMessages）。
	contextToolCallPageSize = 200
	contextToolCallMaxPages = 5
)

// replayActionToolNames 是八期动作类工具的**唯一**名单（契约 7.8 写死）：
// 会触达真实系统（远端主机 / 网络设备 / 外部数据库 / 编排引擎）的 11 个
// 工具。回放据此为它们生成 execution 步；其余 7 个纯本地查询/交互工具
// （list_servers / list_databases / get_metrics / list_alerts /
// audit_search / get_inspection_report / ask_user）只有 tool_call 步。
//
// 禁止在 httpapi（或任何其它包）复制这份名单：判据与回放目录 SQL 的
// $action_tools 参数都必须来自 ReplayActionTools()（契约 2.12 约定 3 /
// 5.19.1 / 11 节第 22 条）。
var replayActionToolNames = []string{
	ToolSSHExec,
	ToolServerFacts,
	ToolContainerAction,
	ToolContainerList,
	ToolContainerInspect,
	ToolContainerLogs,
	ToolImageList,
	ToolDBQuery,
	ToolBatchExecute,
	ToolRunDiagnosis,
	ToolRunInspection,
}

// ReplayActionTools 返回动作类工具的共享清单（契约 7.8 的 11 项，顺序与
// 表 7.8 一致）。返回副本，调用方修改不影响包内名单。
func ReplayActionTools() []string {
	out := make([]string, len(replayActionToolNames))
	copy(out, replayActionToolNames)
	return out
}

// ToolDefinitions 返回发送给模型的固定 11 个工具。
// 所有参数 schema 均为 type=object、additionalProperties=false。
func ToolDefinitions() []llm.Tool {
	return []llm.Tool{
		{
			Name:        ToolListServers,
			Description: "列出已配置的服务器与网络设备元数据（含 os/device_kind/netdev_brand/access_proto/container_runtime），不包含凭据。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`),
		},
		{
			Name:        ToolServerFacts,
			Description: "采集指定资产的系统事实（Linux/Windows/网络设备各有固定模板：主机名、OS/软件版本、接口、内存、运行时长等；Linux/Windows 另含容器运行时探测结果 container_runtime）。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"server_id":{"type":"string","minLength":1,"maxLength":64}},"required":["server_id"]}`),
		},
		{
			Name:        ToolSSHExec,
			Description: "在指定服务器或网络设备执行一条命令（Linux：登录 shell；Windows：PowerShell；网络设备：单条 CLI 行）。高风险命令会要求人工审批。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"server_id":{"type":"string","minLength":1,"maxLength":64},"command":{"type":"string","minLength":1,"maxLength":16384},"timeout_seconds":{"type":"integer","minimum":1,"maximum":300}},"required":["server_id","command"]}`),
		},
		{
			Name:        ToolListDatabases,
			Description: "列出已配置的外部数据库元数据（含 engine），不包含凭据。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`),
		},
		{
			Name:        ToolDBQuery,
			Description: "在指定外部数据库执行一条只读 SQL。写语句会被拒绝且不会执行。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"database_id":{"type":"string","minLength":1,"maxLength":64},"sql":{"type":"string","minLength":1,"maxLength":65536}},"required":["database_id","sql"]}`),
		},
		{
			Name:        ToolAskUser,
			Description: "向用户提出一个澄清问题并结束本轮；用户下一条消息即回答。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"question":{"type":"string","minLength":1,"maxLength":2000}},"required":["question"]}`),
		},
		{
			Name:        ToolContainerList,
			Description: "列出指定服务器上的全部容器（docker/podman，含状态与端口）。仅 container_runtime=docker/podman 的服务器可用。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"server_id":{"type":"string","minLength":1,"maxLength":64}},"required":["server_id"]}`),
		},
		{
			Name:        ToolContainerInspect,
			Description: "查看指定容器的详情摘要（状态、exit_code、PID、启停时间、健康检查、挂载）。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"server_id":{"type":"string","minLength":1,"maxLength":64},"container_id":{"type":"string","minLength":1,"maxLength":128}},"required":["server_id","container_id"]}`),
		},
		{
			Name:        ToolContainerAction,
			Description: "对指定容器执行 start/stop/restart。start 为 safe；stop/restart 需要人工审批。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"server_id":{"type":"string","minLength":1,"maxLength":64},"container_id":{"type":"string","minLength":1,"maxLength":128},"action":{"type":"string","enum":["start","stop","restart"]}},"required":["server_id","container_id","action"]}`),
		},
		{
			Name:        ToolContainerLogs,
			Description: "读取指定容器最近 N 行日志（tail 1..1000，缺省 200）。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"server_id":{"type":"string","minLength":1,"maxLength":64},"container_id":{"type":"string","minLength":1,"maxLength":128},"tail":{"type":"integer","minimum":1,"maximum":1000}},"required":["server_id","container_id"]}`),
		},
		{
			Name:        ToolImageList,
			Description: "列出指定服务器上的本地镜像（repo/tag/size/created）。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"server_id":{"type":"string","minLength":1,"maxLength":64}},"required":["server_id"]}`),
		},
		{
			Name:        ToolGetMetrics,
			Description: "查询指定服务器的历史监控指标（cpu/mem/disk/load/net，60s 粒度）。minutes 15..1440 缺省 60。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"server_id":{"type":"string","minLength":1,"maxLength":64},"minutes":{"type":"integer","minimum":15,"maximum":1440},"metric":{"type":"string","enum":["cpu_pct","mem_pct","disk_pct","load_1","net_in_bps","net_out_bps"]}},"required":["server_id"]}`),
		},
		{
			Name:        ToolListAlerts,
			Description: "查询告警事件（可按状态/等级过滤，含规则名与触发值）。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"status":{"type":"string","enum":["firing","resolved"]},"severity":{"type":"string","enum":["info","warning","critical"]},"limit":{"type":"integer","minimum":1,"maximum":200}}}`),
		},
		{
			Name:        ToolAuditSearch,
			Description: "检索审计日志（如「web-01 上周做过什么」：按服务器/动作/资源/时间过滤）。时间参数用 ISO8601。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"server_id":{"type":"string","minLength":1,"maxLength":64},"action":{"type":"string","minLength":1,"maxLength":100},"resource_type":{"type":"string","minLength":1,"maxLength":100},"from":{"type":"string","format":"date-time"},"to":{"type":"string","format":"date-time"},"limit":{"type":"integer","minimum":1,"maximum":200}}}`),
		},
		{
			Name:        ToolBatchExecute,
			Description: "对多台服务器（1..50）批量执行同一条命令。内部逐台风险分级：forbidden 跳过、safe 且已开自动执行直接执行、其余逐台创建人工审批（用户在批量页决定）。返回逐台分派概要，不等待审批。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"server_ids":{"type":"array","items":{"type":"string","minLength":1,"maxLength":64},"minItems":1,"maxItems":50},"command":{"type":"string","minLength":1,"maxLength":16384},"description":{"type":"string","minLength":0,"maxLength":2000}},"required":["server_ids","command"]}`),
		},
		{
			Name:        ToolRunDiagnosis,
			Description: "对指定服务器发起一次 AI 自诊断（内部只执行 safe 只读命令，产出原因分析/证据/修复建议；修复需另行审批）。可关联告警事件。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"server_id":{"type":"string","minLength":1,"maxLength":64},"alert_event_id":{"type":"integer","minimum":1}},"required":["server_id"]}`),
		},
		{
			Name:        ToolRunInspection,
			Description: "对指定服务器立即执行一次巡检（basic/full 模板，返回评分与异常项；目标 1..50 台）。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"server_ids":{"type":"array","items":{"type":"string","minLength":1,"maxLength":64},"minItems":1,"maxItems":50},"template":{"type":"string","enum":["basic","full"]}},"required":["server_ids"]}`),
		},
		{
			Name:        ToolGetInspectionReport,
			Description: "查询巡检报告详情（per-server 评分、异常项、与上次对比）。report_id 与 limit 二选一，都给时以 report_id 为准。",
			Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"report_id":{"type":"string","minLength":1,"maxLength":64},"limit":{"type":"integer","minimum":1,"maximum":20}}}`),
		},
	}
}

// ToolNames 返回所有合法工具名。
func ToolNames() []string {
	return []string{
		ToolListServers, ToolServerFacts, ToolSSHExec, ToolListDatabases, ToolDBQuery, ToolAskUser,
		ToolContainerList, ToolContainerInspect, ToolContainerAction, ToolContainerLogs, ToolImageList,
		ToolGetMetrics, ToolListAlerts, ToolAuditSearch,
		ToolRunDiagnosis, ToolRunInspection, ToolGetInspectionReport, ToolBatchExecute,
	}
}

func knownTool(name string) bool {
	for _, n := range ToolNames() {
		if n == name {
			return true
		}
	}
	return false
}

type listServersArgs struct{}

type serverFactsArgs struct {
	ServerID string `json:"server_id"`
}

type sshExecArgs struct {
	ServerID       string `json:"server_id"`
	Command        string `json:"command"`
	TimeoutSeconds *int   `json:"timeout_seconds,omitempty"`
}

type listDatabasesArgs struct{}

type dbQueryArgs struct {
	DatabaseID string `json:"database_id"`
	SQL        string `json:"sql"`
}

type askUserArgs struct {
	Question string `json:"question"`
}

// C1 四期：容器工具参数。
type containerServerArgs struct {
	ServerID string `json:"server_id"`
}

type containerIDArgs struct {
	ServerID    string `json:"server_id"`
	ContainerID string `json:"container_id"`
}

type containerActionArgs struct {
	ServerID    string `json:"server_id"`
	ContainerID string `json:"container_id"`
	Action      string `json:"action"`
}

type containerLogsArgs struct {
	ServerID    string `json:"server_id"`
	ContainerID string `json:"container_id"`
	Tail        *int   `json:"tail,omitempty"`
}

// M1 五期：监控告警工具参数。
type getMetricsArgs struct {
	ServerID string  `json:"server_id"`
	Minutes  *int    `json:"minutes,omitempty"`
	Metric   *string `json:"metric,omitempty"`
}

type listAlertsArgs struct {
	Status   *string `json:"status,omitempty"`
	Severity *string `json:"severity,omitempty"`
	Limit    *int    `json:"limit,omitempty"`
}

type auditSearchArgs struct {
	ServerID     *string `json:"server_id,omitempty"`
	Action       *string `json:"action,omitempty"`
	ResourceType *string `json:"resource_type,omitempty"`
	From         *string `json:"from,omitempty"`
	To           *string `json:"to,omitempty"`
	Limit        *int    `json:"limit,omitempty"`
}

// M3 五期：批量执行工具参数（server_ids 1..50；description 记录自然语言
// 意图，契约 7.1）。
type batchExecuteArgs struct {
	ServerIDs   []string `json:"server_ids"`
	Command     string   `json:"command"`
	Description string   `json:"description,omitempty"`
}

// M2 五期：诊断/巡检工具参数。
type runDiagnosisArgs struct {
	ServerID     string `json:"server_id"`
	AlertEventID *int64 `json:"alert_event_id,omitempty"`
}

type runInspectionArgs struct {
	ServerIDs []string `json:"server_ids"`
	Template  *string  `json:"template,omitempty"`
}

type getInspectionReportArgs struct {
	ReportID *string `json:"report_id,omitempty"`
	Limit    *int    `json:"limit,omitempty"`
}

// decodeStrictArgs 解析工具参数：拒绝未知字段与尾随内容，与 additionalProperties=false 一致。
func decodeStrictArgs(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("unexpected trailing data")
	}
	return nil
}

// truncateResult 保证写入 tool_calls.result 与 SSE 的结果不超过 max 字节且保持合法 JSON。
func truncateResult(raw json.RawMessage, max int) json.RawMessage {
	if max <= 0 || len(raw) <= max {
		return raw
	}
	preview := raw
	limit := max / 2
	if limit > len(raw) {
		limit = len(raw)
	}
	preview = raw[:limit]
	wrapped, err := json.Marshal(map[string]any{
		"truncated":      true,
		"original_bytes": len(raw),
		"preview":        string(preview),
	})
	if err != nil {
		return json.RawMessage(`{"truncated":true}`)
	}
	return wrapped
}

// errorMessageMaxRunes 是落库 error_message 的长度上限，防止底层错误
// （如驱动返回的超长串）撑爆列内容。
const errorMessageMaxRunes = 512

// truncateErrorMessage 按 rune 截断错误消息，保证不切断多字节字符。
func truncateErrorMessage(msg string) string {
	if utf8.RuneCountInString(msg) <= errorMessageMaxRunes {
		return msg
	}
	runes := []rune(msg)
	return string(runes[:errorMessageMaxRunes])
}

// errorResult 构造工具错误作为模型可见的 tool 消息内容。
func errorResult(code, message string) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
	if err != nil {
		return json.RawMessage(`{"error":{"code":"INTERNAL","message":"tool failed"}}`)
	}
	return raw
}

// serverFactsCommand 是内部固定只读命令模板（对应 risk 的 SAFE_FACTS）。
// 四期 C1（契约 7.5）：追加容器运行时探测命令（同一 SAFE_FACTS 模板集，
// risk.ContainerDetectFactsCommand 同文），探测输出（docker|podman|
// containerd|none 裸词）追加进 raw_output，由 parseContainerRuntimeTail 解析。
var serverFactsCommand = `printf 'hostname=%s\n' "$(hostname 2>/dev/null)"
os=$( ( . /etc/os-release 2>/dev/null && printf '%s' "$PRETTY_NAME" ) || uname -s )
printf 'os=%s\n' "$os"
printf 'kernel=%s\n' "$(uname -r 2>/dev/null)"
printf 'arch=%s\n' "$(uname -m 2>/dev/null)"
printf 'uptime_seconds=%s\n' "$(cut -d. -f1 /proc/uptime 2>/dev/null)"
printf 'cpu_count=%s\n' "$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null)"
printf 'load_1=%s\n' "$(awk '{print $1}' /proc/loadavg 2>/dev/null)"
printf 'load_5=%s\n' "$(awk '{print $2}' /proc/loadavg 2>/dev/null)"
printf 'load_15=%s\n' "$(awk '{print $3}' /proc/loadavg 2>/dev/null)"
printf 'memory_total_bytes=%s\n' "$(awk '/MemTotal/{print $2*1024}' /proc/meminfo 2>/dev/null)"
printf 'memory_available_bytes=%s\n' "$(awk '/MemAvailable/{print $2*1024}' /proc/meminfo 2>/dev/null)"
printf 'ip=%s\n' "$(hostname -I 2>/dev/null)"
df -Pk 2>/dev/null | awk 'NR>1{printf "disk=%s|%s|%s|%s|%s\n",$6,$1,$2*1024,$3*1024,$4*1024}'
` + risk.ContainerDetectFactsCommand()

type diskFact struct {
	Mount          string `json:"mount"`
	Filesystem     string `json:"filesystem"`
	SizeBytes      int64  `json:"size_bytes"`
	UsedBytes      int64  `json:"used_bytes"`
	AvailableBytes int64  `json:"available_bytes"`
}

type serverFacts struct {
	Hostname             string     `json:"hostname"`
	OS                   string     `json:"os"`
	Kernel               string     `json:"kernel"`
	Arch                 string     `json:"arch"`
	UptimeSeconds        int64      `json:"uptime_seconds"`
	CPUCount             int        `json:"cpu_count"`
	Load1                float64    `json:"load_1"`
	Load5                float64    `json:"load_5"`
	Load15               float64    `json:"load_15"`
	MemoryTotalBytes     int64      `json:"memory_total_bytes"`
	MemoryAvailableBytes int64      `json:"memory_available_bytes"`
	Disks                []diskFact `json:"disks"`
	IPAddresses          []string   `json:"ip_addresses"`
	// C1 四期（契约 7.5）：Linux/Windows 含 container_runtime
	//（none|docker|podman；Windows 不探测 ctr）；网络设备 facts 无此键。
	ContainerRuntime string `json:"container_runtime,omitempty"`
}

// parseServerFacts 解析固定模板输出；无法解析的项保持零值，raw_output 仍完整返回。
func parseServerFacts(output string) serverFacts {
	facts := serverFacts{Disks: []diskFact{}, IPAddresses: []string{}}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "hostname":
			facts.Hostname = value
		case "os":
			facts.OS = value
		case "kernel":
			facts.Kernel = value
		case "arch":
			facts.Arch = value
		case "uptime_seconds":
			facts.UptimeSeconds, _ = strconv.ParseInt(value, 10, 64)
		case "cpu_count":
			facts.CPUCount, _ = strconv.Atoi(value)
		case "load_1":
			facts.Load1, _ = strconv.ParseFloat(value, 64)
		case "load_5":
			facts.Load5, _ = strconv.ParseFloat(value, 64)
		case "load_15":
			facts.Load15, _ = strconv.ParseFloat(value, 64)
		case "memory_total_bytes":
			facts.MemoryTotalBytes, _ = strconv.ParseInt(value, 10, 64)
		case "memory_available_bytes":
			facts.MemoryAvailableBytes, _ = strconv.ParseInt(value, 10, 64)
		case "ip":
			for _, ip := range strings.Fields(value) {
				if ip != "" {
					facts.IPAddresses = append(facts.IPAddresses, ip)
				}
			}
		case "disk":
			parts := strings.Split(value, "|")
			if len(parts) != 5 {
				continue
			}
			d := diskFact{Mount: parts[0], Filesystem: parts[1]}
			d.SizeBytes, _ = strconv.ParseInt(parts[2], 10, 64)
			d.UsedBytes, _ = strconv.ParseInt(parts[3], 10, 64)
			d.AvailableBytes, _ = strconv.ParseInt(parts[4], 10, 64)
			facts.Disks = append(facts.Disks, d)
		}
	}
	sort.SliceStable(facts.Disks, func(i, j int) bool { return facts.Disks[i].Mount < facts.Disks[j].Mount })
	return facts
}

// ---------------------------------------------------------------------------
// Windows 执行通道支撑（W1，契约 2.7 双轨路由）
// ---------------------------------------------------------------------------

// wrapPowerShellForSSH 是 SSH 通道执行 Windows 命令时的外壳包装
// （契约 2.7.2：包装发生在 agent，sshx 保持协议层无业务语义）。
// 风险分类在包装前完成（对模型给的原始 PowerShell 命令分类）。
func wrapPowerShellForSSH(command string) string {
	return "powershell -NoProfile -NonInteractive -Command " + command
}

// winFactsCommand 返回 os=windows 的 server_facts 固定模板内部脚本
// （契约 7.5；与 risk.WIN_SAFE_FACTS 精确匹配同一常量）。
func winFactsCommand() string { return risk.WinFactsCommand }

// parseWinServerFacts 解析 Windows 模板输出的 JSON（ConvertTo-Json）；
// 键名与 Linux 版对齐（load_* 恒 null、kernel 恒空串）。无法解析的项保持
// 零值，raw_output 仍完整返回；输出可能带 BOM/尾部空白，先剥离再解析。
func parseWinServerFacts(output string) serverFacts {
	facts := serverFacts{Disks: []diskFact{}, IPAddresses: []string{}}
	trimmed := strings.TrimPrefix(strings.TrimSpace(output), "\uFEFF")
	if trimmed == "" {
		return facts
	}
	_ = json.Unmarshal([]byte(trimmed), &facts)
	if facts.Disks == nil {
		facts.Disks = []diskFact{}
	}
	if facts.IPAddresses == nil {
		facts.IPAddresses = []string{}
	}
	sort.SliceStable(facts.Disks, func(i, j int) bool { return facts.Disks[i].Mount < facts.Disks[j].Mount })
	return facts
}

// parseContainerRuntimeTail 从 Linux facts 模板输出（末尾追加了四期探测
// 命令的裸词输出）解析容器运行时：取最后一个非空行，值为
// docker|podman|containerd|none 之一才采纳（facts 行是 key=value 形态，
// 不会与裸词混淆；未识别输出返回空串=未探测）。
func parseContainerRuntimeTail(output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		switch line {
		case "docker", "podman", "containerd", "none":
			return line
		}
		return ""
	}
	return ""
}
