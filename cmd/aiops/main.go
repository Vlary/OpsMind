// Command aiops 是服务组装入口：只做装配，不含业务规则。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // 注册 /debug/pprof 到 DefaultServeMux；默认不挂载，见 pprofEnabled。
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
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
	"aiops/internal/httpapi"
	"aiops/internal/inspection"
	"aiops/internal/llm"
	"aiops/internal/logx"
	"aiops/internal/monitor"
	"aiops/internal/netdev"
	"aiops/internal/rdpx"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/store/postgres"
	"aiops/internal/termx"
	"aiops/internal/winrmx"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "aiops: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logx.New(cfg.LogLevel)
	log.Info("starting",
		"addr", cfg.Addr,
		"version", version(),
		"web_dir", cfg.WebDir,
		"database", cfg.RedactedDatabaseURL(),
	)

	masterKey, err := cryptox.LoadMasterKey(cfg.DataDir, cfg.MasterKeyB64)
	if err != nil {
		return err
	}
	cipher, err := cryptox.New(masterKey)
	if err != nil {
		return err
	}

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelStartup()

	st, err := postgres.Open(startupCtx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}

	clock := time.Now
	authSvc := auth.NewService(st, auth.Config{SessionTTL: cfg.SessionTTL, Clock: clock})
	if err := authSvc.EnsureAdmin(startupCtx, cfg.AdminPassword, os.Stdout); err != nil {
		st.Close()
		return fmt.Errorf("ensure admin: %w", err)
	}

	auditWriter := audit.NewWriter(st, log, clock)
	limiter := auth.NewLoginLimiter(5, 12*time.Second, clock)
	approvalsSvc := approvals.NewService(st, auditWriter, cfg.ApprovalTTL, clock, log)
	registry := llm.NewRegistry(st, cipher, newLLMHTTPClient(), log)
	streams := agent.NewStreamGuard(cfg.MaxStreams)
	sshPool := sshx.New(sshx.Config{}, log)
	dbPool := dbproxy.New(dbproxy.Config{}, log)
	// W1：WinRM 连接池（password_ntlm/password_basic 服务器的测试与执行）。
	winrmPool := winrmx.New(winrmx.Config{}, log)
	// N1：网络设备 CLI 执行器（device_kind=network_device 的测试与执行；
	// SSH 通道复用 sshx 的连接与 TOFU，Telnet 通道走 netdev/telnetx）。
	netdevPool := netdev.New(netdev.Config{}, sshPool, log)
	// W3：终端会话治理（PTY↔WS 桥 + asciinema 录制），复用 SSH 池拨 PTY。
	termManager := termx.NewManager(termx.Config{
		MaxSessions:   cfg.TermMaxSessions,
		IdleTimeout:   cfg.TermIdleTimeout,
		MaxDuration:   cfg.TermMaxDuration,
		RecordingsDir: cfg.RecordingsDir,
	}, st, sshPool, auditWriter, clock, log)
	// W4：RDP 网关（guacd 透传 + .guac 录制）；GuacdAddr 为空时 Open
	// 返回 GUACD_UNAVAILABLE，治理循环无会话可管，构造始终安全。
	rdpGateway := rdpx.NewGateway(rdpx.Config{
		Addr:          cfg.GuacdAddr,
		MaxSessions:   cfg.RdpMaxSessions,
		IdleTimeout:   cfg.RdpIdleTimeout,
		MaxDuration:   cfg.RdpMaxDuration,
		RecordingsDir: cfg.RdpRecordingsDir,
	}, st, auditWriter, cipher, clock, log)
	runner := agent.NewRunner(
		st, registry, sshPool, dbPool, approvalsSvc, auditWriter, cipher,
		agent.RunnerConfig{StreamMaxDuration: cfg.StreamMaxDuration}, clock, log,
	)
	runner.SetNetCLIExecutor(netdevPool)

	// 五期 M3：批量操作编排（契约 5.18）。网络设备执行通道经注入适配
	//（batchx 不 import netcli，签名同 containerx.ExecFunc——httpapi 同款
	// 辅助适配：凭据解密 + netdevPool 单条 CLI 执行）。
	batchSvc := batchx.NewService(batchx.ServiceConfig{}, st, approvalsSvc, sshPool, winrmPool, cipher, auditWriter, clock, log)
	batchSvc.SetNetExecFactory(netExecFactory(cipher, netdevPool))
	runner.SetBatchExecutor(batchSvc)

	// 五期 M2：AI 自诊断引擎 + 定时巡检管理器（契约 5.16/5.17）。固定上限
	// 配置（11 节）；契约 2.1 第 6 步的启动顺序在下方 Start 调用链保证
	//（诊断引擎先于 monitor 评估器 Start；告警评估器触发自动诊断经
	// monitor.Manager.SetDiagnosisStarter 注入的回调回到 diagEngine）。
	diagEngine := diagnosis.NewEngine(diagnosis.DefaultEngineConfig(), st, registry,
		sshPool, winrmPool, cipher, auditWriter, clock, log)
	inspectMgr := inspection.NewManager(inspection.DefaultManagerConfig(), st,
		sshPool, winrmPool, cipher, nil, clock, log)
	inspectMgr.SetAuditWriter(auditWriter)
	runner.AttachPhase5(diagEngine, inspectMgr)

	// 五期 M1：监控告警管理器（采集/评估/通知投递/30 天保留四循环——
	// 契约 5.15）。CollectorConfig.Enabled=true 是进程级总开关：运行时
	// 启停由 settings["monitor.collector"] 控制（缺省 enabled=true，
	// 变更下一轮生效）；其余上限由 NormalizeCollectorConfig 钳到契约
	// 固定值。critical 告警的自动诊断经回调注入（monitor 不 import
	// diagnosis——契约 5.15 包边界）。
	monitorMgr := monitor.NewManager(monitor.CollectorConfig{Enabled: true},
		monitor.NotifyConfig{}, st, sshPool, winrmPool, cipher, auditWriter, clock, log)
	monitorMgr.SetDiagnosisStarter(func(ctx context.Context, ev store.AlertEvent) {
		eventID := ev.ID
		if _, err := diagEngine.Create(ctx, store.CreateDiagnosisParams{
			ServerID: ev.ServerID, AlertEventID: &eventID, Trigger: store.DiagnosisTriggerAlert,
		}); err != nil {
			log.Warn("monitor_auto_diagnose_create_failed",
				"server_id", ev.ServerID, "error", err.Error())
		}
	})

	deps := &httpapi.Deps{
		Config:    cfg,
		Log:       log,
		Store:     st,
		Cipher:    cipher,
		Auth:      authSvc,
		Limiter:   limiter,
		Approvals: approvalsSvc,
		LLM:       registry,
		Agent:     runner,
		SSH:       sshPool,
		WinRM:     winrmPool,
		NetCLI:    netdevPool,
		DB:        dbPool,
		Term:      termManager,
		RDP:       rdpGateway,
		Diagnosis: diagEngine,
		Inspect:   inspectMgr,
		Monitor:   monitorMgr,
		Batch:     batchSvc,
		Audit:     auditWriter,
		Streams:   streams,
		Clock:     clock,
		Version:   version(),
	}

	router := httpapi.NewRouter(deps)
	handler := http.Handler(router)
	if pprofEnabled() {
		handler = withPprof(router, authSvc)
		log.Info("pprof endpoints enabled", "prefix", "/debug/pprof", "auth", "session cookie required")
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 审计 writer 与审批 sweeper 不订阅 signal ctx：SIGTERM 后 HTTP 还有最长
	// 30s 的排空窗口，期间在途流的审计事件与 pending 审批仍需正常处理；
	// 它们由 shutdown() 在 HTTP 排空完成后按序显式停止（Approvals.Stop →
	// Audit.Close → 池/存储关闭），提前停止会把排空期事件全部丢进 ErrClosed。
	auditWriter.Start(context.Background())
	approvalsSvc.Start(context.Background())
	authSvc.Start()
	// 五期 M3：批量过期扫描（与审批 sweeper 同语义：不订阅 signal ctx，
	// 排空期内仍需收敛 awaiting_approval 台；由 shutdown 在审批后停止）。
	batchSvc.Start(context.Background())
	// 五期 M2：诊断引擎先于告警评估器启动（契约 2.1 第 6 步——评估器触发
	// 自动诊断时引擎必须就绪）；巡检 cron tick 随后启动。
	diagEngine.Start(context.Background())
	inspectMgr.Start(context.Background())
	// 五期 M1：监控四循环最后启动（契约 2.1 第 6 步：诊断引擎已就绪，
	// 评估器的自动诊断回调才可能被触发）。
	monitorMgr.Start(context.Background())
	// 服务关闭时取消全部活跃 SSE 流：Shutdown 不影响活跃请求，30 分钟的长流
	// 只有被主动取消才能在排空预算内结束并完成最终落库（WithoutCancel+5s）。
	srv.RegisterOnShutdown(func() {
		log.Info("shutdown: cancelling active streams")
		streams.ShutdownActive()
	})

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	log.Info("http listening", "addr", cfg.Addr)

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			shutdown(log, srv, deps)
			return err
		}
		shutdown(log, srv, deps)
		return nil
	}

	shutdown(log, srv, deps)
	log.Info("shutdown complete")
	return nil
}

// shutdown 按依赖逆序关闭：HTTP → 会话清理 → 审批扫描 → 终端/RDP 封盘
// → 审计 flush → 连接池。终端与 RDP 的 Shutdown 会同步封盘并等待
// session.end 审计事件入队（各 ≤2s），必须先于 Audit.Close 完成，
// 否则关闭事件的落库会丢进 ErrClosed。
func shutdown(log *slog.Logger, srv *http.Server, deps *httpapi.Deps) {
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), 30*time.Second)
	if err := srv.Shutdown(httpCtx); err != nil {
		log.Error("http shutdown failed", "error", err)
	}
	cancelHTTP()

	if deps.Auth != nil {
		deps.Auth.Stop()
	}
	if deps.Approvals != nil {
		deps.Approvals.Stop()
	}
	if deps.Batch != nil {
		deps.Batch.Stop()
	}
	// 五期 M2：巡检/诊断同步收尾各 ≤5s（契约 11 节停机语义——进行中的
	// 诊断会话由 Engine.Stop 置 failed(SERVER_SHUTTING_DOWN)）。
	// 五期 M1：监控循环先于巡检/诊断停止（启动逆序；且停诊断引擎前
	// 评估器不得再触发新的自动诊断会话）。
	if deps.Monitor != nil {
		deps.Monitor.Stop()
	}
	if deps.Inspect != nil {
		deps.Inspect.Stop()
	}
	if deps.Diagnosis != nil {
		deps.Diagnosis.Stop()
	}
	if deps.Term != nil {
		termCtx, cancelTerm := context.WithTimeout(context.Background(), 5*time.Second)
		deps.Term.Shutdown(termCtx)
		cancelTerm()
	}
	if deps.RDP != nil {
		rdpCtx, cancelRdp := context.WithTimeout(context.Background(), 5*time.Second)
		deps.RDP.Shutdown(rdpCtx)
		cancelRdp()
	}
	if deps.Audit != nil {
		auditCtx, cancelAudit := context.WithTimeout(context.Background(), 5*time.Second)
		if err := deps.Audit.Close(auditCtx); err != nil {
			log.Error("audit close failed", "error", err)
		}
		cancelAudit()
	}
	if deps.SSH != nil {
		_ = deps.SSH.Close()
	}
	if deps.WinRM != nil {
		_ = deps.WinRM.Close()
	}
	if deps.NetCLI != nil {
		_ = deps.NetCLI.Close()
	}
	if deps.DB != nil {
		_ = deps.DB.Close()
	}
	if deps.LLM != nil {
		deps.LLM.Close()
	}
	if deps.Store != nil {
		deps.Store.Close()
	}
}

// newLLMHTTPClient 构造 LLM 上游 HTTP client：不设全局 Timeout，避免截断流。
func newLLMHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// pprofEnabled 读取 AIOPS_PPROF；默认关闭，生产行为不变。
func pprofEnabled() bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("AIOPS_PPROF")))
	return err == nil && v
}

// withPprof 在主路由前挂载 /debug/pprof（仅在 AIOPS_PPROF 打开时被使用）。
//
// pprof 端点暴露进程内部状态（cmdline、堆、goroutine 栈、trace），必须与
// API 同等鉴权：要求管理员会话 Cookie（opsmind_session），匿名访问一律 401。
// 此前独立 mux 完全绕过 requireAuth，开一次排查开关就让端点整网可达。
func withPprof(next http.Handler, authSvc *auth.Service) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/debug/pprof/", requireSession(http.DefaultServeMux, authSvc))
	mux.Handle("/", next)
	return mux
}

// requireSession 校验 opsmind_session Cookie 后放行；authSvc 为 nil 时端点
// 直接视为不存在（404）。
func requireSession(next http.Handler, authSvc *auth.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authSvc == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		cookie, err := r.Cookie(httpapi.SessionCookieName)
		if err != nil || cookie.Value == "" {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if _, err := authSvc.Authenticate(r.Context(), cookie.Value); err != nil {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// netExecFactory 构造批量编排的网络设备执行通道工厂（契约 5.18：batchx
// 不 import netcli，网络设备执行经注入适配，签名同 containerx.ExecFunc；
// httpapi 同款辅助——凭据解密绑定闭包，命令单条原样发送）。
func netExecFactory(cipher *cryptox.Cipher, pool netdev.Executor) batchx.NetExecFactory {
	return func(srv store.Server) (batchx.ExecFunc, error) {
		if srv.AuthMethod != store.AuthMethodPassword {
			return nil, errors.New("batch: network devices require password auth")
		}
		pw, err := cipher.Decrypt(srv.PasswordEncrypted)
		if err != nil {
			return nil, err
		}
		enable, err := cipher.Decrypt(srv.EnableSecretEncrypted)
		if err != nil {
			return nil, err
		}
		port := srv.Port
		if port == 0 {
			port = 22
		}
		target := netdev.Target{
			ServerID:     srv.ID,
			Brand:        netdev.BrandOf(srv.NetdevBrand),
			AccessProto:  srv.AccessProto,
			Host:         srv.Host,
			Port:         port,
			Username:     srv.Username,
			Password:     pw,
			EnableSecret: enable,
			KnownHostKey: srv.HostKeyPublic,
		}
		return func(ctx context.Context, command string, timeout time.Duration) (batchx.ExecResult, error) {
			out, err := pool.Execute(ctx, target, netdev.RunRequest{
				Command: command, Timeout: timeout, MaxOutputBytes: 262144,
			})
			if err != nil {
				return batchx.ExecResult{}, err
			}
			return batchx.ExecResult{
				ExitCode: out.ExitCode, Combined: out.Combined,
				Truncated: out.Truncated, DurationMS: out.DurationMS,
			}, nil
		}, nil
	}
}
