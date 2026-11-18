// cli.go 是主机侧离线恢复子命令的解析与装配（契约 12.3 第 11/25 条）。
//
// 子命令在启动任何监听之前判定：命中即执行并返回，绝不进入服务主循环。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"aiops/internal/config"
	"aiops/internal/logx"
	"aiops/internal/offline"
	"aiops/internal/store"
	"aiops/internal/store/postgres"
)

// offlineUsage 是子命令帮助文本。
const offlineUsage = `用法：
  aiops admin reset --user <username> [--reason <text>] [--confirm]
  aiops break-glass --action approve --approval-id <uuid> --reason <text> --confirm

主机侧离线恢复（需文件系统/数据库权限；不经过 HTTP、不监听端口）：
  admin reset         置 must_change_password=true、吊销全部会话并作废该用户
                      待处理审批/未消费终端授权，写 admin.offline_reset 审计。
  break-glass         一次性批准一条 pending 审批，写 admin.break_glass 审计。
`

// offlineCommand 是解析后的子命令。
type offlineCommand struct {
	kind       string // admin-reset | break-glass
	username   string
	approvalID string
	reason     string
	confirm    bool
	// out 是 audit anchor 的输出文件路径（仅 audit-anchor）。
	out string
}

// parseOfflineCommand 解析 argv（不含程序名）。非离线子命令返回 nil, nil，
// 由调用方进入正常服务启动。
//
// 任何 --help 也返回 nil,nil 之外的用法错误（退出码 2），避免把拼错参数
// 静默当成「启动服务」——这正是评审 high：break-glass 曾直接起了一个服务进程。
func parseOfflineCommand(argv []string) (*offlineCommand, error) {
	if len(argv) < 2 {
		return nil, nil
	}
	switch argv[0] {
	case "admin":
		if argv[1] != "reset" {
			return nil, fmt.Errorf("unknown admin subcommand %q", argv[1])
		}
		fs := flag.NewFlagSet("admin reset", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		username := fs.String("user", "", "target username")
		reason := fs.String("reason", "offline admin reset", "audit reason")
		confirm := fs.Bool("confirm", false, "required to apply")
		if err := fs.Parse(argv[2:]); err != nil {
			return nil, fmt.Errorf("admin reset: %w", err)
		}
		if fs.NArg() > 0 {
			return nil, fmt.Errorf("admin reset: unexpected arguments %v", fs.Args())
		}
		if strings.TrimSpace(*username) == "" {
			return nil, errors.New("admin reset: --user is required")
		}
		return &offlineCommand{kind: "admin-reset", username: *username, reason: *reason, confirm: *confirm}, nil
	case "audit":
		if argv[1] != "anchor" {
			return nil, fmt.Errorf("unknown audit subcommand %q", argv[1])
		}
		fs := flag.NewFlagSet("audit anchor", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		out := fs.String("out", "", "anchor output file (append-only JSON lines)")
		if err := fs.Parse(argv[2:]); err != nil {
			return nil, fmt.Errorf("audit anchor: %w", err)
		}
		if fs.NArg() > 0 {
			return nil, fmt.Errorf("audit anchor: unexpected arguments %v", fs.Args())
		}
		if strings.TrimSpace(*out) == "" {
			return nil, errors.New("audit anchor: --out is required")
		}
		return &offlineCommand{kind: "audit-anchor", out: *out}, nil
	case "break-glass":
		fs := flag.NewFlagSet("break-glass", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		action := fs.String("action", "approve", "only approve is supported")
		approvalID := fs.String("approval-id", "", "pending approval id")
		reason := fs.String("reason", "", "audit reason")
		confirm := fs.Bool("confirm", false, "required to apply")
		if err := fs.Parse(argv[1:]); err != nil {
			return nil, fmt.Errorf("break-glass: %w", err)
		}
		if fs.NArg() > 0 {
			return nil, fmt.Errorf("break-glass: unexpected arguments %v", fs.Args())
		}
		if *action != "approve" {
			return nil, fmt.Errorf("break-glass: unsupported action %q (only approve)", *action)
		}
		if strings.TrimSpace(*approvalID) == "" {
			return nil, errors.New("break-glass: --approval-id is required")
		}
		if strings.TrimSpace(*reason) == "" {
			return nil, errors.New("break-glass: --reason is required")
		}
		return &offlineCommand{kind: "break-glass", approvalID: *approvalID, reason: *reason, confirm: *confirm}, nil
	default:
		return nil, fmt.Errorf("unknown subcommand %q", argv[0])
	}
}

// runOfflineCommand 执行离线子命令并返回进程退出码（0 成功 / 1 失败 / 2 用法）。
func runOfflineCommand(cmd *offlineCommand, argv []string, stdout io.Writer) int {
	if cmd == nil {
		return 0
	}
	// 先校验 kind 与 --confirm，再加载配置/连库：参数错误不应触发任何
	// 外部副作用（也不该因配置缺失而报成 1）。
	switch cmd.kind {
	case "admin-reset", "break-glass", "audit-anchor":
	default:
		fmt.Fprintf(stdout, "未知子命令 %q\n", cmd.kind)
		return 2
	}
	if cmd.kind == "audit-anchor" {
		return runAnchorCommand(cmd.out, "", stdout)
	}
	if !cmd.confirm {
		fmt.Fprint(stdout, offlineUsage)
		fmt.Fprintln(stdout, "\n拒绝执行：缺少 --confirm（离线恢复会改变安全状态）。")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stdout, "配置错误：%v\n", err)
		return 1
	}
	log := logx.New(cfg.LogLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := postgres.Open(ctx, cfg.DatabaseURL, log)
	if err != nil {
		fmt.Fprintf(stdout, "数据库不可用：%v\n", err)
		return 1
	}
	defer st.Close()

	now := time.Now().UTC()
	switch cmd.kind {
	case "admin-reset":
		u, err := offline.ResetAdmin(ctx, st, cmd.username, cmd.reason, now)
		if err != nil {
			fmt.Fprintf(stdout, "admin reset 失败：%v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "已重置 %s（id=%s）：must_change_password=true，全部会话与待处理授权已作废。\n",
			u.Username, u.ID)
		fmt.Fprintln(stdout, "该账号下次登录后必须先修改密码。")
		return 0
	case "break-glass":
		ap, err := offline.BreakGlassApprove(ctx, st, cmd.approvalID, cmd.reason, now)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				fmt.Fprintf(stdout, "break-glass 失败：审批 %s 不是 pending（已被决策或过期）。\n", cmd.approvalID)
			} else if errors.Is(err, store.ErrNotFound) {
				fmt.Fprintf(stdout, "break-glass 失败：审批 %s 不存在。\n", cmd.approvalID)
			} else {
				fmt.Fprintf(stdout, "break-glass 失败：%v\n", err)
			}
			return 1
		}
		fmt.Fprintf(stdout, "已通过 break-glass 批准审批 %s（source=%s, command=%q），break_glass=true。\n",
			ap.ID, ap.Source, ap.Command)
		return 0
	default:
		fmt.Fprintf(stdout, "未知子命令 %q\n", cmd.kind)
		return 2
	}
}

// runAnchorCommand 执行 `aiops audit anchor --out <file>`：以独立进程/只读
// 角色导出审计链头锚点（契约 12.3 第 19 条「外部 anchor 定期导出」）。
//
// 独立于 HTTP 服务：可由 cron/systemd timer 以只读 DB 账户运行，输出文件
// 由该账户独占（0600、只追加）。
func runAnchorCommand(out, reason string, stdout io.Writer) int {
	if strings.TrimSpace(out) == "" {
		fmt.Fprintln(stdout, "audit anchor：必须指定 --out <file>")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stdout, "配置错误：%v\n", err)
		return 1
	}
	log := logx.New(cfg.LogLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st, err := postgres.Open(ctx, cfg.DatabaseURL, log)
	if err != nil {
		fmt.Fprintf(stdout, "数据库不可用：%v\n", err)
		return 1
	}
	defer st.Close()
	rec, err := offline.ExportAnchor(ctx, st)
	if err != nil {
		fmt.Fprintf(stdout, "导出 anchor 失败：%v\n", err)
		return 1
	}
	if err := offline.WriteAnchor(out, rec); err != nil {
		fmt.Fprintf(stdout, "写入 anchor 失败：%v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "已导出 anchor：head_id=%d rows=%d legacy=%d -> %s\n",
		rec.HeadID, rec.Rows, rec.Legacy, out)
	return 0
}
