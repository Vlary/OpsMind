# 云枢 OpsMind

**单管理员、单实例部署的 AI 运维控制台**：在对话里用自然语言下达运维指令，模型通过固定的 18 个工具读取服务器/数据库/容器信息、执行命令（Linux 走 SSH；Windows 走 OpenSSH 或 WinRM，按 PowerShell 语义风险分级；网络设备走 SSH/Telnet 双通道，按厂商 CLI 语义风险分级）、查询外部数据库（PostgreSQL/MySQL/SQLite/SQL Server/Redis/MongoDB，全部只读）；高风险命令必须先人工审批，所有执行、审批、资产变更和登录事件写入只追加的审计日志。二期新增：堡垒机式 Web 终端（xterm.js + WebSocket + PTY，asciinema v2 全程录制、浏览器内回放）与浏览器内 RDP 远程桌面（guacd 边车、凭据服务端注入、会话录制）。三期新增：网络设备接入（Cisco/华为/H3C/Juniper/通用 五品牌，SSH+Telnet 双通道含明文确认，网络 OS 风险分册，facts 适配，终端 PTY 支持）与 MongoDB 数据库（JSON 命令文档 + 聚合白名单守卫）。四期新增：容器管理（Docker/Podman/containerd 三运行时探测与适配，容器/镜像列表、日志、start/stop/restart 经 console 审批，AI 容器工具五个，容器风险分册重写）。五期新增——**AI 自动化**：监控告警（SSH/WinRM 固定模板采集六指标入 PG、阈值规则评估、防风暴、webhook/飞书/钉钉/邮件四渠道通知）、AI 自诊断（告警触发→只读工具集会话→报告与建议修复，修复必须走审批）、AI 定时巡检（cron 任务+模板+规则评分+趋势对比，不用 LLM）、批量操作（自然语言→逐台风险分类→逐台审批→并行执行→轮询进度）；**四大能力全部注册为 AI 工具（对话中一句自然语言直接可用）并同时提供页面入口**。六期新增——**交互深化**：审批门限四档（`all`/`standard`/`high`/`none`，全局设置 + 会话覆盖，服务器关闭「自动执行」时非只读命令仍硬保护性拦截）、审批时命令改写（审批卡直接编辑命令，服务端重新分类后执行改写命令，审计同时留原命令与改写命令）、会话执行回放（对话工具栏与审计页两处入口，时间轴播放器：播放/暂停/单步/1x·2x·4x 速度，步骤含用户消息、工具调用、审批、执行、深度思考与回答）、全站复制（统一 `CopyButton`，关键字段一键复制，剪贴板两级降级 + 可选中文本兜底）、深度思考（`ReasoningBlock`：流式折叠摘要「深度思考 · 耗时 Ns」、展开可复制全文，`reasoning` 持久化并作为独立步骤进入回放）。

后端是 Go 单二进制，前端是 React SPA，元数据存 PostgreSQL；凭据（SSH/WinRM 密码、私钥、数据库密码、模型 API Key）经主密钥加密后入库。

## 界面预览

> 以下均为实际运行界面截图（默认浅色主题）。

**对话主界面** — 自然语言下达运维指令，模型调用工具读取资产并给出结论：

![对话主界面](docs/screenshots/chat.png)

**服务器资产** — Linux / Windows / 网络设备统一纳管，含自动执行策略与连接测试：

![服务器资产](docs/screenshots/servers.png)

**监控** — 六指标趋势、告警状态与采集配置：

![监控](docs/screenshots/monitor.png)

**审计** — 只追加日志，可按动作/目标类型/目标 ID 筛选：

![审计](docs/screenshots/audit.png)

---

## 目录

- [1. 界面说明](#1-界面说明)
- [2. 快速开始](#2-快速开始)
- [3. 生产部署](#3-生产部署)
- [4. 功能说明](#4-功能说明)
- [5. 安全模型](#5-安全模型)
- [6. 技术栈、项目结构与维护](#6-技术栈项目结构与维护)
- [7. 已知限制](#7-已知限制)
- [8. 测试与验证](#8-测试与验证)

---

## 1. 界面说明

登录后进入左侧导航 + 主内容区的布局（移动端导航收进抽屉）：

| 页面 | 路径 | 说明 |
|---|---|---|
| 登录 | `/login` | 内置管理员 `admin`，页面只填**密码** |
| 对话 | `/chat`、`/c/:conversationId` | 主页面。左侧会话列表；中间消息流；底部输入框 |
| 服务器 | `/servers` | 资产表格、连接测试、自动执行开关；Linux（SSH）、Windows（OpenSSH/WinRM）与网络设备（SSH/Telnet，三期） |
| 数据库 | `/databases` | PostgreSQL / MySQL / SQLite / SQL Server / Redis / MongoDB 资产、连接测试、只读 SQL/命令/JSON 文档控制台 |
| 模型 | `/providers` | 模型供应商与模型列表、连接测试、拉取模型 |
| 终端 | `/term` | 堡垒机式 Web 终端：选服务器开 PTY 会话，全程 asciinema v2 录制 |
| 回放 | `/recordings` | 终端录制（.cast）浏览器内回放与下载；RDP 录制（.guac）元数据与下载 |
| 桌面 | `/rdp` | 浏览器内 RDP 远程桌面（guacd），会话创建/接入/断开 |
| 审计 | `/audit` | 只追加审计日志，按动作/目标类型/目标 ID 筛选 |
| 容器 | `/containers` | 四期：选服务器查看容器/镜像列表、日志抽屉；start/stop/restart 走审批确认弹窗 |
| 监控 | `/monitor` | 五期：监控概要（已采集服务器数/触发中告警/最近巡检均分/采集器状态四卡）、采集配置（开关+间隔）、服务器指标趋势 |
| 告警 | `/alerts` | 五期：告警规则/告警事件/通知渠道三 Tab；告警行可拉起 AI 诊断抽屉（报告+建议修复审批） |
| 巡检 | `/inspections` | 五期：巡检任务（cron）列表与创建/编辑、立即巡检、报告抽屉（per-server 评分/异常项/趋势对比） |
| 批量 | `/batch` | 五期：批量任务创建（命令+目标多选+实时风险预览）、任务列表、详情抽屉（per-server 审批/进度/结果） |
| 设置 | `/settings` | 六期：全局审批门限四档选择（全部审批 / 危险命令审批 / 仅高危审批 / 免审批）与各档说明，保存即生效 |

**对话页**：首次发送前需要选择供应商与模型（默认取第一个供应商的 `default_model`，也可手填模型）；会话一旦绑定供应商/模型，后续只能新建会话切换。输入框上方可用标签附加“服务器上下文 / 数据库上下文”，这些 ID 会随消息一起发给模型。回答以 SSE 流式渲染，工具调用以卡片展示，高风险命令在消息流中出现审批卡片（命令、服务器、风险级别、原因、倒计时、批准/拒绝）。流进行中发送按钮变为“停止”；离开会话或点击停止会中断当前流，未处理的审批随之失效。没有自动重连，刷新后从服务端拉取已持久化的消息。六期起：会话头部有**审批门限覆盖**下拉（跟随全局 / 四档任选，显示当前生效档位与来源）与**回放**入口（流式进行中禁用，结束后打开时间轴播放器）；模型输出 `reasoning` 时先出现**深度思考块**（流式中自动展开显示「思考中…」，首个正文 token 到达后折叠为「深度思考 · 耗时 Ns」，展开可复制全文）；审批卡命令可直接编辑后再批准（改写命令服务端重新分级，`forbidden` 拒绝）；消息气泡、工具卡命令/输出、代码块、思考全文均有统一复制按钮。

**服务器页**：新增/编辑抽屉支持密码认证或私钥认证（私钥可带口令），可设置标签和“自动执行”开关；`os` 选 `linux`/`windows`（三期扩 `network`，此时资产形态为 `device_kind=network_device`，需选厂商 cisco/huawei/h3c/junos/generic 与接入协议 ssh/telnet，Telnet 必须勾选明文确认，可选填 enable/super 特权密码）。Windows 机器认证方式四选（`password`/`private_key` 走 OpenSSH；`password_ntlm`/`password_basic` 走 WinRM，后者明文认证，表单会提示仅内网或 HTTPS 5986 使用）。连接测试会显示延迟、服务端版本和主机密钥指纹（WinRM 探针与 Telnet 通道无主机密钥语义，指纹为空）。首次连接采用 TOFU 记录指纹；修改主机或端口会清空已信任指纹，下次连接重新确认。

**数据库页**：按引擎切换表单（SQLite 只填绝对路径；PG/MySQL 填主机、端口、库名、用户、密码、SSL 模式；SQL Server 额外可填实例名，与端口互斥；Redis 填主机、端口、可选用户/密码与逻辑库号；MongoDB 填主机、端口、目标库、可选用户名/密码成对与认证库 `auth_db`，端口缺省 27017）。SQL 控制台是**只读**抽屉，支持 `Ctrl/⌘ + Enter` 执行，结果最多 1000 行 / 1MB；MongoDB 的输入是单条 JSON 命令文档（如 `{"find":"orders","limit":10}`，见 4.11）。

**模型页**：内置 9 个预设（OpenAI、DeepSeek、通义千问、Moonshot/Kimi、智谱 GLM、Anthropic Claude、Gemini、Ollama、自定义 OpenAI 兼容），选预设自动填名称/协议/Base URL/建议模型；Base URL 可覆盖；`custom` 可自选协议类型。已保存的 API Key 只显示 `****` + 末 4 位，留空表示不修改；可单独勾选清除。支持连接测试和从上游拉取模型列表。

**审计页**：按动作、目标类型、目标 ID 筛选，游标分页；每条记录展示 actor、动作、资源、耗时、request_id、detail JSON，以及哈希链的 `prev_hash` / `hash`。审批没有独立页面，只在对话内出现。

---

## 2. 快速开始

### 2.1 前置条件

- **Go 1.27**（`go.mod` 声明 `go 1.27`）
- **Node.js 22+**（构建镜像使用 Node 24）
- **pnpm 10**
- **Docker + Docker Compose**（仅用于本地 PostgreSQL）

### 2.2 步骤

所有命令默认在 `aiops/` 目录执行。

```bash
cd aiops

# 1) 启动开发用 PostgreSQL（127.0.0.1:65432，用户/密码/库均为 aiops）
docker compose up -d --wait db

# 2) 下载 Go 依赖
go mod download

# 3) 安装前端依赖
pnpm -C web install

# 4) 配置环境变量（后端不会自动读取 .env）
export AIOPS_DATABASE_URL='postgres://aiops:aiops@127.0.0.1:65432/aiops?sslmode=disable'
export ADMIN_PASSWORD='换成一个强密码'   # 仅首次创建 admin 时使用

# 可选：显式提供主密钥；不提供则自动生成 ./data/master.key
# export AIOPS_MASTER_KEY="$(openssl rand -base64 32)"

# 5) 启动后端（默认 :8080）
go run ./cmd/aiops

# 6) 另开一个终端启动前端（Vite，默认 :5173，代理 /api 与 /healthz 到 8080）
pnpm -C web dev
```

打开 **http://localhost:5173**，输入管理员密码登录。

> 也可以复制 `.env.example` 为 `.env`，然后 `set -a; . ./.env; set +a` 后再启动后端。后端本身不加载 `.env`。`docker compose` 会自动读取项目目录下的 `.env` 做变量插值。

### 2.3 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `AIOPS_ADDR` / `PORT` | `:8080` | HTTP 监听地址。`PORT=8080` 会规范化为 `:8080` |
| `AIOPS_DATABASE_URL` / `DATABASE_URL` | 无（**必填**） | PostgreSQL DSN，日志中密码脱敏 |
| `AIOPS_MASTER_KEY` | 空 | 32 字节主密钥的标准 base64；优先于文件 |
| `AIOPS_DATA_DIR` / `DATA_DIR` | `./data` | `master.key` 所在目录 |
| `ADMIN_PASSWORD` | 空 | 仅首次创建 `admin` 时使用；已有账号时忽略 |
| `AIOPS_ALLOWED_ORIGINS` | `http://localhost:5173,http://localhost:8080` | 写请求 Origin 精确匹配列表，逗号分隔 |
| `AIOPS_PUBLIC_ORIGIN` | 空 | 站点对外的 scheme://host[:port]，用于判断是否启用 Secure Cookie；**https 部署必须设置**（如 `https://ops.example.com`），否则 Cookie 不带 Secure 标志 |
| `AIOPS_SECURE_COOKIES` | 自动 | `true/false`，覆盖按 `AIOPS_PUBLIC_ORIGIN` 的自动判断 |
| `AIOPS_TRUST_PROXY` | `false` | 仅信任 `127.0.0.1` / `::1` 的 `X-Forwarded-For` |
| `AIOPS_WEB_DIR` | `./web/dist` | 生产静态文件目录；显式设为空串则只提供 API |
| `AIOPS_LOG_LEVEL` | `info` | `debug/info/warn/error` |
| `AIOPS_SESSION_TTL` | `24h` | 会话绝对有效期 |
| `AIOPS_APPROVAL_TTL` | `120s` | 审批超时 |
| `AIOPS_STREAM_MAX_DURATION` | `30m` | 单条 SSE 上限 |
| `AIOPS_MAX_STREAMS` | `20` | 全局并发流上限（1..100） |
| `AIOPS_PPROF` | `false` | 打开时在 `/debug/pprof` 挂载 pprof（与 API 同鉴权；loadgen 采样依赖它） |
| `AIOPS_RECORDINGS_DIR` | `<DATA_DIR>/recordings` | 终端会话 `.cast` 录制目录（二期；启动时创建 0750，文件 0600） |
| `AIOPS_TERM_MAX_SESSIONS` | `4` | 全局并发终端会话上限（1..16；同服务器同时只允许 1 个） |
| `AIOPS_TERM_IDLE_TIMEOUT` | `10m` | 终端会话空闲超时（无输入/输出即断开封盘） |
| `AIOPS_TERM_MAX_DURATION` | `8h` | 终端会话最长时长 |
| `AIOPS_GUACD_ADDR` | 空 | guacd 边车地址（`host:port`，如 `guacd:4822`）；**空 = RDP 功能禁用**（其余功能不受影响） |
| `AIOPS_RDP_RECORDINGS_DIR` | `<DATA_DIR>/rdp-recordings` | RDP 会话 `.guac` 录制目录（二期；须与 guacd 容器共享同一卷） |
| `AIOPS_RDP_MAX_SESSIONS` | `2` | 全局并发 RDP 会话上限（1..8；同服务器同时只允许 1 个） |
| `AIOPS_RDP_IDLE_TIMEOUT` | `30m` | RDP 会话空闲超时 |
| `AIOPS_RDP_MAX_DURATION` | `8h` | RDP 会话最长时长 |
| `TEST_DATABASE_URL` | `postgres://aiops:aiops@127.0.0.1:65432/aiops?sslmode=disable` | Go 集成测试 / smoke 使用的本地库 |

### 2.4 首次登录

首次启动且 `users` 表为空时，后端会创建唯一账号 `admin`：

- 设置了 `ADMIN_PASSWORD`：用该密码登录。
- 未设置：自动生成 24 字符随机密码，**只在启动日志/标准输出打印一次**（`admin password: ...`）。请立即保存。
- 之后再次启动会忽略 `ADMIN_PASSWORD`，不会重置密码。
- 密码轮换/修改：登录后从顶栏头像下拉打开「修改密码」（需提供旧密码；成功后所有会话失效，须用新密码重新登录），详见 5.2。

用户名固定为 `admin`，登录页不需要填写用户名。

### 2.5 演示数据（无 Key 演示）

后端启动后，一条命令播种演示资产（幂等，重复执行自动跳过已存在项）：

```bash
# 1) 启动假 LLM（OpenAI 兼容 SSE，无需真实 API Key）
node web/e2e/fixtures/fake-llm.mjs &

# 2) 播种：两台示例服务器（192.0.2.x 文档地址，不会真正连通）+
#    指向假 LLM 的供应商 + 「演示：资产巡检」会话
ADMIN_PASSWORD=<你的管理员密码> make seed

# 3) 可选：在演示会话发一条消息，走通对话→工具→回复全链路
ADMIN_PASSWORD=<你的管理员密码> make seed CHAT=1
```

说明：

- `make seed` 通过运行中的 API 创建数据，目标后端默认 `http://localhost:8080`，
  可用 `SEED_BASE_URL` 覆盖（该地址同时作为写请求 Origin，需在后端
  `AIOPS_ALLOWED_ORIGINS` 白名单内）。
- 供应商默认指向假 LLM `http://127.0.0.1:18081`，可用 `SEED_LLM_BASE_URL`
  指向任何 OpenAI 兼容服务（此时建议换成真实 Key 的供应商）。
- 换成真实 LLM 时把供应商的 `base_url`/`api_key` 改掉即可，演示资产不受影响。

---

## 3. 生产部署

### 3.1 方式一：docker compose（app + postgres）

```bash
cd aiops
export ADMIN_PASSWORD='生产环境强密码'
docker compose up -d --build
docker compose logs -f app
```

- `db` 使用 `postgres:16-alpine`，数据卷 `pgdata`；`app` 由多阶段 `Dockerfile` 构建（前端 `pnpm build` + Go 静态二进制 `CGO_ENABLED=0`，运行镜像为 `gcr.io/distroless/static-debian12:nonroot`，实测 `aiops-app:latest` 约 56MB）。也可单独 `make docker-build`（等价 `docker build -t aiops:local .`）。
- `aiops-data` 卷挂载到 `/app/data`，保存 `master.key`。**必须备份该卷或改用 `AIOPS_MASTER_KEY`**，否则凭据无法解密。
- 应用启动时自动执行 goose 迁移，失败即退出；DB 需先 healthy。
- 首次启动创建管理员，之后 `ADMIN_PASSWORD` 不再生效。
- **app 发布在宿主机 `127.0.0.1:18081 → 容器 8080`**（本仓库部署验证的约定端口，与 db 一致只绑环回，避免管理/审批 API 暴露到全部网络接口）。`AIOPS_ALLOWED_ORIGINS` / `AIOPS_PUBLIC_ORIGIN` 默认 `http://localhost:18081`，可由环境变量覆盖。对外提供服务时改这两个值为浏览器实际访问的 `scheme://host:port`，并建议前面挂反向代理（见 3.3）。
- **健康检查**：栈就绪后 `curl http://127.0.0.1:18081/healthz` 应返回 `200` 与 `{"status":"ok",...,"database":"ok"}`（本轮实测通过，见第 8 节）。
- **guacd 边车（二期 RDP）**：compose 栈含 `guacamole/guacd:1.5.0` 服务（容器内 `guacd:4822`，宿主机 `127.0.0.1:14822` 供本地直连调试），app 经 `AIOPS_GUACD_ADDR=guacd:4822` 连接；`.guac` 录制写入共享卷 `rdp-recordings`（两侧同挂 `/app/rdp-recordings`）。guacd 不可达只影响 RDP 端点（503 `GUACD_UNAVAILABLE`），其余功能正常；**不用 RDP 的部署可整体移除 guacd 服务与该依赖**。本地直连调试 RDP 用 `docker compose up -d --wait db guacd` + `AIOPS_GUACD_ADDR=127.0.0.1:14822`。

### 3.2 方式二：连接外部 PostgreSQL

应用只依赖 `AIOPS_DATABASE_URL`，不要求数据库与 app 同容器。先准备好空库和一个具备建表权限的账号（启动时自动建表/索引/触发器），然后：

```bash
cd aiops
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

说明：

- 使用 `AIOPS_MASTER_KEY` 时主密钥来自环境变量，`/app/data` 只用于其他运行时数据；无论哪种方式都要妥善备份密钥。
- 只允许单实例运行，不要对 app 做多副本（原因见[已知限制](#7-已知限制)）。
- 应用本身不终止 TLS；HTTPS 由反向代理负责。

### 3.3 反向代理注意事项

SSE 长连接对代理配置敏感，`/api/chat` 必须满足：

- **关闭响应缓冲**：Nginx `proxy_buffering off; proxy_cache off;`，并保留应用返回的 `X-Accel-Buffering: no`。
- **不压缩**：对 `/api/chat` 关闭 `gzip`（应用本身不注册全局 gzip）。
- **上游使用 HTTP/1.1**：`proxy_http_version 1.1;` + `proxy_set_header Connection "";`。对外（浏览器 → 代理）可以开 HTTP/2，但到 app 是明文 HTTP/1.1（应用不提供 h2c）；若前面还有 CDN/网关，需确认它不会聚合缓冲 SSE。
- **超时足够长**：单条流上限 30 分钟，读/写超时至少 31 分钟（例如 `proxy_read_timeout 1860s; proxy_send_timeout 1860s;`）。
- 转发 `X-Request-ID`（若有）和 Cookie；不要改写 `Origin`（应用按允许列表精确校验）。
- 仅当反向代理在本机（`127.0.0.1`/`::1`）时才设置 `AIOPS_TRUST_PROXY=true`；否则客户端可伪造 `X-Forwarded-For` 绕过登录限流。
- HTTPS 终止在代理后，设置 `AIOPS_PUBLIC_ORIGIN=https://...`、`AIOPS_ALLOWED_ORIGINS=https://...`，并确保 Secure Cookie 生效。

**终端与 RDP 的 WebSocket（二期新增）**：`GET /api/term/sessions/{id}/ws`（子协议 `opsmind-term-v1`）与 `GET /api/rdp/sessions/{id}/ws`（子协议 `guacamole`）是长连接 WebSocket，代理必须放行协议升级，否则浏览器端握手直接失败：

- **透传 Upgrade 头**：`proxy_set_header Upgrade $http_upgrade;` + `proxy_set_header Connection $connection_upgrade;`（`$connection_upgrade` 需用 `map` 按 `$http_upgrade` 生成，见下例）；**不能**沿用 SSE 那条 `Connection ""` 的配置——那会剥掉升级头。
- **不要缓冲/压缩**：`proxy_buffering off; gzip off;`（终端输出与 guacamole 指令流都是低延迟双向流）。
- **读超时覆盖应用层空闲治理**：终端空闲 10 分钟、RDP 空闲 30 分钟才由应用主动断开（终端另有 30s WS ping / 90s pong 判定，RDP 有 guacd 心跳）；代理的 `proxy_read_timeout` 必须大于这些值（建议 ≥ 1900s），否则代理会先掐断空闲会话。
- **不要限制子协议**：两个端点各自校验子协议并在 101 响应中回显，代理只需原样透传 `Sec-WebSocket-Protocol`。

Nginx 片段示例：

```nginx
# WebSocket 升级映射（http 块）
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

location /api/chat {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
    proxy_cache off;
    proxy_request_buffering off;
    gzip off;
    proxy_read_timeout 1860s;
    proxy_send_timeout 1860s;
}

# 终端与 RDP 的 WebSocket（注意 Upgrade/Connection 与 /api/chat 不同）
location ~ ^/api/(term|rdp)/sessions/[^/]+/ws$ {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
    gzip off;
    proxy_read_timeout 1900s;
    proxy_send_timeout 1900s;
}

location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

---

## 4. 功能说明

### 4.1 对话式运维

- 模型可调用固定 **18 个工具**：一期 6 个（`list_servers`、`server_facts`、`ssh_exec`、`list_databases`、`db_query`、`ask_user`）+ 四期 5 个容器工具（`container_list`、`container_inspect`、`container_logs`、`container_action`、`image_list`，见 4.12）+ 五期 7 个自动化工具（`get_metrics`、`list_alerts`、`audit_search` 恒 safe 只读；`run_diagnosis`、`run_inspection`、`get_inspection_report`、`batch_execute`，见 4.13–4.16——**对话中一句自然语言可直接发起诊断、巡检与批量操作**）。工具名沿用一期（Windows 与网络设备执行也走 `ssh_exec`，历史工具调用与审计不迁移）。
- 一次对话同一时刻只允许一条流；全局最多 20 条；单流最长 30 分钟；单轮工具迭代最多 12 次。
- 工具串行执行；SSH/WinRM 输出合并 stdout/stderr，最多 256KB，超出标记 `truncated`；数据库结果最多 1000 行 / 1MB。
- `os=windows` 的 `ssh_exec` 统一按 **PowerShell 语义**执行：OpenSSH 通道由后端包装 `powershell -NoProfile -NonInteractive -Command <cmd>`，WinRM 通道走 PowerShell 端点并附加 `; exit $LASTEXITCODE`；风险分类在**包装前**对原始命令做（见 4.6 与 5.3）。
- `device_kind=network_device` 的 `ssh_exec` 按**单条 CLI 命令**执行（见 4.10）：一次一条、不分段不组合，风险分类走网络设备分册 `risk.ClassifyNet(command, brand)`（5.3）。
- `ask_user` 会结束本轮，把问题作为助手消息返回，用户下一条消息即回答。
- 流式事件：`token`、`reasoning`、`tool_call`、`tool_result`、`approval_required`、`approval_resolved`、`done`、`error`。`done` 与 `error` 互斥且为最后一个事件。
- 前端不自动重连；断线/刷新后重新拉取已持久化消息。

### 4.2 服务器管理

- 资产字段：名称、主机、端口、登录用户、认证方式（密码 / 私钥 + 可选口令）、标签、自动执行开关；二期新增 `os`（`linux`/`windows`，缺省 `linux`）与 Windows 专用的 `winrm_port`（缺省 5985）；三期新增 `device_kind`（`server`/`network_device`）、`netdev_brand`、`access_proto`、`enable_secret`（见 4.10）；四期新增 `container_runtime` 探测缓存列（`''`/`none`/`docker`/`podman`/`containerd`，只由探测写回、不可经创建/更新设置，见 4.12）。
- 执行路由按 `os` × 认证方式双轨（详见 4.6）：`linux` 只走 SSH；`windows` 的 `password`/`private_key` 走 Windows OpenSSH Server（`port`），`password_ntlm`/`password_basic` 走 WinRM（`winrm_port`）；`network`（网络设备）按 `access_proto` 走 SSH PTY 或 Telnet（详见 4.10）。
- 连接测试只做握手，不执行命令；返回延迟、服务端版本、主机密钥指纹（SSH 通道）。
- 主机密钥 TOFU：首次连接接受并记录指纹（写审计）；之后不匹配会拒绝执行并返回 `SSH_HOST_KEY_CHANGED`。修改主机或端口会清空已信任指纹。WinRM 探针（`Get-WmiObject Win32_ComputerSystem`）无主机密钥语义，指纹返回空串；Telnet 通道同理（指纹恒空串、`first_seen` 恒 false）。
- 每服务器最多 4 条 SSH 连接，空闲 60s 回收；全局 64 条；WinRM 连接池独立失效（改 `winrm_port` 即失效）；网络设备每台最多 2 条并发连接（设备侧并发普遍有限），全局 32，见 4.10。
- “自动执行”只放行 `safe` 命令；`needs_approval` 永远审批，`forbidden` 永远不执行。开关不能降低风险等级。
- 删除服务器前先结束其活动终端/RDP 会话（`ended_reason=error`），再级联删行。

### 4.3 数据库管理

- 支持 PostgreSQL、MySQL、SQLite（SQLite 以只读方式打开绝对路径，禁用 `:memory:`）、二期新增的 **SQL Server、Redis**（见 4.7）与三期新增的 **MongoDB**（见 4.11），共六种引擎。
- 连接测试返回延迟与服务端版本（mssql 用 `SELECT @@VERSION`，redis 用 `PING` + `INFO server`）；创建/修改/删除后自动失效该库连接池。
- SQL 控制台只允许只读查询：单条 `SELECT`、`WITH … SELECT`、`EXPLAIN`（非 `ANALYZE`）。多语句、DML、DDL、`COPY`、`CALL`、`SELECT … FOR UPDATE/SHARE`、`SELECT … INTO` 等直接拒绝，返回 `SQL_NOT_READONLY`。
- 执行层再叠加只读事务/`query_only`、语句超时（默认 15s，最大 60s）、1000 行 / 1MB 截断。
- **数据库写操作没有执行路径**，也不会创建审批；需要写库时只能通过 SSH/WinRM 执行数据库 CLI，并走命令风险审批。

### 4.4 模型供应商

内置 9 个预设，均可在创建时覆盖 Base URL：

| preset | 名称 | 协议 | 默认 Base URL | API Key |
|---|---|---|---|---|
| `openai` | OpenAI | OpenAI 兼容 | `https://api.openai.com/v1` | 必填 |
| `deepseek` | DeepSeek | OpenAI 兼容 | `https://api.deepseek.com/v1` | 必填 |
| `dashscope` | 通义千问（DashScope） | OpenAI 兼容 | `https://dashscope.aliyuncs.com/compatible-mode/v1` | 必填 |
| `moonshot` | Moonshot / Kimi | OpenAI 兼容 | `https://api.moonshot.cn/v1` | 必填 |
| `zhipu` | 智谱 GLM | OpenAI 兼容 | `https://open.bigmodel.cn/api/paas/v4` | 必填 |
| `anthropic` | Anthropic Claude | Anthropic 原生 | `https://api.anthropic.com` | 必填 |
| `gemini` | Google Gemini | OpenAI 兼容 | `https://generativelanguage.googleapis.com/v1beta/openai` | 必填 |
| `ollama` | Ollama | OpenAI 兼容 | `http://127.0.0.1:11434/v1` | 可选 |
| `custom` | 自定义 OpenAI 兼容 | 可选 OpenAI 兼容 / Anthropic | 用户填写 | 可选 |

- 自定义方式：选 `custom`，填写名称、协议类型、Base URL、API Key（可空）、默认模型；模型列表可手填或用“拉取模型”从上游 `/models` 获取。
- API Key 只写不读：接口只返回 `has_api_key` 与 `api_key_masked`（`****` + 末 4 位）。
- 会话首次发送时绑定供应商/模型；已绑定后请求不同的供应商/模型返回 `CONVERSATION_MODEL_LOCKED`。

### 4.5 审批与审计

- **审批覆盖 SSH/WinRM/网络设备命令与容器页操作**：对话内 `ssh_exec`（Linux、Windows 双通道与网络设备 SSH/Telnet 双通道）；四期起容器页的 start/stop/restart 也走审批（**console 审批**：`source=console`，无对话/工具调用上下文，`command` 为按运行时固化好的逐字节容器 CLI 命令，批准时 decide 接口同步执行并返回 `execution`，见 4.12）。**哪些命令进入审批由审批门限四档决定**（六期，见 4.17）：`standard` 档下 `needs_approval` 与「安全但未开启自动执行」的 `safe` 命令都进入审批；`all` 档连只读命令也审批；`high`/`none` 档放宽为直执行（`none` 仅关闭「自动执行」的服务器仍拦非只读命令）。
- 审批默认 120 秒过期（`AIOPS_APPROVAL_TTL`），后台每秒扫描一次；只有 `pending` 可批准/拒绝；**是否进入审批由审批门限四档决定**（六期，见 4.17，全局 `standard` 为一至五期行为）；批准后执行命令与审批内容逐字节一致，不会静默改写——**用户可在批准时显式改写命令**（六期，见 4.18：改写命令服务端重新分级，审计同时记录原命令与改写命令）。
- 客户端断开、请求取消或服务关闭时，等待中的审批置为 `expired`（原因 `stream_cancelled`），绝不执行。
- 审计只追加：表上有 `BEFORE UPDATE OR DELETE` 触发器直接抛异常，应用不提供修改/删除 API。
- 审计覆盖登录成功/失败/限流、登出、密码修改（成功/失败）、资产 CRUD、连接测试、命令预览/执行/拒绝/审批、SQL 查询（含 MongoDB JSON 命令文档）、供应商变更、会话创建/删除、流错误等；二期新增终端/RDP 会话生命周期（`session.start` / `session.end`，`resource_type` 区分 `terminal_session`/`rdp_session`）与终端输入摘要（`session.input_summary`，见 4.8）；三期 Telnet 网络设备的创建/更新审计携带 `detail.confirm_telnet_cleartext=true`（明文确认留痕，见 4.10）；四期容器页操作经 console 审批执行后落 `container.action` 审计（`detail` 携带 `runtime`/`container_id`/`action`/`exit_code`/`source:console`，见 4.12）；五期新增 `metric.collect`（每轮采集聚合行）、`alert.triggered`/`alert.resolved`、`diagnosis.started`/`diagnosis.completed`、`inspection.started`/`inspection.completed`、`batch.created`/`batch.cancelled`（批量/诊断内执行与拒绝复用 `command.executed`/`command.denied`/`command.forbidden`，见 4.13–4.16）；六期新增 `settings.update`（审批门限变更）与 `command.approved` 的真实写入点，`command.executed` 的无条件 `command`（实际执行命令）、`approval.expired` 的逐条明细与 `container.action` 的门限快照 `approval_mode`（见 4.17–4.19）。
- 审计哈希链：首条 `prev_hash` 为 64 个 `0`，后续 `hash = SHA-256(prev_hash ‖ 规范化字段)`；单 writer 串行批量写入，保证顺序。
- `detail` 写入前脱敏（password/token/authorization/private_key/api_key/secret 等替换为 `[REDACTED]`），单字段 8KB、整体 32KB 上限。

### 4.6 Windows 服务器（二期）

**能力**：服务器资产区分 `os=linux|windows`。Windows 执行双轨（契约 2.7 定稿）：`password`/`private_key` 走 **Windows OpenSSH Server**（Server 2019/Win10 1809+ 内置，复用一期 `sshx` 全栈——连接池、TOFU 主机密钥、双认证）；未装 OpenSSH 的机器用 **WinRM 2.0**（`password_ntlm` 工作组默认协商、`password_basic` 需服务端显式开启，`winrm_port` 缺省 5985），由 `winrmx`（`masterzen/winrm`）执行。两条通道对同一命令产生一致的 PowerShell 执行语义（OpenSSH 由后端包装 `powershell -NoProfile -NonInteractive -Command`，WinRM 走 PowerShell 端点附加 `; exit $LASTEXITCODE`）。`ssh_exec` 工具名不变，Windows 命令按 **PowerShell/cmd 风险分册**（5.3）在包装前分类；`server_facts` 用固定 PowerShell 模板输出与 Linux 对齐的 JSON（`load_*` 恒 null、`kernel` 恒空串）。RDP 会话两通道都支持（凭据用密码，见 4.9）。

**边界**：WinRM 单请求超时上限 60s（协议 `MaxTimeoutms=60000`）；`password_basic` 是**明文认证**，只建议内网或 HTTPS（`winrm_port=5986`）；WinRM 认证的机器**开不了 Web 终端**（无 PTY 语义，400 `TERM_UNSUPPORTED`，前端直接禁用该项）；RDP 端口沿用 `server.port`（缺省 3389）。

**安全注意**：`auto_exec_enabled` 同样只放行 `safe`——Windows 分册的 `safe` 白名单是只读 cmdlet/命令（`Get-Service`、`Get-Process`、`ipconfig`、`systeminfo` 等），任何写操作至少 `needs_approval`；`-EncodedCommand` 等 Base64/隐藏窗口载荷**不解码直接 forbidden**；`WIN_REMOTE_EXEC`（`Invoke-Command`/`Enter-PSSession`/`PsExec` 等）一律 forbidden——它们会在被审计通道之外创建远端执行。开启自动执行前请确认 Windows 分册规则（5.3）覆盖你的实际用法。

### 4.7 SQL Server 与 Redis 数据库（二期）

**能力**：数据库资产新增两个引擎。**SQL Server**（`mssql`，`go-mssqldb`）：支持 `instance_name`（与端口互斥，经 SQL Server Browser UDP 1434 解析）或直连端口（缺省 1433）；连接测试 `SELECT @@VERSION`；SQL 控制台执行单条只读 T-SQL。**Redis**（`redis`，`go-redis/v9`）：配置 host/port/可选用户名密码/逻辑库号（0..1023，`redis_db`）；SQL 控制台输入单条 Redis 命令（如 `ZRANGE key 0 -1 WITHSCORES`），白名单内只读命令可执行，结果按固定规则映射成表格（单值一列、数组 index+value、`SCAN`/`HSCAN` 等 cursor+键、`INFO` 按行、二进制 base64）。

**边界**：两者都**只读**，与一期引擎同一裁决——写语句/写命令没有执行路径，也不能通过审批获得写权限（422 `SQL_NOT_READONLY`）。mssql 守卫是词法分类（T-SQL 无成熟 Go parser，不引 AST）：只允许单条 `SELECT`/`WITH…SELECT`，显式拒绝 `SELECT … INTO`、`FOR BROWSE/XML/JSON`、`OPENROWSET`/`OPENDATASOURCE`、`EXEC`/`sp_*`/`xp_*`、多语句、`BACKUP`/`RESTORE`。redis 是**命令白名单**（键/哈希/列表/集合/有序集合/位/服务器只读命令）：一切写命令（`SET`/`DEL`/`EXPIRE`/`FLUSH*`/`RESTORE`/`MIGRATE`/`DUMP`/`SHUTDOWN`/`CLIENT KILL` 等）、管道/内嵌 `MULTI` 与未知命令全部拒绝（`REDIS_NOT_WHITELISTED`）；`KEYS` 允许（O(N) 阻塞，UI 提示改用 `SCAN`）；`CONFIG GET` 允许但 `requirepass`/`masterauth` 等凭据参数与通配符形态拒绝（防响应明文回显服务端凭据）；`SELECT` 不在白名单——逻辑库由连接参数 `redis_db` 决定。mssql 执行不开显式事务（守卫已限定单条 SELECT；`SET LOCK_TIMEOUT` 会引入隐式事务，不设）。

**安全注意**：`db_query` 对 mssql/redis 同样走 5.5 的三道闸（分类守卫 → 执行器复检 → 驱动层限制），查询与拒绝均写审计（`database.query` / `database.query_rejected`）。`ssl_mode` 对 mssql 映射 `encrypt`/`TrustServerCertificate`，对 redis 映射 TLS；`require` 档为跳过证书校验的加密，仅 `verify-ca`/`verify-full` 做证书校验——生产库建议用校验档。

### 4.8 Web 终端与 asciinema 录制回放（二期）

**能力**：`/term` 页提供堡垒机式交互终端：选择 SSH 通道服务器（Linux 全部 + Windows OpenSSH）创建会话，浏览器内 xterm.js 经 WebSocket（子协议 `opsmind-term-v1`，input/output 帧 base64）直连后端 PTY，支持窗口尺寸同步（resize）、断开与重开。**全程录制**：asciinema v2（`.cast` NDJSON，header + 输出 `o` 事件 + resize `r` 事件，不录输入流），每 ≤64 事件或 1s 落盘一次，会话结束同步封盘并回填字节数。`/recordings` 页浏览器内回放（自研 castParser + xterm，含静默段进度插值、短录制禁拖）与 `.cast` 下载。

**边界**：同服务器同时只允许 1 个终端会话（第二个 409），全局并发上限默认 4（`AIOPS_TERM_MAX_SESSIONS`，1..16）；空闲 10 分钟 / 时长 8 小时自动断开封盘；会话不可重连，断开后须新建。输入帧解码后 >8KB 直接断开；resize 越界仅报错不断开。WinRM 认证的 Windows 机器无 PTY，不能开终端（见 4.6）。

**安全注意**：**终端会话不做命令级风险分类**——交互会话里用户敲的命令不经过 5.3 的规则引擎，治理手段是：全量输出录制（.cast）、输入摘要审计（每次会话结束前按行切分写入 `session.input_summary` 审计，每行截 512 字节、最多 1000 行，控制序列按不可打印字符替换后展示）与会话生命周期审计。因此**录制文件含会话全部屏幕输出**（可能包含密码回显、敏感文件内容等），`session.input_summary` 也会把输入行落审计——请把 `AIOPS_RECORDINGS_DIR` 与审计表同等级别保管，并注意磁盘容量（8h 会话的输出流可能很大）。“自动执行”开关与风险分册只约束对话里的 `ssh_exec`，**不约束**终端内的人工输入。

### 4.9 RDP 远程桌面（二期）

**能力**：`/rdp` 页在浏览器内打开 Windows 服务器的远程桌面：选择 Windows 服务器（两通道均可，凭据取已保存的密码）与分辨率（预设 1280×800 / 1600×900 / 1920×1080，dpi 96），会话经 WebSocket（子协议 `guacamole`，文本帧）→ Go 网关（`rdpx`，纯字节透传，不解析 guacamole 指令）→ **guacd 边车**（`guacamole/guacd:1.5.0`，FreeRDP）连到目标 3389。画布由 guacamole-common-js 渲染，支持剪贴板（双向）与全屏。

**边界**：分辨率创建后**不可变**（guac rdp 不支持运行中 resize），浏览器窗口变化由客户端侧自适应；同服务器同时只允许 1 个 RDP 会话，全局默认 2（`AIOPS_RDP_MAX_SESSIONS`，1..8）；空闲 30 分钟 / 时长 8 小时自动断开；guacd 未配置（`AIOPS_GUACD_ADDR` 为空）时 RDP 端点返回 503 `GUACD_UNAVAILABLE`，不影响其他功能，不用 RDP 可整体移除 guacd 服务。`.guac` 会话录制由 guacd 写入共享卷；**v1 回放仅元数据 + 下载**——浏览器内播放不支持 .guac 格式，回放页提示下载后离线转码/处理。

**安全注意**：RDP 凭据**服务端注入**——解密后的明文密码只出现在 guacd 握手 `connect` 参数里（进程内存、握手瞬间），不落日志、不进 WebSocket、不进审计（`logx.Redact` 兜底脱敏）。RDP 是**图形会话，没有命令级审计**：只有会话生命周期审计（`session.start`/`session.end`）与 `.guac` 录制文件，**录制含完整屏幕流**（可能包含敏感画面），保管要求同 4.8 的 `.cast`。RDP 会话同样不经风险分册——桌面内的操作不受本系统的命令治理约束，请仅对可信管理员开放。

### 4.10 网络设备接入（三期）

**能力**：服务器资产新增 `device_kind=network_device` 形态（与 `os=network` 双向绑定），支持五品牌 `netdev_brand`（`cisco`/`huawei`/`h3c`/`junos`/`generic`）与双通道 `access_proto`（`ssh`/`telnet`，缺省 `ssh`；Telnet 端口建议 23，SSH 与主机共用 `port` 字段缺省 22）。网络设备 CLI 是提示符驱动的交互式行协议，由 `internal/netdev` 承载（契约 5.2B；SSH 通道经 `sshx.Session` 在 PTY 内逐行 expect，连接与 TOFU 主机密钥完全复用 `sshx`；Telnet 通道为包内自研 `internal/netdev/telnetx` 裸 TCP IAC 清洗层——原契约指定的 `ziutek/telnet` 自 2016 年停止维护，三期定稿改为自研）。执行流程固定：打开通道 →（仅 telnet）应答用户名/密码 → 等提示符 → 发品牌分页关闭命令（如 `terminal length 0`）→ 发目标命令 → 等提示符 → 清洗输出（去命令回显、去 `--More--` 分页残留，分页提示自动续读）→ `exit` 关闭；每条命令独立连接。`enable_secret`（Cisco `enable` / Huawei、H3C `super` 的特权密码）独立于登录密码加密存储；当命令本身是 `enable`/`super` 且设备提示 `Password:` 时自动应答一次，其余命令不注入任何输入。命令风险按**厂商 CLI 语义**整体分类（`risk.ClassifyNet`，五分册 `NET_CISCO_`/`NET_HUAWEI_`/`NET_H3C_`/`NET_JUNIPER_`/`NET_GENERIC_`，见 5.3）；`server_facts` 按品牌用固定只读命令模板（`show version` / `display version` + 接口摘要）尽力解析；连接测试走品牌探针命令，`server_version` 取探针输出的软件版本串；`access_proto=ssh` 的设备可开 Web 终端（`termx` 直用 `sshx.Session` 的 PTY，不经 `netdev`）。

**边界**：一次只执行**一条 CLI 命令**——网络设备 CLI 的 `|` 是输出过滤修饰符（`begin`/`include`/`exclude`/`section`），不是进程管道，不做 shell 分段/组合；模式切换命令（`enable`/`super`/`configure terminal`/`system-view`/`configure`）本身可执行（分册定为 `needs_approval`），模式随连接关闭即丢弃、不跨命令保持。网络设备认证**只支持密码**（`password`）——设备侧密钥认证 v1 不支持，其他认证方式创建即 400；每台设备最多 2 条并发连接（全局 32），空闲 60s 回收；单命令 expect 预算缺省 60s（最大 300s），输出 256KB 截断。Telnet 通道无主机密钥语义（`fingerprint` 恒空串、`first_seen` 恒 false）；`access_proto=telnet` 的设备不能开 Web 终端（400 `TERM_UNSUPPORTED`，v1 不做 Telnet 终端桥）；网络设备一律不能开 RDP（400 `RDP_UNSUPPORTED`）。命令需要特权而未配置 `enable_secret` 时，设备侧报错原样返回（exit_code=1），不自动尝试空 enable 密码。

**安全注意**：**Telnet 是明文协议**——登录密码与 enable 密码在网络上明文传输，创建/更新 `access_proto=telnet` 必须显式携带 `confirm_telnet_cleartext=true`（缺失或 false 返回 400，不落库），明文告警贯穿全链路：审计 `detail.confirm_telnet_cleartext=true`、连接测试响应与执行结果携带 `cleartext_warning`、连接测试弹窗展示告警；**设备支持 SSH 就不要用 Telnet**。**网络设备自动执行同样只放行 `safe`**（分册 `SAFE_READ`/`SAFE_PING`/`SAFE_FACTS`）：一切配置变更动词（`interface …`、`vlan …`、`undo …`、`set …` 等）落 `DEFAULT_ASK` 进审批，`reload`/`write erase`/`request system zeroize`/`boot system` 等失联/擦除/启动变更类一律 `forbidden` 不可审批；配置全文导出（`show running-config`/`display current-configuration` 等）定为 `needs_approval`——配置含密钥且会进入 LLM 上下文。网络设备 CLI 无 shell 展开语义：命令中出现反引号、`$(...)`、变量拼接等 shell 元字符一律按注入企图 `forbidden`（失败关闭）。Cisco 支持任意唯一前缀缩写，cisco/generic 分册对命令动词做保留字前缀归一（`sh ver`→`show version`、`relo`→`reload`，多候选不归一落保守方向）。

### 4.11 MongoDB 数据库（三期）

**能力**：数据库资产新增第六种引擎 `mongodb`（`go.mongodb.org/mongo-driver` v1.17.4，go.mod 锁定，不引 v2）。配置：`host`/`port`（创建未提供时填 27017）/目标库 `database_name` 必填；`username`/`password` 成对——要么都空（无认证部署）要么都非空；`auth_db` 为认证库（AuthSource，空表示驱动默认 `admin`）；`ssl_mode` 语义为 TLS（`disable`/`prefer`→无 TLS；`require`→加密跳过证书校验；`verify-ca`/`verify-full`→系统信任链校验）。SQL 控制台与 `db_query` 的输入是**单条 JSON 命令文档**（与 MongoDB database command 同形）：`{"find":"orders","filter":{"status":"paid"},"limit":10}`、`{"aggregate":"orders","pipeline":[{"$match":{}},{"$group":{"_id":"$city","n":{"$sum":1}}}]}`、`{"count":"orders","query":{}}`、`{"distinct":"users","key":"role"}`、`{"estimatedDocumentCount":"orders"}`。执行：`readPreference` 恒 primary（不作为用户配置——secondaryPreferred 无 causal session 时引入 stale read 误导排障）；每请求 ctx 超时（15s 默认/60s 上限）+ 服务端 `SetMaxTime` 双保险；结果列序取第一行 BSON 键序，`ObjectID`/`DateTime`/`Decimal128` 字符串化、二进制 base64、嵌套文档保留为 JSON 对象，`count` 单行单列、`distinct` 展开多行；1000 行 / 1MB 截断。连接测试 `Ping` + `buildInfo`，`server_version` 取 `version` 字段。

**边界**：只读裁决与一、二期引擎一致——守卫（见 5.5 的六层白名单）拒绝的命令返回 422 `SQL_NOT_READONLY`，写路径不存在、不创建审批。顶层命令白名单只有 `find`/`aggregate`/`count`/`distinct`/`estimatedDocumentCount` 五个（`insert`/`update`/`delete`/`drop`/`eval`/`findAndModify`/`mapReduce` 等一律拒绝）；每命令的请求参数（`filter`/`pipeline`/`limit` 等）与聚合管道阶段（`$match`/`$group`/`$lookup` 等 25 个，`$facet`/`$unionWith` 子管道递归校验）各有白名单，未知即拒；全文档递归扫描拒绝任意嵌套深度的 `$where`/`$function`/`$accumulator`/`$out`/`$merge`（JS 执行与服务端写）；用户提供的 `maxTimeMS` ≤ 60000、`limit` ≤ 1000。不提供 `uri` 直连形态——连接参数全部走结构化列（host 仅经 `SetHosts` 传递、不经 `ApplyURI`，host 含 URI 保留字符也不会被驱动解析为查询选项），凭据仍经 cryptox 加密入库，避免引入第二条参数/凭据通道。

**安全注意**：`db_query` 与 SQL 控制台同样走 5.5 的三道闸（守卫 → 执行器复检 → 驱动层限制），查询与拒绝均写审计（`database.query` / `database.query_rejected`）。白名单是本系统侧的只读闸——生产部署建议同时使用 MongoDB 服务端只读账号；`require` 档为跳过证书校验的加密，仅 `verify-ca`/`verify-full` 做证书校验，生产库建议用校验档。

### 4.12 容器管理（四期）

**能力**：容器是**服务器能力**而非新资产——不建容器表、不加资产形态。`device_kind=server`（linux/windows）的服务器经现有执行通道（SSH/WinRM，2.7 路由不变）调 docker/podman CLI 完成探测与管理，由 `internal/containerx` 承载（零 internal 依赖，`ExecFunc` 注入；docker JSONL 与 podman JSON 数组两套解析分册——`Names` 字符串 vs 数组、`CreatedAt` 时间串 vs 人类可读、镜像 `Size` 人类可读串按 1000 进制解析）。**三运行时探测**：`servers.container_runtime`（`''`=未探测 / `none` / `docker` / `podman` / `containerd`）由 `server_facts`、`POST /api/servers/:id/test` 与容器接口/AI 工具在 `''` 时同步探测（15s 超时）写回；探测判据 `docker version --format '{{.Server.Version}}'`（daemon 不可达 exit 1）与 `podman info`（无 daemon、本地 store 可查即成功），containerd（`ctr version`）**只探测展示、不参与管理面**。REST 五路由：`GET /api/servers/:id/containers[/:cid[/logs]]`、`GET /api/servers/:id/images` 与 `POST .../containers/:cid/action`；前端 `/containers` 页（服务器选择含运行时徽标、容器表格、镜像 Tab、日志抽屉 tail 100/200/500/1000 缺省 200），服务器列表行内同样展示运行时徽标。AI 五工具：`container_list`/`container_inspect`/`container_logs`/`image_list` 固定只读模板恒 safe 无审批，`container_action` 的 `start`=safe、`stop`/`restart`=needs_approval（`CONTAINER_CHANGE`），`auto_exec_enabled` 语义与 `ssh_exec` 完全一致；`server_facts` 追加容器探测并写回运行时。

**边界**：管理面仅 docker/podman——`''`/`none`/`containerd` 与 `network_device` 一律 400 `CONTAINER_UNSUPPORTED`；CLI 执行失败（exit≠0 且无 JSON 输出）502 `CONTAINER_CLI_FAILED`，容器不存在 `CONTAINER_NOT_FOUND`，容器 ID 先验 `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$` 不合法直接 400。**REST 的 start/stop/restart 经审批门限分流**（六期起，见 4.17）：需审批时创建 `source=console` 的审批（命令固化为按运行时拼好的逐字节命令如 `docker stop <cid>`），用户经 `POST /api/approvals/:id/decide` 决策，批准时 decide handler 同步逐字节执行并返回 `execution`（exit_code/duration/error_code），响应为 `{approval, execution:null}`；门限判定免审批（`none`/`high` + `auto_exec_enabled=true`，或 `standard` + safe 动作 + `auto_exec_enabled=true`）时同步执行并直接返回 `{approval:null, execution}`，仍落 `container.action` 审计（含 `approval_mode` 快照）。四期「控制台操作恒审批」的裁决由六期门限取代。CLI 超时 60s（探测 15s），输出 256KB 截断。容器数据不落库（实时 CLI 查询），审批走既有 approvals 表（`source` 列 + `conversation_id`/`tool_call_id` 可空，迁移 `00006_phase4.sql`）。

**安全注意**：**容器危险操作分级与 `docker exec` 的命令分类联动**——手工经 `ssh_exec` 执行的 `docker`/`podman`/`nerdctl`（含 `compose` 子命令树）一律走 5.3 的容器分册**完整分类链**：`docker logs` = needs_approval（`CONTAINER_LOGS`——日志可能含凭据进入 LLM 上下文），全局选项 `-H`/`--context`/`--tls*` 指向远程 daemon = `CONTAINER_REMOTE`，`run --privileged`/宿主敏感挂载（`/`、`/etc`、`/proc`、`/sys`、`/dev`、`docker.sock`）= `CONTAINER_ESCAPE` **forbidden**，`exec ... sh -c '...'` 的 shell `-c` 载荷**递归分类内部命令取最高级别**（`docker exec c sh -c 'rm -rf /'` 一样 forbidden）。产品化路径不提供 exec/run 工具（`container_action` 仅 start/stop/restart），杜绝「经容器工具间接执行任意命令」；固定模板工具的恒 safe 与手工命令分级不矛盾（同 `server_facts` 模板与手工命令的关系）。四期修正了旧分册的两处 safe 误放（`docker logs`/`docker compose logs`）与 `-H` 全局选项穿透，Windows 分册补齐 `WIN_CONTAINER_*` 规则（此前完全无容器规则）。

### 4.13 监控告警（五期）

**能力**：`internal/monitor` 对 `device_kind=server`（linux/windows）的资产做**固定模板指标采集**（Linux 一条命令按 `===stat`/`===loadavg`/`===meminfo`/`===df`/`===netdev` 分段解析 /proc，Windows 单条 PowerShell 输出 JSON；六指标 `cpu_pct`/`mem_pct`/`disk_pct`/`load_1`/`net_in_bps`/`net_out_bps`），经现有 SSH/WinRM 通道直连执行（**不经审批**——模板命令整串登记 `SAFE_COLLECT`/`WIN_SAFE_COLLECT` 精确匹配集，机制同 `server_facts`，任何改动走正常分类失败关闭），每轮批量写 `server_metrics`（≤500 行/批）+ UPSERT `server_metrics_latest`（每台最新值物化表），30 天保留每日清理；采集间隔由 `settings["monitor.collector"]`（JSON，缺省 60s，合法 30..3600）控制，`/monitor` 页可改，下一轮生效。**告警规则**：`metric × comparator(gt/lt) × threshold × duration_windows(1..30 个连续点) × cooldown_seconds(60..86400)`，评估器每 30s（含采集轮后追加）对每条 enabled 规则 × 每台有最新指标的资产，从进程内环形缓冲（1440 点/台、1024 台上限）取最近 N 点评估——全部点越限且无 firing → 创建 `firing` 事件；firing 存在且全部点恢复 → `resolved`。**防风暴三件套**：冷却期（`resolved_at + cooldown` 内同规则同服务器不再触发）、静默（`silent_until` 期间事件照常入库但 `notify_status=skipped` 不投递）、同一 firing 只投递一次通知。**通知四渠道**：webhook（HMAC-SHA256 签名头）/飞书（自建应用 webhook+签名）/钉钉（加签 URL）/email（SMTP，收件人 ≤50）——渠道配置经 cryptox 加密落库，投递尽力而为（缓冲 256、worker 2、重试 3 次退避 1s/5s/15s，最终失败 `notify_status=failed` 不阻塞评估）。连续采集失败 5 次暂停该台 10 分钟并产系统告警事件，恢复自动重采。AI 工具：`get_metrics`（时间序列 15..1440 分钟）、`list_alerts`（firing/resolved × severity 过滤）恒 safe 无审批。

**边界**：网络设备不采集（无指标行，监控/巡检页不出现）；单轮采集目标上限 500 台、单台命令超时 30s、输出 256KB 截断；通知渠道绑定在规则上（`notify_channel_id` 为 NULL 则只入库不投递）；`critical` 级告警可选触发自动诊断（`settings["monitor.auto_diagnose"]`，缺省 false，见 4.14）。

**安全注意**：采集模板与 `server_facts` 同一信任模型——固定只读模板、整串精确匹配、写审计聚合行 `metric.collect`；模板常量在 `risk`（`rules_collect.go`）与 `monitor`（`collector.go`）两边各持一份（`risk` 不 import `monitor`），测试断言字节一致。通知内容只含规则名/服务器/指标值/时间，不含凭据；渠道密钥（Secret/SMTP 密码）加密存储、API 只返回掩码。

### 4.14 AI 自诊断（五期）

**能力**：`internal/diagnosis` 是**独立的受控只读诊断会话**（不落用户对话）：装配上下文（告警事件 + 最新指标 + `server_facts` 摘要）→ 模型 mini-loop 最多 6 轮，每轮模型给出一条候选命令 → `risk.Classify`（按目标 os，网络设备走 `ClassifyNet`）→ **仅 `safe` 执行**（needs_approval/forbidden 一律不执行、拒绝原因回给模型继续推理——诊断阶段不存在审批交互，代码强制不写在提示词）→ 模型产出报告 JSON（原因分析/证据列表 ≤20 条每条摘录 4KB/建议修复命令 ≤10 条/整体 64KB，forbidden 建议过滤）。会话状态机 `pending→diagnosing→report→awaiting_fix→done|expired|failed`；全局并发 2、队列 100（满时 429 `DIAGNOSIS_BUSY`）、单会话 300s 超时、`awaiting_fix` 30 分钟过期。入口三态：告警页手动发起（`trigger=manual`）、critical 告警自动触发（`trigger=alert`，需开 `monitor.auto_diagnose`）、对话中「web-01 为什么 CPU 高」（`trigger=chat`，`run_diagnosis` 工具，120s 轮询至终态返回报告摘要）。

**边界**：诊断**只执行 safe 只读命令**——模型在诊断会话里没有任何获得写能力的路径；报告的建议修复命令**没有绕过校验的通道**：`POST /api/diagnosis/sessions/:id/fix` 要求命令逐字节等于报告 `suggestions[].command` 之一（否则 400），然后按审批门限分流（六期起，见 4.17）——需审批时创建 `source=console` 审批，批准后走 console 执行路径同步逐字节执行；门限判定免审批（`none`/`high` + `auto_exec_enabled=true`，或 `standard` + 该命令为 safe）时同步直接执行并落 `command.executed` 审计（含 `approval_mode`）。对话中让模型执行建议则走 `ssh_exec` 完整审批链。所有路径都经既有 `approvals`/统一门限，不存在独立放行逻辑。

**安全注意**：诊断 mini-loop 与主对话工具循环**物理隔离**（不 import `agent` 包、独立提示词、独立轮数/超时预算）；候选命令每轮都过 `risk.Classify`，不是白名单命令集而是「分类结果必须 safe」的运行时断言；报告中的建议命令在展示层标记风险级别，fix 端点二次逐字节校验（防模型建议被篡改后注入）。

### 4.15 AI 定时巡检（五期）

**能力**：`internal/inspection` 对目标资产（`target_tags` 匹配 `servers.tags` 任一，空数组=全部 server；单次上限 200 台）逐台执行**固定模板命令**（`basic`：uptime/df/loadavg/meminfo/`systemctl --failed`/核数探测；`full` 另加 journal 错误行与容器状态），按契约写死的评分规则打分 0..100（初始 100：失败服务每单元 -10 封顶 -30、磁盘 >85% -15 / 75-85% -5、负载 >2×核数 -20 / 核数-2×核数 -10、内存 >90% -10、journal 错误行 -5、活跃告警每条 -10 封顶 -20；采集失败 0 分）并产出异常项数组；报告落 `inspection_reports` + `inspection_report_items`（原始输出截断 64KB/台），报告详情携带 `previous_scores` **趋势对比**。**巡检不用 LLM**——执行是模板+规则评分；「AI 解读」由对话完成（`get_inspection_report` 工具）。**cron 调度**：五段标准表达式（UTC，支持 `* / - ,`），调度器每分钟 tick，同 job 上一轮未结束不重复触发，全局并发 job 上限 2；`POST /api/inspection/jobs/:id/run` 立即巡检（202 异步）。AI 工具：`run_inspection`（1..50 台立即巡检）、`get_inspection_report`（报告详情/最近报告列表）。

**边界**：cron 变更（建/改/删 job）**只经 REST 页面**，不注册建 job 工具——定时任务变更不交给模型；巡检模板命令整串登记 `SAFE_INSPECT`/`WIN_SAFE_INSPECT` 精确匹配集，执行前逐条 `Classify` 断言 safe（失败关闭跳过该条）；单台执行预算 60s；巡检报告完成可投递通知（复用 4.13 渠道，`notify_enabled` + `notify_channel_id`）。

**安全注意**：巡检与采集同一信任模型（固定只读模板、不经审批、写审计）；评分规则与模板命令写死在契约与代码（`risk.rules_inspect` 单一来源，`inspection` 包直接引用常量）；docker 状态命令仅 `container_runtime=docker/podman` 的服务器条件追加，模板登记不变。

### 4.16 批量操作（五期）

**能力**：`internal/batchx` 接收 `(description 自然语言原文, command, server_ids ≤50)`：**逐台** `risk.Classify(command, os)`——`forbidden` → 该台 `skipped`（不建审批）；`safe` 且该服务器开了自动执行 → 直接执行（goroutine 池上限 8）；其余（safe 未开 auto_exec、needs_approval）→ **逐台创建 `source=batch` 审批**（`approvals.source` 五期扩枚举，`command` 逐字节同命令）。对话是主入口：「对所有 web 机器执行 df -h」→ 模型调 `batch_execute` → 立即返回逐台分派概要（状态计数 + 审批提示），**不在 chat 流内等待审批**；用户在 `/batch` 页或审批列表逐台 decide，decide 批准后同步逐字节执行并按 `approval_id` 反查回填该台结果（exit_code/output/duration）；job 状态归并：全部台终态后 `done`（有失败台则 `partial`）。进度走 `GET /api/batch/jobs/:id` 轮询（前端 running 时 2s），**不新增 SSE 事件**。页面入口同构：命令框 + 目标多选 + 实时风险预览 + 创建分派 + 详情抽屉（per-server 行/审批按钮/进度/结果）。

**边界**：单 job 目标上限 50 台、单台执行 60s/输出 256KB 截断；`batch_execute` 的 `server_ids` 同样 ≤50；批量审批逐台独立（不存在「一次批准全部」——每台都是独立审批卡，与单命令审批同 TTL 120s）；过期扫描每 30s 收敛 awaiting_approval 台（审批终态未回填的按审批结果收敛）；取消任务把 pending/awaiting_approval 台置 cancelled，已执行台结果保留。

**安全注意**：批量**不降低任何一台的风险等级**——逐台分类意味着同一命令在 Windows 与 Linux 服务器上分别按各自分册分级；网络设备按厂商分册 `ClassifyNet`。`forbidden` 命令在批量中同样不可审批（skipped 落审计）；审批卡命令与执行命令逐字节一致（复用四期 console 执行路径的保证）；`audit_search` 工具（恒 safe）让「web-01 上周做过什么」在对话中可直接检索审计（不返回 hash 内部字段）。

### 4.17 审批门限四档（六期）

**能力**：审批与否从四处内联判定收敛为**唯一策略函数** `approvals.NeedsApproval({mode, auto_exec_enabled, level, severity})`（`internal/approvals/policy.go`），四档门限：

| 档位 | 语义 |
|---|---|
| `all` | 全部命令审批，含只读 |
| `standard` | 危险命令审批、只读且开「自动执行」的直执行（**全局缺省**，一至五期现行行为） |
| `high` | 仅高危审批：`needs_approval` 中 `warning` 级的命令在开「自动执行」时免审直执行（`high` 级规则仍审批） |
| `none` | 免审批、全部自动执行（仍必须落审计；关闭「自动执行」的服务器仍拦非只读命令） |

**落库与解析**：全局 `settings["approval.mode"]`（JSON 字符串，迁移 `00008_phase6.sql` 幂等插入 `"standard"`，读取时键缺失/非法同样兜底 `standard`）；会话覆盖 `conversations.approval_mode`（可空=NULL=跟随全局），有效门限 = **会话覆盖 > 全局**（`approvals.EffectiveMode`）。`GET/PUT /api/settings` 只暴露公开键（v1 仅 `approval_mode`，写路径审计 `settings.update`）；对话页头部 `PATCH /api/conversations/:id` 携带 `approval_mode`（null 清除覆盖）。变更对**创建之后**的审批生效，已 pending 的审批不迁移、按 `approvals.approval_mode` 创建时快照展示。

**服务器级硬保护**：`auto_exec_enabled=false` 的服务器，非只读命令（`needs_approval`）无论门限一律审批——`all`/`standard`/`high`/`none` 四列都拦截；只读命令（`safe`）是否直执行由门限决定（`all`/`standard` 仍审批，`high`/`none` 直执行）。门限对每个条件单调：`all ≥ standard ≥ high ≥ none`（审批面只缩不涨）。

**统一入口（五条执行路径全部走本函数）**：① 对话 `ssh_exec`；② `container_action` 工具；③ `batchx` 逐台分派（chat 批量取会话门限、console 批量取全局）；④ REST 容器 action（`POST /api/servers/:id/containers/:cid/action` 按门限分流：需审批返 `{approval, execution:null}`，直执行返 `{approval:null, execution}`）；⑤ 诊断修复 `POST /api/diagnosis/sessions/:id/fix`（同样分流；五期「修复没有免审批路径」在 none/high+auto_exec 档放宽为直执行，命令逐字节等于建议之一的校验不变）。诊断引擎内部只读闸与巡检模板闸**不接入**门限（原样）。

**边界**：门限不改变审批状态机、TTL（120s）与等待/超时扫描机制；`approvals.approval_mode` 只是创建时快照，decide 不按它重算；`preview-risk` 端点不携带门限信息（风险级别与门限正交）。四档文案与说明见 `/settings` 与对话页头部下拉，`none` 档以危险色标注。

### 4.18 审批时命令改写（六期）

**能力**：待审批的审批卡可直接**编辑命令**后再批准（`POST /api/approvals/:id/decide` 的可选 `edited_command`，仅 `decision=approve` 合法；`deny` 携带返回 400）。服务端必须**重新分类**：按审批 `server_id` 取服务器（网络设备走 `risk.ClassifyNet`，否则 `risk.Classify`）；改写后 `forbidden` → 403 `COMMAND_FORBIDDEN` 且**审批保持 pending**；否则 `edited_command`/`edited_risk_level`/`edited_rule_id` 与决策状态同一 UPDATE 落库，**原 `command`/`risk_level`/`rule_id`/`reason` 不动**（创建时的历史事实）。

**执行使用改写后命令**：chat 路径 runner 执行 `Decision.EditedCommand`；console/batch 路径 decide handler 同步执行改写值；SSE `approval_resolved` 携带 `edited_command`。改写不改变「已批准」语义。

**约束**：1..2000 字符、禁止 NUL（服务端 `strings.ContainsRune(cmd, 0)` 拒绝）；`server_id` 为空的审批不允许改写（400）；**空草稿不得提交**——编辑态为空/纯空白时「保存并批准」禁用并提示「命令不能为空，如需放弃修改请点「取消编辑」」，放弃修改的唯一出口是「取消编辑」（不调 decide）。**四个审批入口**（对话审批卡、容器操作弹窗、诊断修复抽屉、批量详情待审批行）共用 `ApprovalCommandEditor`，编辑时实时 `preview-risk` 展示新风险级别。

**审计双记录**：`approval.approved` 的 detail 携带 `original_command`/`edited_command`/`original_risk_level`/`edited_risk_level`（未改写不增）；`command.approved` 同样携带；`command.executed` 的 detail 携带**实际执行命令** `command` 与改写时的 `original_command`。工具卡据 `edited_command` 显示「模型原命令」「实际执行 <命令>」+「用户改写」Tag（`arguments` 原值保留为审计事实）。

### 4.19 会话执行回放（六期）

**能力**：`GET /api/conversations/:id/replay` 从**已持久化的三张表重建**（不建回放表）：`messages`（user_message / assistant_text / reasoning）、`tool_calls`（tool_call；execution 步仅 `ssh_exec`/`container_action`，从 result JSON 提取 exit_code/output/duration_ms）、`approvals`（经 conversation_id 或 tool_call_id 关联，`command` 为实际执行命令=edited ?? original）。**顺序稳定写死**：`(timestamp ASC, kind 权重, 来源键)`，权重 `user_message=0 < tool_call=1 < approval=2 < execution=3 < reasoning=4 < assistant_text=5`；排序后重编 `seq=1..N`。

**两个入口**：对话页头部「回放」按钮（流式进行中禁用，等 `done` 后可开）与审计页展开行的「回放」链接（同一播放器，按审计行所属会话打开）。

**播放器**：步骤卡片按 kind 分色；播放/暂停/上一步/下一步 + 进度条（seq 比例）+ 速度 1x/2x/4x（对应 800/400/200ms 步进），自动播放逐步高亮并滚动；`user_message` 剥离 `<!--opsmind-context … -->` 内部上下文前缀；`assistant_text` 用 Markdown 渲染（代码围栏高亮 + 复制代码）；`reasoning` 步复用 `ReasoningBlock`（摘要含服务端落库耗时）；`tool_call` 步状态中文并标注免审批直执行；`approval` 步展示批准/拒绝/过期、来源（对话/控制台/批量）与「用户改写」双行命令；`execution` 步输出默认折叠（展开后 `content-visibility:auto`），命令/输出/退出码可复制。

**截断**：步数上限 20000（丢弃最旧，`truncated=true`）；单状态审批满 200 条时更早审批不纳入回放。响应带 `truncated_reason`（`''`/`steps`/`approvals`），前端按原因二选一提示。

### 4.20 全站复制与深度思考（六期）

**全站复制**：统一 `CopyButton`（`web/src/components/CopyButton.tsx`）是复制清单的唯一实现，禁止各页手写复制。行为链：`navigator.clipboard.writeText` → 失败降级 `document.execCommand('copy')` → 两级都失败时弹**可选中文本面板**（自动聚焦全选，用户 Ctrl+C 兜底）并 toast 报错；成功切 √ 图标 + 「已复制」，1.5s 冷却内不可重复触发；空文本按钮禁用（延迟求值形态点击时提示「没有可复制的内容」）。落位（按契约 8.7 清单）：服务器页（主机、`host:port`、服务器 ID，含编辑弹窗只读态）、数据库页（连接串、数据库 ID，编辑弹窗只读态）、模型页（Base URL、模型名含下拉选项）、审计页（detail JSON 逐值、完整命令）、容器页（镜像 `repo:tag`、容器名/ID、日志抽屉整段日志）、监控页（指标当前值）、告警页（规则 ID、事件 ID）、巡检报告（命令列）、批量详情（每台命令与输出）、终端页（当前输入行，按钮常显）、对话页（消息气泡正文、工具卡命令/输出、代码块、审批卡命令、深度思考全文）、回放播放器（命令/输出）。**实测落位范围**（`rg -n "<CopyButton" web/src` 与 `rg -l` 实测）：应用共 **15 个页面**（登录 + 导航 14 项），其中 **11 个数据页**（chat / servers / databases / providers / audit / term / containers / monitor / alerts / inspections / batch）有复制入口，另有 **3 个跨页共享组件**（`ApprovalCard`、`CodeBlock`、`ReasoningBlock`）——共 **20 个源文件、40 处调用点**；**`/recordings`、`/rdp`、`/settings`、`/login` 四个页面没有复制按钮**（录制页与桌面页是会话/元数据操作，设置页只有单选，登录页无数据可复制）。所以准确口径是「15 个页面中的 11 个数据页 + 3 个跨页组件」，不是 15 页逐页都有。

**深度思考**：模型输出 `reasoning_content`（OpenAI 兼容）或 `thinking`（Anthropic）时，assistant 消息渲染 `ReasoningBlock`：**默认收起**，摘要行「深度思考 · 耗时 Ns」；**流式期间自动展开**（首个 reasoning delta 到达即展开、首个正文 token 到达即回折叠），流式中摘要为「思考中…」；展开态右上角可复制全文；**折叠不丢内容**（`display:none` 收起，DOM 与字符串保留）。耗时来源：流式中取 `useChatStream` 本地测量（首/尾 reasoning delta 时间差），刷新后取服务端落库的 `messages.reasoning_duration_ms`（迁移 `00009_reasoning_duration.sql`，agent 在 reasoning 首/尾 delta 打点，随 assistant 消息同一条 INSERT 写入）；`0` 显示「耗时 0s」，无计时数据（无 reasoning 或旧行）显示「耗时 —」不静默省略。`reasoning` 与正文同为 `messages.reasoning` 持久化字段，回放中作为独立 `reasoning` 步渲染。

---

## 5. 安全模型

### 5.1 密钥与加密

- 主密钥 32 字节；优先使用 `AIOPS_MASTER_KEY`（标准 base64），否则读取 `AIOPS_DATA_DIR/master.key`；文件不存在时自动生成（目录 `0700`、文件 `0600`）。已存在文件的权限必须恰好是 `0600`，不会自动 chmod。
- 算法 **AES-256-GCM**：12 字节随机 nonce，AAD 固定 `aiops:v1`，密文格式 `v1:<base64(nonce)>:<base64(ciphertext+tag)>`。
- 加密字段：SSH 密码、SSH 私钥、私钥口令、数据库密码、模型 API Key。
- API 响应只返回 `has_password` / `has_private_key` / `has_api_key` / `api_key_masked`，永不返回明文或密文；解密只发生在执行前一刻，明文只在内存中，不写日志、不进错误信息、不进审计。
- **主密钥丢失 = 所有已保存凭据不可恢复**；v1 不支持轮换。请把 `master.key` 或 `AIOPS_MASTER_KEY` 纳入备份，并与数据库备份分开保管。

### 5.2 认证与会话

- 单账号 `admin`；密码 bcrypt cost 12。
- 登录按 IP 令牌桶限流：容量 5、每 12 秒恢复 1 个（约 5 次/分钟），超限返回 `429 AUTH_RATE_LIMITED`。
- 会话 token 32 字节随机，数据库只存 SHA-256；绝对有效期 24 小时，不滑动续期。
- Cookie `opsmind_session`：`HttpOnly`、`SameSite=Lax`、`Path=/`，HTTPS 下 `Secure`。
- 密码可随时修改：顶栏头像下拉「修改密码」或 `POST /api/auth/password`（需旧密码，新密码 1..72 字节）。成功即写审计 `auth.password_changed` 并**吊销该用户全部会话**（含当前会话），需用新密码重新登录；旧密码错误返回 `401` 并写审计 `auth.password_change_failed`，不影响现有会话。忘记密码时没有找回通道，只能重置数据库重新引导。
- 所有非 GET 的 `/api/*` 请求必须带 `Origin` 且与允许列表精确匹配，否则 `403 ORIGIN_FORBIDDEN`；SameSite 只是第二道防线。
- SSH 主机密钥 TOFU，指纹不匹配时拒绝执行并写高危审计。

### 5.3 被拦截的命令

风险分级是**失败关闭**的：命中禁止规则直接 `forbidden`，无法证明安全则至少 `needs_approval`。禁止命令不创建审批、不可批准、不执行。

**永久禁止（`forbidden`，示例）**：

- `rm -rf /`、`rm -rf /*`、删除 `/home`、`/root`、`/bin`、`/etc`、`/usr`、`/var`、`/boot`、`/dev`、`/proc`、`/sys` 等关键路径；
- `mkfs*`、`mkswap`、`wipefs`、`fdisk`、`parted`、`sgdisk`、`gdisk`；
- `dd ... of=/dev/sd*`、重定向写块设备；
- `shutdown`、`reboot`、`poweroff`、`halt`、`init 0/6`、`systemctl reboot/poweroff/halt/kexec`；
- `systemctl stop/disable/mask sshd` 等可能导致失联或失去审计的关键服务；
- `curl|sh`、`wget|bash`、`bash <(curl ...)`、`base64 -d|sh` 等远程代码执行；
- `eval`、`sh -c` 接变量、动态生成的代码；
- `$IFS`、反引号、超过 4 层嵌套等规避检测写法；分词失败；
- fork bomb；`history -c`、清空/删除历史文件；删除/截断 `/var/log`、`journalctl --vacuum*`；
- 通过 `psql/mysql/sqlite3` 执行 `DROP`、`TRUNCATE`、无 `WHERE` 的 `DELETE/UPDATE`；
- `kill -9 1`、`killall systemd/sshd`；`iptables -F`、`nft flush ruleset`、`ufw disable`；
- 对 `/`、`/etc`、`/usr`、`/var`、`/boot` 递归 `chmod/chown`。

**需要人工审批（`needs_approval`，示例）**：其他 `rm -r/-f`、`systemctl start/stop/restart/reload/enable/disable`、其他 `kill/pkill/killall`、包管理安装/升级/卸载、`chmod/chown/chgrp`、网络配置变更、`mount/umount`、账号变更、`crontab`、`tee`/`sed -i`/写系统目录、容器与 Kubernetes 变更（容器子命令四期起见下方容器分册）、`ssh host '...'` 远端命令、`env/printenv`、数据库 CLI 写语句，以及**未命中任何规则的未知命令**（默认审批）。

**可直接执行（`safe`）**：`ls/pwd/whoami/id/hostname/uname/uptime/date/df/du/free/ps/ss/netstat/cat/head/tail/grep/rg/wc/stat/file/which` 等只读命令；`systemctl status/is-active/...`、`journalctl`（非 vacuum/rotate）、`docker ps/logs/inspect/images`、`ip addr/route` 等状态查询；所有段均为 safe 的只读管道。

`auto_exec_enabled=true` 只允许 `safe` 直接执行，不能把 `needs_approval` 降级，也不能放行 `forbidden`。**六期起审批门限可进一步收缩或放宽**（见 4.17）：全局缺省 `standard` 即上述行为；`all` 连只读命令也审批；`high` 让 `warning` 级 `needs_approval` 免审；`none` 全部自动执行——但 `auto_exec_enabled=false` 的服务器上非只读命令在任何档位都必须审批（服务器级硬保护），`forbidden` 在任何档位都不可执行、不可审批。

**Windows 分册（二期，`risk.Classify(command, OSWindows)`）**：`os=windows` 目标的命令按 PowerShell 语义规范化（`;`/`|`/`&&`/`||` 分段、`$(...)` 子表达式递归、别名等价表展开 `iex`/`iwr`/`rm` 等）后逐段分类，规则 ID 用 `WIN_` 前缀，与 Linux 分册**互不混用**。同样失败关闭：未知命令 `WIN_DEFAULT_ASK`（needs_approval），解析失败/超深度/无法分词 `WIN_OBFUSCATION`（forbidden）。对照 MITRE ATT&CK 的 forbidden 类（示例）：

- `WIN_RM_PROTECTED`：`Remove-Item -Recurse -Force C:\Windows`、`rd /s /q C:\`、`del /f /s /q` 删系统关键目录；
- `WIN_FORMAT`：`Format-Volume`、`Clear-Disk`、`format`、`diskpart`、`vssadmin delete shadows`、`bcdedit /set`；
- `WIN_POWER`：`Stop-Computer`、`Restart-Computer`、`shutdown`；
- `WIN_CRITICAL_SERVICE_STOP`：停 `sshd`/`WinRM`/`EventLog`/`WinDefend`/`TermService` 等关键服务，`taskkill /f lsass`；
- `WIN_REMOTE_EXEC`（T1059.001/T1105）：`Invoke-Command -ComputerName`、`Enter-PSSession`、`PsExec`、`IEX (New-Object Net.WebClient).DownloadString(...)`、`DownloadString/DownloadFile` 本身；
- `WIN_ENCODED_EXEC`（T1027/T1562）：`powershell -enc`、`-w hidden`、`FromBase64String` + `IEX`；
- `WIN_OBFUSCATION`：`-f` 格式串拼接命令名、反引号拼接、`${}` 变量切片、`Set-Alias` 重命名、AMSI 补丁；
- `WIN_LOLBIN_EXEC`（T1218）：`rundll32`/`regsvr32 /i:http`/`mshta`/`msbuild`/`InstallUtil` 等；
- `WIN_CRED_ACCESS`（T1003）：`mimikatz`、`procdump -ma lsass`、`reg save HKLM\SAM`、`ntdsutil`；
- `WIN_DEFENSE_EVADE`（T1562/T1070）：`Set-MpPreference -DisableRealtimeMonitoring $true`、`wevtutil cl`、`Clear-History`；
- `WIN_PERSISTENCE`（T1543/T1547/T1053）：`schtasks /create`、`New-ItemProperty ...\Run`、WMI 事件订阅。

needs_approval 类（示例）：其余 `Remove-Item -Recurse/-Force`、`Start/Stop/Restart-Service`、`net user /add`、`New-NetFirewallRule`、`netsh advfirewall`、`reg add`、`choco/winget install`、`msiexec`、`Set-Content/Out-File` 写文件、`Get-ChildItem Env:`（可能泄漏凭据）。safe 类：`Get-Service`/`Get-Process`/`dir`/`systeminfo`/`ipconfig`/`tasklist` 等只读命令与全 safe 只读管道。

**网络设备分册（三期，`risk.ClassifyNet(command, brand)`）**：`device_kind=network_device` 目标的命令按**厂商 CLI 语义规范化后整体分类单条命令**（不分段——`|` 是输出过滤修饰符），规则 ID 用五前缀 `NET_CISCO_`/`NET_HUAWEI_`/`NET_H3C_`/`NET_JUNIPER_`/`NET_GENERIC_`，五分册规则语义同构（同一语义只换前缀），generic 是四家命令词的**保守并集**；Linux/Windows 分册的规则在网络设备目标上不生效（反之亦然）。cisco/generic 分册对命令动词做保留字前缀归一（`sh`→`show`、`relo`→`reload`、`conf t`→`configure terminal`），huawei/junos 只展开显式别名（`sy`→`system-view`、`conf`→`configure`）。同样失败关闭：未命中任何规则（含一切配置态动词 `interface`/`vlan`/`undo`/`set` 等）落 `DEFAULT_ASK`（needs_approval），含反引号/`$(...)`/NUL/超长/无法分词 `INVALID`（forbidden）。语义分级：

- safe（`SAFE_READ`/`SAFE_PING`/`SAFE_FACTS`）：厂商只读查看命令白名单（`show version`/`display version`/`show ip interface brief`/`display clock` 等，可带 `| begin/include/exclude/section` 过滤）、`ping`/`traceroute`（huawei/h3c 为 `tracert`）、`server_facts` 固定模板命令；
- needs_approval：`CONFIG_DUMP`（`show running-config`/`display current-configuration`/`show log` 等配置全文与日志——含密钥/认证信息且进入 LLM 上下文）、`ENABLE_MODE`（`enable`/`super`）、`CONF_MODE`（`configure terminal`/`system-view`/`configure`）、`CONFIG_SAVE`（`write memory`/`save`/`commit`/`rollback` 及配置外发 tftp/ftp）、`CONFIG_LOAD`（`copy tftp: running-config`/`restore`/`load override` 外部配置注入）、`CAPTURE`（`monitor capture`/`packet-capture` 抓包）；
- forbidden：`RELOAD`（`reload`/`reboot`/`request system reboot|halt|power-off`——失联）、`FACTORY_RESET`（`write erase`/`erase startup-config`/`reset saved-configuration`/`request system zeroize`）、`BOOT_CHANGE`（`boot system`/`boot-loader`——下次重启可能起不来）、`INVALID`（shell 元字符注入企图）。

`auto_exec_enabled` 同样只放行 safe；forbidden 命中不进审批。

**容器分册（四期，`risk.classifyContainer` 重写，契约 7.3D）**：手工 `ssh_exec` 路径与 `POST /api/ops/preview-risk` 的 `docker`/`podman`/`nerdctl`（含 `docker compose`/`podman compose` 子命令树与独立 `compose` 二进制）按**判定顺序**分级：全局选项检查（`CONTAINER_REMOTE`）→ 别名前缀归一（`container ls|ps`→`ps`、`image ls`→`images`、`container/image <verb>` 剥前缀、`nerdctl` 与 docker 同表）→ exec/run 载荷递归 → 逃逸判定 → 子命令分级表 → `DEFAULT_ASK`。规则 ID 沿用既有 `CONTAINER_*`/`SAFE_SYSTEM_STATUS` 前缀：

- safe（`SAFE_SYSTEM_STATUS`）：`ps`、`images`、`inspect`、`version`、`info`、`stats --no-stream`、`top`、`system df`、`network/volume ls|inspect`、`image history`、`compose ps/config/version/ls`——**`logs` 不在其中**；
- needs_approval：`CONTAINER_LOGS`（`docker/podman [compose] logs`——日志可能含凭据进入 LLM 上下文，与三期 `show log` 归 `CONFIG_DUMP` 同一裁决）；`CONTAINER_REMOTE`（`-H`/`--host`/`--context`/`-c`/`--tls*`/`--remote`/`--url` 出现在子命令前、`context create/use/rm`、`DOCKER_HOST=` 赋值——指向远程 daemon 绕过本机资产与审计面，不得因选项被剥离而按子命令判 safe）；`CONTAINER_CHANGE`（`run`/`rm`/`rmi`/`stop`/`kill`/`start`/`restart`/`pause`/`unpause`/`rename`/`update`/`create`/`build`/`pull`/`push`/`load`/`import`/`save`/`export`/`cp`/`tag`/`commit`/`wait`/`attach`/`login`/`logout`、`network/volume` 变更、`system/image/container prune`、`swarm join/leave`、`compose` 变更子命令——数据注入/外带与状态变更）；
- forbidden：`CONTAINER_ESCAPE`（`run` 含 `compose run` 带 `--privileged`、`--pid=host`、`--network=host`、`--ipc=host`、`--userns=host`，或 `-v/--mount` 挂载 `/`、`/etc`、`/proc`、`/sys`、`/dev`、`/var/run/docker.sock`、`/run/docker.sock`；挂载目标含未解析变量同样 forbidden）——特权或宿主敏感挂载等同宿主 root；
- **递归**：`exec`（含 `compose exec`）载荷含 shell（`sh/bash/zsh/dash/ash/ksh`）`-c` 时递归分类内部命令取最高级别（载荷 safe 则整条 safe），无 `-c` 载荷 `CONTAINER_CHANGE`；`run`/`compose run` 镜像名后出现 shell 及参数同样 `-c` 载荷递归，与逃逸/变更判定取最高。

四期修正记录（对照旧实现）：`logs`/`compose logs` 由 safe 改 needs_approval；只读白名单扩充并加全局选项前置检查（旧实现跳过 `-` 前缀参数，`docker -H tcp://x ps` 误判 safe）；`container`/`image` 前缀别名与 `nerdctl` 归一（旧落 DEFAULT_ASK）；`run` 增加逃逸判定与载荷递归；`network/volume/context/login/save/load/cp` 显式分级。Windows 分册同步补齐 `WIN_CONTAINER_*` 五行（此前完全无容器规则）。产品化路径（REST 容器接口与 `container_*` 工具）不经本表——固定模板 + 动作封闭集（4.12）。

**五期固定模板豁免（`SAFE_COLLECT`/`WIN_SAFE_COLLECT`/`SAFE_INSPECT`/`WIN_SAFE_INSPECT`）**：五期监控采集（4.13）与巡检（4.15）的固定只读模板命令以**整串规范化精确匹配**登记进 risk 匹配集（机制与 `SAFE_FACTS` 完全一致）——命中放行 safe，模板本体任何改动（多一个空格也不行）落回正常分类链（多为 needs_approval/forbidden，失败关闭）。采集模板常量因包边界（`risk` 不 import `monitor`）两边各持一份、测试断言字节一致；巡检模板 `inspection` 包直接引用 `risk` 常量（单一来源），执行前仍逐条 `Classify` 断言 safe，断言失败跳过该条。诊断 mini-loop（4.14）没有模板豁免——模型每轮候选命令走**完整分类链**且仅 `safe` 执行。

### 5.4 审批超时策略

- 默认 TTL **120 秒**，可配置 `AIOPS_APPROVAL_TTL`；创建时写入 `expires_at`。
- 后台每 **1 秒**扫描 `expires_at <= now AND status='pending'`，批量置为 `expired`。
- 只有 `pending` 可以 `approve`/`deny`；重复决策返回冲突，决策使用单条条件更新，避免先读后写。
- 流断开/服务关闭时，等待中的审批置为 `expired`（`stream_cancelled`），命令绝不执行。
- 拒绝、超时、取消都不会执行命令；模型会收到 `APPROVAL_DENIED` / `APPROVAL_TIMEOUT` 等工具错误并如实解释。
- 六期起是否进入审批由门限四档决定（见 4.17），但**审批生命周期本身不变**：状态机 `pending→approved/denied/expired`、TTL 120s、等待者与过期扫描均原样；已 pending 的审批不因门限变更而迁移，按创建时快照展示。审批过期除聚合审计行外，另有逐条 `approval.expired` 明细（单轮最多 200 条，`detail` 含 `command`/`risk_level`/`approval_mode`/`source`/`server_name`），免审批档的直执行同样落 `command.executed` + `approval_mode`。

### 5.5 SQL 只读守卫

三道闸，全部通过才执行：

1. **分类守卫**：PostgreSQL 用 AST（`auxten/postgresql-parser`），MySQL/SQLite 去字符串/注释/反引号后做词法分类，mssql 去 `N'...'`/`[bracket]`/注释后词法分类，redis 走写死的命令白名单（见 4.7），mongodb 走**六层 JSON 命令文档白名单**（见 4.11：严格单一 JSON 对象 → 顶层键恰好一个且属五命令白名单 → 请求参数白名单 → 聚合管道阶段白名单（子管道递归）→ 全文档递归拒 `$where`/`$function`/`$accumulator`/`$out`/`$merge` → `maxTimeMS`/`limit` 上限）；只允许单条 `SELECT` / `WITH…SELECT` / `EXPLAIN`（非 `ANALYZE`），redis 只允许白名单内只读命令，mongodb 只允许白名单内只读命令文档。
2. **执行器复检**：`dbproxy.Query` 内部再次调用守卫，不通过返回 `ErrNotReadOnly`。
3. **驱动层限制**：PostgreSQL `BEGIN TRANSACTION READ ONLY` + `statement_timeout`；MySQL `START TRANSACTION READ ONLY` + `MAX_EXECUTION_TIME` + `multiStatements=false`；SQLite `mode=ro` + `query_only(1)`；mssql 守卫限定单条 SELECT（不开显式事务，避免 `SET` 引入隐式事务）、ctx 超时；redis 白名单保证只读命令、结果映射固定；mongodb 白名单保证只读命令文档 + ctx 超时与 `SetMaxTime` 服务端硬停 + `readPreference=primary`。

结果为 1000 行 / 1MB 上限，超限截断并标记；所有查询带超时。数据库写语句**没有执行路径**，也不能通过审批获得写权限。

### 5.6 部署建议

- **优先部署在内网**，或至少放在 HTTPS 反向代理之后；应用自身不处理 TLS。
- 单管理员模型意味着拿到管理员密码即可配置任意主机、数据库和 LLM 端点。这是产品设计（管理员被信任），因此不要把控制台暴露到公网匿名可访问。
- 系统**不禁止配置内网地址**（否则会破坏运维用途），也不做 SSRF 白名单；安全边界是审批、风险规则、只读守卫和审计，而不是网络目标限制。
- 定期备份：PostgreSQL、`master.key` / `AIOPS_MASTER_KEY`；两者缺一不可。二期另请规划 `AIOPS_RECORDINGS_DIR`（`.cast`）与 `AIOPS_RDP_RECORDINGS_DIR`（`.guac`）的容量与归档——应用不清理录制文件（见已知限制 27）。
- 审计日志只追加，应用不删除；归档请在数据库层完成。

---

## 6. 技术栈、项目结构与维护

### 6.1 技术栈

| 层 | 选型 |
|---|---|
| 后端 | Go 1.27、Gin、`log/slog` JSON |
| 数据库 | PostgreSQL 16、pgx v5 / pgxpool、goose v3（embed SQL，启动自动迁移） |
| 鉴权 | bcrypt cost 12、32 字节会话 token（库存 SHA-256）、HttpOnly + SameSite=Lax Cookie、Origin 校验、按 IP 登录限流 |
| 加密 | AES-256-GCM，主密钥来自环境变量或 `data/master.key` |
| SSH | `golang.org/x/crypto/ssh` + knownhosts，TOFU，每服务器连接池；二期补 PTY（Web 终端）与 Windows OpenSSH 复用；三期网络设备 SSH 通道同样复用 |
| WinRM（二期） | `masterzen/winrm`（NTLM/Basic，PowerShell 端点，独立连接池 `winrmx`） |
| 网络设备 CLI（三期） | `internal/netdev`（五品牌 profile + expect 执行器 + 连接池）+ 包内自研 `internal/netdev/telnetx`（裸 TCP IAC 清洗，不引第三方 Telnet 库） |
| 容器运行时（四期） | `internal/containerx` 自研（docker/podman CLI 命令模板 + JSON 解析分册 + 三运行时探测，零 internal/第三方依赖，经现有 SSH/WinRM 通道执行，不引 Docker SDK） |
| 监控告警（五期） | `internal/monitor` 自研（固定模板采集 + 进程内环形缓冲 1440 点/台 + PG 时序表/最新值物化表 + 阈值评估 + 防风暴 + webhook/飞书/钉钉/SMTP 四渠道投递） |
| AI 自诊断（五期） | `internal/diagnosis` 自研（独立 mini-loop ≤6 轮、仅 safe 只读执行、报告 schema 校验截断、worker 2 + 队列 100、CAS 状态机） |
| AI 定时巡检（五期） | `internal/inspection` 自研（五段 cron 解析 + 每分钟 tick 调度 + 模板命令 + 写死评分规则 + 趋势对比，不用 LLM） |
| 批量操作（五期） | `internal/batchx` 自研（逐台风险分类 + per-server 批量审批（复用 approvals，source=batch）+ 池 8 并行执行 + 轮询进度） |
| 审批门限/命令改写/回放（六期） | `internal/approvals/policy.go` 统一策略函数（四档矩阵 + 有效门限解析，五条执行路径共用）；decide 改写走 `risk.Classify`/`ClassifyNet` 重分类；回放从 `messages`/`tool_calls`/`approvals` 三表重建（稳定排序 + 20000 步上限），不建回放表 |
| 深度思考（六期） | 供应商侧解析 `reasoning_content`（OpenAI 兼容）/ `thinking`（Anthropic）为 `reasoning` SSE 事件；`messages.reasoning` 落库、`reasoning_duration_ms` 服务端打点；前端 `ReasoningBlock` 折叠摘要 |
| 外部数据库 | pgx / `go-sql-driver/mysql` / `modernc.org/sqlite` / `microsoft/go-mssqldb`（二期）/ `redis/go-redis/v9`（二期）/ `go.mongodb.org/mongo-driver` v1.17.4（三期） |
| SQL 守卫 | PostgreSQL AST（`auxten/postgresql-parser`）+ MySQL/SQLite/mssql 词法分类 + redis 命令白名单 + mongodb 六层 JSON 命令文档白名单（三期） |
| LLM | `net/http` 原生流式，OpenAI 兼容 + Anthropic 两套适配器，不引入 SDK |
| WebSocket（二期） | `gorilla/websocket`（终端 opsmind-term-v1 子协议 + RDP guacamole 子协议） |
| RDP 网关（二期） | Go 纯字节透传 ↔ guacd 边车（`guacamole/guacd:1.5.0`，FreeRDP） |
| 前端 | React 19、Vite 8、TypeScript strict、Ant Design 6、react-router-dom 7、TanStack Query 5、TanStack Virtual、Zustand、openapi-fetch |
| 终端/桌面前端（二期） | `@xterm/xterm@5` + `@xterm/addon-fit`（终端与回放渲染）、`guacamole-common-js@1.5.0`（RDP 客户端）；asciicast v2 解析自研 |
| 实时 | SSE（对话，`fetch` + `ReadableStream`，不自动重连）+ WebSocket（终端/RDP） |
| 测试 | Go 原生 fuzzing（risk/sqlguard，语料入 `testdata/fuzz`）、Playwright（真实 Chromium E2E）、Vitest（前端纯逻辑单测）、自研 `cmd/loadgen`（浸泡/压测判定）、`cmd/e2esshd`（E2E 假 sshd，真 PTY 链路） |

### 6.2 项目结构

```text
aiops/
├── cmd/
│   ├── aiops/              # 入口：装配、启动、优雅关闭（二期含 termx/rdpx 装配）
│   ├── smoke/              # 独立 HTTP 冒烟（假 LLM + httptest，S01–S18）
│   ├── seed/               # 向运行中的后端播种演示数据（make seed）
│   ├── loadgen/            # 自包含负载/浸泡工具（selftest/soak/stress，报告落 reports/）
│   └── e2esshd/            # E2E 假 sshd（SSH 18083 + 控制 HTTP 18084，供终端真链路）
├── internal/
│   ├── config/             # 环境变量读取与校验
│   ├── logx/               # JSON 日志、request_id、脱敏
│   ├── apierr/             # 错误码与统一 JSON 错误
│   ├── cryptox/            # 主密钥加载、AES-256-GCM
│   ├── store/              # 领域模型、参数、仓储接口（不含 SQL）
│   │   ├── postgres/       # pgx 实现、事务、goose
│   │   └── migrations/     # 00001_init.sql … 00011_inspection_item_command.sql（二期/三期/四期/五期/六期表结构演进；六期为 00008–00011）
│   ├── auth/               # 管理员引导、bcrypt、会话、限流
│   ├── audit/              # 异步批量写入、哈希链、脱敏
│   ├── risk/               # 命令风险分级与防绕过（rules.go Linux（含容器分册 classifyContainer，四期重写）/ rules_win.go Windows / rules_netdev.go 网络设备五分册 / rules_collect.go 与 rules_inspect.go 五期采集/巡检模板精确匹配集）
│   ├── sqlguard/           # SQL 单语句/只读分类（guard.go / mssql.go / redis.go / mongo.go 六层 JSON 白名单）
│   ├── sshx/               # SSH 连接池、TOFU 主机密钥、执行、PTY（pty.go）
│   ├── winrmx/             # WinRM 连接池与执行（二期）
│   ├── netdev/             # 网络设备 CLI 执行器（五品牌 profile、expect、连接池；telnetx/ 自研 Telnet 通道，三期）
│   ├── containerx/         # 容器运行时探测与 docker/podman CLI 适配、JSON 解析分册（四期，零 internal 依赖）
│   ├── monitor/            # 监控告警（五期 M1）：采集器、环形缓冲、评估引擎、四渠道通知、生命周期 Manager
│   ├── diagnosis/          # AI 自诊断（五期 M2）：受控 mini-loop、报告固化、worker 池与过期扫描
│   ├── inspection/         # AI 定时巡检（五期 M2）：cron 解析、tick 调度、模板执行、评分规则
│   ├── batchx/             # 批量操作编排（五期 M3）：逐台分类、per-server 批量审批、并行执行与回填
│   ├── dbproxy/            # 外部数据库连接池、只读查询（mssql.go / redis.go / mongo.go）
│   ├── termx/              # 终端会话管理、asciinema v2 录制器、治理（二期；三期网络设备 SSH 终端复用）
│   ├── rdpx/               # RDP 网关（guacd 握手、凭据注入、字节透传，二期）
│   ├── approvals/          # 审批状态机、等待、超时扫描；六期：统一门限策略 policy.go（NeedsApproval/EffectiveMode）与命令改写重分类
│   ├── llm/                # 供应商适配器、预设、Registry（reasoning delta：OpenAI reasoning_content / Anthropic thinking）
│   ├── agent/              # 提示词、工具循环、风险路由、上下文裁剪；六期：ssh_exec/container_action 走统一门限、执行改写命令、reasoning_duration_ms 落库
│   └── httpapi/            # 路由、中间件、handler、SSE、终端/RDP WS；六期：/api/settings、会话门限 PATCH、GET /api/conversations/:id/replay
├── api/openapi.yaml        # 唯一 HTTP 接口事实源
├── docs/screenshots/       # 界面截图（README 引用）
├── web/                    # React SPA；src/api/schema.d.ts 由 OpenAPI 生成；e2e/ 为 Playwright 用例
├── Dockerfile
├── docker-compose.yml      # app + db + guacd（guacamole/guacd:1.5.0 RDP 边车，可移除）
├── Makefile
└── CHANGELOG.md
```

约定：持久层 SQL 只出现在 `internal/store/postgres` 与 `internal/store/migrations`；前端不得手写 API 类型，`web/src/api/schema.d.ts` 必须由 `api/openapi.yaml` 生成并提交。

### 6.3 常用命令

在 `aiops/` 目录执行：

| 命令 | 用途 |
|---|---|
| `docker compose up -d --wait db` | 启动开发 PostgreSQL |
| `go mod download` | 下载 Go 依赖 |
| `pnpm -C web install` | 安装前端依赖 |
| `go run ./cmd/aiops` | 启动后端（默认 `:8080`） |
| `pnpm -C web dev` | 启动 Vite 开发服务器（`:5173`） |
| `go vet ./...` | Go 静态检查 |
| `go test ./...` | Go 单测 + 集成测试（需要本地测试库；不可达会失败而不是跳过） |
| `pnpm -C web typecheck` | TypeScript 类型检查 |
| `pnpm -C web lint` | ESLint，零警告 |
| `pnpm -C web build` | 前端生产构建（输出 `web/dist`） |
| `pnpm -C web test` | Vitest 纯逻辑单测 |
| `pnpm -C web test:e2e` | Playwright 浏览器 E2E（`pnpm -C web exec playwright test` 等价）。globalSetup 自动起 docker PostgreSQL（不可达时 `docker compose up -d --wait db`）、构建后端与前端、拉起假 LLM 与 vite preview，结束全量回收 |
| `go run ./cmd/smoke` | HTTP 冒烟（自动创建/删除 `aiops_smoke` 库） |
| `go test ./internal/risk -run '^$' -fuzz 'FuzzClassify$' -fuzztime=30s` | 命令风险分级模糊测试续跑（Windows 分册为 `-fuzz '^FuzzWinClassify$'`、网络设备分册为 `-fuzz '^FuzzNetClassify$'`，见 8.3） |
| `go test ./internal/sqlguard -run '^$' -fuzz '^FuzzCheck$' -fuzztime=30s` | SQL 只读守卫模糊测试续跑（见 8.3） |
| `AIOPS_REAL_GUACD=127.0.0.1:14822 go test ./internal/rdpx/ -run RealGuacd -count=1` | 可选：对运行中的 guacd 容器验证 RDP 网关握手（未设置环境变量时自动跳过） |
| `pnpm -C web exec playwright test e2e/phase6-replay-copy.spec.ts e2e/polish-p6sprint1.spec.ts e2e/polish-p6sprint2.spec.ts e2e/polish-p6sprint3.spec.ts e2e/polish-p6sprint4.spec.ts e2e/polish-p6sprint5.spec.ts "e2e/polish-p6sprint6.spec.ts"` | 六期专项 E2E（回放/门限/改写/复制/深度思考，7 文件；六期终版 43 条、七期后 44 条；2026-11-09 实测 44 passed / 2.3m） |
| `go run ./cmd/loadgen -mode=selftest` | 负载自检（90s）；soak/stress 见 8.5 |
| `make seed` | 向运行中的后端播种演示数据（见 2.5；`CHAT=1` 附带演示消息） |
| `pnpm -C web openapi` | 由 OpenAPI 重新生成 `src/api/schema.d.ts` |
| `make build` | 构建 `bin/aiops` + 前端 |

> `cmd/smoke` 只允许主机 `127.0.0.1`/`localhost`、端口 `65432`，并使用独立库 `aiops_smoke`；每次运行结束删除该库。绝不要把它指向生产库。

### 6.4 维护步骤

所有扩展遵循**契约先行**：涉及 HTTP 时先改 `api/openapi.yaml`（唯一 HTTP 接口事实源），再改后端实现与测试，最后用 `make gen-api` 重新生成前端类型；前端类型与 OpenAPI 不一致会导致 typecheck 失败，这是本项目的接口门禁。

#### 新增模型供应商预设

1. 在 `internal/store/models.go` 的 `ProviderPreset` 增加常量（若协议全新，才增加 `ProviderKind`）。
2. 在 `api/openapi.yaml` 的 `ProviderPresetName` 枚举增加同名值。
3. 在 `internal/llm/presets.go` 的 `Presets()` 增加条目：名称、协议、默认 Base URL、建议模型、是否要求 API Key。
4. OpenAI 兼容端点无需新代码，复用 `NewOpenAI`（Base URL 需能处理 `/chat/completions` 与 `/models`）；协议不同则新增适配器并在 `Registry.Client` 的 `switch kind` 注册。
5. `providers.preset` 的 CHECK 约束通过**新迁移**扩展，不能改已应用的迁移。
6. 前端表单从 `/api/providers/presets` 读取，不硬编码第二份列表。
7. 运行 `pnpm -C web openapi`、Go 单测、`pnpm -C web typecheck`。

#### 新增工具

1. 先评估风险与审批方式；禁止类工具不提供给模型。
2. 在 `internal/agent/tools.go` 的 `ToolDefinitions()` 增加 schema（`additionalProperties=false`，参数有明确上下限）。
3. 在 `internal/agent/runner.go` 的 `executeTool` 增加分支；结果必须是 JSON 对象，并受 `ToolResultBytes` 截断。
4. 涉及 SSH/DB 时复用 `sshx.Executor` / `dbproxy.Executor`，不得新建连接；必须写 `tool_calls` 与审计。
5. 需要人工确认时复用 `approvals.Service`；若确需新 SSE 事件，同步修改 OpenAPI、契约、前端解析与测试。
6. 在系统提示词中写明调用条件，但审批/风险规则必须由代码强制。
7. 补 fake LLM 工具循环、风险分支、截断和审计测试。

#### 新增数据表

1. 新增 `internal/store/migrations/0000N_<name>.sql`，包含 `-- +goose Up` / `-- +goose Down`，不修改旧迁移。
2. 表、索引、外键、CHECK 一并写入；时间列统一 `timestamptz NOT NULL DEFAULT now()`；外键明确 `ON DELETE`。
3. 在 `internal/store/models.go` 加领域结构体（密钥字段 `json:"-"`）、`params.go` 加参数、`interfaces.go` 加窄接口，并挂到 `store.Store`。
4. 在 `internal/store/postgres/` 新增实现；持久层 SQL 只允许出现在这里和 migrations。
5. 涉及 HTTP 时先改 OpenAPI，再改 handler 和 DTO；响应不得包含密文。
6. 集成测试覆盖：迁移 Up 两次、CRUD、唯一约束、外键级联、分页顺序；核心资产补 smoke 路径。

#### 升级迁移

1. **备份数据库和 `master.key`**。
2. 新增连续版本号的迁移文件，禁止编辑生产已应用的旧文件。
3. 在空测试库执行 Up，再执行一次验证幂等；在测试库验证 Down/Up。
4. 运行 `go test ./...` 与 `go run ./cmd/smoke`。
5. 发布时应用启动自动 Up；失败则进程退出。回滚只能发布旧二进制并手工执行对应 Down（需评估数据丢失），应用不自动 Down。
6. 大表索引可使用 `-- +goose NO TRANSACTION` + `CREATE INDEX CONCURRENTLY`；v1 表规模小，默认普通索引即可。

#### 新增/修改 API

1. 改 `api/openapi.yaml` → `pnpm -C web openapi` 生成类型。
2. 需要时在 `internal/apierr/codes.go` 增加错误码并同步契约 9.6。
3. 在 `internal/httpapi` 对应文件加 handler 与路由；写请求必须过 Origin 校验，受保护路由必须挂鉴权，响应不得含密钥。
4. 前端只使用生成类型；新增 query key 同步 `queryKeys.ts` 与失效规则。
5. 更新 smoke/单测，确保 OpenAPI lint 通过。

---

## 7. 已知限制

以下均为当前实现的真实边界，不是待办清单：

1. **单管理员、无多用户/RBAC**：系统只有一个 `admin` 账号，没有用户管理、角色或权限隔离；密码可在登录后自助修改（顶栏头像下拉 → 修改密码，改密即吊销全部会话），但忘记密码没有找回通道，只能重置数据库重新引导。
2. **只支持单实例**：每会话 SSE 占用表、审批等待者、审计哈希链 writer 都在进程内，不能安全水平扩容；不要对 app 做多副本。
3. **数据库只读（含二、三期引擎）**：外部数据库（PG/MySQL/SQLite/mssql/redis/mongodb）只有查询路径，写语句/写命令/写命令文档不会执行，也不会创建审批；需要写库只能通过 SSH/WinRM 调用数据库 CLI（受命令风险规则约束）。redis 是只读命令白名单（`KEYS` 因 O(N) 已提示改 `SCAN` 但服务端不拦），mssql 是词法分类（非 AST，见 4.7），mongodb 是六层 JSON 命令文档白名单（见 4.11）。
4. **审批不覆盖数据库写与终端/RDP（容器页操作四期起、批量执行与诊断修复五期起已入审批；六期起按门限可免审批直执行）**：审批卡片服务 `ssh_exec`（Linux、Windows 双通道与网络设备 SSH/Telnet 双通道）、容器页的 start/stop/restart（console 审批，见 4.12）、批量任务的逐台执行（batch 审批，见 4.16）与诊断建议修复（console 审批，见 4.14）；六期起这些路径是否真的创建审批由门限四档决定（见 4.17），免审批档的直执行仍落审计并带 `approval_mode` 快照；数据库写操作没有“批准后执行”的通道；**终端与 RDP 会话不经审批**（见 26）。
5. **主密钥不可轮换**：丢失 `master.key` / `AIOPS_MASTER_KEY` 后，已保存的 SSH/数据库/供应商凭据无法恢复。
6. **SSH 能力有限**：不分配 PTY、不支持交互式命令、不做端口转发/agent 转发；输出合并截断到 256KB。
7. **无文件上传/下载**：命令输出只以文本/JSON 回传。
8. **SSE 不自动重连**：刷新页面、切换会话或点击停止会中断当前流；未处理审批失效，已持久化的消息仍可拉取。
9. **风险分级是启发式**：基于规范化、分段、包装器剥离和规则匹配，不是沙箱或内核级隔离；无法证明安全的命令一律审批，解析失败一律禁止，因此可能对良性命令过度审批，也不能保证拦截所有精心构造的绕过。
10. **模型协议只有两类**：OpenAI 兼容与 Anthropic 原生；Gemini 走 OpenAI 兼容端点，其他协议需自行新增适配器。
11. **前端测试没有组件渲染单测**：Vitest 只测纯逻辑（SSE 解析、流缓冲、Markdown 分块、错误归一化、live 回合撤除判定、终端会话列表/尺寸、RDP 会话、回放解析、数据库/网络设备表单切换、容器运行时徽标/服务器默认选择/日志视图/错误映射、五期规则/渠道表单/诊断状态/巡检视图/批量视图、六期门限矩阵 `approvalMode.test.ts`/审批编辑 `approvalEdit.test.ts`/双层降级 `copyButtonLogic.test.ts`/思考块 `reasoningBlock.test.ts`/回放播放器 `replayPlayer.test.ts`、七期主题令牌与语义色断言等，当前 82 文件 1370 项）；组件与页面行为由 Playwright E2E（`web/e2e/`，50 个 spec 文件 222 项，覆盖登录、聊天 SSE、审批、管理页、错误提示、终端/回放/RDP、网络设备与 MongoDB 表单/错误链路、容器页与 console 审批流、五期监控/告警/巡检/批量四页与诊断-修复审批链路、六期门限/改写/回放/复制/深度思考（7 个 spec 44 条），及历轮打磨与冲刺的回归场景）在真实浏览器中验证，但没有组件级渲染单测（如 Testing Library）。
12. **MySQL 真实服务未纳入 CI**：CI 只启动 PostgreSQL；MySQL 相关测试不依赖真实 MySQL 服务，SQLite/PG 有实际执行测试；二期 mssql/redis 与三期 mongodb 同理（守卫纯逻辑默认执行，驱动层协议测试用容器 + build tag 标注 `integration_mssql`/`integration_redis`/`integration_mongodb`，CI 不强制）。
13. **应用不终止 TLS**：没有内置 HTTPS，必须由反向代理终止 TLS。
14. **审计不可删除/归档**：应用只追加，不提供归档或清理接口；归档需在数据库层完成。
15. **无内置指标端点**：可观测性依赖 JSON 日志和审计表，没有 `/metrics`。
16. **不限制内网目标**：出于运维用途，允许配置内网服务器/数据库/LLM 端点，不做 SSRF 白名单；安全依赖审批、风险规则、只读守卫和审计。
17. **实现存在契约外的增量路由**：例如 smoke 路由列表中可见 `POST /api/conversations/:id/messages`，当前 OpenAPI/CONTRACT 未列出；以 `api/openapi.yaml` 为准使用公开接口。
18. **真实 Windows 主机、真实网络设备、真实外部数据库实例、真实容器运行时、真实通知渠道与真实大模型仍未接入验证**：全部自动化验证使用替身——Linux 终端经 `cmd/e2esshd` 假 sshd 走通**真 PTY 链路**（含 resize 同步），RDP 经 `e2e/fixtures/fake-guacd.mjs` 走通真 WS 链路、guacd 握手另有可选真容器集成测试（本机 `guacamole/guacd:1.5.0` 实测 PASS，见 8.2），WinRM/mssql/redis/mongodb 有 fake 端点或纯逻辑测试，网络设备经 fake 设备 TCP server（提示符/登录/分页仿真）与假 sshd 验证 netdev 执行链，容器管理经注入 `containerx.ExecFunc` 的假执行桩（命令模板拼接、docker JSONL/podman JSON 数组解析、错误语义）与 E2E 假服务器（探测通道错误路径）+ 按内容 mock 的列表/日志/镜像数据验证；**容器管理未对真实 Docker/Podman 守护进程做过端到端验证**——本机虽装有 Docker daemon，但测试桩不连接它（探测判据 `docker version --format` 的 daemon 不可达行为是四期调研期对本机 CLI 的实测结论，契约已记录，系统链路层面仍是假桩）；真实 daemon 的 `--format json` 输出在不同 Docker/Podman 版本间的差异、Windows 上 Docker Desktop/Podman 的探测模板行为未实测。五期同理：**告警的「采集→评估→firing→通知」链路中，只有「评估→firing」经真实 HTTP/库端到端验证**（E2E 直插越限数据点后轮询 `/api/alert-events` 断言 firing 出现，见 8.4），采集解析是注入假执行桩的 Go 单测（Linux /proc 分段与 Windows JSON 解析），**未经「真实高负载服务器→SSH 采集→越限→firing→通知外发」的完整真机触发**；四渠道通知（webhook/飞书/钉钉/SMTP）是纯逻辑单测（签名/URL 拼装/模板渲染），**未对真实飞书/钉钉机器人或真实 SMTP 服务器外发过**。巡检 cron 调度的解析（NextRun 边界/非法表达式/永不匹配有界终止）与调度约束（同 job 互斥、全局并发 2）为注入假时钟的 Go 单测，**未做多天期真实墙钟到期触发验证**（调度器是进程内每分钟 tick，服务重启后按 next_run_at 重新对齐）。同样未对**真实 Windows 主机（OpenSSH/WinRM 协商、真实 PowerShell 行为）、真实网络设备（Cisco/Huawei/H3C/Juniper 真机的提示符/分页/enable 行为）、真实 SQL Server / Redis / MongoDB 实例、真实 RDP 服务端（3389）与真实供应商 SSE 细节**做过端到端实测，Windows/网络设备/容器风险分册与守卫规则可能存在与真实环境语义的偏差。
19. **首屏 JS gzip 预算（已回预算内并接入 CI 门禁）**：≤ 150KB 的预算（见 8.7）现由 CI 强制（frontend job 的 `bundle size gate` 步骤，即 `pnpm -C web check:bundle-size`，超标即失败）。第三轮打磨把根部 `AntdAppRoot`（ConfigProvider 壳 + `antd/locale/zh_CN`）下沉到懒加载边界（`src/App.tsx`），修复第 4 轮全站中文化引入的超标（曾达 224 KiB）；三期终版实测 **118.1 KiB 达标**；四期终版实测 **120.1 KiB 达标**（容器页为懒加载路由，不进首屏）；五期终版实测 **120.9 KiB 达标**（监控/告警/巡检/批量四页同为懒加载路由）；六期终版实测 **121.4 KiB 达标**（回放播放器与设置页同为懒加载路由，`CopyButton`/`ReasoningBlock` 轻量并入既有 chunk）；七期实测 **121.5 KiB 达标**（主题变量层）。
20. **负载验证有本机规模上限**：loadgen 出于宿主机内存保护限制并发（stress 封顶 40），浸泡已实测 15m（多次，含六期终版浸泡 2026-11-09T04:28）/ 30m / 45m 各档（均 PASS）；45 分钟以上、生产规模、多机压测未做，真实反向代理下的 SSE/WS 长连接表现也未实测，loadgen 亦未覆盖网络设备/MongoDB 查询、容器管理、五期监控采集/巡检/批量链路与六期回放接口。
21. **审计目标 ID 筛选校验不一致**（第二轮第 5 轮走查发现，低影响；**第三轮打磨已修复**）：目标 ID 输入框此前不走 uuidFilter，非法 UUID 静默按 200 空结果处理。现三个 UUID 筛选框（目标/会话/服务器 ID）统一接入轻校验——非法 UUID 提示「必须是合法的 UUID」且不发请求（`web/src/features/audit/AuditPage.tsx`，E2E 回归见 `web/e2e/audit.spec.ts`）。
22. **`.env.example` 缺 `AIOPS_PPROF`**（第二轮第 5 轮走查发现，低影响；**第三轮打磨已补**）：示例文件已列出该变量及注释，与 2.3 环境变量表一致（loadgen 采样依赖）。
23. **成品度冲百循环未达 100 分**：走查评分规则为 high −10 / medium −4 / low −1、无可做项才计 100（见 8.8）；一期 10 轮循环（累计采纳并完成 22 项，每轮门禁全绿）中每轮走查仍存在 1–4 项可做项，单轮最高 **99/100**（第 1/6/9 轮）；第 10 轮 97/100 的 3 项完成、门禁全绿后循环收尾，未再组织新一轮复评，因此没有 100 分记录。二期 6 轮冲刺同理（见 31）。
24. **Windows 交互终端仅限 OpenSSH 场景**：Web 终端依赖 PTY，只有 SSH 通道（`linux` 全部 + `windows` 的 `password`/`private_key`）可开终端；WinRM 认证（`password_ntlm`/`password_basic`）的机器没有 PTY 语义，服务端 400 `TERM_UNSUPPORTED`、前端下拉直接禁用（契约 2.7 定稿路由）。需对 WinRM 机器交互操作时只能用对话内 `ssh_exec` 或 RDP。
25. **RDP 分辨率创建后不可变，且 v1 回放仅元数据 + 下载**：guac rdp 不支持运行中 resize（浏览器窗口变化靠客户端缩放自适应）；`.guac` 录制是 guacd 原生格式，浏览器内播放 v1 不支持，回放页只展示元数据并提供下载（提示离线转码/处理）。终端 `.cast` 回放则完整支持。
26. **终端与 RDP 会话不经命令级风控**：交互会话建立时不重跑 `risk.Classify`（契约 2.7：交互会话的治理靠录制、输入摘要与审计，不是命令级风控）——终端里敲的命令、RDP 桌面里的操作均不受 5.3 规则与审批约束（三期网络设备 SSH 终端同理，不经 `ClassifyNet`）；“自动执行”开关与风险分册只管对话里的 `ssh_exec`。
27. **录制文件含全量会话内容且无自动轮转**：`.cast` 录制终端全部输出（可能含密码回显、敏感文件内容），`.guac` 录制完整屏幕流；输入摘要也会把输入行写审计。文件 0600 但管理员可经回放页下载；应用不提供录制清理/轮转接口（8h 会话的输出可能很大），归档与磁盘容量管理需自行处理。
28. **WinRM Basic 为明文认证且单请求 60s 上限**：`password_basic` 之外通道不做 TLS 时凭据可被窃听，仅建议内网或 HTTPS（`winrm_port=5986`）；WinRM 协议单请求 `MaxTimeoutms=60000`，长任务会被截断。
29. **`.env.example` 缺二期环境变量**：二期新增的 `AIOPS_GUACD_ADDR`、`AIOPS_RECORDINGS_DIR`、`AIOPS_TERM_*`、`AIOPS_RDP_*` 已列在 2.3 表与 `internal/config/config.go`，但 `.env.example` 尚未同步（含注释与默认值），待后续补齐。
30. **二、三、四、五期新代码使部分包覆盖率相对回落**：五期终版总语句覆盖率 69.4%（四期 72.1% → 五期 69.4%，五期并入 monitor/diagnosis/inspection/batchx 四个新包后回落（五期提交合计 +34,150/-2,590 行，含契约与测试））；auth 96.3%、sqlguard 95.2%、approvals 95.0%、risk 89.8%（均与四期持平）；agent 77.0%（四期 85.5%，五期 7 工具并入）、httpapi 72.7%（四期 81.9%，五期 4 组 handler 并入）；五期新包 monitor 66.1%、diagnosis 62.0%、inspection 71.5%、batchx 69.9%；netdev 84.6%、telnetx 88.4%、containerx 84.9%，winrmx 83.4% / termx 80.2% / rdpx 80.2%；dbproxy 38.0% 偏低——mongo/mssql/redis 驱动层测试标注 `integration_*` build tag（CI 不强制），默认套件不执行（见 12）。
31. **二期成品度冲刺 6 轮最高 95/100，未达 100**：二期沿用走查评分规则（同 23），6 轮冲刺（`5b32eb1` → `aa4b357`，每轮门禁复验）最高单轮 **95/100**，未出现 100 分记录；终版复验另发现 fuzz 门禁失败项（见 8.1 注），修复后全部通过。
32. **网络设备接入的能力边界（三期）**：设备认证只支持密码（`password`）——网络设备 SSH 密钥认证 v1 不支持，失败关闭；无自动 enable 流程——`enable`/`super` 由模型按需作为命令发送（经审批），仅当命令本身是 enable/super 且配置了 `enable_secret` 时自动应答密码提示；Telnet 通道不能开 Web 终端（400 `TERM_UNSUPPORTED`，v1 不做 Telnet 终端桥）、网络设备不能开 RDP；`generic` 分册是四家命令词的保守并集，对未识别厂商的设备可能过度审批（安全方向只会更严）；五分册规则按厂商 CLI 文档写死，未经真机验证（见 18）。
33. **三期成品度冲刺 6 轮最高 99/100，未达 100**：三期沿用走查评分规则（同 23），6 轮冲刺（`f95c947` → `61926c1`，每轮门禁复验）最高单轮 **99/100**，未出现 100 分记录（每轮走查仍存在可做项；评分记录见三期工作记录，本节如实按最高分记录）。终版复验全部通过（见 8 节）。
34. **容器管理的能力边界（四期）**：containerd 只探测展示、不参与管理面（一切容器接口 400 `CONTAINER_UNSUPPORTED`）；**不提供 exec/run 的产品化路径**（`container_action` 仅 start/stop/restart，容器内执行任意命令只能经 `ssh_exec` 走完整命令分类链）；`container_runtime` 是探测缓存而非实时状态——写回时机仅 `server_facts`/`POST test`/容器接口 `''` 时同步探测三处，服务器上安装/卸载运行时后需重新探测（`none` 服务器在容器页可手动重探）；探测通道错误（超时/拨号失败）不落库（保持 `''` 待重探），只有探测确认的结论才写回；容器数据不落库（每次实时 CLI 查询），无容器事件流/watch。分册规则按 docker/podman CLI 文档写死，未经真实 daemon 验证（见 18）。
35. **四期成品度冲刺 6 轮最高 99/100，未达 100**：四期沿用走查评分规则（同 23），6 轮冲刺（`f2462d7` → `f67f0ee`，每轮门禁复验）最高单轮 **99/100**，未出现 100 分记录（每轮走查仍存在可做项；评分记录见四期工作记录，本节如实按最高分记录）。终版复验全部通过（见 8 节）。
36. **五期自动化的能力边界**：监控告警——网络设备不采集（仅 `device_kind=server`）；指标只存 30 天、无降采样（`get_metrics` 点数上限 1440）；通知尽力而为（队列满丢弃置 failed，不重投不回滚事件）；告警规则绑定单渠道。诊断——mini-loop ≤6 轮/会话 300s/全局并发 2，模型轮数耗尽或超时产出部分报告（error_code 记录）；建议修复只能整条逐字节采纳，不支持编辑后再批。巡检——cron 是进程内调度（单实例语义，服务重启后按 next_run_at 对齐，不做错过的补跑）；评分规则写死（按契约 7.7），不含业务语义（如证书到期、备份成败）；巡检目标不含网络设备。批量——单 job ≤50 台；逐台审批没有「全部批准」按钮（有意设计：每台独立决策）；进度靠轮询无推送。对话中不能创建/修改巡检 cron job（定时任务变更不交给模型，只经页面），模型只能立即巡检或查报告。
37. **五期成品度冲刺 6 轮最高 96/100，未达 100**：五期沿用走查评分规则（同 23），6 轮冲刺（`14c77ff` → `c12e37f`，每轮门禁复验）最高单轮 **96/100**，未出现 100 分记录（每轮走查仍存在可做项；评分记录见五期工作记录，本节如实按最高分记录）。终版复验全部通过（见 8 节）。
38. **五期新增 `settings` 运行时键与 `.env.example` 无关但未列入 29 的缺口**：`monitor.collector` / `monitor.auto_diagnose` 是数据库 `settings` 表的运行时键（页面可改），不是环境变量——五期没有新增环境变量，`.env.example` 的二期缺口（见 29）依旧未补。
39. **六期交互深化的能力边界**：
    - **门限四档**：`none` 档是真正的「全部自动执行」——`needs_approval`（含 `high` 级）命令在 `auto_exec_enabled=true` 的服务器上**不经任何人工确认**直接执行，只靠审计事后追溯；只有关闭「自动执行」开关的服务器会拦住非只读命令。这是产品选择而非缺陷，但误设代价高（`/settings` 与对话页对该档的 Tag 与选项文案用 `--color-danger` 红字标注）。门限不改变风险分级本身：`forbidden` 在任何档位都不可执行、不可审批。
    - **命令改写**只支持在**批准时**改写、不支持「改写成另一台服务器/另一个工具」；改写后必须重新分类且不能是 `forbidden`；`server_id` 为空的审批（历史数据）不可改写；只做静态风险分级，不做语义等价校验（例如把 `rm -rf /tmp/a` 改成 `rm -rf /tmp/b` 都属 needs_approval，服务端无法判断用户真实意图）。
    - **回放粒度**：数据只从 `messages`/`tool_calls`/`approvals` 三表重建，**不是逐 token 的录像**——reasoning 与正文按整段消息落库，回放按「步骤」呈现而非按时间回放 token 流；工具执行只覆盖 `ssh_exec`/`container_action` 两类（`db_query`、容器只读工具、五期自动化工具没有 execution 步）；`tool_calls.result` 已受 64KB 截断，回放不再二次截断但也无法还原更长的原始输出；步数上限 20000、单状态审批上限 200 条，超出分别置 `truncated_reason=steps`/`approvals` 并丢弃最旧/更早记录；回放不校验查看者归属（v1 单管理员无会话归属列，与 messages 端点同权限）。
    - **复制降级**：`navigator.clipboard` 仅在安全上下文（HTTPS 或 localhost）可用；HTTP 内网部署下走 `document.execCommand('copy')`（浏览器已标记废弃但当前仍可用），两级都不可用时只能弹出可选中文本面板由用户手动 Ctrl+C——这是**降级而非等价**，且 `execCommand` 的可用性随浏览器版本变化。复制的是「当时的纯文本内容」，不含格式；超长内容（如 256KB 工具输出）写入剪贴板可能被浏览器拒绝或截断（未做长度预检）。
    - **深度思考大小**：`reasoning` 与正文同为 `text` 列落库，**没有独立的长度上限或截断**——模型持续输出的长思考会完整入库并完整下发到前端（受单次 LLM 调用 `MaxTokens=4096`、单流 30 分钟与 12 轮工具迭代的间接约束，但没有按 reasoning 单独设限；单条消息体受 `POST /api/chat` 的 64KB 请求体上限约束的是**用户输入**，不是模型输出）。展开态一次性把全文放进 `Typography.Text`（非虚拟滚动），极长思考的渲染开销未经大文本实测；摘要耗时取首/尾 delta 差，**多轮工具迭代中的思考区间被合并成一个总耗时**，不区分每轮。`reasoning` 不回灌给模型（`buildMessages` 只发 content 与工具结果），因此深度思考不占后续轮次的上下文预算。
40. **六期成品度冲刺 6 轮最高 83/100，未达 100**：六期沿用走查评分规则（同 23），6 轮冲刺（`a9cd6f3`/`d42ded0`/`24f867d`/`d3f577b`/`c72547a`/`34e0511`，每轮门禁复验）中**有明确评分的三轮为 68 → 77 → 83**（第 1、2、3 轮，各轮走查采纳全部项后重评），单轮最高 **83/100**；第 4–6 轮的工作记录未给出走查总分，本节不臆造分数，只按项列影响级别与修复要点（见 8.8）。终版复验全部通过（见 8 节）。

---

## 8. 测试与验证

验证体系分六层：**冒烟 → 单元/集成 → 模糊 → 端到端（浏览器）→ 浸泡/压测 → 部署验证**。以下命令均在 `aiops/` 目录执行。**状态（2026-11-09，六期终版 + 七期主题打磨后）**：六期终版门禁全部通过（8.1 表为六期 sprint 6 实测：Go 27 包 ok、前端单测 79 文件 1305 项、E2E 205 passed / 11.6m、覆盖率 69.9%、首屏 121.4 KiB 达标）；七期为纯前端主题改动，门禁复验同样全绿（E2E 222 passed / 11.5m，前端单测 82 文件 1370 项，见 8.1 注）。六期成品度冲刺 6 轮**有明确评分的三轮为 68 → 77 → 83，单轮最高 83/100**（第 4–6 轮无评分记录，见 8.8 与已知限制 40）。

### 8.1 门禁总览（六期打磨 sprint 6 实测，2026-11-08；七期复验见末列）

| 检查 | 命令 | 六期终版结果 | 七期复验（2026-11-09） |
|---|---|---|---|
| Go 静态检查 | `go vet ./...` | 退出码 0 | 退出码 0 |
| Go 格式 | `gofmt -l .` | 无输出 | 无输出 |
| Go 单测/集成 | `TEST_DATABASE_URL='postgres://aiops:aiops@127.0.0.1:65432/aiops?sslmode=disable' go test -count=1 ./...` | 27 个测试包全部 `ok`、0 个 `FAIL`，退出码 0（集成测试需要本地 65432 库，不可达会失败而不是跳过）。sprint 6 新增：`monitor.TestParseLinuxOutputNetRateIsBytesPerSecond`（60s/60MB 增量 → 1,000,000 B/s，不乘 8）、`approvals.TestExpireEmitsPerApprovalAudit` / `TestExpireSkipsConcurrentlyDecidedApproval` / `TestExpireNoCandidatesNoAudit`、`agent.TestRunDeniedCommandAuditCarriesCommandAndRisk`、`httpapi.TestContainerDirectExecutionAuditsApprovalMode` | 27 包 `ok`、0 FAIL（文档同步时另单跑了六期相关四包：`go test -count=1 ./internal/approvals/ ./internal/httpapi/ ./internal/batchx/ ./internal/agent/` → `ok` 3.199s / 10.464s / 1.737s / 0.197s） |
| Go 覆盖率 | `go test -coverpkg=./internal/...,./cmd/... -coverprofile=... ./internal/... ./cmd/...` 后 `go tool cover -func` | 语句覆盖率 **69.9%**（六期打磨 sprint 6 实测；数字为 `go tool cover -func` 末行 total）（sprint 5 69.8%、sprint 4 69.7%、sprint 3 69.6%、sprint 2 69.6%、sprint 1 69.5%、五期 69.4%） | 未重测（七期无后端改动） |
| HTTP 冒烟 | `TEST_DATABASE_URL=...（同上） go run ./cmd/smoke` | 输出 `SMOKE_OK`，S02 健康检查并发 p95 = 23.03ms（冒烟为 S01–S18 固定步骤，见 8.2） | `SMOKE_OK`，S02 p95 16.26ms |
| 前端类型检查 | `pnpm -C web typecheck` | 退出码 0 | 退出码 0 |
| 前端 Lint | `pnpm -C web lint` | 退出码 0（`--max-warnings=0`） | 退出码 0 |
| 前端单测 | `pnpm -C web test` | 79 个文件、**1305 个测试**全部通过（Vitest 纯逻辑单测；sprint 6 新增 6 项：`containerActionPolicy.test.ts` 4 组（8 格矩阵 + 未知门限 + 单调性）、`containerFailures.test.ts` confirm 分流标签 2 项） | 82 文件、**1370 项**通过（文档同步时实跑：`pnpm -C web test` → `Test Files 82 passed / Tests 1370 passed`，3.57s） |
| 前端构建 | `pnpm -C web build` + `pnpm -C web check:bundle-size` | 构建成功；首屏 JS gzip **124,348 B ≈ 121.4 KiB**（六期打磨 sprint 6 实测，**达标**） | **124,389 B ≈ 121.5 KiB 达标**；`check:colors` PASS（142 文件，白名单 3，命中 0） |
| 浏览器 E2E | `pnpm -C web test:e2e` | **205 个测试全部通过**（11.6m，真实 Chromium，49 个 spec 文件；sprint 6 新增 8 条：监控网络速率按字节/秒展示、容器弹窗免审批档「确认执行」直执行文案、standard 档审批文案回归、TTL 到期「已过期」、回放 assistant_text 代码块渲染+复制代码、命令拒绝审计带 command、巡检低分大字红色、审批过期逐条审计可定位） | **222 个测试全部通过**（11.5m，50 spec 文件；`--list` 实测 `Total: 222 tests in 50 files`）。2026-11-09 实跑**六期 7 个 spec 文件 44 条**：`44 passed (2.3m)` |
| 模糊测试 | `go test ./internal/risk -run '^$' -fuzz 'FuzzClassify$' -fuzztime=30s`（sqlguard、Windows 与网络设备分册同款，见 8.3） | 本轮按 10s 短跑复验四个目标（六期打磨 sprint 6，全量 30s 口径见 8.3）：FuzzClassify 2,779,736 / FuzzWinClassify 1,794,121 / FuzzNetClassify 174,476 / FuzzCheck 201,902 次执行，均 PASS 无崩溃 | 四目标各 10s：FuzzClassify 2,827,861 / FuzzWinClassify 1,709,951 / FuzzNetClassify 177,853 / FuzzCheck 205,233，均 PASS |
| 负载自检 | `go run ./cmd/loadgen -mode=selftest` | verdict **PASS**（六期打磨 sprint 6 复验：chats=811 / approvals=815 / 5xx=0 / sse_p95=279ms / RSS 59536→62128KB (+4.4%) / goroutine 49→49；报告已刷新），见 8.5 | verdict **PASS**（chats=804 / approvals=808 / 5xx=0 / sse_p95=282ms / RSS 59940→64772KB (+8.1%)） |
| 真 guacd 集成 | `AIOPS_REAL_GUACD=127.0.0.1:14822 go test ./internal/rdpx/ -run RealGuacd -count=1` | **PASS**（sprint 6 实际运行：`ok aiops/internal/rdpx 2.011s`） | **PASS**（`ok aiops/internal/rdpx 2.009s`） |
| 部署验证 | `docker compose build app` + `docker compose up -d --wait app` + `curl http://127.0.0.1:18081/healthz` | **PASS**（sprint 6 实际运行：镜像构建成功、容器 Healthy，`{"status":"ok","version":"dev",...,"database":"ok"}`） | **PASS**（镜像构建成功、容器 Healthy、`database=ok`） |

> 2026-11-09 文档同步时另实跑：`gofmt -l .` 无输出（退出码 0）、`go vet ./...` 退出码 0。六期终版的 E2E 是 **205 passed / 49 spec**，七期变为 **222 passed / 50 spec**——增量 17 = 新增 `polish-p7.spec.ts` 16 条 + `polish-p6sprint6.spec.ts` 的巡检色阶用例改为浅/深双主题各跑一次（+1）。**六期本身没有 222 这个数字**，引用时不要混用。

CI（`.github/workflows/ci.yml`）在推送/PR 时运行等价命令（backend：gofmt/vet/test/smoke + PostgreSQL 服务容器；frontend：install → 生成类型并校验已提交 → typecheck/lint/test/build → 首屏体积门禁）。

### 8.2 HTTP 冒烟（cmd/smoke）

`go run ./cmd/smoke` 是独立 HTTP 冒烟：自建/自删 `aiops_smoke` 库，启动真实 HTTP 服务 + 假 LLM，覆盖 **S01–S18** 固定步骤（登录限流、资产 CRUD、连接测试、SQL 守卫、审批流、审计哈希链、SSE 工具循环、登出等），S02 输出健康检查并发 p95。它只允许主机 `127.0.0.1`/`localhost`、端口 `65432`，**绝不要指向生产库**。

> **更正（四期文档同步时核实）**：三期文档曾把冒烟范围写作「S01–S20（含 S19 终端/RDP 错误路径与 S20 三期路径）」——经核对 `cmd/smoke/main.go`（git 历史自一期 `eb1be27` 后未再改动），冒烟步骤实际为 S01–S18，二/三/四期的终端、RDP、网络设备、MongoDB 与容器路径**均未加入 smoke**，这些能力的验证在 Go 单测/集成测试与 Playwright E2E 层完成（见 8.1/8.4 与各功能小节）。本节按代码事实修正。

### 8.3 模糊测试（Go 原生 fuzzing）与续跑

四个模糊目标，各自固化了核心不变式（任何输入不 panic、字段合法；含命令替换/危险命令/危险载荷绝不判 `safe`；多语句/写语句/危险函数绝不放行）：

| 目标 | 位置 | 五期终版 30s 实测（2026-10-27） |
|---|---|---|
| `FuzzClassify`（Linux 命令风险分册） | `internal/risk/fuzz_test.go` | 6,754,392 次执行，无失败（四期种子补容器命令样本；本轮与 E2E 并行跑，执行数偏低） |
| `FuzzWinClassify`（Windows PowerShell/cmd 分册，二期新增） | `internal/risk/fuzz_test.go` | 2,789,942 次执行，无失败（种子覆盖 7.3B 危险命令与绕过手法：`-EncodedCommand`、Base64、别名、嵌套 `powershell -c`、`cmd /c` 包装、反引号拼接、`${}` 变量切片；四期补 `WIN_CONTAINER_*` 容器样本） |
| `FuzzNetClassify`（网络设备五分册，三期新增） | `internal/risk/fuzz_test.go` | 225,287 次执行，无失败（种子覆盖五品牌危险命令、缩写归一与 shell 元字符注入形态；已有 1 个历史崩溃语料入库 `testdata/fuzz/FuzzNetClassify/`） |
| `FuzzCheck`（SQL 只读守卫，一份输入同时派生 PG/MySQL/SQLite） | `internal/sqlguard/fuzz_test.go` | 475,358 次执行，无失败 |

> **终版复验踩坑（已修复）**：Windows 分册的 fuzz 目标最初命名 `FuzzClassifyWindows`——门禁脚本用无锚点的 `-fuzz FuzzClassify` 跑 Linux 分册时，该前缀模式会匹配到两个目标，Go 直接拒绝执行（exit 1）。已改名为 `FuzzWinClassify`，门禁模式改为锚定 `FuzzClassify$`；命名注意已写入 `fuzz_test.go` 注释防止回归。

**续跑方式**：Go fuzzing 天然支持增量续跑，无需额外参数——每次 `-fuzz` 运行会自动回放三层语料并继续变异：

1. 测试文件内 `f.Add(...)` 固化种子（含历轮 fuzz 引爆样本：挂起、OOM、漏判、误报形态）；
2. 仓库已提交的回归语料 `internal/risk/testdata/fuzz/FuzzClassify/` 与 `internal/sqlguard/testdata/fuzz/FuzzCheck/`（各 66 个文件）；
3. `$(go env GOCACHE)/fuzz` 里本机累积的新语料（跨轮增量，只在本机生效）。

```bash
# 快验（约 30 秒）
go test ./internal/risk -run '^$' -fuzz '^FuzzClassify$' -fuzztime=30s
go test ./internal/sqlguard -run '^$' -fuzz '^FuzzCheck$' -fuzztime=30s

# 长跑示例（10 分钟）
go test ./internal/risk -run '^$' -fuzz '^FuzzClassify$' -fuzztime=10m
```

若发现崩溃，最小化输入会写入该目标的 `testdata/fuzz/` 目录并使测试失败；修复后**把崩溃语料提交入库**作为回归种子（仓库现有 134 个语料文件：risk/FuzzClassify 67 + risk/FuzzNetClassify 1 + sqlguard/FuzzCheck 66；Windows 分册目标尚无入库崩溃语料）。注意：普通 `go test ./...` 也会回放 `testdata/fuzz` 语料（不做变异），所以回归种子随每次全量测试生效。

### 8.4 端到端（Playwright）与本地复现

```bash
pnpm -C web install        # 首次
pnpm -C web test:e2e       # 全量（七期打磨实测 222 passed / 11.5m；六期终版为 205 passed / 11.6m）

# 只跑一个文件（调试时）
pnpm -C web exec playwright test e2e/chat.spec.ts
```

globalSetup（`web/e2e/global-setup.ts`）**全自动编排、结束全量回收**，无需手工准备环境：

1. 探测 127.0.0.1:65432 PostgreSQL，不可达时自动 `docker compose up -d --wait db`；
2. 重建全新 `aiops_e2e` 库（后端启动时 goose 自动迁移）；
3. 并行构建后端二进制（`/tmp/aiops-e2e`）、假 sshd（`cmd/e2esshd`）与前端产物（`web/dist`）；
4. 启动假 OpenAI 兼容 SSE 服务（`e2e/fixtures/fake-llm.mjs`，端口 **18081**）与假 guacd（`e2e/fixtures/fake-guacd.mjs`，端口 **18082**，RDP 真链路）；假 sshd 以 SSH **18083** + 控制 HTTP **18084** 运行（终端真链路）；
5. 以独立临时 `AIOPS_DATA_DIR`、`AIOPS_GUACD_ADDR`（指向假 guacd）启动后端（127.0.0.1:18080）；
6. vite preview 提供页面（127.0.0.1:4173，代理 `/api` 到后端，`ws: true` 放行终端/RDP WS 升级）；
7. 通过 API 播种指向假 LLM 的供应商与假服务器，并把登录态写入 `e2e/.auth/user.json` 供 storageState 复用（避开登录限流）。

前提：本机可用 Docker（仅 PostgreSQL）、Go 与 pnpm 工具链。用例（50 个 spec 文件）覆盖登录/限流/改密、聊天 SSE 流式与工具卡、审批倒计时与批准/拒绝（含双窗口审批同步）、服务器/数据库/模型管理页、404 与错误提示链路，历批产品打磨与一期冲百循环 10 轮冲刺的回归场景（`web/e2e/polish*.spec.ts`），二期专项：终端真链路（假 sshd 真 PTY：建立、PTY 尺寸同步、断开与录制封盘）、RDP 真链路（假 guacd：创建/接入/断开、guacamole 子协议）、回放页（列表/回放/下载、RDP 元数据提示）、WinRM/mssql/redis 表单与错误分类（`term-recordings.spec.ts`、`polish-p2sprint1/3/4/5.spec.ts`），三期专项（`polish-p3round6.spec.ts`，8 用例）：服务器表单 `device_kind`/品牌/协议联动与 Telnet 明文确认、网络设备连接测试错误反馈（中文分类、剥英文前缀）、SSH 网络设备创建与终端页可选性（Telnet 设备不可选并展示常驻说明、RDP 下拉隐藏网络设备）、MongoDB 表单联动与本地成对校验、JSON 查询抽屉（本地校验/服务端拒绝/连接失败口径）、审计页三期事件筛选、对话上下文下拉设备标识、编辑 Telnet 设备回填，四期专项（`containers.spec.ts` 3 用例 + `polish-p4sprint2–5.spec.ts` 共 9 用例）：容器页真链路空态（探测通道错误按 none 返回 400 且不写回、横幅「本次探测失败」、下拉可重探）、日志抽屉骨架（tail 选择/刷新/复制/按行渲染/截断提示，mock 数据）、容器操作 console 审批弹窗（确认→审批卡→decide 执行结果回填）、运行时徽标与写回失效、无名称容器短 ID 回退、exited 行禁用停止、审计页 `container.action` 筛选、SSH 不可达不误报 daemon 未运行（`SSH_EXEC_FAILED` 语义），五期专项（`phase5-monitor-batch.spec.ts` 2 用例 + `polish-p5sprint1–3.spec.ts` 10 用例 + `walkthrough-p5round4–6.spec.ts` 32 用例）：监控页真链路（概要四卡/采集配置/空态）、**告警评估引擎装配验证**（创建 `cpu_pct>85` 规则后 psql 直插越限点 + UPSERT latest，轮询 `/api/alert-events` 断言 75s 内 firing 自动出现——验证「数据落库→评估循环→事件」链路）、诊断会话真链路（假 LLM 按协议轮次回 probe/report，会话真实到达 `awaiting_fix`，深链抽屉出现报告与修复审批、首点批准与幽灵行）、批量页真链路（safe 预览→创建分派→详情抽屉 per-server 待审批行→取消任务）、巡检任务表单/报告抽屉/趋势对比、对话四场景工具链（fake-llm 回放 `get_metrics`/`run_diagnosis`/`run_inspection`/`batch_execute` 工具调用），**六期专项（7 个 spec 文件）**：`phase6-replay-copy.spec.ts`（8）、`polish-p6sprint1.spec.ts`（2）、`polish-p6sprint2.spec.ts`（6）、`polish-p6sprint3.spec.ts`（5）、`polish-p6sprint4.spec.ts`（7，含 2 档视口参数化）、`polish-p6sprint5.spec.ts`（7）与 sprint 6 的 `polish-p6sprint6.spec.ts`（**六期终版 8 条**，七期把巡检色阶用例改为浅/深双跑后 9）——**六期终版合计 43 条**（与五期终版 162 → 六期 205 的增量吻合：162 + 43 = 205），七期后为 44 条；2026-11-09 实跑这 7 个文件 `44 passed / 2.3m`（`--list` 实测 `Total: 44 tests in 7 files`）。覆盖：回放播放器 kind 分色、单步/自动播放/速度/进度条与 truncated 提示、审计页「回放」链接打开同一播放器、门限四档切换持久化与 none 档警示、审批改写后按改写命令批准并落库、服务器/数据库/模型页与审计展开行的 CopyButton 写入剪贴板、复制清单补位（编辑弹窗只读态、模型名下拉、终端当前行常显）、深度思考流式全链路（fake-llm `__scenario_reasoning__`）、nav/页头/标签页三方一致、空命令审批拦截、回放载荷补齐与失败步、审计动作补全与命令摘要、批量改写与门限快照、折叠内复制按钮不连带开合、审计 `command.approved` 落地、回放截断文案二选一、容器弹窗门限分流文案、回放 Markdown 渲染、拒绝/过期/直执行审计可定位、巡检大数字色阶。失败时 trace（`retain-on-failure`）与截图（`only-on-failure`）保留在 `web/e2e/.test-results/`。

除 222 项自动化 E2E 外，每期冲刺轮次另有走查评分驱动的修复与复验（见 8.8；六期 7 文件专项用例 2026-11-09 实跑 44 passed）；一期第二轮第 5 轮（2026-10-25）的 9 项人工自由走查记录见 CHANGELOG。

> 端口约定：E2E 的 18081 是**假 LLM**；compose 部署验证的 18081 是 **app 健康检查端口**（见 3.1 与 8.6）；E2E 的 18082/18083/18084 分别是假 guacd、假 sshd SSH、假 sshd 控制 HTTP。

### 8.5 负载与浸泡（cmd/loadgen）

`loadgen` 是自包含的负载/浸泡工具：自动编排本机 PostgreSQL（不可达时 `docker compose up -d --wait db`）、每次运行重建的 `aiops_load` 库、`go build` 出的服务端（`AIOPS_PPROF=1`、`GOMEMLIMIT=768MiB`）与内嵌假 LLM；随后以 N 个并发会话循环执行 SSE 聊天（含审批自动批准）、资产 CRUD churn 与审计分页，每 5s 采样服务端 RSS / goroutine / 各请求延迟分位数，按固定阈值判定并产出 `reports/<mode>.md` 与 `.json`。

```bash
go run ./cmd/loadgen -mode=selftest                       # 90s / 并发 4，快速自检
go run ./cmd/loadgen -mode=soak -duration=15m             # 浸泡（默认 30m / 并发 8）
go run ./cmd/loadgen -mode=soak -duration=45m             # 更长浸泡，直接覆盖时长即可
go run ./cmd/loadgen -mode=stress -duration=5m -concurrency=32   # 压测（默认 10m / 并发 32）
```

| 模式 | 默认时长/并发 | 并发上限（本机内存保护） |
|---|---|---|
| `selftest` | 90s / 4 | 12 |
| `soak` | 30m / 8 | 12 |
| `stress` | 10m / 32 | 40 |

**判定阈值**（任一触发即非零退出，报告标 FAIL）：

| 检查项 | 阈值 |
|---|---|
| HTTP 5xx 响应 | = 0 |
| SSE 流内 `error` 事件（200 头已写出，视同服务器错误） | = 0 |
| SSE 聊天 p95 | ≤ 1500ms |
| warmup（前 1/4 时长）后 goroutine 峰值增幅 | ≤ 25% |
| warmup 后 RSS 峰值增幅 | ≤ 30% |
| 服务端 RSS 绝对峰值 | ≤ 1.5GB |
| 运行中宿主机 MemAvailable | ≥ 2GiB（跌破立即停止负载并判失败） |

**如何产出报告**：`loadgen` 会把结果写到 `reports/<mode>.md` 与 `.json`（该目录不随仓库提供，运行时生成）。下表是开发期实测结果的节选，供判断性能量级参考：

| 报告 | 命令 | 关键结果 |
|---|---|---|
| selftest | `loadgen -mode=selftest -duration=1m30s -concurrency=4`（2026-11-09T03:23） | 804 聊天轮、808 审批自动批准、32,160 token 事件，SSE p95 282ms；RSS 峰 63MB（增幅 8.1%）、goroutine 增幅 0.0%；0 错误 |
| stress | `loadgen -mode=stress -duration=5m0s -concurrency=32`（一期） | 37,450 聊天轮、2,247,000 token 事件，SSE p95 188ms；RSS 峰 64MB（增幅 5.0%）、goroutine 增幅 3.7%；0 错误 |
| soak | `loadgen -mode=soak -duration=15m0s -concurrency=8`（2026-11-09T04:28） | 10,000 完成聊天轮（chat_sse 请求 10,008）、10,008 审批自动批准、600,000 token 事件，SSE p95 401ms；RSS 峰 73MB（增幅 0.7%）、goroutine 增幅 0.0%；0 错误 |

> 说明：上表数字为开发期实测记录，不同批次存在正常波动；复跑 `loadgen` 可在本地重新生成。

**45 分钟档浸泡已实测通过**（一期，2026-10-25T01:35，PASS ✅）：`loadgen -mode=soak -duration=45m0s -concurrency=8`——29,918 聊天轮、29,920 审批自动批准、1,795,080 token 事件，SSE p95 403ms；RSS 峰 60MB（增幅 1.6%）、goroutine 增幅 0.0%、0 错误。loadgen 目前只驱动对话/资产/审计链路，未覆盖终端/RDP 会话、网络设备/MongoDB/容器查询、五期监控采集/巡检/批量链路负载，也未覆盖六期回放接口与门限四档（对话链路固定 standard 档）。

### 8.6 部署验证（镜像构建 + 18081 健康探针）

五期终版（2026-10-27）实际执行：

```bash
docker compose build app          # 多阶段构建 app 镜像（五期实测）
docker compose up -d --wait app   # app + guacd 边车 + db 三容器
curl http://127.0.0.1:18081/healthz
# → 200 {"status":"ok","version":"dev","time":"...","database":"ok"}
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18081/
# → 200（SPA index.html）
```

实测结果：镜像 `aiops-app:latest` 构建成功（distroless nonroot，**88.7MB**，较四期 67.8MB 增大约 21MB，来自五期 monitor/diagnosis/inspection/batchx 四包与前端四个新页面）；`aiops-app-1`（连带 guacd 边车）正常启动（`--wait` Healthy），健康探针与首页均 200，数据库连接 ok。验证完成后 `docker compose stop app` 回收（db 与 guacd 可保留供开发）。端口与 Origin 说明见 3.1；单独构建镜像用 `make docker-build`。

### 8.7 性能预算：首屏 JS gzip

- **预算**：首屏 JS（`web/dist/index.html` 直接以 `<script type="module">` 或 `modulepreload` 引用的 chunk，gzip 后字节总和）**≤ 150KB**；由 CI 门禁强制（见下）。
- **当前实测**（2026-11-09 七期打磨，`pnpm -C web build` 后）：**124,389 B ≈ 121.5 KiB——达标**（六期终版 124,348 B ≈ 121.4 KiB，sprint 6 实测）。构成（gzip，五期终版三 chunk 数据，六/七期仍保持三 chunk 结构）：

| chunk | gzip 字节 |
|---|---|
| `react-vendor` | 108,077 |
| `index` | 15,304 |
| `rolldown-runtime` | 431 |

- **历史**：一期第二轮第 4 轮（`0d6b56a`）为全站中文化在根部 `AntdAppRoot` 引入 `antd/locale/zh_CN`，首屏一度 229,347 B ≈ 224 KiB 超标；第三轮打磨把 `AntdAppRoot` 下沉到懒加载边界（`src/App.tsx`）后回到预算内。二期新增终端/回放/RDP 页面全部走路由懒加载（xterm、guacamole-common-js 等重依赖不进首屏），首屏构成保持三 chunk 不变；三期网络设备/Mongo 表单并入既有页面，首屏构成仍为三 chunk；四期容器页同为懒加载路由（`index` chunk 由 12,454 B 增至 14,492 B），构成不变；五期监控/告警/巡检/批量四页同为懒加载路由（`index` chunk 增至 15,304 B），构成不变；**六期回放播放器（`ReplayViewer`）与设置页同为懒加载路由，`CopyButton`/`ReasoningBlock` 是轻量组件并入既有 chunk——首屏仍三 chunk，总量 124,348 B（较五期 +536 B）**；七期主题系统（`tokens.css` + `theme.ts` 语义色变量）使首屏再增 41 B 至 124,389 B。
- **CI 门禁**（已知限制 19 的修复）：`.github/workflows/ci.yml` frontend job 在 build 后运行 `pnpm -C web check:bundle-size`（`web/scripts/check-bundle-size.mjs`，与下述测量命令同口径，七期实测 124,389 B，PASS），超标即失败，防止懒加载边界与分包策略被无意劣化。
- 测量命令（复现）：

```bash
pnpm -C web build
cd web/dist && for f in $(grep -oE 'assets/[A-Za-z0-9._-]+\.js' index.html | sort -u); \
  do gzip -c "$f" | wc -c; done | paste -sd+ | bc
```

分包策略见 `web/vite.config.ts`：rolldown `codeSplitting.groups` 互斥分组（`react-vendor` / `icons-vendor` / `markdown-vendor`），antd 与 rc-* 重组件不整包捕获、由路由懒加载自然分块。markdown 渲染链等重尾依赖仍在懒加载路由块中，不进首屏。

### 8.8 成品度冲百循环（一期 10 轮 + 二期 6 轮 + 三期 6 轮 + 四期 6 轮 + 五期 6 轮 + 六期 6 轮走查评分）

测试覆盖强化之后，以「走查评分」驱动产品打磨闭环：每轮对产品做全量走查并按 **high −10 / medium −4 / low −1** 量化扣分，**无可做项才计 100 分**；采纳的项完成实现后整组门禁复验，全绿才收轮。共 10 轮（冲刺 1–10，对应提交 `d7089b5` … `bdd8804`），累计采纳并完成 **22 项**，每轮门禁全绿：

| 轮 | 评分 | 采纳/完成 | 提交 | 本轮要点 |
|---|---|---|---|---|
| 1 | 99/100 | 1 项 | `d7089b5` | 工具卡耗时剔除审批等待段，denied/expired 不再显示 45s 级误读耗时 |
| 2 | 98/100 | 2 项 | `b58576b` | 改密失败提示场景化「旧密码不正确」；E2E 实证审计动作筛选可关键词过滤 |
| 3 | 93/100 | 4 项 | `746ba41`（+E2E flaky 修复 `c0daa8d`、`c4d9595`） | 快流工具卡竞态修复、审计目标 ID 接入 UUID 校验、首屏 gzip 224→117.5 KiB 并加 CI 体积门禁、`.env.example` 补 `AIOPS_PPROF`（详见 CHANGELOG「sprint 打磨第 3 轮」条目） |
| 4 | 93/100 | 4 项 | `189886a` | 停止回答即时作废未决审批卡、数据库重名 409 具体化、404 页换自定义插画、审计 UUID 筛选归一化小写 |
| 5 | 97/100 | 3 项 | `3fa0b3d` | 登录错密提示改「管理员密码错误」、侧栏搜索无匹配联动加载更早会话、审计 UUID 提示中英文补空格 |
| 6 | 99/100 | 1 项 | `1c16a66` | 会话侧栏新建/更多纯图标按钮补 aria-label 可访问名称 |
| 7 | 90/100 | 1 项 | `920f1d7` | 登出/改密后登录页不再把会话结束误报成 INTERNAL 服务器故障横幅 |
| 8 | 86/100 | 2 项 | `44c11d9` | SSH 22 端口拨号地址不再被 knownhosts.Normalize 剥掉端口；工具失败底层原因落库 error_message 供历史会话展示 |
| 9 | 99/100 | 1 项 | `6521a81` | SQL 查询抽屉空语句不再静默无响应——执行按钮禁用，Ctrl+Enter 给出「请输入要执行的 SQL」提示 |
| 10 | 97/100 | 3 项 | `bdd8804` | 审计动作补改密标签、服务器搜索含端口、双窗口审批后补偿刷新 |

评分曲线：`99 → 98 → 93 → 93 → 97 → 99 → 90 → 86 → 99 → 97`。**最高 99/100（第 1/6/9 轮），未达 100**：按评分规则，100 分要求走查无可做项，而 10 轮走查每轮都存在 1–4 项可做项（扣 1–14 分不等，低分轮多为集中发现的中低影响体验项，完成即回升）；第 10 轮采纳的 3 项完成、门禁全绿后循环收尾，未再开启新一轮复评，因此没有 100 分记录（见已知限制 23）。

**二期冲刺（p2 sprint 1–6，2026-10-26）**：二期功能合入后沿用同一走查评分规则继续打磨，共 6 轮（提交 `5b32eb1` → `aa4b357`，每轮门禁复验全绿后收轮），**单轮最高 95/100，未达 100**（每轮走查仍存在可做项；评分记录见二期工作记录，本节如实按最高分记录，见已知限制 31）。各轮要点：

| 轮 | 提交 | 本轮要点 |
|---|---|---|
| 1 | `5b32eb1` | 装配 Term/RDP/WinRM 执行器与 mssql/redis 建库分支；终端五处交互修复 |
| 2 | `7f6568c` | RDP WS 补 guacamole 子协议与隧道失败提示；回放静默段进度插值；列表 meta 改「由 admin 发起」 |
| 3 | `052ca69` | RDP WS 改文本帧（guacamole-common-js 对二进制帧抛 TypeError）并补真链路 e2e；连接测试错误中文分类折叠 winrm 冗余包装；redis 命令失败标题对齐命令口径 |
| 4 | `9caa98f` | RDP 断开后秒点新建会话的 409 场景化引导对齐终端；终端与 RDP 断开后会话列表乐观结束不再与页头矛盾 |
| 5 | `f95f874` | 终端 WS 握手期排队 resize 修 PTY 尺寸同步（CONNECTING 期帧不再静默丢弃）并补真链路 e2e；数据库测试/查询错误中文分类；断开后录制大小「封盘中…」占位；短录制回放禁拖 |
| 6 | `aa4b357` | 终端输入摘要跨帧缓冲修复逐按键成单字符行，并防超长无换行缓冲膨胀 |

**三期冲刺（p3 sprint 1–6，2026-10-26）**：三期功能（契约先行 `1893d92`）实现合入后沿用同一走查评分规则继续打磨，共 6 轮（提交 `f95c947` → `61926c1`，每轮门禁复验全绿后收轮），**单轮最高 99/100，未达 100**（每轮走查仍存在可做项；评分记录见三期工作记录，本节如实按最高分记录，见已知限制 33）。各轮要点：

| 轮 | 提交 | 本轮要点 |
|---|---|---|
| 1 | `f95c947` | 网络设备连接测试弹窗通道化并剥离英文前缀；服务器重名 409 文案中文化；mongodb 审计动作标签去 SQL 化 |
| 2 | `778d10d` | RDP 服务器下拉隐藏网络设备；`database.query_rejected` 审计标签去 SQL 化 |
| 3 | `3379d69` | 三期实现本体（约 9,700 行）与三期评审修复一并入库（见 CHANGELOG）；NETCLI 测试弹窗剥英文前缀；Mongo 编辑本地成对校验；chat 服务器下拉同口径；Telnet 编辑隐藏主机密钥提示；mongo 查询失败去双重前缀 |
| 4 | `b7b1c32` | Telnet 连接测试弹窗明文告警中文化（未知值回退原文）；SQL 只读警示补参数超白名单同样拒绝说明 |
| 5 | `65e4b52` | netdev 测试弹窗登录/执行失败补中文分类短语（剥英文哨兵链）；终端页 Telnet 网络设备常驻能力说明（另有 `4207cd8` 修 E2E 数据库列表分页溢出 flaky） |
| 6 | `61926c1` | 连接测试/查询失败弹窗剥包名哨兵链与 mongo topology dump，reason 只留 `dial tcp` / `server selection` 级最终原因；p3round6 走查 spec 入库；修 term 下拉虚拟滚动并行 flaky |

**四期冲刺（p4 sprint 1–6，2026-10-27）**：四期功能（契约先行 `e0ac009`）实现合入后沿用同一走查评分规则继续打磨，共 6 轮（提交 `f2462d7` → `f67f0ee`，每轮门禁复验全绿后收轮），**单轮最高 99/100，未达 100**（每轮走查仍存在可做项；评分记录见四期工作记录，本节如实按最高分记录，见已知限制 35）。各轮要点：

| 轮 | 提交 | 本轮要点 |
|---|---|---|
| 1 | `f2462d7` | 容器错误码中文映射、探测失败横幅语义化（通道错误不写回不误报「当前运行时：none」）、镜像大小 1000 进制与审批哨兵值语义化 |
| 2 | `266f46b` | 审计页补 `container.action` 中文标签与筛选项；容器审批弹窗 pending 态「稍后处理」改为中性「关闭」并旁注过期后果 |
| 3 | `1d995e7` | 容器页探测成功写回后失效 servers——页头运行时徽标与操作确认弹窗预览命令同步带上 docker/podman 前缀（另有 `c55713f` prettier 全量格式化） |
| 4 | `091ff4b` | none 服务器重新探测改走 POST test 真实探测并补成功/失败反馈；无名称容器弹窗正文回退短 ID；查询重试跳过 4xx/5xx 业务错误；exited 容器行禁用停止 |
| 5 | `3a012f9` | 容器操作弹窗命令区标签按 confirm/pending/result 三阶段取文案（拒绝细分「未执行命令」），decide 完成后不再误显「待执行命令」 |
| 6 | `f67f0ee` | 容器页 SSH 不可达不再误报 daemon 未运行（通道错误映射 `SSH_EXEC_FAILED`） |

**五期冲刺（p5 sprint 1–6，2026-10-27）**：五期功能（契约先行 `e6c358c`，实现 `49255df` 携 M2 诊断+巡检本体与 M3 共享改动、`164e4b2` 携 M1 监控+M3 批量本体与 M4 前端四页）合入后沿用同一走查评分规则继续打磨，共 6 轮（提交 `14c77ff` → `c12e37f`，每轮门禁复验全绿后收轮），**单轮最高 96/100，未达 100**（每轮走查仍存在可做项；评分记录见五期工作记录，本节如实按最高分记录，见已知限制 37）。各轮要点：

| 轮 | 提交 | 本轮要点 |
|---|---|---|
| 1 | `14c77ff` | 巡检报告/即时巡检按契约返回嵌套 detail；工具卡深链改新标签并按 pending 审批区分守卫文案；五期专属视图提升出折叠面板；fake-llm 补四场景工具链 e2e |
| 2 | `164e4b2` | 服务入口装配 monitor.Manager（采集/评估/通知/保留四循环+自动诊断回调）并补越限→firing e2e；fake-llm 按协议轮次回 probe/report 使诊断会话真实到达 awaiting_fix；评估回退查库锁死最近 N 点并补双向单测；诊断终态码与巡检错误列补中文映射 |
| 3 | `ce2bfc3` | 诊断 fix 端点按契约直返 Approval（修首点审批 decide 打模板 URL 500/命令空白）；退出码非 0 改 warning 语义；巡检删除补失效报告缓存；void mutateAsync 全站改 mutate 消 unhandledrejection |
| 4 | `d6f3e0d` | 批量通道错误码回落通用中文映射（`SSH_EXEC_FAILED` 不再直显）；对话批量工具卡状态 Tag 与计数行复用批量页中文标签 |
| 5 | `bd6d9e5` + `db19d6c` | 批量审批通道失败时 decide 响应 execution 补 exit_code=-1（契约必填口径，修「退出码 undefined」）；走查 e2e 全量竞态修复（链式 project 串行执行 + monitor 空态用例独占清场），全量 156 用例全绿 |
| 6 | `c12e37f` | 监控概要补装 last_inspection（巡检均分卡不再恒显 —）+ 规则重名 409 具体中文「规则名称已存在」+ 巡检触发 toast 去 HTTP 状态码；走查 round6 入库链式编排（batch 用例适配下拉虚拟滚动），全量 162 用例全绿 |

**六期冲刺（p6 sprint 1–6，2026-11-07 / 11-08）**：六期交互深化（审批门限四档/命令改写/执行回放/全站复制/深度思考）合入后沿用同一走查评分规则继续打磨，共 6 轮（提交 `a9cd6f3` → `34e0511`，每轮门禁复验全绿后收轮）。**评分如实记录：前 3 轮给出总分 68 → 77 → 83，第 4–6 轮工作记录未给出总分，故不臆造——单轮最高 83/100，未达 100**（见已知限制 40）。各轮要点如下：

**六期打磨 sprint 1（2026-11-07）**：第 1 轮走查按 high −10 / medium −4 / low −1 计 **68/100**（1 high、5 medium、2 low），采纳全部 8 项并完成，七层门禁全绿后提交：

| 项 | 影响 | 修复要点 |
|---|---|---|
| 对话内批量忽略门限 | high | `batchx.Dispatch` 先取 `approvals.GlobalMode` 再 `ResolveMode(conv, global)`（此前把兜底 all 当全局传入，chat 批量在 standard/none 下仍逐台造审批）；失败关闭保留；补 `Test2_11ChatBatchHonoursGlobalMode` 三档矩阵单测 |
| 回放 reasoning 步无耗时 | medium | openapi `ReplayStep` 增可空 `reasoning_duration_ms`，handler 从 `messages.reasoning_duration_ms` 填充，`ReplayViewer` 透传 `ReasoningBlock.durationMs`——回放摘要不再恒「耗时 —」 |
| 回放 execution 输出未折叠 | medium | 输出改 antd `Collapse`（默认收起、标题「输出」+ `复制输出`），展开态仍 `content-visibility:auto`（11 节防 OOM） |
| 复制清单 4 处未落位 | medium | 巡检报告补「命令」列（新列 `inspection_report_items.command`，迁移 00011，模板展开后的清单）；服务器/数据库编辑弹窗只读态补 ID 与 `host:port` / 连接串复制（组合串抽到 `databaseView.ts` 与列表共用）；模型下拉 `optionRender` 与当前值补模型名复制；终端「复制当前命令」改常显（此前 `!ended` 时整块消失） |
| 复制按钮标题三方不一致 | low | 页头跟随导航：`/rdp`「RDP 桌面」→「桌面」、`/batch`「批量操作」→「批量」，与 nav/title 对齐；补单测与逐页 e2e 断言 |
| 工具卡门限徽标刷新即消失 | medium | 新列 `tool_calls.approval_mode`（迁移 00010，命令执行类工具落快照）+ openapi `ToolCall.approval_mode` + `toToolCallView` 映射；刷新/重进会话徽标仍在 |
| 审计缺实际执行命令 | medium | `command.executed` 的 detail 增 `command`（实际执行、改写后）文本，与 `original_command` 互补——免审批档事后可从审计复原「跑的是什么」 |
| 免审批档徽标语义冲突 | low | `toolCallBadges.isExemptExecution`：none/high 档 + needs_approval + 未建审批 + 非等待态 → 隐藏「需审批」RiskBadge、「高风险」文案改「高风险 · 免审批执行」 |

**六期打磨 sprint 2（2026-11-07）**：第 2 轮走查按同一规则计 **77/100**（1 high、5 medium、2 low），采纳全部 8 项并完成，七层门禁全绿后提交：

| 项 | 影响 | 修复要点 |
|---|---|---|
| 审批编辑态空命令仍可批准执行 | high | `validateEditedCommand` 增「空/纯空白」分支（「命令不能为空，如需放弃修改请点「取消编辑」」），`canSubmitEditedCommand` 随之 false、「保存并批准」禁用；`handleApprove` 再加兜底拒绝空草稿。此前空串走 `buildDecideBody` 的「未改写」分支提交纯 `approve`，服务端按**原命令**批准并真实执行（走查实测执行了 `env`，泄漏 `SSH_CONNECTION`/`PATH`） |
| 采集不可达系统告警永远创建失败 | high | 哨兵规则从 `cpu_pct + threshold=101` 改为 `load_1 + 1e12`（`suspendRuleMetric`/`suspendRuleThreshold` 常量）——前者必然撞迁移 00007 的 `alert_rules_check`（pct 指标 threshold≤100）、映射 `store.ErrInvalid`，连续采集失败的暂停事件从未落库；补 store 集成断言（哨兵规则可创建）与 `TestCollectorSuspendRuleUsesLoadMetric`（指标非 pct、阈值>100、enabled=false、事件与审计真实落库）；另修同一路径的第二处静默隐患：规则被用户从 `/alerts` 删除后缓存 ID 失效、此前直接放弃，现由 `suspendRule` 清缓存重建一次（`TestCollectorSuspendRuleRecreatedAfterDelete`） |
| 回放免审批直执行仍显示「需审批」 | medium | `ReplayStep` 增可空 `approval_mode`（openapi + `replayStep` 结构体），handler 从 `tool_calls.approval_mode` 填充；前端新增 `isReplayExemptExecution`（复用 `toolCallBadges.isExemptExecution`，以 `approvedToolCallIds` 判定是否已有 approval 步），回放 tool_call 步改显「高风险 · 免审批执行」 |
| 回放 `failed` 直显英文 | medium | `toolCallStatusLabel` 增 `case 'failed': return '失败'`（此前交给 `approvalStatusLabel` 的 default 分支原样返回英文）；顺带补 `awaiting_user` 中文；单测断言各状态文案不含 ASCII 字母 |
| 回放失败调用无 error_code / execution 步 | medium | `buildReplaySteps` 的 tool_call 步填 `tc.ErrorCode`；`replayExecutionStep` 对「无 result 且 status=failed」的命令执行类调用也生成 execution 步（`failed` + `error_code` + `exit_code=-1` + 空 output），denied/expired 仍不产生（命令从未执行）；补 `TestConversationReplayToolCallFailureFields` 三态断言 |
| 回放展示 `<!--opsmind-context … -->` | medium | 后端 `stripReplayContextPrefix`（前缀锚定正则，不误剥普通 HTML 注释）+ 前端 `stripContextPrefix` 兜底；补 `TestReplayUserMessageStripsContextPrefix` 三例（带前缀/普通注释/无前缀） |
| 两处手写复制未走 CopyButton | low | `MessageItem` 的 `BubbleCopyButton`（`copyText` + 局部 toast）与 `ContainerLogsDrawer` 的文本「复制」按钮（+「日志已复制」toast）均替换为统一 `CopyButton`（`aria-label="复制消息内容"` / `"复制日志"`），获得两级剪贴板降级面板与 1.5s 冷却；既有 e2e 断言同步改为 `复制日志` + 「已复制」 |
| 单步会话进度条 NaN | low | `replayProgress` 增 `if (total <= 1) return 0;`（此前 `0/(1-1)` = 0/0 → NaN，antd Slider 不写 `aria-valuenow`、轨道恒 0%）；补 `(0,1)/(1,1)/5,1/-1,1` 单测与 e2e `aria-valuenow` 断言 |

**六期打磨 sprint 3（2026-11-08）**：第 3 轮走查按同一规则计 **83/100**（2 high、3 medium、2 low），采纳全部 7 项并完成，七层门禁全绿后提交：

| 项 | 影响 | 修复要点 |
|---|---|---|
| 批量直执行风险级别写死 safe | high | `batchx.runDirect` 增 `riskLevel risk.Level` 形参（由 `decision.Level` 透传），`CreateResult` 与 `command.executed` 审计的 `risk_level` 同用分类快照——none/high 档下 `rm -rf /tmp/x`（RM_ANY，needs_approval）免审直执行时不再被落成「安全」（与同函数审批台 `string(decision.Level)` 同口径）；补 `TestBatchDirectExecutionCarriesClassifiedRiskLevel` |
| 审批取消后 tool_call 永久待审批 | high | 新增 `cancelApprovalAndConverge` / `convergeToolCallStatus` / `convergeResolvedToolCall`：`runSSHExec` 与 `runContainerAction` 的 emit 失败、Wait 异常、`approval_resolved` 发不出去三条分支在取消审批的同时把 tool_call 从 `awaiting_approval` 收敛为 `expired` + 新错误码 `APPROVAL_CANCELLED`（`approval_resolved` 分支落 `CLIENT_DISCONNECTED`，因审批本身未被取消）；收敛前先读状态，不覆盖他人写入的终态；补 5 条 agent 单测（含 ctx 取消的 Fatal 主路径与「TTL 真超时仍为 APPROVAL_TIMEOUT」回归护栏） |
| 批量审计无实际执行命令 | medium | `runDirect` 与 `OnApprovalDecided` 的 `command.executed` detail 均增 `command`（后者为改写后实际执行值）——审计页「命令摘要」列对批量行不再恒「—」；前端 `commandSummary` 本就优先读 `command` 键，无需改动；补两条 batchx 断言 |
| 回放把「流断开取消」显示成「已超时」 | medium | `approvalStatusLabel('expired')` 改为中性「已过期」——expired 同时覆盖 TTL 超时与 Cancel（note=stream_cancelled），而 ReplayStep 无 note 字段，说「超时」会与同会话对话卡的「已取消」矛盾；同步更新单测并加「不得含『超时』」护栏 |
| 三个审批入口无「编辑命令」 | medium | 抽出 `components/ApprovalCommandEditor.tsx`（复用 `approvalEdit` 的校验/`buildDecideBody`/`preview-risk` 预览/空草稿拦截与 forbidden 禁提交），接入容器操作弹窗、诊断修复抽屉、批量详情待审批行（后者新增「编辑」按钮 + 单条审批查询 `qk.approval` + 编辑弹窗）；三处 decide mutation 均支持 `edited_command`；补 `approvalEdit.test.ts` 4 项跨入口断言与批量真实链路 e2e |
| 门限选择器无可访问名 | low | `ApprovalModeSelect` 增 `ariaLabel`（默认「审批门限」），ChatPage 传「会话审批门限」、SettingsPage 传「审批门限」；e2e 按 role=combobox + name 断言两处 |
| 回放无 command 的失败执行步渲染空块 | low | `ReplayViewer` 的 execution 步仅在 `step.command` 非空时渲染「命令」代码块，否则显示「命令未知（通道失败前未下发）」，error_code 说明仍可见；e2e 断言无 codeblock、无「命令」标签 |

**六期打磨 sprint 4（2026-11-08）**：第 4 轮走查采纳全部 7 项（1 high、3 medium、3 low），七层门禁全绿后提交（本轮工作记录未给出走查总分，故不臆造分数，按项列影响级别与修复要点）：

| 项 | 影响 | 修复要点 |
|---|---|---|
| 私钥认证未填 passphrase 三路全灭 | high | monitor/inspection/diagnosis 三个 `decrypt` 辅助补空串短路（`passphrase` 按 openapi 4.3 可选，库中为空串；`cryptox.Decrypt("")` 返回 `ErrInvalidCiphertext` 被误判成解密失败）。此前巡检 0 分且错误列直出 `inspection: decrypt passphrase failed`、采集 5 次失败后退避暂停、AI 诊断整条失败；与 agent/batchx 早已存在的短路对齐，非法密文与 nil cipher 仍报错。前端补 `decrypt … failed` → 「凭据解密失败（请重新录入该服务器的登录凭据）」 |
| 审计页缺 18 个动作与 7 个资源类型 | medium | `AUDIT_ACTIONS` 补 `settings.update` 与五期全部 alert_rule/notification/alert.*/diagnosis.*/inspection.*/metric.collect/batch.*，`RESOURCE_TYPE_OPTIONS` 补 monitor/alert_rule/alert_event/notification_channel/diagnosis_session/inspection_report/batch_job；此前动作列回落英文原码、筛选输入「设置」「批量」0 命中 |
| 折叠面板内复制按钮连带开合 | medium | 回放输出、批量详情每台输出、巡检报告原始输出三处 CopyButton 外层包 `stopPropagation`（click + keydown）——按钮在 Collapse label 区内，点击冒泡到 header 触发开合 |
| 小视口侧栏末项被折叠条压住 | medium | Sider 内菜单外再包 `paddingBottom: 53`（antd `triggerHeight = 45 + 8×2`，`position: fixed; bottom: 0` 不计入滚动区留白）；1366×768 的「设置」、1280×720 的「批量」「设置」可滚到折叠条上方并正常点击 |
| 审批卡过期措辞与 Tag 不一致 | low | 抽出 `components/approvalStatus.ts` 的 `APPROVAL_STATUS_LABELS`，`expired` 统一「已过期」，与 StatusTag / 回放同源（此前 ApprovalCard 单独写「已超时」） |
| 「采集不可达」告警全英文 | low | collector 消息改「连续 5 次指标采集失败，采集已暂停（10 分钟后自动重试）」；规则名保持英文键，前端 `ruleDisplayName` 映射「采集不可达（系统）」（规则行/事件行，原名在 Tooltip 与副行） |
| 回放截断横幅口径矛盾 | low | `ReplayResponse` 增 `truncated_reason`（`''`/`steps`/`approvals`，抽 `replayApplyTruncation`），前端按原因二选一——审批满 200 条不再说「超出 20000 步上限」 |

**六期打磨 sprint 5（2026-11-08）**：第 5 轮走查采纳全部 8 项（5 medium、3 low），七层门禁全绿后提交（本轮工作记录未给出走查总分，故不臆造分数，按项列影响级别与修复要点）：

| 项 | 影响 | 修复要点 |
|---|---|---|
| 审计页「命令批准/命令预览」是永空死筛选项 | medium | `command.approved` 补真实写入点：`approvals.Service.Decide` 在 chat/console 批准时写（`resource_type=server`，detail 含 `approval_id/tool_call_id/source/risk_level/approval_mode/command`，改写时另带 `edited_command/original_command/*_risk_level`），`source=batch` 由 `batchx.OnApprovalDecided` 写（detail 加 `batch_job_id`，approvals 侧对该来源跳过——一次批准只落一行）；`command.preview` 全仓无写入点（命令预览统一落 `risk.preview`），从 `AUDIT_ACTIONS` 移除但保留中文标签映射（`AUDIT_ACTION_LABELS_RESERVED`，历史行不回落原码） |
| 审批行「命令摘要」恒「—」、批量拒绝无回放入口 | medium | `approval.approved`/`approval.denied` 的 detail 无条件带 `command`（原命令；改写时另有 `edited_command`）；`batchx` 的 `command.denied` detail 补 `command` 与 `risk_level`——审计页「命令摘要」列与展开「完整命令」据此可读 |
| 批量详情已执行行看不出改写 | medium | `BatchResult` 增 `edited_command`（store 查询经 `approval_id` LEFT JOIN `approvals` 一次读出，50 台 job 不逐行回取审批；openapi 必填字段）；`BatchDetailDrawer` 新增「命令」列：改写行显示「实际执行 <edited_command>」+「用户改写」Tag + 复制按钮，未改写行回落 job 原文命令 |
| 回放审批来源直出英文 | low | `replayPlayer.ts` 增 `approvalSourceLabel`（chat→对话 / console→控制台 / batch→批量，未知原样、空值「—」），`ReplayViewer` 审批步头部改用中文；单测断言映射不含 ASCII 字母 |
| 改写后工具卡仍显示模型原命令且标红 | low | 抽 `toolCommandPanel.ts`：关联审批 `edited_command`（或工具结果 `result.command`）与 `arguments.command` 不同则面板标题改「模型原命令」/「模型原 SQL」并在其下补「实际执行 <命令>」+「用户改写」Tag；`arguments` 原值保留（审计事实），红色高风险标注随之取消 |
| 门限文案里的数据库字段名 | low | `approvalMode.ts` 的 none 档说明与 `SettingsPage` 的免审批警示同步改为「关闭了「自动执行」开关的服务器仍会拦截非只读命令」，`auto_exec_enabled=false` 只留代码注释；单测断言四档说明不含 snake_case 字段名与 `=` |
| 顶栏会话标题无 tooltip | low | `ChatPage` 顶栏标题加原生 `title`（与侧栏 `ConversationSidebar` 同口径）；同时修 `.chat-topbar-title` 的 `max-width` 对 inline 元素不生效（补 `display:inline-block`），260px 省略号截断才真正成立 |
| /chat 缺页头、三方名称不一致 | low | 顶栏补视觉弱化的 `h3`「对话」（15px/次级色，`chat-topbar-page-title`）+ 分隔符，nav「对话」/ 页头「对话」/ 标签页 title「对话」三方一致；会话标题紧随其后仍是主角 |

**六期打磨 sprint 6（2026-11-08）**：第 6 轮走查采纳全部 8 项（1 high、2 medium、5 low），七层门禁全绿后提交（本轮工作记录未给出走查总分，故不臆造分数，按项列影响级别与修复要点）：

| 项 | 影响 | 修复要点 |
|---|---|---|
| 监控网络入/出读数 8 倍虚高 | high | `parse_linux.go` 的 `net_*_bps` 去掉 `* 8`（契约 7.7「字节差 ÷ interval」、Windows 路径 `BytesReceivedPersec` 直取、前端 `formatBps` 字节单位三处同口径）；60s 内 +60,000,000 字节现在解析为 1,000,000 B/s（此前 8,000,000，页面显示 8.00 MB/s）；补 `TestParseLinuxOutputNetRateIsBytesPerSecond` 锚定，既有 `TestParseLinuxOutputSecondRoundDiffs` 期望同步改为 30000/38333 |
| 容器弹窗 confirm 文案与实际分流相反 | medium | 抽 `containerActionPolicy.containerActionNeedsApproval`（`NeedApproval` 矩阵的容器动作切片：start=safe、stop/restart=needs_approval+severity high，8 格 + 未知门限，单测逐格覆盖）；`ContainerActionModal` 按「全局门限 × 服务器 auto_exec × 动作」渲染——需审批「操作不直接执行…将创建一条需人工批准的审批」+「创建审批」，免审批「当前审批门限判定为免审批——点击后将立即执行该命令（不经人工批准），并记录审计」+「确认执行」（danger），门限未加载用中性文案；`commandSectionLabel` confirm 分支随之细分「将直接执行的命令」/「将执行的命令」；`ContainerActionTarget` 增 `autoExecEnabled` |
| 回放 assistant_text 不渲染 Markdown | medium | `ReplayViewer` 的 assistant_text 步改走 `LazyMarkdownView`（与 `MessageItem` 同源，不引入新依赖）——```bash 围栏渲染成 `CodeBlock`（高亮 + 「复制代码」），不再以字面量塞进 `<p>`；user_message 保持纯文本气泡；`.replay-step-content--markdown` 关掉 pre-wrap |
| 对话拒绝审计缺 command/risk_level | low | `agent/runner.go` 的 `command.denied` detail 补 `command` 与 `risk_level`（与 `batchx` 同口径）——审计页「命令拒绝」行的命令摘要不再恒「—」；补 `TestRunDeniedCommandAuditCarriesCommandAndRisk` 与 E2E |
| 容器弹窗 TTL 文案「已超时」 | low | 改用 `APPROVAL_STATUS_LABELS.expired`（「已过期」），与 StatusTag / ApprovalCard / 回放同源；E2E 断言弹窗只出现「已过期」且批准/拒绝禁用 |
| 容器直执行审计缺 approval_mode | low | `containers_handlers.go` 直执行分支的 `container.action` detail 初始化即带 `"approval_mode": string(mode)`（与审批分支 `executeConsoleApproval` 同字段）；补 `TestContainerDirectExecutionAuditsApprovalMode`（none 档 + auto_exec 直执行 restart，审计 detail 断言 none） |
| 巡检报告大数字恒绿 | low | `inspectionView.ts` 增 `SCORE_COLOR_VAR`/`scoreColorVar`（`scoreColor` 的色值投影，green/gold/red → `--ctp-green/-yellow/-red`），`ReportDrawer` 的 44px 大数字改用——30 分报告现在显示红色，与列表 Tag 分档同源 |
| 审批过期审计只有聚合行 | low | `approvals.Service.Expire` 在批量 UPDATE **之前**抓到期候选快照（`List(pending, limit=200)`，有界），UPDATE 后逐条 `Get` 复核 `status='expired'` 再 emit（`resource_id=<approval_id>`，detail：`{approval_id, reason:'ttl_expired', source, command, risk_level, approval_mode, server_name}`；并发 decide 抢先的行跳过不误报），聚合行保留并附 `approval_ids`（≤200，超出置 `detail_truncated`）；补 `TestExpireEmitsPerApprovalAudit` / `TestExpireSkipsConcurrentlyDecidedApproval` / `TestExpireNoCandidatesNoAudit` 与 E2E |

**七期打磨（2026-11-09）**：七期主题契约落地后的验收走查采纳全部 6 项（2 high、3 medium、1 low），每项均补单测或 E2E，七层门禁复验全绿后提交（本轮工作记录未给出走查总分，故不臆造分数，按项列影响级别与修复要点）：

| 项 | 影响 | 修复要点 |
|---|---|---|
| 窄屏顶栏被主题切换器挤爆 | high | 移动端不再渲染品牌文字（`isMobile` 时省掉「云枢 OpsMind」），切换器在 `< md` 断点改**图标态**（太阳/月亮/桌面，`title` 保留三态说明）；`.app-header` 增 `flex-wrap: nowrap` + 子项 `min-width: 0`，页头标题 `ellipsis`。实测 360/375/390/414 顶栏 `scrollHeight 48 ≤ clientHeight 47 + 1`、`scrollWidth == clientWidth`、用户名按钮不越界；修复前品牌 rect 14×356px、header 216 vs 47 |
| 输入框/Select 外框 1.55:1 | high | `components` 里给 `Input`/`InputNumber`/`Select`/`DatePicker` 各钉 `colorBorder: --color-border-strong`（浅 4.07 / 深 5.81 on container），**全局 `colorBorder` 不动**（表格/卡片分隔线保持装饰级弱线）；实测搜索框与 Select 的 `border-top-color` 分别为 `rgb(118,126,145)` / `rgb(147,153,178)` |
| 图表 tooltip 浅色边界消失 | medium | `MetricChart` 自绘 tooltip 补 `border: 1px solid var(--color-border)` + `boxShadow: var(--color-shadow)`；浅色靠阴影、深色靠描边，两主题同名变量；`data-testid="metric-chart-tooltip"` 供 e2e 断言 |
| 回放进度条未播放轨道 ≈1.09:1 | medium | `components.Slider` 钉 `railBg`/`railHoverBg = --color-border-strong`、`trackBg`/`trackHoverBg` = primary / primary-hover、`handleColor`/`handleActiveColor` = primary |
| 表格空态提示用禁用色 | low | `components.Table` 覆盖 `colorTextDisabled = --color-text-tertiary`（浅 6.04 / 深 7.37）；作用域不外溢——表格内禁用按钮实测仍为 `rgb(118,126,145)`；顺带改善表头筛选空结果与搜索图标 |
| 容器页默认选中无运行时服务器并立即 400 | low | `pickDefaultServer` 改为 docker/podman 优先、`''` 未探测兜底、none/containerd 跳过；全部不可选返回 `null`，页面显示「请先选择支持容器运行时的服务器」并保留「重新探测」（`POST /test`）。实测零容器列表请求、无 `ant-alert-error`、无 console 400 |

### 8.9 明确未验证的部分

- **五期告警未做「真实高负载真机→采集→firing→通知外发」全链路实测**：告警链路分层验证——评估侧经真实 HTTP/库端到端（E2E 直插越限点断言 75s 内 firing 出现，见 8.4）；采集侧为注入假执行桩的 Go 单测（Linux /proc 分段解析、Windows JSON 解析、退避暂停/恢复）；通知侧为纯逻辑单测（webhook HMAC 签名、飞书/钉钉签名 URL、SMTP 报文拼装、模板渲染）+ 渠道 test 端点。**未对真实飞书/钉钉机器人或真实 SMTP 服务器外发过通知**；未在真实服务器上制造高负载触发真实采集越限。
- **五期巡检 cron 未做多天期真实墙钟验证**：cron 解析边界（NextRun/非法表达式/永不匹配有界终止）与调度约束（同 job 互斥、全局并发 2、每分钟 tick）为注入假时钟的 Go 单测与集成测试；未让一个 job 跨多天真实到期触发（服务重启后的 next_run_at 对齐行为亦未长跑验证）。
- **五期批量/诊断的执行段未对真实多机并发实测**：batchx/diagnosis 的分类、审批、执行回填、状态归并经注入假 SSH 桩的 Go 单测与 E2E（假服务器/mock LLM）；未对 50 台真实服务器并发执行验证池上限与归并语义。
- **未对真实容器运行时做端到端验证**：容器管理的探测判据（`docker version --format` daemon 不可达 exit 1、`podman info` 本地 store 成功）是四期调研期对本机 CLI 的实测结论（契约记录在案），但系统链路层面的验证全部使用注入 `containerx.ExecFunc` 的假执行桩（命令模板、JSON 解析、错误语义）与 E2E 假服务器/mock 数据；本机虽装有 Docker daemon，测试桩不连接它——真实 daemon 的 `--format json` 输出版本差异、真实 Docker Desktop/Podman on Windows 的探测模板行为、真实容器的 start/stop/restart 与 console 审批执行链均未实测。
- **未连接真实 Windows 主机**：OpenSSH/WinRM 协商、真实 PowerShell 执行语义、Windows 风险分册对真实环境的覆盖，均只经 fake WinRM SOAP 端点与规则单测验证（五期 Windows 采集/巡检模板同理）。
- **未连接真实网络设备**：五品牌分册、netdev expect 流程（提示符/登录/分页/enable）与 facts 解析均只经 fake 设备 TCP server（按厂商 profile 仿真提示符/登录/分页）与假 sshd 验证；未对 Cisco/Huawei/H3C/Juniper 真机（含不同 OS 版本的 CLI 变体）做过实测，分册规则可能存在与真机语义的偏差。
- **未连接真实 RDP 服务端（3389）**：RDP 链路验证止于 fake guacd（E2E 真链路）与真 guacd 容器握手（可选集成测试，PASS）；guacd → Windows 3389 段未实测。
- **未连接真实 MySQL / SQL Server / Redis / MongoDB 实例**：mssql/redis/mongodb 守卫与结果映射有纯逻辑与 fake 测试，驱动层协议测试标注 build tag（`integration_mssql`/`integration_redis`/`integration_mongodb`，CI 不强制），SQLite/PG 有实际执行测试。
- **未调用真实 LLM 供应商**（全部使用本地假 OpenAI 兼容服务——五期诊断 mini-loop 与「AI 解读」、六期深度思考的模型行为同样经假 LLM 脚本化验证；深度思考的 `reasoning_content`/`thinking` 解析只有 `internal/llm/openai_test.go` 与 `anthropic_test.go` 的协议单测 + `fake-llm` 的 `__scenario_reasoning__` 场景，未对接过任何真实推理模型的 reasoning 流）。
- **未验证真实反向代理（Nginx/CDN）下的 SSE/WS 长连接表现**（3.3 的配置建议未实测；E2E 的 WS 链路走 vite preview 本地代理）。
- **六期执行回放未经真实长会话/大文本实测**：回放的分页读取（messages/tool_calls 每页 200、审批 4×200）、20000 步上限与两种截断原因均在单测与 E2E 中用例化（`internal/httpapi/replay_test.go` 用 `buildReplaySteps` 直接构造超限 fixture，避免 SQL 灌 2 万行），但**没有在真实积累到数千步的会话上跑过完整回放**；播放器的自动播放/速度/进度条只在 E2E 的固定步数会话上验证，长时间自动播放的滚动与内存表现未测。
- **六期审批门限的免审批直执行只在测试库上验证**：`none`/`high` 档 + `auto_exec_enabled=true` 的直执行路径由 Go 单测（`approvals.TestNeedsApproval*` 矩阵、`TestBatchDirectExecutionCarriesClassifiedRiskLevel`、`TestContainerDirectExecutionAuditsApprovalMode`）与 E2E（容器弹窗 confirm 文案、诊断 fix 分流）覆盖，**未在真实服务器上以 `none` 档执行过 needs_approval 级命令**验证端到端审计与执行一致性（风险面明确，仅取证需要）。
- **六期复制按钮的降级链路未在真实非安全上下文验证**：E2E 通过 `permissions: ['clipboard-read', 'clipboard-write']` 授权后断言剪贴板内容，覆盖的是**第一级 Clipboard API 成功**路径；`execCommand` 降级与「可选中文本面板」由 `copyButtonLogic.test.ts` 纯逻辑单测覆盖，**没有在 HTTP 内网部署 + 拒绝剪贴板权限的真实浏览器环境里走通完整的降级 UI**（降级面板是否被浏览器遮罩、自动全选是否生效未实测）。
- **深度思考只验证到「4KB 级」文本**：`reasoningBlock.test.ts` 用短文本覆盖摘要/折叠/复制判定，`polish-p6sprint1.spec.ts` 的 `__scenario_reasoning__` 是脚本化的短 reasoning 流；**没有用真实模型产出的超长思考（数万字符）验证过展开渲染性能与剪贴板写入**，也没有对 `messages.reasoning` 单独设长度上限（见已知限制 39）。
- 浸泡已覆盖 15m（七次）/ 30m / 45m 多档（均 PASS）；**45 分钟以上与生产规模并发**（stress 封顶 40）未实测；loadgen 未覆盖终端/RDP 会话、网络设备/MongoDB/容器查询与五期监控采集/巡检/批量链路负载，也**未覆盖六期回放接口与门限四档的负载**（loadgen 的对话链路固定走 standard 档审批自动批准）。
- UI 交互由 222 项 Playwright E2E 覆盖（终端/RDP 为真链路用例；网络设备/MongoDB 为表单与错误链路用例，不经真机/真库；容器页为探测通道错误真链路 + mock 数据用例，不经真实容器运行时；五期监控/告警/巡检/批量四页与评估引擎装配为真链路用例，诊断会话经假 LLM 脚本化、批量执行经假服务器；六期 7 个 spec 文件覆盖回放/门限/改写/复制/深度思考（六期终版 43 条），2026-11-09 实跑 44 passed）；未做组件级渲染单测（见已知限制 11）。

功能与验证的演进历史见 [CHANGELOG.md](CHANGELOG.md)。
