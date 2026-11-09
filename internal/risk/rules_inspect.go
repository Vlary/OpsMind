// rules_inspect.go 登记五期 M2 巡检器的固定模板（契约 7.7）：
// Linux `SAFE_INSPECT` / Windows `WIN_SAFE_INSPECT`，机制同 SAFE_FACTS/
// SAFE_COLLECT——整串规范化精确匹配放行 safe，任何改动走正常分类
// （失败关闭）。inspection 包 import risk 引用这些常量（M2 可 import risk，
// 模板单一来源，无需两边各持一份）。
package risk

// 巡检 CPU 核数探测命令（契约 7.7：getconf 已在 SAFE_FACTS 匹配集，
// 并入 SAFE_INSPECT）。
const InspectCPUCountCommand = "getconf _NPROCESSORS_ONLN"

// InspectBasicCommands 是 Linux basic 模板命令清单（契约 7.7 写死；逐条
// 独立命令、独立解析）。
var InspectBasicCommands = []string{
	"uptime",
	"df -B1",
	"cat /proc/loadavg",
	"grep -E '^(MemTotal|MemAvailable):' /proc/meminfo",
	"systemctl --failed --no-legend",
	InspectCPUCountCommand,
}

// InspectFullCommands 是 Linux full 模板命令清单（basic 全部 + 三条；契约
// 7.7 写死）。docker 两条仅 container_runtime=docker/podman 时执行（runner
// 侧条件追加，模板登记不变）。
var InspectFullExtraCommands = []string{
	"journalctl -p err -n 50 --no-pager",
	"docker ps --format json",
	"docker stats --no-stream --format json",
}

// WinInspectBasicCommands 是 Windows basic 模板（契约 7.7 写死）：
// ① 采集模板（同 WIN_SAFE_COLLECT，已在 winCollectTemplates 登记）；
// ② 停止的自动启动服务（前 20）。
var WinInspectBasicCommands = []string{
	`Get-Service | Where-Object {$_.Status -eq 'Stopped' -and $_.StartType -eq 'Automatic'} | Select-Object -First 20 Name,DisplayName | ConvertTo-Json -Compress`,
}

// WinInspectFullCommands 是 Windows full 模板追加命令（契约 7.7 写死）：
// Application 日志最近 20 条 error 级事件。
var WinInspectFullCommands = []string{
	`Get-WinEvent -FilterHashtable @{LogName='Application';Level=2} -MaxEvents 20 | Select-Object TimeCreated,ProviderName,Message | ConvertTo-Json -Compress`,
}

// inspectTemplates 是 SAFE_INSPECT 精确匹配的规范化形式集合（Linux 全部
// 模板命令）。
var inspectTemplates = func() map[string]bool {
	m := map[string]bool{}
	for _, cmd := range append(append([]string(nil), InspectBasicCommands...), InspectFullExtraCommands...) {
		if n, err := normalize(cmd); err == nil {
			m[n] = true
		}
	}
	return m
}()

// winInspectTemplates 是 WIN_SAFE_INSPECT 精确匹配的规范化形式集合：
// Windows 模板本体与 SSH 通道外壳包装（无引号/带引号，同 winCollectTemplates）。
var winInspectTemplates = func() map[string]bool {
	m := map[string]bool{}
	all := append(append([]string(nil), WinInspectBasicCommands...), WinInspectFullCommands...)
	for _, cmd := range all {
		if n, err := normalize(cmd); err == nil {
			m[n] = true
		}
		if n, err := normalize(`powershell -NoProfile -NonInteractive -Command ` + cmd); err == nil {
			m[n] = true
		}
		if n, err := normalize(`powershell -NoProfile -NonInteractive -Command "` + cmd + `"`); err == nil {
			m[n] = true
		}
	}
	return m
}()

func isInspectTemplate(normalized string) bool { return inspectTemplates[normalized] }
