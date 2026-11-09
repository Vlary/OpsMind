// rules_collect.go 登记五期 M1 monitor 采集器的固定指标模板（契约 7.7）：
// Linux `SAFE_COLLECT` / Windows `WIN_SAFE_COLLECT`，机制同 SAFE_FACTS——
// 整串规范化精确匹配放行 safe，任何改动走正常分类（失败关闭）。
// monitor 包不 import risk（契约 2.3 包边界），模板常量两边各持一份，
// 由 monitor 的测试断言字节一致（G30）。
package risk

// LinuxCollectCommand 是 monitor 采集器的 Linux 固定指标模板（契约 7.7
// 写死；输出按 ===stat/===loadavg/===meminfo/===df/===netdev 分段解析）。
const LinuxCollectCommand = `echo ===stat; cat /proc/stat; echo ===loadavg; cat /proc/loadavg; echo ===meminfo; grep -E '^(MemTotal|MemAvailable):' /proc/meminfo; echo ===df; df -B1; echo ===netdev; cat /proc/net/dev`

// WinCollectCommand 是 monitor 采集器的 Windows 固定指标模板（契约 7.7
// 写死；单条 PowerShell 输出 JSON：cpu/mem/disk/load_1/net_in/out_bps——
// net 为 PerfFormattedData 计数器瞬时值，非差值）。
const WinCollectCommand = `$os=Get-CimInstance Win32_OperatingSystem; $cpu=[int](Get-CimInstance Win32_Processor|Measure-Object LoadPercentage -Average).Average; $mem=[math]::Round(100*($os.TotalVisibleMemorySize-$os.FreePhysicalMemory)/$os.TotalVisibleMemorySize,1); $d=Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3'|Sort-Object -Property @{Expression={$_.FreeSpace/($_.Size+1)}}|Select-Object -First 1; $disk=[math]::Round(100*(1-$d.FreeSpace/$d.Size),1); $p=Get-CimInstance Win32_PerfFormattedData_PerfOS_System; $n=Get-CimInstance Win32_PerfFormattedData_Tcpip_NetworkInterface|Measure-Object -Property BytesReceivedPersec,BytesSentPersec -Sum; [pscustomobject]@{cpu_pct=$cpu;mem_pct=$mem;disk_pct=$disk;load_1=[double]$p.ProcessorQueueLength;net_in_bps=[int64]$n.Sum[0];net_out_bps=[int64]$n.Sum[1]}|ConvertTo-Json -Compress`

// collectTemplates 是 SAFE_COLLECT 精确匹配的规范化形式（模板本体）。
var collectTemplates = func() map[string]bool {
	m := map[string]bool{}
	if n, err := normalize(LinuxCollectCommand); err == nil {
		m[n] = true
	}
	return m
}()

// winCollectTemplates 是 WIN_SAFE_COLLECT 精确匹配的规范化形式集合：
// 内部脚本本体、SSH 通道外壳包装（无引号/带引号两种形态，同
// winFactsTemplates 的做法）。
var winCollectTemplates = func() map[string]bool {
	m := map[string]bool{}
	if n, err := normalize(WinCollectCommand); err == nil {
		m[n] = true
	}
	if n, err := normalize(`powershell -NoProfile -NonInteractive -Command ` + WinCollectCommand); err == nil {
		m[n] = true
	}
	if n, err := normalize(`powershell -NoProfile -NonInteractive -Command "` + WinCollectCommand + `"`); err == nil {
		m[n] = true
	}
	return m
}()

func isCollectTemplate(normalized string) bool { return collectTemplates[normalized] }
