# 云枢 OpsMind

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/brand/logo-horizontal-dark.png">
  <img src="docs/brand/logo-horizontal.png" alt="云枢 OpsMind" width="420">
</picture>

**AI 驱动的运维控制台**：用自然语言下达运维指令，大模型通过固定的 18 个工具读取资产、执行命令、查询数据库；高风险命令强制人工审批，全部动作写入只追加的审计哈希链。

单管理员、单实例部署。后端 Go 单二进制，前端 React SPA，元数据存 PostgreSQL。

## 界面预览

> 实际运行界面截图（默认浅色主题）。

**对话主界面** — 自然语言下达运维指令，模型调用工具读取资产并给出结论：

![对话主界面](docs/screenshots/chat.png)

**服务器资产** — Linux / Windows / 网络设备统一纳管，含自动执行策略与连接测试：

![服务器资产](docs/screenshots/servers.png)

**监控** — 六指标趋势、告警状态与采集配置：

![监控](docs/screenshots/monitor.png)

**审计** — 只追加日志，可按动作/目标类型/目标 ID 筛选：

![审计](docs/screenshots/audit.png)

## 功能

| 能力 | 说明 |
|---|---|
| **对话式运维** | SSE 流式输出，工具调用以卡片展示；可附加服务器/数据库上下文 |
| **资产接入** | Linux（SSH）、Windows（OpenSSH / WinRM）、网络设备五品牌（SSH / Telnet） |
| **数据库只读** | PostgreSQL、MySQL、SQLite、SQL Server、Redis、MongoDB——只有查询路径，写语句不执行 |
| **容器** | Docker / Podman / containerd 探测，容器与镜像列表、日志、start/stop/restart |
| **堡垒机** | Web 终端（PTY + asciinema v2 录制回放）、浏览器内 RDP（guacd 边车） |
| **AI 自动化** | 监控告警、AI 自诊断、定时巡检、批量操作——四项均同时提供对话工具与页面入口 |
| **审批治理** | 门限四档、审批时命令改写、执行回放、全站复制、深度思考展示 |
| **主题** | 默认浅色，深色可选，跟随系统；全站色值走语义变量 |

**审批门限四档**（全局设置 + 会话覆盖）：

| 档位 | 行为 |
|---|---|
| `all` | 全部命令审批，含只读 |
| `standard` | 危险命令审批，只读免审（默认） |
| `high` | 仅高危审批 |
| `none` | 免审批全部自动执行，但仍落审计 |

服务器关闭「自动执行」时，非只读命令在任何档位都必须审批（服务器级硬保护）。`forbidden` 级命令在任何档位都不可执行、不可审批。

## 快速开始

**前置**：Go 1.27、Node.js 22+、pnpm 10、Docker + Docker Compose（仅用于本地 PostgreSQL）。

```bash
# 1) 启动开发用 PostgreSQL（127.0.0.1:65432，用户/密码/库均为 aiops）
docker compose up -d --wait db

# 2) 安装依赖
go mod download
pnpm -C web install

# 3) 配置环境变量（后端不自动读取 .env）
export AIOPS_DATABASE_URL='postgres://aiops:aiops@127.0.0.1:65432/aiops?sslmode=disable'
export ADMIN_PASSWORD='换成一个强密码'   # 仅首次创建 admin 时使用

# 4) 启动后端（:8080）
go run ./cmd/aiops

# 5) 另开终端启动前端（:5173，代理 /api 到 8080）
pnpm -C web dev
```

打开 http://localhost:5173 ，用户名固定 `admin`，登录页只填密码。

未设置 `ADMIN_PASSWORD` 时，首次启动会生成 24 字符随机密码并**只在日志里打印一次**。之后再启动会忽略该变量，不会重置密码。密码可在顶栏头像下拉「修改密码」中自助修改（改密后全部会话失效）。

### 主要环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `AIOPS_DATABASE_URL` | 无（**必填**） | PostgreSQL DSN，日志中密码脱敏 |
| `AIOPS_MASTER_KEY` | 空 | 32 字节主密钥的 base64；留空则用 `data/master.key` |
| `AIOPS_DATA_DIR` | `./data` | `master.key` 所在目录 |
| `ADMIN_PASSWORD` | 空 | 仅首次创建 `admin` 时使用 |
| `AIOPS_ALLOWED_ORIGINS` | `localhost:5173,8080` | 写请求 Origin 精确匹配列表，逗号分隔 |
| `AIOPS_PUBLIC_ORIGIN` | 空 | **HTTPS 部署必须设置**，否则 Cookie 不带 Secure |
| `AIOPS_WEB_DIR` | `./web/dist` | 生产静态文件目录；设空串则只提供 API |
| `AIOPS_GUACD_ADDR` | 空 | guacd 地址；**空 = RDP 功能禁用**（其余功能不受影响） |
| `AIOPS_LOG_LEVEL` | `info` | `debug/info/warn/error` |

完整列表（终端 / RDP 会话限制、超时、pprof 等）见 `.env.example`。

### 演示数据（无需 API Key）

```bash
node web/e2e/fixtures/fake-llm.mjs &   # 假 LLM（OpenAI 兼容 SSE）
ADMIN_PASSWORD=<密码> make seed         # 播种示例服务器/供应商/会话
ADMIN_PASSWORD=<密码> make seed CHAT=1  # 可选：附带一条演示消息
```

## 生产部署

### docker compose

```bash
export ADMIN_PASSWORD='生产环境强密码'
docker compose up -d --build
docker compose logs -f app
curl http://127.0.0.1:18081/healthz   # 应返回 200 与 {"status":"ok",...}
```

- 镜像多阶段构建（前端 `pnpm build` + Go 静态二进制 `CGO_ENABLED=0`），运行镜像 `distroless/static`，约 56MB。
- `aiops-data` 卷挂载到 `/app/data` 保存 `master.key`。**必须备份该卷或改用 `AIOPS_MASTER_KEY`**，否则凭据无法解密。
- 启动时自动执行 goose 迁移，失败即退出。
- app 默认发布在 `127.0.0.1:18081`。对外服务时把 `AIOPS_ALLOWED_ORIGINS` 与 `AIOPS_PUBLIC_ORIGIN` 改成浏览器实际访问的地址。
- guacd 边车仅 RDP 需要，可整体移除。

### 外部 PostgreSQL

应用只依赖 `AIOPS_DATABASE_URL`，不要求数据库同容器。准备好空库与建表权限账号即可：

```bash
docker build -t aiops:local .
docker run -d --name aiops \
  -p 127.0.0.1:8080:8080 \
  -e AIOPS_DATABASE_URL='postgres://user:pass@db-host:5432/aiops?sslmode=require' \
  -e AIOPS_MASTER_KEY="$(openssl rand -base64 32)" \
  -e AIOPS_ALLOWED_ORIGINS='https://ops.example.com' \
  -e AIOPS_PUBLIC_ORIGIN='https://ops.example.com' \
  -e AIOPS_SECURE_COOKIES='true' \
  -e ADMIN_PASSWORD='生产环境强密码' \
  -v aiops-data:/app/data \
  aiops:local
```

**只允许单实例运行**——SSE 占用表、审批等待者、审计哈希链 writer 都在进程内，不能水平扩容。

### 反向代理

应用不终止 TLS，HTTPS 由反向代理负责。SSE 与 WebSocket 对代理配置敏感：

- `/api/chat`（SSE）：`proxy_buffering off; proxy_cache off; gzip off;`，`proxy_http_version 1.1;` + `proxy_set_header Connection "";`，读写超时 ≥ 31 分钟。
- `/api/term/sessions/{id}/ws` 与 `/api/rdp/sessions/{id}/ws`（WebSocket）：必须透传 `Upgrade` / `Connection` 头，读写超时 ≥ 1900s。
- 不要改写 `Origin`（应用按允许列表精确校验）；仅在代理与本机同机时才设 `AIOPS_TRUST_PROXY=true`。

## 安全模型

**凭据加密**：AES-256-GCM，12 字节随机 nonce，AAD 固定 `aiops:v1`。加密 SSH 密码/私钥/私钥口令、数据库密码、模型 API Key。API 响应只返回 `has_password` / `api_key_masked` 之类的布尔或掩码，永不返回明文或密文；解密只在执行前一刻发生在内存中。

**主密钥丢失 = 所有已保存凭据不可恢复**，且 v1 不支持轮换。请把 `master.key` 或 `AIOPS_MASTER_KEY` 单独备份。

**认证**：单账号 `admin`，bcrypt cost 12；会话 token 32 字节、库存 SHA-256、绝对有效期 24h；Cookie `HttpOnly` + `SameSite=Lax`；登录按 IP 令牌桶限流（约 5 次/分钟）；所有非 GET 请求校验 `Origin`；SSH 主机密钥 TOFU，指纹不匹配拒绝执行并写高危审计。

**命令风险分级是失败关闭的**：命中禁止规则直接 `forbidden`，无法证明安全则至少 `needs_approval`。

| 级别 | 示例 |
|---|---|
| `forbidden` | `rm -rf /`、`mkfs*`、`dd of=/dev/sd*`、`shutdown/reboot`、`systemctl stop sshd`、`curl\|sh`、fork bomb、`iptables -F`、清空日志/历史 |
| `needs_approval` | 其余 `rm -r/-f`、`systemctl start/stop/restart`、包管理、`chmod/chown`、网络配置、`mount`、账号变更、`crontab`、写系统目录、未知命令 |
| `safe` | `ls/cat/grep/ps/df/free/ss` 等只读命令、`systemctl status`、`journalctl`、`docker ps/logs/inspect`、全 safe 的只读管道 |

按目标类型分册：Linux、Windows（PowerShell 语义，规则 ID `WIN_*`）、网络设备（五厂商，`NET_*_` 前缀）、容器（`CONTAINER_*`）。各分册互不混用，未命中一律走默认审批。

**数据库守卫**：PostgreSQL 用 AST 分析，MySQL/SQLite/SQL Server 用词法分类，Redis 用只读命令白名单，MongoDB 用六层 JSON 命令文档白名单（拒绝 `$where`/`$function`/`$out`/`$merge` 等）。写路径不存在，不会创建审批。

**审计**：只追加，带哈希链（`prev_hash` / `hash`）。不提供删除或归档接口。

## 技术栈

| 层 | 选型 |
|---|---|
| 后端 | Go 1.27、Gin、`log/slog` |
| 数据库 | PostgreSQL 16、pgx v5、goose（embed SQL，启动自动迁移） |
| SSH / WinRM | `x/crypto/ssh`（含 PTY）、`masterzen/winrm` |
| 前端 | React 19、Vite 8、TypeScript strict、Ant Design 6、TanStack Query、Zustand |
| 实时 | SSE（对话，`fetch` + `ReadableStream`）、WebSocket（终端 / RDP） |
| 接口契约 | `api/openapi.yaml` 为唯一事实源，`web/src/api/schema.d.ts` 由它生成 |
| LLM | 原生 `net/http` 流式，OpenAI 兼容 + Anthropic 两套适配器，不引 SDK |
| 测试 | Go fuzzing、Playwright（真实 Chromium）、Vitest、自研 `loadgen` |

### 项目结构

```text
aiops/
├── cmd/
│   ├── aiops/       # 入口：装配、启动、优雅关闭
│   ├── smoke/       # HTTP 冒烟测试
│   ├── seed/        # 播种演示数据
│   ├── loadgen/     # 负载/浸泡工具（selftest/soak/stress）
│   └── e2esshd/     # E2E 假 sshd（真 PTY 链路）
├── internal/
│   ├── agent/       # 工具循环、提示词、风险路由
│   ├── approvals/   # 审批状态机与统一门限策略
│   ├── risk/        # 命令风险分级（Linux/Windows/网络设备/容器分册）
│   ├── sqlguard/    # SQL 只读守卫
│   ├── sshx/ winrmx/ netdev/   # 连接通道
│   ├── monitor/ diagnosis/ inspection/ batchx/   # 五期自动化
│   ├── store/       # 领域模型 + postgres 实现 + 迁移
│   └── httpapi/     # 路由、中间件、SSE、终端/RDP WS
├── api/openapi.yaml # 唯一 HTTP 接口事实源
├── docs/screenshots/
└── web/             # React SPA + Playwright E2E
```

约定：持久层 SQL 只出现在 `internal/store/postgres` 与 `internal/store/migrations`；前端不得手写 API 类型。

## 常用命令

| 命令 | 用途 |
|---|---|
| `go run ./cmd/aiops` | 启动后端（`:8080`） |
| `pnpm -C web dev` | 启动 Vite（`:5173`） |
| `go vet ./...` | Go 静态检查 |
| `go test ./...` | Go 单测 + 集成测试（需本地测试库） |
| `pnpm -C web typecheck` | TypeScript 类型检查 |
| `pnpm -C web lint` | ESLint，零警告 |
| `pnpm -C web test` | Vitest 单测 |
| `pnpm -C web test:e2e` | Playwright E2E（自动编排 postgres/后端/假 LLM/假 sshd） |
| `go run ./cmd/smoke` | HTTP 冒烟（自动建删 `aiops_smoke` 库） |
| `go run ./cmd/loadgen -mode=selftest` | 负载自检（90s）；soak/stress 见 `-mode` |
| `pnpm -C web openapi` | 由 OpenAPI 重新生成前端类型 |
| `make build` | 构建 `bin/aiops` + 前端 |

## 测试与验证

验证分六层：冒烟 → 单元/集成 → 模糊 → 端到端（浏览器）→ 浸泡/压测 → 部署验证。

当前状态（2026-11-09）：

| 检查 | 结果 |
|---|---|
| `go vet ./...` / `go test ./...` | 全部包通过 |
| `pnpm typecheck` / `lint` / `build` | 全通过 |
| Vitest 单测 | 82 文件 / 1370 项 |
| Playwright E2E | 222 项通过（真实 Chromium） |
| 模糊测试 | `FuzzClassify` / `FuzzCheck` 无崩溃 |
| 浸泡 15 分钟 | PASS，SSE p95 ≈ 400ms，RSS 峰 73MB |
| 首屏 JS gzip | 121.5 KiB（预算 150KB） |
| 镜像与 compose | 构建、起停、健康探针通过 |

```bash
# 模糊测试续跑（快验 30s）
go test ./internal/risk -run '^$' -fuzz 'FuzzClassify$' -fuzztime=30s
go test ./internal/sqlguard -run '^$' -fuzz 'FuzzCheck$' -fuzztime=30s

# 端到端
pnpm -C web test:e2e
```

## 已知限制

1. **单管理员、无 RBAC**：只有 `admin` 一个账号，没有用户管理或权限隔离。忘记密码无找回通道，只能重置数据库重新引导。
2. **只支持单实例**：SSE 占用表、审批等待者、审计 writer 都在进程内，不能水平扩容。
3. **数据库只读**：外部数据库只有查询路径；需要写库只能通过 SSH 调数据库 CLI（受命令风险规则约束）。
4. **主密钥不可轮换**：丢失后已保存凭据无法恢复。
5. **SSH 能力有限**：对话内执行不分配 PTY、不支持交互式命令与端口转发；输出截断到 256KB。
6. **无文件上传/下载**：命令输出只以文本/JSON 回传。
7. **SSE 不自动重连**：刷新或切换会话会中断当前流；未处理审批失效，已持久化消息仍可拉取。
8. **风险分级是启发式**：基于规范化与规则匹配，不是沙箱。可能对良性命令过度审批，也不能保证拦截所有精心构造的绕过。
9. **终端与 RDP 会话不经命令级风控**：交互会话的治理靠录制、输入摘要与审计，不重跑 `risk.Classify`。
10. **录制文件含全量会话内容且无自动轮转**：`.cast` 含终端全部输出（可能有密码回显），`.guac` 含完整屏幕流；应用不提供清理接口。
11. **应用不终止 TLS**：必须由反向代理终止。
12. **无内置指标端点**：可观测性依赖 JSON 日志与审计表，没有 `/metrics`。
13. **不限制内网目标**：允许配内网资产与端点，不做 SSRF 白名单；安全依赖审批、风险规则、只读守卫与审计。
14. **真实环境未全面验证**：自动化测试使用替身——Linux 终端经假 sshd 走真 PTY 链路，RDP 经假 guacd 走真 WS 链路，网络设备经 fake 设备 TCP server，容器管理经注入的假执行桩。**真实 Windows 主机、网络设备真机、真实容器 daemon、真实 SQL Server/Redis/MongoDB、真实 RDP 3389、真实大模型供应商均未接入端到端验证**，规则可能存在与真实环境语义的偏差。
15. **容器管理边界**：containerd 只探测展示不参与管理；不提供 `exec`/`run` 的产品化路径；容器数据不落库（每次实时查询）；`container_runtime` 是探测缓存，装/卸载运行时后需重新探测。
16. **五期自动化边界**：网络设备不采集指标；指标只存 30 天、无降采样；通知尽力而为；诊断 mini-loop ≤6 轮；巡检 cron 是进程内调度（重启按 `next_run_at` 对齐，不补跑）；批量单 job ≤50 台，逐台审批无「全部批准」。
17. **六期门限 `none` 档风险**：`needs_approval` 级命令在 `auto_exec_enabled=true` 的服务器上不经任何人工确认直接执行，只靠审计事后追溯。这是产品选择而非缺陷，但误设代价高。
18. **回放粒度**：从 `messages`/`tool_calls`/`approvals` 三表重建，不是逐 token 录像；按「步骤」呈现；只覆盖 `ssh_exec`/`container_action` 两类执行。

---

其他说明见 [CHANGELOG.md](CHANGELOG.md)。
