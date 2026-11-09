// score.go 实现巡检评分规则（契约 7.7 写死，唯一实现）：初始 100 扣至 0；
// 失败服务/磁盘/负载/内存/journal 错误行/活跃告警逐项扣分；采集失败
// score=0。输出解析器全部有输入大小限制（防 OOM 铁律）。
package inspection

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// 契约 7.7 写死的扣分常量。
const (
	scoreServicePenalty    = 10 // systemctl --failed 每单元
	scoreServicePenaltyCap = 30
	scoreDiskCritical      = 15 // disk_pct > 85
	scoreDiskWarning       = 5  // 75 < disk_pct <= 85
	scoreLoadCritical      = 20 // load_1 > 2*cores
	scoreLoadWarning       = 10 // cores < load_1 <= 2*cores
	scoreMemWarning        = 10 // mem_pct > 90
	scoreJournalWarning    = 5  // journalctl err 行
	scoreAlertPenalty      = 10 // 活跃 firing 告警每条
	scoreAlertPenaltyCap   = 20
	defaultCPUCores        = 4  // 取不到核数按 4 计（契约 7.7）
	maxAlertAnomalyItems   = 10 // 活跃告警异常项上限（防 anomalies jsonb 无界）
	maxServiceAnomalyItems = 20 // 失败单元异常项上限
	journalDetailLines     = 3  // journal 异常 detail 摘要行数
	parserInputLimit       = 262144
)

// Anomaly 是异常项（anomalies jsonb 元素，契约 3.2）。
type Anomaly struct {
	Item     string `json:"item"`
	Detail   string `json:"detail"`
	Severity string `json:"severity"`
}

// alertInput 是评分输入中的活跃告警（由 runner 查库装配）。
type alertInput struct {
	ID       int64
	Severity string
	Message  string
}

// scoreInput 是单台评分输入：模板命令输出（命令串→输出）+ 活跃告警 +
// 目标 os（windows 时 outputs 为采集 JSON + 服务/事件 JSON）。
type scoreInput struct {
	IsWindows bool
	Outputs   map[string]string
	Alerts    []alertInput
	ExecErr   error // 采集失败（通道错误/超时）
}

// scoreResult 是单台评分结果（RawOutput/Truncated 由 runner 落库前填充）。
type scoreResult struct {
	Score     int
	Anomalies []Anomaly
	RawOutput string
	// Command 是模板展开后的命令清单（换行分隔；六期打磨 sprint 1，
	// 契约 8.7 ⑧ 报告「命令列」复制）。
	Command   string
	Truncated bool
}

// scoreServer 按契约 7.7 评分表计算单台得分。
func scoreServer(in scoreInput) scoreResult {
	anomalies := make([]Anomaly, 0, 8)
	if in.ExecErr != nil {
		return scoreResult{Score: 0, Anomalies: []Anomaly{{
			Item: "collect", Detail: truncateStr(in.ExecErr.Error(), 512),
			Severity: "critical",
		}}}
	}

	score := 100
	deduct := func(n int) {
		score -= n
		if score < 0 {
			score = 0
		}
	}

	var cpuCores int
	var load1, memPct, diskPct float64
	if in.IsWindows {
		cpuCores, load1, memPct, diskPct = parseWindowsMetrics(in.Outputs)
	} else {
		for cmd, out := range in.Outputs {
			out = clampParserInput(out)
			switch cmd {
			case "cat /proc/loadavg":
				load1 = parseLoadavg(out)
			case "df -B1":
				diskPct = parseMaxDiskPct(out)
			case "getconf _NPROCESSORS_ONLN":
				if n, err := strconv.Atoi(strings.TrimSpace(out)); err == nil && n > 0 {
					cpuCores = n
				}
			default:
				if strings.HasPrefix(cmd, "grep") {
					memPct = parseMeminfo(out)
				}
			}
		}
	}
	if cpuCores <= 0 {
		cpuCores = defaultCPUCores
	}

	// 磁盘（disk_pct：任一保留文件系统的最大使用率）。
	if diskPct > 85 {
		deduct(scoreDiskCritical)
		anomalies = append(anomalies, Anomaly{Item: "disk",
			Detail: "disk " + strconv.FormatFloat(diskPct, 'f', 1, 64) + "% > 85%", Severity: "critical"})
	} else if diskPct > 75 {
		deduct(scoreDiskWarning)
		anomalies = append(anomalies, Anomaly{Item: "disk",
			Detail: "disk " + strconv.FormatFloat(diskPct, 'f', 1, 64) + "% > 75%", Severity: "warning"})
	}

	// 负载（CPU 逻辑核数取 getconf 实时值，缺省 4）。
	if load1 > 2*float64(cpuCores) {
		deduct(scoreLoadCritical)
		anomalies = append(anomalies, Anomaly{Item: "load",
			Detail:   "load_1 " + strconv.FormatFloat(load1, 'f', 2, 64) + " > 2x" + strconv.Itoa(cpuCores) + " cores",
			Severity: "critical"})
	} else if load1 > float64(cpuCores) {
		deduct(scoreLoadWarning)
		anomalies = append(anomalies, Anomaly{Item: "load",
			Detail:   "load_1 " + strconv.FormatFloat(load1, 'f', 2, 64) + " > " + strconv.Itoa(cpuCores) + " cores",
			Severity: "warning"})
	}

	// 内存。
	if memPct > 90 {
		deduct(scoreMemWarning)
		anomalies = append(anomalies, Anomaly{Item: "memory",
			Detail: "mem " + strconv.FormatFloat(memPct, 'f', 1, 64) + "% > 90%", Severity: "warning"})
	}

	if !in.IsWindows {
		// 失败服务（systemctl --failed 每单元 -10，合计上限 -30）。
		if out, ok := in.Outputs["systemctl --failed --no-legend"]; ok {
			units := parseSystemctlFailed(clampParserInput(out))
			penalty := len(units) * scoreServicePenalty
			if penalty > scoreServicePenaltyCap {
				penalty = scoreServicePenaltyCap
			}
			if len(units) > 0 {
				deduct(penalty)
				for i, u := range units {
					if i >= maxServiceAnomalyItems {
						break
					}
					anomalies = append(anomalies, Anomaly{Item: "service",
						Detail: truncateStr("unit "+u+" failed", 256), Severity: "warning"})
				}
			}
		}
		// journal 错误行（full 模板）。
		if out, ok := in.Outputs["journalctl -p err -n 50 --no-pager"]; ok {
			if lines := journalErrorLines(clampParserInput(out)); len(lines) > 0 {
				deduct(scoreJournalWarning)
				anomalies = append(anomalies, Anomaly{Item: "journal",
					Detail:   truncateStr(strings.Join(lines[:min(len(lines), journalDetailLines)], " | "), 512),
					Severity: "warning"})
			}
		}
	}

	// 活跃 firing 告警（每条 -10，合计上限 -20；severity=事件等级）。
	if len(in.Alerts) > 0 {
		penalty := len(in.Alerts) * scoreAlertPenalty
		if penalty > scoreAlertPenaltyCap {
			penalty = scoreAlertPenaltyCap
		}
		deduct(penalty)
		for i, a := range in.Alerts {
			if i >= maxAlertAnomalyItems {
				break
			}
			anomalies = append(anomalies, Anomaly{Item: "alert",
				Detail: truncateStr(a.Message, 256), Severity: a.Severity})
		}
	}

	return scoreResult{Score: score, Anomalies: anomalies}
}

// ---------------------------------------------------------------------------
// 输出解析（每个解析器输入 ≤ parserInputLimit——铁律：解析器必须有输入
// 大小限制）
// ---------------------------------------------------------------------------

func clampParserInput(s string) string {
	if len(s) > parserInputLimit {
		return s[:parserInputLimit]
	}
	return s
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// parseLoadavg 解析 /proc/loadavg 首字段。
func parseLoadavg(out string) float64 {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

// parseMeminfo 解析 grep MemTotal/MemAvailable 输出 → 内存使用率。
func parseMeminfo(out string) float64 {
	var total, avail float64
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(fields[1], 64)
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	if total <= 0 {
		return 0
	}
	return (total - avail) / total * 100
}

// pseudoFS 匹配剔除的伪文件系统（契约 7.7：tmpfs|devtmpfs|overlay|squashfs）。
var pseudoFS = regexp.MustCompile(`^(tmpfs|devtmpfs|overlay|squashfs)`)

// parseMaxDiskPct 解析 df -B1 输出：剔除伪文件系统后取最大 used/(used+avail)。
func parseMaxDiskPct(out string) float64 {
	maxPct := 0.0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[0] == "Filesystem" {
			continue
		}
		if pseudoFS.MatchString(fields[0]) {
			continue
		}
		used, err1 := strconv.ParseFloat(fields[2], 64)
		avail, err2 := strconv.ParseFloat(fields[3], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		if used+avail <= 0 {
			continue
		}
		pct := used / (used + avail) * 100
		if pct > maxPct {
			maxPct = pct
		}
	}
	return maxPct
}

// parseSystemctlFailed 解析 systemctl --failed --no-legend 输出：每行首列
// 为失败单元名。
func parseSystemctlFailed(out string) []string {
	var units []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue // UNIT LOAD ACTIVE SUB DESCRIPTION 至少 4 列
		}
		if fields[2] != "failed" {
			continue
		}
		units = append(units, fields[0])
	}
	return units
}

// journalErrRe 匹配 journal 错误行（error|failed|fatal，忽略大小写）。
var journalErrRe = regexp.MustCompile(`(?i)\b(error|failed|fatal)\b`)

// journalErrorLines 返回含错误关键词的行（≤ journalDetailLines*4 条候选，
// 输入已被 clamp）。
func journalErrorLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || len(line) > 256 {
			if line != "" {
				line = truncateStr(line, 256)
			}
		}
		if line != "" && journalErrRe.MatchString(line) {
			lines = append(lines, line)
			if len(lines) >= 12 {
				break
			}
		}
	}
	return lines
}

// winMetrics 是 Windows 采集模板输出的 JSON 结构（WIN_SAFE_COLLECT）。
type winMetrics struct {
	CPUPct  float64 `json:"cpu_pct"`
	MemPct  float64 `json:"mem_pct"`
	DiskPct float64 `json:"disk_pct"`
	Load1   float64 `json:"load_1"`
}

// parseWindowsMetrics 从 outputs 中找采集模板输出（值最大的 JSON 候选——
// runner 以命令全文作键；这里按 JSON 形态识别，键不含空格路径），解析
// 指标。核数取不到（模板不采集核数）→ 0（评分回退缺省 4）。
func parseWindowsMetrics(outputs map[string]string) (cores int, load1, memPct, diskPct float64) {
	for _, out := range outputs {
		out = strings.TrimPrefix(strings.TrimSpace(clampParserInput(out)), "\uFEFF")
		if out == "" || out[0] != '{' {
			continue
		}
		var m winMetrics
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			continue
		}
		if m.MemPct > 0 || m.DiskPct > 0 || m.Load1 > 0 {
			return 0, m.Load1, m.MemPct, m.DiskPct
		}
	}
	return 0, 0, 0, 0
}

// aggregateReport 汇总报告：avg_score=均值取整（四舍五入），0 台=100
// （契约 G34）。
func aggregateReport(scores []int) int {
	if len(scores) == 0 {
		return 100
	}
	sum := 0
	for _, s := range scores {
		sum += s
	}
	return int(math.Round(float64(sum) / float64(len(scores))))
}
