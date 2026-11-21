# 云枢 OpsMind

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/brand/logo-horizontal-dark.png">
  <img src="docs/brand/logo-horizontal.png" alt="云枢 OpsMind" width="420">
</picture>

**AI 驱动的运维控制台**：用自然语言下达运维指令，大模型通过固定的 18 个工具读取资产、执行命令、查询数据库；高风险命令强制人工审批，全部动作写入只追加的审计哈希链。

多用户、单实例部署。后端 Go 单二进制，前端 React SPA，元数据存 PostgreSQL；固定四角色 RBAC（admin / operator / approver / auditor）共 19 条权限，每用户恰一个角色，权限由服务端按角色计算。

## 界面预览

> 实际运行界面截图（默认浅色主题）。

**对话主界面** — 自然语言下达运维指令，模型调用工具读取资产并给出结论：

![对话主界面](docs/screenshots/chat.png)

**监控** — 集群压力趋势（24 小时 / 7 天分桶）+ 最紧张服务器 Top 12，下方是单机六指标历史曲线：

![监控](docs/screenshots/monitor.png)

**告警** — 按小时 × 严重度的告警分布、最吵规则 Top 8，点击图元下钻到事件表：

![告警](docs/screenshots/alerts.png)

**巡检** — 巡检均分走势与逐份报告（评分分档着色，异常项可下钻）：

![巡检](docs/screenshots/inspections.png)

**审批** — 待处理队列：发起人、目标服务器、待执行命令、风险原因与倒计时：

![审批](docs/screenshots/approvals.png)

**用户与角色** — 四角色 RBAC，权限由服务端按角色计算，界面只按权限展示可用操作：

![用户与角色](docs/screenshots/users.png)

**服务器资产** — Linux / Windows / 网络设备统一纳管，含自动执行策略与连接测试：

![服务器资产](docs/screenshots/servers.png)

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
| **审批治理** | 门限四档、职责分离审批、执行回放、全站复制、深度思考展示 |
| **用户与权限** | `/users` 用户管理（仅管理员）：邀请、改角色、启停、重置密码、吊销会话；最后管理员与自我操作守卫 |
| **主题** | 默认浅色，深色可选，跟随系统；全站色值走语义变量 |

**审批门限四档**（全局设置 + 会话覆盖）：

| 档位 | 行为 |
|---|---|
| `all` | 全部命令审批，含只读 |
| `standard` | 危险命令审批，只读免审（默认） |
| `high` | 仅高危审批 |
| `none` | 免审批全部自动执行，但仍落审计 |

服务器关闭「自动执行」时，非只读命令在任何档位都必须审批（服务器级硬保护）。`forbidden` 级命令在任何档位都不可执行、不可审批。

## 监控看板

5 张只读图表分布在 `/monitor`、`/alerts`、`/inspections` 三页，均为原生 SVG；数据来自 6 个只读接口（监控概览/趋势、告警统计/事件、巡检任务/走势）。

| 图表（页面） | 回答的问题 | 数据口径（时间桶 / 点数上限） | 权限 |
|---|---|---|---|
| 最紧张服务器 Top 12（`/monitor`） | 此刻哪几台服务器最紧张？ | `GET /api/monitor/overview` 的 `servers[]`：每台一行最新采样（仅 `device_kind=server`，网络设备不采集）；CPU / 内存 / 磁盘切换排序，全量扫描取前 12，缺失值排最后；采样年龄超过 `max(5 分钟, 2×采集间隔)` 标为过期；采样时间超前服务端时钟的服务器不参与排名并提示台数。无时间桶，上限 12 条。 | `monitor:read` |
| 集群压力分桶趋势（`/monitor`，近 24 小时 / 7 天） | 集群整体压力有没有恶化，峰值在哪个时段？ | `GET /api/monitor/trend`：24h 档 1 小时/桶 × 24 点，7d 档 6 小时/桶 × 28 点，按 UTC 桶边界对齐；无样本桶为 `null`，折线断开；统计上界钳到服务端当前时刻，超前样本不计入；绘制 CPU P95 / 均值 / 峰值。上限 28 点。 | `monitor:read` |
| 告警发生分布 · 严重度时间分布（`/alerts`，按小时 × 严重度） | 告警集中在哪些时段，严重度构成如何？ | `GET /api/alert-events/stats` 的 `severity_timeline[]`：桶宽同趋势图（1h / 6h），空桶补零；24h 档 24 桶、7d 档 28 桶；按严重 / 警告 / 信息堆叠，点柱按半开时间桶下钻事件表；未走完的末桶标「进行中」。上限 28 桶。 | `monitor:read` |
| 最吵的规则 Top 8（`/alerts`） | 哪些告警规则在制造噪音？ | 同一接口的 `top_rules[]`：按窗口内触发次数降序取前 8（接口默认 8、可调 1–20）；显示影响服务器数与最近触发时间；点条按 `rule_id` 下钻，下钻事件表与图同时间窗。上限 8 条。 | `monitor:read` |
| 巡检均分走势（`/inspections`，按任务） | 定时巡检的整体质量在变好还是变差？ | `GET /api/inspection/trend`：按报告份数取最近 30 份（接口 1–60，默认 30；`truncated` 表示还有更早），不是时间桶；任务下拉含「全部任务」，默认最近一份报告所属任务（最近一份为即时巡检时选「全部任务」）；均分 0–100，即时巡检报告也计入。上限 30 份。 | `inspection:manage` |

当前权限矩阵下仅 `admin` 与运维工程师具备上述两项权限；审批者、审计员不渲染图表与入口。图表只读；采集配置的修改另需 `monitor:manage`。

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

打开 http://localhost:5173 ，登录页填用户名与密码；首次部署用 `admin` + `ADMIN_PASSWORD`。后续账号由管理员在 `/users` 创建邀请，不能共用 `admin`。

未设置 `ADMIN_PASSWORD` 时，首次启动会生成 24 字符随机密码并**只在日志里打印一次**。之后再启动会忽略该变量，不会重置密码。密码可在顶栏头像下拉「修改密码」中自助修改（改密后全部会话失效）。

可承担审批职责的启用账号（`admin` + `approver`）不足 2 个时进入单管理员降级模式：**不能执行 `needs_approval` 命令、不能开终端/RDP、不能放宽审批策略**。请先用独立通道创建第二个可审批账号（`admin` 或 `approver`）；「1 个 admin + 1 个 approver」是合规部署，不会降级。

### 主要环境变量

多用户 RBAC 下，邀请与密码重置 token 只能经独立 SMTP 通道投递（API 永不返回 token）。未配置 SMTP 时管理员仍可登录，但无法创建第二个账号；请在生产环境设置上述 `AIOPS_SMTP_*`。

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
| `AIOPS_SMTP_HOST` | 空 | invite/reset token 投递的 SMTP 主机；**空 = 无法创建用户/重置密码（503 DELIVERY_UNAVAILABLE）** |
| `AIOPS_SMTP_PORT` | `587` | SMTP 端口；配合 `AIOPS_SMTP_USE_TLS` 决定 TLS 方式（见下） |
| `AIOPS_SMTP_USERNAME` / `AIOPS_SMTP_PASSWORD` | 空 | SMTP 认证（可选；有用户名时使用 PLAIN） |
| `AIOPS_SMTP_FROM` | 同用户名 | 发件人地址 |
| `AIOPS_SMTP_USE_TLS` | `true` | `true`：465 走 implicit TLS，其余端口 STARTTLS（服务器不支持 STARTTLS 则投递失败）；`false`：明文（仅内网中继） |
| `AIOPS_SMTP_IMPLICIT_TLS` | `false` | 非 465 的自建 SMTPS 端口置 `true`，强制连接即 TLS |
| `AIOPS_BOOTSTRAP_APPROVER_EMAIL` | 空 | 首次启动为该邮箱创建 approver 邀请（需先配 SMTP）；空 = 单管理员降级模式 + `rbac_single_admin_warning` 审计 |

启动日志会打印实际生效的 TLS 模式（`tls_mode=starttls|implicit|disabled`）与端口；投递失败时先用它核对端口与模式是否匹配（587 配 implicit 会在首包 TLS 握手上被服务器断开）。

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
# 可选但强烈建议：配置 SMTP 后管理员才能创建第二个账号（否则审批永远
# 卡在待处理）。未配置时 compose 仍能起来，但 POST /api/users 返回 503。
export AIOPS_SMTP_HOST='smtp.example.com'
export AIOPS_SMTP_PORT=587            # 465 自动改用 implicit TLS
export AIOPS_SMTP_USERNAME='ops@example.com'
export AIOPS_SMTP_PASSWORD='...'
export AIOPS_SMTP_FROM='ops@example.com'
# 可选：首次启动直接创建 approver 邀请，跳过手建第二个账号。
export AIOPS_BOOTSTRAP_APPROVER_EMAIL='approver@example.com'
docker compose up -d --build
docker compose logs -f app      # 确认 tls_mode=starttls 且无 smtp 投递告警
curl http://127.0.0.1:18081/healthz   # 应返回 200 与 {"status":"ok",...}
```

- 部署后**先验证 SMTP**：用管理员登录 → 「用户」页创建一个审批者；返回 201 且目标邮箱收到邀请即通道正常，返回 503 `DELIVERY_UNAVAILABLE` 则按启动日志的 `tls_mode` 核对端口/模式（587=STARTTLS、465=implicit）。
- `AIOPS_SMTP_*` 与 `AIOPS_BOOTSTRAP_APPROVER_EMAIL` 已由 `docker-compose.yml` 透传；也可把 `.env.example` 复制为 `.env` 后由 compose 读取。

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

### 角色与权限

角色固定四类，每用户恰一个角色；权限全集 19 条（`resource:action`），由服务端 `internal/authz` 的 `rolePermissions` 计算，客户端提交的 role/permissions 一律不采信（未知角色 fail-closed）。

| 角色 | 中文名 | 权限数 | 职责 |
|---|---|:---:|---|
| `admin` | 管理员 | 19 | 用户、资产凭据、供应商、审批策略、执行与审计；不能审批自己发起的请求，不能单方面放宽门限 |
| `operator` | 运维工程师 | 8 | 发起对话、只读探测、命令/容器/批量执行（高危走审批）、巡检与 JIT 终端；不管用户、凭据、策略 |
| `approver` | 审批者 | 3 | 只审批不发起：查看待办、有界上下文与资产元数据，决定他人发起的命令/终端授权/策略变更；不能发起对话或执行命令 |
| `auditor` | 审计员 | 1 | 只读治理：审计日志、哈希链校验、回放目录、跨用户回放与录制；不能执行、审批、升级 WS 或修改任何资源 |

**职责分离的执行点**：

- **路由覆盖**：`internal/httpapi/authz_guards.go` 的权限守卫表（`routeRules`，契约 12.4 称 `routeGuards`）是唯一事实来源；`TestRoutePermissions` 遍历 `gin.Engine.Routes()`，断言每条非静态 `/api` 路由恰好有一条 guard（`authenticated`、单权限或「权限+实例规则」），未列出路由测试/启动失败；`NoRoute` 对 `/api/*` 返回 404。
- **鉴权与会话**：`AuthenticateSession` 返回 user 与 auth session；`SessionStore.Get` 供 WS 每 30 秒复检；`requireAuth` 注入 `user/user_id/auth_session_id`。邀请未接受（`status=invited`）或停用（`status=disabled`）登录 401，停用用户的存量会话在下一次 `Authenticate` 时 401。`must_change_password=` 为真时 REST、WS、pprof 共用同一道门，除 `me`/`logout`/`change-password` 外全部 403 `PASSWORD_CHANGE_REQUIRED`。
- **用户管理**：`/users` 仅管理员可见。创建用户只签发一次性邀请 token（SMTP 独立通道，API 永不返回 token）；管理员永不设置或知道他人密码；改角色、停用、重置、吊销会话会立即吊销目标全部 auth session、关闭 live 终端/RDP，并在同一事务内作废该账号的待处理审批、awaiting_approval 工具调用、已批准未消费的终端授权与批量审批台（账号重新启用后旧审批不能再被批准）。投递失败时创建/重置整体回滚（不产生占用用户名的僵尸账号、不误踢人下线）。最后管理员与自我操作由服务端 409 `LAST_ADMIN` / `SELF_OPERATION` 守卫。
- **登出**：`POST /api/auth/logout` 删除 auth session 后按该 session 关闭终端/RDP WebSocket 与 SSE 流（登出后既有 shell 立即断开）；停用/降级/吊销会话则按用户关闭全部交互连接。
- **审批**：`requested_by == decided_by` 返回 403 `APPROVAL_SELF_DECISION`；审批决定只接受 `decision` 与 `note`，不接受 `edited_command`。历史 pending 改写行迁移时置 `expired`；需要改命令时先拒绝再重新发起。
- **单管理员**：可承担审批职责的启用账号（`admin` + `approver`，`CountEnabledApprovers`）≤ 1 时进入降级模式——审批批准、终端/RDP 授权与会话、policy 放宽全部 409 `NO_SECOND_APPROVER`，启动时写 `rbac_single_admin_warning` 审计。判定只看「可审批账号」而不是「管理员人数」：1 个 admin + 1 个 approver、或 1 个 approver + 若干 operator 都是正常/需预警的两种不同部署，前者不降级、后者降级。恢复只提供主机侧、受文件系统权限保护的一次性 CLI（强审计、单次、需 `reason`/`confirm`），不提供 HTTP break-glass：
  - `aiops admin reset --user <name> --reason <r> --confirm`：置 `must_change_password=true`、吊销全部会话、作废待处理授权、写 `admin.offline_reset`；
  - `aiops break-glass --action approve --approval-id <id> --reason <r> --confirm`：单次批准一条 pending 审批、置 `break_glass=true`、写 `admin.break_glass`；
  - `aiops audit anchor --out <file>`：以独立账号定期导出审计链头锚点（追加 JSON 行、0600、拒绝符号链接）。

**凭据加密**：AES-256-GCM，12 字节随机 nonce，AAD 固定 `aiops:v1`。加密 SSH 密码/私钥/私钥口令、数据库密码、模型 API Key。API 响应只返回 `has_password` / `api_key_masked` 之类的布尔或掩码，永不返回明文或密文；解密只在执行前一刻发生在内存中。

**主密钥丢失 = 所有已保存凭据不可恢复**，且 v1 不支持轮换。请把 `master.key` 或 `AIOPS_MASTER_KEY` 单独备份。

**认证**：多用户登录，bcrypt cost 12；会话 token 32 字节、库存 SHA-256、绝对有效期 24h；Cookie `HttpOnly` + `SameSite=Lax`；登录按 IP 令牌桶限流（约 5 次/分钟）；所有非 GET 请求校验 `Origin`；邀请未接受/停用账号登录 401，停用后存量会话 401；SSH 主机密钥 TOFU，指纹不匹配拒绝执行并写高危审计。

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
│   ├── authz/       # 角色→19 条权限映射与守卫
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

当前状态（2026-11-18 第 12 轮走查与终版复验；门禁全绿）：

| 检查 | 结果 |
|---|---|
| `go vet ./...` / `go test ./...` | 全部包通过（30 包，含真实 Postgres 审计链校验集成测试） |
| `pnpm typecheck` / `lint` / `build` | 全通过 |
| Vitest 单测 | 97 文件 / 1524 项 |
| Playwright E2E | 299 项通过（真实 Chromium，11.9m） |
| 模糊测试 | `FuzzClassify` / `FuzzWinClassify` / `FuzzNetClassify` / `FuzzCheck` 各 10s 无崩溃；第 12 轮复验 0 崩溃 |
| 浸泡 15 分钟 | PASS（exit 0），SSE p95 32ms，RSS 峰 74MB（2026-11-18 14:48 产物） |
| 首屏 JS gzip | 123.4 KiB（126,405 B；预算 150KB） |
| 镜像与 compose | 构建、起停、健康探针通过；终版复验镜像 0 失败（第 11 轮：compose app Healthy + `/healthz` database=ok） |

本轮 RBAC 收口累计新增两类测试资产：`web/e2e/rbac-roles.spec.ts`（四角色真实浏览器端到端，22 项断言：登录导航项、无权路由 403、审批者工作流、审计员只读、会话失效）与 `internal/httpapi/rbac_matrix_test.go`（全路由越权矩阵，16 项：规则表双向对账、4 角色 × 每条非公开路由、fail-closed、未认证扫描、职责分离）。

RBAC 收口走查 12 轮得分 44 / 78 / 52 / 80 / 92 / 95 / 95 / 94 / 84 / 95 / 85 / 95（通过线 99），累计修复 56 项，最高 95/100，剩余 2 项未清零。终版复验：门禁绿、浸泡 15 分钟 exit 0、模糊 0,0、镜像 0。

```bash
# 模糊测试续跑（快验 30s）
go test ./internal/risk -run '^$' -fuzz 'FuzzClassify$' -fuzztime=30s
go test ./internal/sqlguard -run '^$' -fuzz 'FuzzCheck$' -fuzztime=30s

# 端到端
pnpm -C web test:e2e
```

## 已知限制

1. **RBAC 边界**：角色固定四类（`admin` / `operator` / `approver` / `auditor`），每用户恰一个角色；不做自定义角色、多角色/角色继承，也不按 server/database 实例授权（仅会话、终端/RDP 录制、batch/diagnosis/inspection 的 owner 规则）。不做 SSO/OIDC/LDAP/MFA/邮件之外的独立身份证明：密码认证 + 管理员创建账号无法在密码学上证明两个账号由两个独立自然人控制；本设计用独立通道投递 invite/reset token、创建者 IP 兑换拒绝、同 IP 告警降低红队第 3 条风险，但不把它宣传为强 SoD，需要强双人时上 SSO/MFA。单管理员部署默认降级：不能执行 `needs_approval`、不能开终端/RDP、不能放宽 policy；只提供主机文件系统权限保护的一次性 break-glass CLI（强审计、单次、需 `reason`/`confirm`），不提供 HTTP break-glass。不做审批时命令改写：改写会让审批者成为动作作者；历史 pending 改写行迁移时直接 `expired`，新请求不允许 `edited_command`，需要改命令先 deny 再重发。
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
19. **RBAC 收口测试**：最高 95/100（通过线 99）；RBAC 收口测试未达通过线，剩余 2 项见报告。
20. **监控看板走查**：18 轮走查最高 100/100（通过线 99），第 18 轮无可做项；终版复验仍存在未通过项。

---

其他说明见 [CHANGELOG.md](CHANGELOG.md)。
