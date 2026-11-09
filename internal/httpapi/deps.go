package httpapi

import (
	"log/slog"
	"time"

	"aiops/internal/agent"
	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/auth"
	"aiops/internal/batchx"
	"aiops/internal/config"
	"aiops/internal/cryptox"
	"aiops/internal/dbproxy"
	"aiops/internal/diagnosis"
	"aiops/internal/inspection"
	"aiops/internal/llm"
	"aiops/internal/monitor"
	"aiops/internal/netdev"
	"aiops/internal/rdpx"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/termx"
	"aiops/internal/winrmx"
)

// Deps 是 HTTP 层的全部依赖；每个字段都必须非 nil，由 main/smoke 保证。
type Deps struct {
	Config    *config.Config
	Log       *slog.Logger
	Store     store.Store
	Cipher    *cryptox.Cipher
	Auth      *auth.Service
	Limiter   *auth.LoginLimiter
	Approvals *approvals.Service
	LLM       *llm.Registry
	Agent     *agent.Runner
	SSH       sshx.Executor
	WinRM     winrmx.Executor // W1：WinRM 认证服务器的连接测试与池失效
	NetCLI    netdev.Executor // N1：网络设备（device_kind=network_device）的测试与执行
	DB        dbproxy.Executor
	Term      *termx.Manager
	RDP       *rdpx.Gateway       // W4：RDP 网关（guacd 握手注入与透传）
	Monitor   *monitor.Manager    // 五期 M1：监控采集/评估/通知
	Diagnosis *diagnosis.Engine   // 五期 M2：AI 自诊断（REST /api/diagnosis/sessions*）
	Inspect   *inspection.Manager // 五期 M2：定时巡检（REST /api/inspection/*）
	Batch     *batchx.Service     // 五期 M3：批量操作编排（REST /api/batch/jobs* 与 decide 回填）
	Audit     *audit.Writer
	Streams   *agent.StreamGuard
	Clock     func() time.Time
	Version   string
}
