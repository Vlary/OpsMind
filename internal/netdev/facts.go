// facts.go 是 server_facts 的网络设备固定模板与输出解析（契约 7.5，N1
// 写死）：两条固定只读命令（generic 仅命令 1），经 netdev 执行，命中
// risk.<P>SAFE_FACTS；输出非 JSON——解析器尽力提取，失败的字段留空，
// 原文进 raw_output，不因个别字段失败整体报错。
package netdev

import (
	"regexp"
	"strings"
)

// NetInterface 是接口摘要行（无 IP 概念的行 ip 留空）。
type NetInterface struct {
	Name     string `json:"name"`
	IP       string `json:"ip"`
	Status   string `json:"status"`
	Protocol string `json:"protocol"`
}

// NetFacts 是网络设备 facts（契约 7.5 固定键：hostname/vendor/model/
// software_version/serial_number/uptime/interfaces）。
type NetFacts struct {
	Hostname        string         `json:"hostname"`
	Vendor          string         `json:"vendor"`
	Model           string         `json:"model"`
	SoftwareVersion string         `json:"software_version"`
	SerialNumber    string         `json:"serial_number"`
	Uptime          string         `json:"uptime"` // 原文串，不做秒换算
	Interfaces      []NetInterface `json:"interfaces"`
}

// FactsCommands 返回该品牌的固定只读命令（命令 1：版本/主机名/序列号/
// uptime；命令 2：接口摘要；generic 仅命令 1——契约 7.5 表）。
func FactsCommands(b Brand) []string {
	switch b {
	case BrandCisco:
		return []string{"show version", "show ip interface brief | exclude unassigned"}
	case BrandHuawei, BrandH3C:
		return []string{"display version", "display ip interface brief"}
	case BrandJunos:
		return []string{"show version", "show interfaces terse"}
	default:
		return []string{"show version"}
	}
}

// versionRe 是软件版本串的行内锚点（Cisco/VRP/Comware `Version ...`、
// Junos `Junos: ...`）。空白限定 [ \t]：`\s` 会跨行，`show version` 行尾
// 的 version 会捕获到下一行整行。
var versionRe = regexp.MustCompile(`(?i)\b(?:version|junos)[ \t]*[:=]?[ \t]*([0-9][^\r\n]*)`)

// parseSoftwareVersion 从探针/版本输出解析软件版本串；失败返回原始首行
// （契约 5.2B Test）。
func parseSoftwareVersion(b Brand, out string) string {
	if m := versionRe.FindStringSubmatch(out); m != nil {
		return strings.TrimSpace(m[1])
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// serialRe 是序列号锚点行：`SN:` / `Serial Number:` / `Chassis`（契约 7.5）。
var serialRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bSN\s*[:#]?\s*([A-Z0-9][A-Za-z0-9-]{3,})`),
	regexp.MustCompile(`(?i)\bSerial(?:\s*Number)?\s*[:#]?\s*([A-Z0-9][A-Za-z0-9-]{3,})`),
	regexp.MustCompile(`(?i)\bChassis\b[^\r\n]*?\b([A-Z0-9]{2,}[A-Za-z0-9-]*)\s*$`),
}

// uptimeRe 是 uptime 原文串锚点（空白限定行内，防跨行）。
var uptimeRe = regexp.MustCompile(`(?i)\b(?:uptime|up)[ \t]*(?:is|has been)?[ \t]*[:\-]?[ \t]*([^\r\n]{3,120})`)

// modelRes 是型号锚点（Cisco Model number/IOS banner 括号型号、
// VRP/Comware 行首型号、Junos Model:）。
var modelRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bModel number\s*:\s*(\S[^\r\n]*)`),
	regexp.MustCompile(`(?i)\bModel\s*:\s*(\S[^\r\n]*)`),
	regexp.MustCompile(`(?i)^\s*([A-Z][A-Za-z0-9]*-[A-Za-z0-9]{2,}[A-Za-z0-9-]*)\s+?(?:Vers|Boot|Software|S/N|PWR)?`),
}

// ParseFacts 解析两条固定命令的输出（versionOut 为命令 1、ifOut 为命令 2，
// 可为空），尽力提取各字段；解析失败的字段留空。
func ParseFacts(b Brand, vendor string, versionOut, ifOut string) NetFacts {
	f := NetFacts{Vendor: vendor, Interfaces: []NetInterface{}}

	// hostname：命令 1 输出前的首行设备名/提示符；清洗后的输出按
	// `hostname uptime is`（IOS）与 `Hostname: x`（Junos）锚点提取，
	// 都失败留空（契约 7.5：解析失败的字段留空）。
	lines := splitLines(versionOut)
	if len(lines) > 0 {
		first := strings.TrimSpace(lines[0])
		if host, ok := hostnameFromPrompt(first); ok {
			f.Hostname = host
		} else if isLikelyHostname(first) {
			f.Hostname = first
		}
	}
	if f.Hostname == "" {
		if m := uptimeHostRe.FindStringSubmatch(versionOut); m != nil {
			f.Hostname = m[1]
		}
	}
	if f.Hostname == "" {
		if m := hostnameKvRe.FindStringSubmatch(versionOut); m != nil {
			f.Hostname = m[1]
		}
	}

	if m := versionRe.FindStringSubmatch(versionOut); m != nil {
		f.SoftwareVersion = strings.TrimSpace(m[1])
	}
	for _, re := range serialRes {
		if m := re.FindStringSubmatch(versionOut); m != nil {
			f.SerialNumber = strings.TrimSpace(m[1])
			break
		}
	}
	if m := uptimeRe.FindStringSubmatch(versionOut); m != nil {
		f.Uptime = strings.TrimSpace(m[1])
	}
	for _, re := range modelRes {
		for _, line := range lines {
			if m := re.FindStringSubmatch(line); m != nil {
				f.Model = strings.TrimSpace(m[1])
				break
			}
		}
		if f.Model != "" {
			break
		}
	}

	f.Interfaces = parseInterfaces(b, ifOut)
	return f
}

// uptimeHostRe 提取 IOS `core-sw uptime is ...` 行的主机名。
var uptimeHostRe = regexp.MustCompile(`(?im)^\s*([A-Za-z0-9][A-Za-z0-9._-]{1,63})[ \t]+uptime[ \t]+is`)

// hostnameKvRe 提取 `Hostname: x`（Junos）行。
var hostnameKvRe = regexp.MustCompile(`(?im)^\s*hostname[ \t]*:[ \t]*(\S[^\r\n]*)$`)

// hostnameFromPrompt 从提示符形态提取主机名（router# / <hw-sw> / [sw] /
// root@device>）。
func hostnameFromPrompt(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return "", false
	}
	if strings.HasPrefix(t, "<") && strings.HasSuffix(t, ">") {
		return strings.TrimSuffix(strings.TrimPrefix(t, "<"), ">"), true
	}
	if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
		return strings.TrimSuffix(strings.TrimPrefix(t, "["), "]"), true
	}
	// root@device> 形态取 @ 后段。
	if i := strings.IndexByte(t, '@'); i >= 0 && (strings.HasSuffix(t, ">") || strings.HasSuffix(t, "#")) {
		return strings.TrimRight(t[i+1:], "># "), true
	}
	if strings.HasSuffix(t, ">") || strings.HasSuffix(t, "#") {
		return strings.TrimRight(t, "># "), true
	}
	return "", false
}

// isLikelyHostname 判断首行是否像主机名（无空格、常用字符集）。
func isLikelyHostname(s string) bool {
	if s == "" || len(s) > 64 || strings.ContainsAny(s, " \t:,%") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '@':
		default:
			return false
		}
	}
	return true
}

func splitLines(s string) []string {
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// parseInterfaces 解析接口摘要（尽力按列提取，表头跳过；失败行丢弃）。
func parseInterfaces(b Brand, ifOut string) []NetInterface {
	rows := []NetInterface{}
	for _, line := range splitLines(ifOut) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		first := fields[0]
		low := strings.ToLower(line)
		// 跳过表头与提示/说明行。
		if strings.Contains(low, "interface") && strings.Contains(low, "address") {
			continue
		}
		if strings.Contains(low, "physical") && strings.Contains(low, "protocol") {
			continue
		}
		switch b {
		case BrandCisco:
			// Interface IP-Address OK Method Status Protocol（6 列；
			// 无 OK 列变体为 5 列：Interface IP-Address Method Status Protocol）。
			switch {
			case len(fields) >= 6 && isIfaceName(first):
				rows = append(rows, NetInterface{
					Name: first, IP: fields[1], Status: fields[4], Protocol: fields[5],
				})
			case len(fields) == 5 && isIfaceName(first):
				rows = append(rows, NetInterface{
					Name: first, IP: fields[1], Status: fields[3], Protocol: fields[4],
				})
			}
		case BrandHuawei, BrandH3C:
			// Interface IP Address/Mask Physical Protocol
			if len(fields) >= 4 && isIfaceName(first) {
				ip := fields[1]
				if ip == "*" || ip == "-" {
					ip = ""
				}
				rows = append(rows, NetInterface{
					Name: first, IP: ip, Status: fields[2], Protocol: fields[3],
				})
			}
		case BrandJunos:
			// Interface Admin Link proto local ...
			if len(fields) >= 3 && isIfaceName(first) {
				ip := ""
				if len(fields) >= 5 {
					ip = fields[len(fields)-1]
				}
				rows = append(rows, NetInterface{
					Name: first, IP: ip, Status: fields[1], Protocol: fields[2],
				})
			}
		default:
			// generic：按 cisco 形态尽力。
			if len(fields) >= 5 && isIfaceName(first) {
				rows = append(rows, NetInterface{
					Name: first, IP: fields[1], Status: fields[3], Protocol: fields[4],
				})
			}
		}
	}
	if rows == nil {
		return []NetInterface{}
	}
	return rows
}

// isIfaceName 判断常见接口名（GigabitEthernet0/0/1、GE1/0/1、Vlanif10、
// xe-0/0/0、eth0 等）。
func isIfaceName(s string) bool {
	if len(s) < 2 || len(s) > 64 {
		return false
	}
	low := strings.ToLower(s)
	for _, prefix := range []string{
		"gigabitethernet", "fastethernet", "ten-gigabitethernet", "ethernet",
		"ge", "fe", "xe", "et", "vlanif", "vlan", "loopback", "mgmt", "eth", "gi", "fa", "te",
		"tunnel", "serial", "atm", "bridge-agg", "route-aggregation", "raft", "reth", "em", "fxp", "ge-", "meth",
	} {
		if strings.HasPrefix(low, prefix) {
			rest := low[len(prefix):]
			if rest == "" {
				return true
			}
			// 后随数字/斜杠/冒号/点视为接口名延续。
			c := rest[0]
			if c >= '0' && c <= '9' || c == '/' || c == ':' || c == '-' || c == '.' {
				return true
			}
		}
	}
	return false
}
