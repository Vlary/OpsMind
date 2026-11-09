// parse_win.go 解析 Windows 采集模板（契约 7.7 WIN_SAFE_COLLECT）。
// 输出：单条 PowerShell ConvertTo-Json -Compress 的 JSON 对象，字段
// cpu_pct/mem_pct/disk_pct/load_1/net_in_bps/net_out_bps（net 为
// PerfFormattedData 计数器瞬时值 bps，无需差值）。
//
// 注意：文件名后缀 _windows.go 会被 Go 隐式附加 GOOS=windows 构建约束
//
// 文件名偏离契约树的 parse_windows.go：_windows.go 后缀会让 Go 隐式附加
// GOOS=windows 构建约束（与任何显式标签相与，实测无法覆盖），Linux 上
// 整个文件不参与编译导致 undefined；故定名 parse_win.go（语义不变）。
//
// 防终止条件铁律：输入长度硬上限 maxWinJSONBytes（第二道防线，通道层
// 已 256KB 截断）；畸形输出全零值，不报错中断采集轮。
package monitor

import (
	"encoding/json"
	"math"
	"strings"
)

// maxWinJSONBytes 是 Windows 采集输出 JSON 的大小硬上限（解析器输入限制）。
const maxWinJSONBytes = 262144

// ParseWindowsOutput 解析模板输出。空串/超限/畸形 JSON 返回零值 Sample。
func ParseWindowsOutput(output string) Sample {
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(output), "\uFEFF"))
	if trimmed == "" || len(trimmed) > maxWinJSONBytes {
		return Sample{}
	}
	var raw struct {
		CPU    *float64 `json:"cpu_pct"`
		Mem    *float64 `json:"mem_pct"`
		Disk   *float64 `json:"disk_pct"`
		Load1  *float64 `json:"load_1"`
		NetIn  *float64 `json:"net_in_bps"`
		NetOut *float64 `json:"net_out_bps"`
	}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return Sample{}
	}
	s := Sample{}
	if raw.CPU != nil {
		s.CPU = clampPct(*raw.CPU)
	}
	if raw.Mem != nil {
		s.Mem = clampPct(*raw.Mem)
	}
	if raw.Disk != nil {
		s.Disk = clampPct(*raw.Disk)
	}
	if raw.Load1 != nil && !math.IsNaN(*raw.Load1) && !math.IsInf(*raw.Load1, 0) && *raw.Load1 >= 0 {
		s.Load1 = *raw.Load1
	}
	if raw.NetIn != nil && *raw.NetIn > 0 {
		s.NetIn = int64(math.Round(*raw.NetIn))
	}
	if raw.NetOut != nil && *raw.NetOut > 0 {
		s.NetOut = int64(math.Round(*raw.NetOut))
	}
	return s
}
