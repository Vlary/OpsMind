// severity.go 实现六期 BE-P 的 needs_approval 规则严重度分档
// （CONTRACT.md 7.3E）：high 门限需要在 needs_approval 内部区分
// 「高危（high）」与「中危（warning）」——warning 在 high/none 档 +
// auto_exec_enabled=true 时免审自动执行，high 仍审批。
//
// 归类原则（契约写死）：warning = 只读或被动观察面（读取配置/日志/
// 环境、进程信号、模式切换——命令本身不改持久状态）；high = 一切变更
// 与未知（未列即 high，失败关闭）。safe 与 forbidden 规则不参与本表。
package risk

// Severity 是 needs_approval 规则的严重度（契约 7.3E）。
type Severity string

const (
	// SeverityWarning 是中危（只读/被动观察面）。
	SeverityWarning Severity = "warning"
	// SeverityHigh 是高危（变更与未知；默认档，失败关闭）。
	SeverityHigh Severity = "high"
)

// warningRules 是 SeverityOf 返回 warning 的完整 RuleID 集（契约 7.3E
// 表格逐条落码；跨 Linux/Windows/网络设备三分册）。网络设备五前缀按
// 代码事实 NET_JUNOS_（rules_netdev.go 的 junos 分册前缀）。
var warningRules = map[string]struct{}{}

// netdevPrefixes 是网络设备分册的 RuleID 前缀（与 rules_netdev.go 一致）。
var netdevPrefixes = [...]string{
	"NET_CISCO_",
	"NET_HUAWEI_",
	"NET_H3C_",
	"NET_JUNOS_",
	"NET_GENERIC_",
}

func init() {
	for _, id := range []string{
		// Linux 分册（7.3）。
		"ENV_DUMP",
		"PROCESS_SIGNAL",
		"CONTAINER_LOGS",
		// Windows 分册（7.3B）。
		"WIN_ENV_DUMP",
		"WIN_PROCESS_CONTROL",
		"WIN_CONTAINER_LOGS",
	} {
		warningRules[id] = struct{}{}
	}
	// 网络设备分册（7.3C）：<P>CONFIG_DUMP / <P>ENABLE_MODE /
	// <P>CONF_MODE / <P>CAPTURE（五前缀 × 四语义 = 20 个 ID）。
	for _, p := range netdevPrefixes {
		for _, suffix := range [...]string{"CONFIG_DUMP", "ENABLE_MODE", "CONF_MODE", "CAPTURE"} {
			warningRules[p+suffix] = struct{}{}
		}
	}
}

// SeverityOf 返回 needs_approval 规则的严重度（契约 7.3E）：warning 集
// 精确匹配，未命中一律 high（失败关闭）。纯静态 map 查找，无 IO 无锁
// （契约 11 节第 18 条）。safe/forbidden 规则的调用方不得使用其返回值
// （契约 7.2：severity 仅在 level==needs_approval 时参与门限判定）。
func SeverityOf(ruleID string) Severity {
	if _, ok := warningRules[ruleID]; ok {
		return SeverityWarning
	}
	return SeverityHigh
}
