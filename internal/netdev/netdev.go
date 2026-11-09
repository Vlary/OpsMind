// Package netdev 是网络设备 CLI 执行器（N1，契约 2.8 / 5.2B——任务定名
// internal/netdev，契约文档行文称 netcli，类型与行为按 5.2B 写死）。
//
// 网络设备 CLI（Cisco IOS/NX-OS、Huawei VRP、H3C Comware、Juniper Junos）
// 是提示符驱动的交互式行协议：SSH 通道在 PTY 内逐行 expect（连接与 TOFU
// 主机密钥校验复用 sshx），Telnet 通道为明文行协议（internal/netdev/telnetx，
// 无主机密钥语义）。全部工作即「提示符正则 + 分页关闭 + enable 流程 +
// echo 清洗」。
package netdev

import (
	"context"
	"time"

	"aiops/internal/netdev/telnetx"
	"aiops/internal/store"
)

// cleartextWarning 复用 telnetx 的固定明文告警文案（单一事实源）。
const cleartextWarning = telnetx.CleartextWarning

// Brand 是厂商 profile 路由键，与 store.NetdevBrand / risk.NetBrand 一一对应。
type Brand string

const (
	BrandCisco   Brand = "cisco"
	BrandHuawei  Brand = "huawei"
	BrandH3C     Brand = "h3c"
	BrandJunos   Brand = "junos"
	BrandGeneric Brand = "generic"
)

// BrandOf 把 store.NetdevBrand 映射为 netdev.Brand；未知值返回 BrandGeneric
// （失败关闭到保守 profile：提示符宽匹配、无分页关闭命令、探针 show version）。
func BrandOf(b store.NetdevBrand) Brand {
	switch b {
	case store.NetdevBrandCisco:
		return BrandCisco
	case store.NetdevBrandHuawei:
		return BrandHuawei
	case store.NetdevBrandH3C:
		return BrandH3C
	case store.NetdevBrandJunos:
		return BrandJunos
	default:
		return BrandGeneric
	}
}

// Target 是一次网络设备连接目标（契约 5.2B）。
type Target struct {
	ServerID     string
	Brand        Brand
	AccessProto  store.AccessProto // ssh | telnet
	Host         string
	Port         int
	Username     string
	Password     string // 登录密码（解密后明文，仅存在于本次调用内存）
	EnableSecret string // enable/super 特权密码；空=未配置
	// 仅 access_proto=ssh 使用；SSH 通道经 sshx 拨号，主机密钥校验语义同 5.2：
	KnownHostKey string // 已信任的 authorized_keys 行；空表示 TOFU
}

// CleartextWarning 在 telnet 接入时返回明文风险告警（透传锚点，契约 2.6），
// ssh 返回空串。
func (t Target) CleartextWarning() string {
	if t.AccessProto == store.AccessProtoTelnet {
		return cleartextWarning
	}
	return ""
}

// TestResult 是连接测试结果（契约 5.2B）。
type TestResult struct {
	LatencyMS     int64  `json:"latency_ms"`
	ServerVersion string `json:"server_version"` // 探针输出的软件版本串
	Fingerprint   string `json:"fingerprint"`    // SSH：真实指纹；Telnet：恒空串
	HostKey       string `json:"-"`
	FirstSeen     bool   `json:"first_seen"` // SSH：真实值；Telnet：恒 false
	// CleartextWarning 是 telnet 明文告警透传（ssh 为空串）。
	CleartextWarning string `json:"cleartext_warning,omitempty"`
}

// RunRequest 是执行请求（契约 5.2B：单条 CLI 命令）。
type RunRequest struct {
	Command        string        // 单条 CLI 命令（契约 2.8 约定 2）
	Timeout        time.Duration // 0 => 60s；最大 300s（expect 总预算）
	MaxOutputBytes int64         // 0 => 262144；最大 262144
}

// RunResult 是执行结果（契约 5.2B）。
type RunResult struct {
	ExitCode   int    `json:"exit_code"` // 成功收到提示符=0；登录失败/错误提示/超时=1
	Combined   string `json:"output"`    // 清洗后输出（去回显、去分页提示），最多 256KB
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
	// CleartextWarning 是 telnet 明文告警透传（ssh 为空串）。
	CleartextWarning string `json:"cleartext_warning,omitempty"`
}

// Executor 是网络设备执行器接口（契约 5.2B）。
type Executor interface {
	Test(ctx context.Context, target Target) (TestResult, error)
	Execute(ctx context.Context, target Target, req RunRequest) (RunResult, error)
	Invalidate(serverID string)
	Close() error
}

// Config 是连接池配置（契约 5.2B）。
type Config struct {
	MaxPerServer     int           // 2（网络设备并发连接普遍有限）
	IdleTimeout      time.Duration // 60s（对齐契约字段；连接为一次性，见 Pool 注释）
	MaxConnections   int           // 32
	DialTimeout      time.Duration // 10s
	HandshakeTimeout time.Duration // 15s（仅 SSH；telnet 无握手语义）
	ExpectTick       time.Duration // 100ms（expect 轮询粒度）
}
