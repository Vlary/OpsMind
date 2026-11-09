// parse_linux.go 解析 Linux 采集模板（契约 7.7 SAFE_COLLECT）输出。
// 模板按 ===stat / ===loadavg / ===meminfo / ===df / ===netdev 分段；
// cpu_pct 与 net_*_bps 为两次采样的差值（首轮为 0，不阻塞入库）。
// net_*_bps 单位是字节/秒（契约 7.7：字节差 ÷ interval，不乘 8）。
//
// 防终止条件铁律：全部解析函数对输入行数与每行字段数有硬上限——
// maxParseLines 限制总行数，超出部分丢弃（采集输出本身受通道层 256KB
// 截断，这里是第二道防线）；所有解析失败只置零值，绝不报错中断采集轮。
package monitor

import (
	"math"
	"strconv"
	"strings"
)

// maxParseLines 是单次解析的行数硬上限（畸形/超长输出兜底终止条件）。
const maxParseLines = 4096

// LinuxSample 是一次采集的原始终态：/proc 的瞬时值 + 网络字节计数器。
type LinuxSample struct {
	CPUTotal   float64 // /proc/stat cpu 行累计 jiffies（user..irq 之和）
	CPUIdle    float64 // idle+iowait
	MemTotalKB float64
	MemAvailKB float64
	DiskMaxPct float64 // 剔除 tmpfs|devtmpfs|overlay|squashfs 后最大使用率
	Load1      float64
	NetInBytes float64 // 物理接口接收字节合计（剔除 lo|veth|br-|docker|virbr）
	NetOutByte float64
}

// skippedFilesystems 是 disk_pct 剔除的（伪）文件系统前缀/子串。
var skippedFilesystems = []string{"tmpfs", "devtmpfs", "overlay", "squashfs"}

// skippedInterfaces 是 net 计数剔除的接口名前缀。
var skippedInterfaces = []string{"lo", "veth", "br-", "docker", "virbr"}

// ParseLinuxOutput 解析模板输出为原始终态。prev 为上一轮同机采样（nil
// 表示首轮，差值指标记 0）；intervalSeconds 为两轮间隔（≤0 时网络速率
// 记 0，避免除零）。任何分段解析失败只影响该分段（零值），不返回错误。
func ParseLinuxOutput(output string, prev *LinuxSample, intervalSeconds float64) (Sample, LinuxSample) {
	var cur LinuxSample
	sections := splitSections(output, maxParseLines)
	for _, line := range sections["stat"][:min(len(sections["stat"]), maxParseLines)] {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		// cpu  user nice system idle iowait irq softirq ...
		var total, idle float64
		ok := true
		for i, f := range fields[1:min(len(fields), 11)] {
			v, err := strconv.ParseFloat(f, 64)
			if err != nil {
				ok = false
				break
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		if ok {
			cur.CPUTotal = total
			cur.CPUIdle = idle
		}
		break
	}
	for _, line := range sections["loadavg"][:min(len(sections["loadavg"]), 8)] {
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
			cur.Load1 = v
			break
		}
	}
	for _, line := range sections["meminfo"][:min(len(sections["meminfo"]), 16)] {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			cur.MemTotalKB = v
		case strings.HasPrefix(line, "MemAvailable:"):
			cur.MemAvailKB = v
		}
	}
	for _, line := range sections["df"][:min(len(sections["df"]), 512)] {
		fields := strings.Fields(line)
		// Filesystem 1B-blocks Used Available Use% Mounted
		if len(fields) < 6 || strings.HasSuffix(fields[0], ":") {
			continue
		}
		if isSkippedFilesystem(fields[0]) {
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
		if pct := 100 * used / (used + avail); pct > cur.DiskMaxPct {
			cur.DiskMaxPct = pct
		}
	}
	for _, line := range sections["netdev"][:min(len(sections["netdev"]), 1024)] {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if isSkippedInterface(name) {
			continue
		}
		fields := strings.Fields(rest)
		// Receive: bytes packets errs drop fifo frame compressed multicast（0..7）
		// Transmit: bytes packets errs drop fifo colls carrier compressed（8..15）
		if len(fields) < 9 {
			continue
		}
		in, err1 := strconv.ParseFloat(fields[0], 64)
		out, err2 := strconv.ParseFloat(fields[8], 64) // 下标 8 = 发送字节
		if err1 != nil || err2 != nil {
			continue
		}
		cur.NetInBytes += in
		cur.NetOutByte += out
	}

	// 差值指标（契约 7.7：首轮差值指标为 0，不阻塞入库）。
	s := Sample{
		Mem:   pct(cur.MemTotalKB-cur.MemAvailKB, cur.MemTotalKB),
		Disk:  clampPct(cur.DiskMaxPct),
		Load1: cur.Load1,
	}
	if prev != nil && intervalSeconds > 0 {
		if dTotal := cur.CPUTotal - prev.CPUTotal; dTotal > 0 {
			dIdle := cur.CPUIdle - prev.CPUIdle
			s.CPU = clampPct(100 * (1 - dIdle/dTotal))
		}
		// 契约 7.7 / 8.7：net_*_bps 的单位是**字节/秒**（两次采样字节差 ÷
		// interval，不乘 8）——与 Windows 路径（PerfFormattedData 的
		// BytesReceivedPersec 直取）和前端 formatBps 的 byte 单位标签同
		// 口径。六期打磨 sprint 6：此前这里乘 8 把字节当 bit，读数 8 倍
		// 虚高（60s 内 60,000,000 B 增量显示成 8 MB/s，真实 1 MB/s）。
		dIn := cur.NetInBytes - prev.NetInBytes
		dOut := cur.NetOutByte - prev.NetOutByte
		if dIn > 0 && intervalSeconds > 0 {
			s.NetIn = int64(math.Round(dIn / intervalSeconds))
		}
		if dOut > 0 && intervalSeconds > 0 {
			s.NetOut = int64(math.Round(dOut / intervalSeconds))
		}
	}
	s.CPU = clampPct(s.CPU)
	s.Mem = clampPct(s.Mem)
	s.Disk = clampPct(s.Disk)
	if s.Load1 < 0 || math.IsNaN(s.Load1) || math.IsInf(s.Load1, 0) {
		s.Load1 = 0
	}
	return s, cur
}

// splitSections 把 ===name 分段输出拆为 map；行数受 lines 上限约束。
func splitSections(output string, lines int) map[string][]string {
	sections := make(map[string][]string, 8)
	cur := ""
	count := 0
	for _, line := range strings.Split(output, "\n") {
		if count >= lines {
			break
		}
		count++
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "===") {
			cur = strings.TrimPrefix(trimmed, "===")
			if len(cur) > 32 {
				cur = cur[:32]
			}
			if _, ok := sections[cur]; !ok {
				sections[cur] = nil
			}
			continue
		}
		if trimmed == "" {
			continue
		}
		sections[cur] = append(sections[cur], line)
	}
	return sections
}

func isSkippedFilesystem(fs string) bool {
	for _, skip := range skippedFilesystems {
		if strings.Contains(fs, skip) {
			return true
		}
	}
	return false
}

func isSkippedInterface(name string) bool {
	for _, prefix := range skippedInterfaces {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func pct(num, den float64) float64 {
	if den <= 0 {
		return 0
	}
	return 100 * num / den
}

func clampPct(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
