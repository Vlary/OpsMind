# 更新日志

本项目的显著变更按时间倒序记录于此。条目附 git 提交号；未标提交号的综合工作（验证补齐、六轮打磨的自迭代评审）来自当日工作记录。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## 2026-11-09 — 七期打磨（6 项验收缺口修复）

七期主题契约落地后的验收走查采纳全部 6 项（2 high / 3 medium / 1 low），每项均补单测或 E2E，七层门禁复验全绿后提交：

- **窄屏顶栏被主题切换器挤爆（high）**：移动端此前无条件渲染品牌文字「云枢 OpsMind」，且三态 `Segmented` 恒为文字态——360/375/390 下品牌名竖排成单字被裁切、header `scrollHeight` 216 vs `clientHeight` 47，375 下用户名按钮右溢 6px。改为：`isMobile` 时不渲染品牌文字、切换器改**图标态**（太阳/月亮/桌面，`title` 与 `aria-label` 保留三态说明）；`.app-header` 增 `flex-wrap: nowrap` 与子项 `min-width: 0`，并给页头标题 `ellipsis`。实测 360/375/390/414 均 `scrollHeight 48 ≤ clientHeight 47 + 1`、`scrollWidth == clientWidth`；宽屏仍是文字态。
- **输入框/Select 外框 1.55:1 不达 3:1（high）**：全局 `colorBorder` 取 `--color-border`（装饰线口径，浅 1.55 / 深 1.80 on container），而 antd 的 Input/InputNumber/Select/DatePicker 外框同消费该 token，低于契约 8.3.11「可操作边界 ≥ 3:1」。在 `components` 里给四类组件各钉 `colorBorder: --color-border-strong`（浅 4.07 / 深 5.81），**不改全局 `colorBorder`**（表格/卡片分隔线保持装饰级弱线）。实测 `.ant-input-affix-wrapper`/`.ant-select` 的 `border-top-color` = `rgb(118,126,145)`（浅）/ `rgb(147,153,178)`（深）。
- **图表 hover tooltip 在浅色下边界消失（medium）**：`MetricChart` 自绘 tooltip 只有背景色，浅色浮层底与白卡片同色（`#fff` on `#fff`、`border: 0px`、`box-shadow: none`）。补 `border: 1px solid var(--color-border)` + `boxShadow: var(--color-shadow)`（两主题同名变量）；深色仍靠 `--color-bg-elevated` 分层，浅色靠阴影 + 描边。
- **回放进度条未播放轨道 ≈1.09:1（medium）**：antd `Slider` 默认 `railBg = colorFillTertiary`（浅色 `rgba(0,0,0,.04)` 压白）。在 `components.Slider` 钉 `railBg`/`railHoverBg = --color-border-strong`、`trackBg`/`handleColor` = `--color-primary`、hover 用 `--color-primary-hover`；实测 rail `rgb(118,126,145)`（浅）/ `rgb(147,153,178)`（深）。
- **表格空态提示用禁用色（low）**：antd 的 `.ant-table-placeholder` 取 `colorTextDisabled`，浅色 4.07:1 < 4.5 且契约明令禁用色不得用于提示文案。`components.Table` 覆盖 `colorTextDisabled = --color-text-tertiary`（浅 6.04 / 深 7.37）；组件作用域不外溢（表格内禁用按钮仍为 `rgb(118,126,145)`，实测）。
- **容器页默认选中无运行时服务器并立即 400（low）**：`pickDefaultServer` 此前取第一台 server 类资产且不看 runtime，种子数据第一台常为 `none`——进页即 `GET /containers` 400 `CONTAINER_UNSUPPORTED` + 一整条红色错误横幅 + console 400。改为 docker/podman 优先、`''` 未探测兜底、none/containerd 跳过；全部不可选时返回 `null`，页面显示「请先选择支持容器运行时的服务器」空态并保留「重新探测」（走 `POST /test` 真实探测）。

- **新增/更新测试**：`web/e2e/polish-p7.spec.ts`（16 条：窄屏四档顶栏 + 宽屏文字态、两主题输入框边框、两主题 tooltip 边界、两主题 slider 轨道、两主题表格空态、容器页三条默认选择链路）；`serverPicker.test.ts` 重写 `pickDefaultServer` 五组用例（docker 优先/未探测兜底/全 none 返回 null/network_device 不参与/空列表）；`themeTokens.test.ts` 增三组断言（四类输入组件 token、Slider 六键、Table 空态色且全局 `colorBorder` 未被抬高）；`polish-p4sprint4.spec.ts` 的空态断言随新的默认选择规则改为「请先选择支持容器运行时的服务器」。
- **门禁复验（全部通过）**：`gofmt -l .` 无输出；`go vet ./...` 退出码 0；`go test -count=1 ./...`（TEST_DATABASE_URL 指向本地 65432）27 包 ok、0 FAIL；`go run ./cmd/smoke` `SMOKE_OK`（S02 p95 16.26ms）；前端 typecheck / lint（`--max-warnings=0`）/ 单测 **82 文件 1370 项** / build + 体积门禁 **124,389 B ≈ 121.5 KiB PASS** / `check:colors` PASS（142 文件，白名单 3，命中 0）；E2E **222 passed（11.5m）**；四目标 fuzz 各 10s 无崩溃（FuzzClassify 2,827,861 / FuzzWinClassify 1,709,951 / FuzzNetClassify 177,853 / FuzzCheck 205,233 次执行）；`loadgen -mode=selftest` **PASS**（chats=804 / approvals=808 / 5xx=0 / sse p95=282ms / RSS +8.1%）；真 guacd 集成 `ok`（2.009s）；部署验证镜像构建 + 容器 Healthy + `/healthz` `database=ok`。
- **文档**：CONTRACT 8.3.8 组件 token 表补 `Table.colorTextDisabled`、`Input/InputNumber/Select/DatePicker.colorBorder`、`Slider.*` 三行与三段条文（输入类边界 / Slider 轨道 / 表格空态）、8.3.11 第 5 条补「禁用色不得复用为提示文案」、8.3.12 入口行写明窄屏图标态、8.3.14 的 E2E 追加清单补 ⑥–⑪、8.7 监控页补 tooltip 边界、8.7 容器页补默认选择规则；README 8.1 门禁表换本轮实测、8.8 增七期打磨表。

## 2026-11-08 — 六期打磨 sprint 6（8 项走查修复）

第 6 轮走查采纳全部 8 项（1 high / 2 medium / 5 low），每项均补单测或 E2E，七层门禁复验全绿后提交：

- **监控「网络入/网络出」读数 8 倍虚高（high）**：`internal/monitor/parse_linux.go` 的 `net_*_bps` 去掉 `* 8`——契约 7.7「两次采样字节差 ÷ interval」、Windows 路径（`Win32_PerfFormattedData_Tcpip_NetworkInterface` 的 `BytesReceivedPersec` 直取，同为字节/秒）与前端 `formatBps`（MB/s 字节单位）三处同口径。此前 60s 内 `/proc/net/dev` +60,000,000 字节（真实 1,000,000 B/s）被解析成 8,000,000，监控卡片与「复制网络入当前值」显示「8.00 MB/s」，同时污染告警阈值与复制值。补 `TestParseLinuxOutputNetRateIsBytesPerSecond`（60s/60MB → 1,000,000 B/s）锚定，既有差值测试期望同步改为 30000/38333，另补 E2E 断言页面显示「当前 1.00 MB/s」且不含「8.00 MB/s」。
- **容器操作弹窗 confirm 文案与实际分流相反（medium）**：文案与按钮此前恒称「操作不直接执行——将创建一条需人工批准的审批」「创建审批」，但六期统一门限下 safe+auto_exec（standard 档）或 none/high 档实际是**直接执行、不产生任何审批**——破坏性命令（stop/restart）已真实执行，用户以为在排队审批。抽 `web/src/features/containers/containerActionPolicy.ts`（`approvals.NeedsApproval` 矩阵的容器动作切片：start=safe、stop/restart=needs_approval+severity high；8 格 + 未知门限，单测逐格 + 单调性断言），`ContainerActionModal` 按「全局门限 × 服务器 `auto_exec_enabled` × 动作」渲染两种文案与按钮（免审批档「确认执行」+ danger、正文「点击后将立即执行该命令（不经人工批准），并记录审计」），门限未加载时用中性文案；`commandSectionLabel` 的 confirm 分支随之细分「将直接执行的命令」/「将执行的命令」。真正分流仍以后端 action 响应为准（`approval=null` 即直执行），策略函数仅用于文案预告（契约 2.11 约定 2 第 ④ 条）。
- **回放 assistant_text 不渲染 Markdown（medium）**：`ReplayViewer` 的 assistant_text 步此前是纯文本 `<p className="replay-step-content">`，Markdown 代码围栏原样显示为 ```bash 字面量，且代码块失去高亮与「复制代码」按钮，与聊天页同一条回答呈现不一致。改走 `LazyMarkdownView`（与 `MessageItem` 同源，不引入新依赖）；user_message 保持纯文本气泡（与聊天侧用户气泡同语义）；`.replay-step-content--markdown` 关掉 pre-wrap。E2E 断言回放步内 `pre.opsmind-codeblock` 存在、含命令文本、有「复制代码」，且不再出现 ```bash 字面量。
- **对话拒绝审计缺 command/risk_level（low）**：`internal/agent/runner.go` 的 `command.denied` detail 只有 `tool_call_id`/`approval_id`，审计页「命令拒绝」行的命令摘要与「结果」列恒「—」；批量路径（`internal/batchx/execute.go`）早已带 `command`/`risk_level`。本轮对齐补两字段，补 `TestRunDeniedCommandAuditCarriesCommandAndRisk` 与 E2E（`detail.command` = `rm -rf /tmp/aiops-e2e`、`detail.risk_level` = `needs_approval`）。
- **容器弹窗 TTL 文案「已超时」（low）**：`ContainerActionModal` 的倒计时归零文案改用 `APPROVAL_STATUS_LABELS.expired`（「已过期」），与 `StatusTag` / `ApprovalCard` 正文 / 回放同源（sprint 4 只收敛了对话审批卡）。E2E 断言过期后弹窗只出现「已过期」且批准/拒绝按钮禁用。
- **容器直执行审计缺 approval_mode（low）**：`internal/httpapi/containers_handlers.go` 门限判定直执行分支的 `container.action` detail 初始化即带 `"approval_mode": string(mode)`——免审批/仅高危档直执行后该审计行能自证是哪档门限放行（与 `executeConsoleApproval` 的审批分支同字段，契约 9.7）。补 `TestContainerDirectExecutionAuditsApprovalMode`（全局 none + auto_exec 直执行 restart，断言 `approval=null`、逐字节命令、审计 `approval_mode=none`）。
- **巡检报告平均评分大字恒绿（low）**：`ReportDrawer` 的 44px 平均评分硬编码 `#a6e3a1`，30 分低分报告也显示为绿色，与列表 Tag 的 `scoreColor`（>=85 绿 / >=60 黄 / 其余红）矛盾。`inspectionView.ts` 增 `SCORE_COLOR_VAR` / `scoreColorVar`（色阶 → 主题色变量），抽屉改用——低分大字现在为红。E2E 断言 30 分大字 `color: rgb(243, 139, 168)`。
- **审批过期审计只有聚合行（low）**：`internal/approvals/service.go` 的 `Expire` 只 emit `ResourceID: ""` + `Detail: {count}`，审计页「审批过期」筛选下目标与命令摘要恒「—」。改为在批量 UPDATE **之前**抓到期候选快照（`List(pending, limit=200)`，有界读取），UPDATE 后逐条 `Get` 复核 `status='expired'` 再 emit（`resource_type=approval`、`resource_id=<approval_id>`、detail：`{approval_id, reason:'ttl_expired', source, command, risk_level, approval_mode, server_name}`；并发 decide 抢先的行跳过、不误报「已过期」），聚合行保留并附 `approval_ids`（≤200；`count > 候选数` 时置 `detail_truncated=true`）。上限写死为 `expireCandidateLimit=200`（防 OOM 铁律，契约 11 节第 20 条）。补 `TestExpireEmitsPerApprovalAudit` / `TestExpireSkipsConcurrentlyDecidedApproval` / `TestExpireNoCandidatesNoAudit` 与 E2E（45s TTL 后既命中逐条行也命中聚合行，审计页可按命令筛选）。
- **新增 E2E** `web/e2e/polish-p6sprint6.spec.ts`（8 条）：监控网络速率按字节/秒展示、容器弹窗免审批档「确认执行」直执行文案、standard 档审批文案回归、TTL 到期「已过期」、回放 assistant_text 代码块渲染+复制代码、命令拒绝审计带 command/risk_level、巡检低分大字红色、审批过期逐条审计可定位。

- **门禁复验（全部通过）**：`gofmt -l .` 无输出；`go vet ./...` 退出码 0；`go test -count=1 ./...`（TEST_DATABASE_URL 指向本地 65432）27 包 ok；覆盖率 **69.9%**；`go run ./cmd/smoke` `SMOKE_OK`（S02 p95 23.03ms）；前端 typecheck / lint（`--max-warnings=0`）/ 单测 **79 文件 1305 项** / build + 体积门禁 **124,348 B ≈ 121.4 KiB PASS**；E2E **205 passed（11.6m，49 spec）**；四目标 fuzz 各 10s 无崩溃（FuzzClassify 2,779,736 / FuzzWinClassify 1,794,121 / FuzzNetClassify 174,476 / FuzzCheck 201,902 次执行）；`loadgen -mode=selftest` **PASS**（chats=811 / approvals=815 / 5xx=0 / sse p95=279ms / RSS +4.4%）；真 guacd 集成 `ok`（2.011s）；部署验证镜像构建 + 容器 Healthy + `/healthz` `database=ok`。
- **文档**：CONTRACT 5.5 补 `Expire` 逐条明细与 200 条上限、7.7 明确 `net_*_bps` 为字节/秒（不乘 8）、8.7 补容器弹窗门限预告文案/回放 assistant_text Markdown/巡检大数字色阶、9.7 补 `command.denied` 与 `approval.expired` 载荷及 `container.action.approval_mode`、11 节第 20 条写死过期审计明细上限；README 8.1 门禁表换本轮实测、8.8 增六期 sprint 6 表；CHANGELOG 本条目。

## 2026-11-08 — 六期打磨 sprint 5（8 项走查修复）

第 5 轮走查采纳全部 8 项（5 medium / 3 low），每项均补单测或 E2E，七层门禁复验全绿后提交：

- **审计页「命令批准 / 命令预览」是永空死筛选项（medium）**：`command.approved` 在契约 9.7 里存在但后端 0 处写入——审计页下拉选中后一行不返回（实测 `/api/audit?action=command.approved` items=[]）。本轮补真实写入点：`approvals.Service.Decide` 在 chat/console 批准时写（`resource_type=server`，detail：`{approval_id, tool_call_id, source, risk_level, approval_mode, command, edited_command?}`），`source=batch` 由 `batchx.OnApprovalDecided` 写（detail 加 `batch_job_id`；approvals 侧对该来源跳过，一次批准只落一行）。`command.preview` 全仓无写入点（命令预览统一落 `risk.preview`），从 `AUDIT_ACTIONS` 移除，但中文标签保留在 `AUDIT_ACTION_LABELS_RESERVED`（历史行不回落英文原码）。补 `TestDecideApprovedWritesCommandToAudit` / `TestDecideDeniedWritesCommandToAudit` / `TestDecideBatchSourceSkipsCommandApproved` / `TestDecideEditedWritesBothCommands` 与 `TestBatchApprovalAuditCarriesCommandAndApproved`，E2E 覆盖筛选命中与死选项消失。
- **审批行「命令摘要」恒「—」、批量拒绝无回放入口（medium）**：`approval.approved`/`approval.denied` 的 detail 此前只有 `approval_id/tool_call_id/source/risk_level/note`（改写时才补 original/edited），批量 `command.denied` 只有 `approval_id + batch_job_id`——审计页「命令摘要」列恒「—」，批量拒绝（无会话）连回放入口都没有。本轮两处 detail 无条件带 `command`（原命令），批量拒绝另带 `risk_level`；审计页 commandSummary 已优先读 `command` 键，前端零改动。E2E 断言拒绝行的 `detail.command` 与表格可读。
- **批量详情已执行行看不出改写（medium）**：用户在批量审批行把命令改为 uptime 并批准后，抽屉只显示 job 原文命令。`BatchResult` 增 `edited_command`（openapi 必填；store 查询经 `approval_id` LEFT JOIN `approvals.edited_command` 一次读出，50 台 job 不逐行回取审批），`BatchDetailDrawer` 新增「命令」列——改写行显示「实际执行 <命令>」+「用户改写」Tag + 复制，未改写行回落 job 原文。补 `batchView.test.ts` 4 项、store 集成 `TestBatchResultsCarryEditedCommand` 与 E2E。
- **回放审批来源直出英文（low）**：`ReplayViewer` 审批步头部 `{step.source}` 直出 chat/console/batch，是全站唯一中英混排。`replayPlayer.ts` 增 `approvalSourceLabel`（对话/控制台/批量，未知原样、空值「—」），单测断言映射不含 ASCII 字母。
- **改写后工具卡仍显示模型原命令且标红（low）**：`ToolCallCard` 只读 `arguments.command`（契约 2.11 约定 3 有意保留原参数），改写后首屏仍是「rm -rf …」+ 红字高风险。抽 `toolCommandPanel.ts`：关联审批 `edited_command`（或工具结果 `result.command`）与模型参数不同则标题改「模型原命令」/「模型原 SQL」、其下补「实际执行 <命令>」+「用户改写」Tag，红色高风险标注随之取消；`arguments` 原值保留（审计事实）。补 7 项单测与 E2E。
- **门限文案里露出数据库字段名（low）**：`approvalMode.ts` 的 none 档说明与 `SettingsPage` 的免审批警示把 `auto_exec_enabled=false` 摆进中文句子。两处同步改为「关闭了「自动执行」开关的服务器仍会拦截非只读命令」，字段名只留代码注释；单测断言四档说明不含 snake_case 与 `=`。
- **顶栏会话标题无 tooltip（low）**：`ChatPage` 顶栏标题超 260px 即省略号截断且无 title（侧栏同名条目有）。补原生 `title`（与侧栏同口径）；顺带修 `.chat-topbar-title` 的 `max-width` 对 inline 元素不生效（补 `display:inline-block`）——此前截断实际未生效（实测宽度 586px）。
- **/chat 缺页头、三方名称不一致（low）**：13 个有 PageHeader 的页面 nav/页头/title 全一致，只有 /chat 无页头、其 h3 是空态标语「开始新的运维对话」。顶栏补视觉弱化的 `h3`「对话」（15px/次级色）+ 分隔符，会话标题紧随其后；E2E 断言 nav / `getByRole('heading', '对话')` / `document.title` 三者一致，空态页同样成立。

- **门禁复验（全部通过）**：`gofmt -l .` 无输出；`go vet ./...` 退出码 0；`go test -count=1 ./...`（TEST_DATABASE_URL 指向本地 65432）27 包 ok；覆盖率 **69.8%**；`go run ./cmd/smoke` `SMOKE_OK`（S02 p95 22.68ms）；前端 typecheck / lint（`--max-warnings=0`）/ 单测 **78 文件 1299 项** / build + 体积门禁 **124,350 B ≈ 121.4 KiB PASS**；E2E **197 passed（11.0m，48 spec）**；四目标 fuzz 各 10s 无崩溃（FuzzClassify 2,357,976 / FuzzWinClassify 1,538,062 / FuzzNetClassify 244,911 / FuzzCheck 164,026 次执行）；`loadgen -mode=selftest` **PASS**（chats=877 / approvals=879 / 5xx=0 / sse p95=267ms / RSS +2.1%）；真 guacd 集成 `ok`（2.015s）；部署验证见 README 8.1。
- **文档**：CONTRACT 9.7 补 `command.approved` 真实写入点与 `command.preview` 无写入点说明、4.26 补 `BatchResult.EditedCommand` 的关联读法；openapi `BatchResult.edited_command`（必填）与描述；README 8.1 门禁表换本轮实测、8.8 增六期 sprint 5 表；CHANGELOG 本条目。

## 2026-11-08 — 六期打磨 sprint 4（7 项走查修复）

第 4 轮走查采纳全部 7 项（1 high / 3 medium / 3 low），每项均补后端单测或 E2E，七层门禁复验全绿后提交：

- **私钥认证未填 passphrase 时三条自动化路径全灭（high）**：`monitor`/`inspection`/`diagnosis` 三个同名 `decrypt` 辅助补空串短路（`if ciphertext == "" { return "", nil }`），与 `agent.Runner.decrypt`、`batchx.Service.decrypt` 早已存在的口径对齐。`passphrase` 是私钥认证的**可选**字段（openapi 4.3），未填时库里存空串，而 `cryptox.Decrypt("")` 返回 `ErrInvalidCiphertext`——此前被当成「解密失败」：私钥认证服务器的巡检 0 分且错误列直出 `inspection: decrypt passphrase failed`、采集持续 `monitor: no executor for server auth method` 5 次后退避暂停并生成「采集不可达」、AI 诊断整条失败。非空但非法的密文仍报错、cipher 未注入仍报错（安全语义不削弱）。补三包各一条 `Test*EmptyPassphraseShortCircuits`（空口令成功 + 非法密文失败 + nil cipher 仍失败），前端 `describeInspectionError` 另补 `decrypt … failed` → 「凭据解密失败（请重新录入该服务器的登录凭据）」兜底（包名前缀随调用方变化）。
- **审计页缺 18 个动作与 7 个资源类型（medium）**：`meta.ts` 的 `AUDIT_ACTIONS` 补齐 `settings.update`（六期）与五期全部 `alert_rule.create/update/delete`、`alert.triggered/resolved/notified`、`notification.create/update/delete/test`、`diagnosis.started/completed`、`inspection.started/completed`、`metric.collect`、`batch.created/cancelled`；`RESOURCE_TYPE_OPTIONS` 补 `monitor`/`alert_rule`/`alert_event`/`notification_channel`/`diagnosis_session`/`inspection_report`/`batch_job`。此前这些动作在「动作」列回落英文原码、筛选下拉搜不到（走查实测输入「设置」「批量」均 0 项），同类资源类型在「目标」列也回落原码。补 4 条单测（清单覆盖、中文关键词命中、资源类型映射）与 1 条 E2E（真实 PUT /api/settings 产审计后筛选「设置变更」）。
- **折叠面板内复制按钮连带开合（medium）**：回放输出面板（`ReplayViewer`）、批量详情每台输出（`BatchDetailDrawer`）、巡检报告原始输出（`ReportDrawer`）三处 `CopyButton` 外层包 `<span onClick/onKeyDown={stopPropagation}>`——按钮在 antd `Collapse` 的 label 区内，点击冒泡到 header 的 `role=button` 处理器，用户点一次「复制输出」面板就自行展开/收起。E2E 连点 3 次断言展开态不变、且面板本身仍可正常开合。
- **小视口侧栏末项被折叠条压住（medium）**：`AppShell` 的 Sider 内菜单外再包一层 `paddingBottom: SIDER_TRIGGER_HEIGHT(53)`。`.ant-layout-sider-trigger` 是 `position: fixed; bottom: 0`（antd `triggerHeight = controlHeightLG 45 + marginXXS×2 = 53`），`overflow: auto` 的滚动区不算它，1366×768 下「设置」被压住 26px、1280×720 下「批量」「设置」都被吞——点文字中部命中的是折叠条，侧栏被折叠而不是跳转。E2E 在两档视口下滚到底后做 `elementFromPoint` 命中测试 + 真实点击断言跳转 `/settings` 且未折叠。
- **审批卡过期措辞与 Tag 不一致（low）**：抽出 `components/approvalStatus.ts` 的 `APPROVAL_STATUS_LABELS`（`expired: '已过期'`），`ApprovalCard` 正文与 `StatusTag` / 回放侧 `approvalStatusLabel` 三者同源——此前只有 ApprovalCard 写「已超时」，同一屏出现「已过期」Tag + 「审批已超时」正文。补单测（表内容 + 「不得含已超时」护栏）与 E2E（TTL 到期后只出现「已过期」）。
- **「采集不可达」告警全英文（low）**：collector 的 `fireSuspendAlert` 消息改中文「连续 5 次指标采集失败，采集已暂停（10 分钟后自动重试）」——此前 `metric collection failed 5 consecutive times; collection paused` 是全站中文 UI 里唯一直出的英文业务文案（告警页「消息」列逐字显示）。规则名保持英文数据库键，前端新增 `ruleForm.ruleDisplayName` 映射「采集不可达（系统）」（规则行 + 事件行，原始名保留在 Tooltip / 副行，删除确认走展示名）。补后端 `TestSuspendAlertMessageIsChinese` 与前端 `ruleDisplayName` 三条单测。
- **回放截断横幅把「审批满 200 条」说成「超出 20000 步上限」（low）**：后端 `ReplayResponse` 增 `truncated_reason`（`''`/`steps`/`approvals`，抽 `replayApplyTruncation` 统一两道上限并让同时命中以 `steps` 为准），openapi 与 CONTRACT 2.11 约定 4 / 8.7 同步；前端按原因二选一——`approvals` 显示「审批记录过多，仅显示最近 N 步（更早的审批未纳入回放）」。此前给只有 200 步的会话显示 20000 上限，自相矛盾。补 `TestReplayApplyTruncationReasons`（四态）、`TestConversationReplayTruncatedReasonApprovals`（真实 handler，200 条审批）与两条 E2E。
- **新增 e2e** `web/e2e/polish-p6sprint4.spec.ts`（7 条）：审计动作可筛选且中文、折叠面板复制不连带开合、1366×768/1280×720 侧栏末项可点击、审批卡过期措辞、回放截断文案两态。
- **门禁复验（全部通过）**：`gofmt -l .` 无输出；`go vet ./...` 退出码 0；`go test -count=1 ./...`（TEST_DATABASE_URL 指向本地 65432）27 包 ok；覆盖率 **69.7%**；`go run ./cmd/smoke` `SMOKE_OK`（S02 p95 22.14ms）；前端 typecheck / lint（`--max-warnings=0`）/ 单测 **76 文件 1283 项** / build + 体积门禁 **124,363 B ≈ 121.4 KiB PASS**；E2E **190 passed（10.4m，47 spec）**；四目标 fuzz 各 10s 无崩溃；`loadgen -mode=selftest` **PASS**（chats=800/approvals=800/5xx=0/sse p95 285ms/RSS +8.0%）；真 guacd 集成 `ok`（2.027s）；部署验证镜像构建 + 容器 Healthy + `/healthz` `database=ok`。
- **文档**：CONTRACT 2.11 约定 4 补 `truncated_reason`、4.3 补「空密文必须短路为空串」、4.19 哨兵规则段补事件消息中文与展示名映射、8.7 回放截断文案按原因二选一；openapi `ReplayResponse.truncated_reason`（含 enum）与 replay 端点描述；README 8.1 门禁表换本轮实测、8.8 增六期 sprint 4 表；CHANGELOG 本条目。

## 2026-11-08 — 六期打磨 sprint 3（7 项走查修复，评分 83/100）

第 3 轮走查采纳全部 7 项（2 high / 3 medium / 2 low），每项均补后端单测或 E2E，七层门禁复验全绿后提交：

- **批量直执行风险级别写死 safe（high）**：`batchx.Service.runDirect` 增 `riskLevel risk.Level` 形参，由 `Dispatch` 的 `decision.Level` 透传；`CreateResult` 与 `command.executed` 审计的 `risk_level` 均改为该分类快照。此前两处都写死字面量 `"safe"`——`none` 档 + `auto_exec_enabled=true` 时 `rm -rf /tmp/aiops-riskcol`（RM_ANY，needs_approval）免审批直执行，批量详情里却被标成「安全」，落库与审计同错（与同一函数审批台的 `string(decision.Level)` 口径不一致）。补 `TestBatchDirectExecutionCarriesClassifiedRiskLevel`（result 与审计的 risk_level 均为 needs_approval、rule_id=RM_ANY、审计带 approval_mode）。
- **审批取消后 tool_call 永久停在 awaiting_approval（high）**：新增 `Runner.cancelApprovalAndConverge` / `convergeToolCallStatus` / `convergeResolvedToolCall`。此前 `runSSHExec` / `runContainerAction` 在 `approval_required` 事件发不出去、`Wait` 异常终止、`approval_resolved` 发不出去三条分支上只取消审批（`expired` + `note=stream_cancelled`）就 `return`，`tool_call` 停在中途态——历史工具卡常驻「待审批」，刷新/回放与同会话审批卡的「已取消」自相矛盾（全仓此前只有 batchx 有过期收敛循环）。现在取消审批的同时把 tool_call 收敛为 `expired` 并落新错误码 `APPROVAL_CANCELLED`（`approval_resolved` 分支用 `CLIENT_DISCONNECTED`，因为审批本身已正常决策）；收敛前先 `ByID` 读一次状态，不覆盖其他路径写入的终态，落库用 `WithoutCancel` + 5s 超时。新错误码已同步 `internal/apierr/codes.go`、openapi `ErrorCode` 枚举、CONTRACT 9.6 与前后端中文映射（`useChatStream` 与 `apiError` 各一条文案）。补 5 条 agent 单测，含 ctx 取消（Fatal 主路径）与「TTL 真超时仍为 APPROVAL_TIMEOUT」回归护栏。
- **批量 command.executed 审计不含实际命令（medium）**：`runDirect` 与 `OnApprovalDecided` 的 detail 均增 `command`（后者取 `edited_command` 非空时的实际执行值）。此前批量直执行审计的键集恒为 `[approval_mode,batch_job_id,exit_code,risk_level,rule_id]`，审计页按「命令执行」筛选后每行「命令摘要」全是「—」，免审批档事后无从复原跑了什么（契约 9.7 明确要求携带实际执行命令文本）。前端 `commandSummary` 本就优先读 `command` 键，无需改动。
- **回放把「流断开被取消」显示成「已超时」（medium）**：`approvalStatusLabel('expired')` 改中性文案「已过期」。approvals 的 expired 有两种来源——TTL 到时与流断开/服务关闭时的 `Cancel`（note=stream_cancelled，对话卡显示「已取消」），而 `ReplayStep` 没有 note 字段，回放侧说成「超时」会与同会话对话卡直接矛盾。补单测含「文案不得含『超时』」护栏。
- **容器操作/诊断修复/批量详情三个审批入口无「编辑命令」（medium）**：抽出 `components/ApprovalCommandEditor.tsx`（复用 `approvalEdit` 的校验/`buildDecideBody`/`preview-risk` 实时预览/空草稿拦截/forbidden 禁提交，与对话审批卡同源），接入 `ContainerActionModal`、`DiagnosisDrawer`、`BatchDetailDrawer`（后者新增待审批行「编辑」按钮 + 单条审批查询键 `qk.approval` + 编辑弹窗）。三处 decide mutation 均支持 `edited_command`；无 `server_id` 的审批按契约 2.11 约定 3 显示「不支持编辑命令」而不提交。补 `approvalEdit.test.ts` 4 项跨入口断言与批量真实链路 e2e（断言改写后的命令确实送达通道、审批 `edited_command=whoami` 且 `command=uptime` 不变）。
- **审批门限选择器无可访问名（low）**：`ApprovalModeSelect` 增 `ariaLabel`（默认「审批门限」），`ChatPage` 传「会话审批门限」、`SettingsPage` 传「审批门限」。此前 antd Select 的 `aria-label`/`aria-labelledby` 全为 null，屏幕阅读器与键盘用户无法得知该下拉是什么。
- **回放 execution 步空命令块（low）**：`ReplayViewer` 仅在 `step.command` 非空时渲染「命令」代码块，否则显示「命令未知（通道失败前未下发）」并保留 `error_code` 说明。此前直接渲染 `step.command ?? ''`，通道在写 result 之前失败（后端 `replayExecutionStep` 的 failed 分支不设 command）时出现空白代码块 + 禁用状态的复制按钮。
- **测试夹具**：`cmd/e2esshd` 记录 `exec` 请求的命令文本（`recorder.execs` + `GET /status`），但**刻意不实现执行**——该夹具语义只服务终端 PTY，且监控采集器也经 sshx 打同一端口，`exec` 一旦成功会污染既有「采集失败」类 e2e 断言。新增 `TestFakeSshdExecRecordsCommand`。
- **新增 e2e** `web/e2e/polish-p6sprint3.spec.ts`（5 条）：回放审批取消态中性文案、无 command 的失败执行步、会话/全局门限 combobox 可访问名、批量详情编辑命令并批准（真实链路）。
- **门禁复验（全部通过）**：`gofmt -l .` 无输出；`go vet ./...` 退出码 0；`go test -count=1 ./...`（TEST_DATABASE_URL 指向本地 65432）27 包 ok；覆盖率 **69.6%**；`go run ./cmd/smoke` `SMOKE_OK`（S02 p95 22.70ms）；前端 typecheck / lint（`--max-warnings=0`）/ 单测 **76 文件 1273 项** / build + 体积门禁 **124,349 B ≈ 121.4 KiB PASS**；E2E **183 passed（9.5m，46 spec）**；四目标 fuzz 各 10s 无崩溃；`loadgen -mode=selftest` **PASS**（chats=796/approvals=800/5xx=0/sse p95 293ms/RSS +4.3%）；真 guacd 集成 `ok`（2.029s）；部署验证镜像构建 + 容器 Healthy + `/healthz` `database=ok`。
- **文档**：CONTRACT 6.3 补「同一次取消必须收敛 tool_call」、9.6 增 `APPROVAL_CANCELLED` 行、9.7 保持 command 口径；README 8.1 门禁表换本轮实测、8.8 增六期 sprint 3 表；CHANGELOG 本条目。

## 2026-11-07 — 六期打磨 sprint 2（8 项走查修复，评分 77/100）

第 2 轮走查采纳全部 8 项（1 high / 5 medium / 2 low），每项均补后端单测或 E2E，七层门禁复验全绿后提交：

- **审批编辑态空命令仍可批准执行（high）**：`validateEditedCommand` 新增「空串/纯空白」分支（返回「命令不能为空，如需放弃修改请点「取消编辑」」），`canSubmitEditedCommand` 因此对空草稿返回 false，「保存并批准」按钮禁用；`ApprovalCard.handleApprove` 再加一层兜底（空草稿直接 return，键盘/程序化触发同样不提交）。此前空串经 `buildDecideBody` 的「未改写」分支提交纯 `approve`，服务端按**原命令**批准并真实执行——走查实测清空 `env` 后点按钮仍执行了 `env`，泄漏 `SSH_CONNECTION`/`PATH` 等环境变量。单测覆盖空串/纯空白/`\t\r\n` 五种草稿。
- **采集不可达系统告警永远创建失败（high）**：`monitor` 的哨兵规则从 `cpu_pct + threshold=101` 改为 `load_1 + 1e12`（新增 `suspendRuleMetric`/`suspendRuleThreshold` 常量）。旧值必然撞迁移 `00007_phase5.sql` 的 `alert_rules_check`（cpu/mem/disk 的 threshold≤100），被 `mapError` 映射为 `store.ErrInvalid`，「采集不可达」事件从未落库、连续采集失败对用户完全静默（全量 e2e 日志 10 次 `monitor_suspend_rule_failed {"error":"store: invalid"}`、库内 `alert_events` 0 条）。规则 `enabled=false` 不参与评估，换指标不影响告警语义。新增 `TestCollectorSuspendRuleUsesLoadMetric`（指标非百分比、阈值 >100、enabled=false、事件与 `alert.triggered` 审计真实落库、ID 缓存复用）与 store 集成断言（同名哨兵规则可创建并清理）。另修同一路径的第二处静默隐患：`suspendRule` 在缓存 ID 失效（用户从 `/alerts` 删除系统规则）时清缓存重建一次——此前只做 `Get` 失败即 `return`，规则一旦被删告警永久不再产生；`TestCollectorSuspendRuleRecreatedAfterDelete` 覆盖「删除→重建→事件落库」。
- **回放免审批直执行仍显示「需审批」（medium）**：openapi `ReplayStep` 增可空 `approval_mode`，`replayStep` 结构体与 `buildReplaySteps` 从 `tool_calls.approval_mode` 填充；前端 `replayPlayer.isReplayExemptExecution` 复用 `toolCallBadges.isExemptExecution`，`approvedToolCallIds` 从 approval 步收集「已走审批」的 tool_call_id 传入（有审批步则照旧显示「需审批」）。none/high 档直执行的调用在回放里显示「高风险 · 免审批执行」，与聊天工具卡一致。
- **回放 `failed` 直显英文（medium）**：`toolCallStatusLabel` 增 `case 'failed': return '失败'`——此前 failed/denied/expired 一起交给 `approvalStatusLabel`，而后者没有 failed 分支、走 default 原样返回英文 `failed`（回放 UI 唯一的中英混排）。顺带补 `awaiting_user` → 「待用户回答」。单测断言全部状态文案不含 ASCII 字母。
- **回放失败调用无 error_code / 无 execution 步（medium）**：`buildReplaySteps` 的 tool_call 步填 `tc.ErrorCode`（openapi 早已声明但此前恒 null）；`replayExecutionStep` 对「无 result 且 `status=failed`」的命令执行类调用也生成 execution 步（`status=failed`、`error_code` 取自 tool_calls、`exit_code=-1`、`output` 空串——runner 在 SSH 失败时于写 result 之前早退）。denied/expired 等「命令从未执行」仍不产生 execution 步。补 `TestConversationReplayToolCallFailureFields`（失败调用 / 已拒绝调用两态，按 kind+tool_call_id 定位，不依赖下标）。
- **回放展示内部上下文前缀（medium）**：后端 `stripReplayContextPrefix` 在 `buildReplaySteps` 剥离 ChatPage 的 `<!--opsmind-context … -->` 前缀（锚定正则，普通 HTML 注释不误剥），前端 `ReplayViewer` 的 user_message 再经 `stripContextPrefix` 兜底（旧缓存/旧后端响应同样干净）。补 `TestReplayUserMessageStripsContextPrefix` 三例。
- **两处手写复制未走 CopyButton（low）**：`MessageItem` 的 `BubbleCopyButton`（`copyText` + 局部 toast）与 `ContainerLogsDrawer` 的文本「复制」按钮（+「日志已复制」toast）替换为统一 `CopyButton`（`aria-label="复制消息内容"` / `"复制日志"`），补齐两级剪贴板降级面板与 1.5s 冷却；删除两处 `handleCopy`。既有 `containers.spec.ts` 的断言同步改为 `复制日志` + 「已复制」（此前抽屉内 `button[aria-label*="复制"]` 数为 0）。
- **单步会话进度条 NaN（low）**：`replayProgress` 增 `if (total <= 1) return 0;`——`index/(total-1)` 在 total=1 时是 0/0，antd Slider 收到 NaN 后不写 `aria-valuenow`、轨道宽度恒 0%。补 `(0,1)/(1,1)/(5,1)/(-1,1)` 单测与 e2e（单步会话 handle 的 `aria-valuenow` 非 NaN）。
- **新增 e2e** `web/e2e/polish-p6sprint2.spec.ts`（6 条）：空命令不可保存并批准且审批保持 pending、失败调用中文「失败」+ error_code + execution 步、上下文前缀不展示、免审批徽标、单步进度条、两处统一 CopyButton。
- **文档**：CONTRACT 2.11 约定 3 补「空草稿不得提交」、约定 4 补回放载荷四项补齐；4.19 alert_rules 补「系统哨兵规则」段（禁改 pct 指标的原因）；8.2 CopyButton 行与 8.7 复制清单补 ⑫ 消息气泡 / ⑬ 容器日志抽屉；8.7 回放播放器补单步进度与中文状态。README 8.8 增六期 sprint 2 表；CHANGELOG 本条目。

## 2026-11-07 — 六期打磨 sprint 1（8 项走查修复 + 两条新迁移）

六期交互深化（门限四档/命令改写/执行回放/全站复制/深度思考）合入后第 1 轮走查，采纳全部 8 项（1 high / 5 medium / 2 low，评分 68/100），每项均补单测或 E2E，七层门禁复验全绿后提交：

- **对话内批量忽略门限（high）**：`batchx.Service.Dispatch` 的 chat 分支此前把兜底值 `store.ApprovalModeAll` 当作「全局门限」传给 `approvals.ResolveMode`——会话无覆盖时有效门限恒为 all，全局 standard/none 下 chat 批量仍逐台造审批（实机：同一台 auto_exec=true 的 host 上 `uptime` 在 chat 路径 `awaiting_approval`、console 路径 `executed`，审批快照 `approval_mode=all` 而全局是 standard）。改为先 `approvals.GlobalMode` 取全局、失败则失败关闭为 all，再用 `ResolveMode(conv, global)` 叠加会话覆盖；补 `Test2_11ChatBatchHonoursGlobalMode` 三档矩阵单测（standard/none 直执行、会话覆盖 all 优先），并给 batchx 测试 fixture 加 `Conversations()` 假实现。
- **回放 reasoning 步无耗时**：`ReplayStep` 增可空 `reasoning_duration_ms`（openapi + `replayStep` 结构体），`buildReplaySteps` 从 `messages.reasoning_duration_ms` 填充，`ReplayViewer` 透传 `ReasoningBlock.durationMs`——回放摘要不再恒「深度思考 · 耗时 —」；`replay_test` 断言 reasoning 步带 1000ms、非 reasoning 步为 null。
- **回放 execution 输出未折叠**：输出从平铺 `CodeBlock` 改为 antd `Collapse`（默认收起、标题「输出」+ 统一 `CopyButton` 复制输出），展开态仍 `content-visibility:auto`（11 节防 OOM）；e2e 断言折叠态隐藏输出、展开可见、复制按钮存在。
- **复制清单 4 处未落位**：① 巡检报告新增「命令」列 + 复制按钮——新列 `inspection_report_items.command`（迁移 00011，`CHECK length<=4096`），runner 在 `inspectOne` 落模板展开后的命令清单；② 服务器编辑弹窗顶部补服务器 ID 与 `host:port` 复制；③ 数据库编辑弹窗补 ID 与连接信息复制（组合串从 `DatabasesPage` 抽到 `databaseView.databaseConnectionString`，两处逐字一致）；④ 模型选择器 `label` 改为 ReactNode（模型名 + 复制按钮；antd AutoComplete 不支持 `optionRender`，实测不生效）并补当前值复制；⑤ 终端「复制当前命令」改常显（此前仅 `!ended` 渲染，会话建立失败/已结束时整个消失）；e2e 三条新用例覆盖弹窗只读态、模型下拉与真实终端会话。
- **导航/页头/标签页三方不一致**：`/rdp` 页头「RDP 桌面」→「桌面」、`/batch`「批量操作」→「批量」（与 nav label 和 `pageTitle` 对齐）；补 `pageTitleNavAlign.test.ts` 与逐页 e2e（nav 选中项、PageHeader h3、document.title 三者逐字相同），同步修正 6 个既有 spec 的 heading 断言。
- **工具卡门限徽标刷新即消失**：新列 `tool_calls.approval_mode`（迁移 00010，可空 + 四档 CHECK），agent 在创建 tool_calls 行时对命令执行类工具落门限快照（历史行为 NULL，前端空串不渲染）；openapi `ToolCall.approval_mode` + `toToolCallView` 映射；补 store 往返集成断言与前端映射单测。
- **审计缺实际执行命令**：`command.executed` 的 detail 增 `command`（实际执行、改写后文本），与 `original_command` 互补——免审批档事后可从审计复原「跑的是什么」；`runner_phase6_test` 补断言。
- **免审批档徽标语义冲突**：新增 `features/chat/toolCallBadges.ts`（纯函数 + 12 项单测）：none/high 档 + needs_approval + 未建审批 + 非等待态 → 隐藏「需审批」RiskBadge、「高风险」Tag 改「高风险 · 免审批执行」；forbidden 与已建审批场景保持原样。
- **门禁复验（全部通过）**：`gofmt -l .` 无输出（顺带修复 HEAD 上既有的 `diagnosis_handlers.go` 对齐）；`go vet ./...` 退出码 0；`go test -count=1 ./...`（TEST_DATABASE_URL 指向本地 65432）27 包 ok；覆盖率 **69.5%**；`go run ./cmd/smoke` `SMOKE_OK`；前端 typecheck / lint（`--max-warnings=0`）/ 单测 **76 文件 1249 项** / build + 体积门禁 **124,238 B ≈ 121.3 KiB PASS**；E2E **172 passed（8.4m，44 spec）**；四目标 fuzz 各 10s 无崩溃；`loadgen -mode=selftest` **PASS**（chats=807/approvals=811/5xx=0/sse p95 281ms/RSS +4.2%）。
- **文档**：CONTRACT 4.8/4.24 表补两列、迁移清单补 00008–00011、2.11 约定 4 补 reasoning 耗时、8.7 回放折叠与复制清单落位口径、9.7 审计 `command` 字段；README 8.1 门禁表换本轮实测、8.8 增六期 sprint 1 表、8.9 数字更新；CHANGELOG 本条目。

## 2026-10-28 — 六期交付：审批门限、命令改写、执行回放、全站复制与深度思考（主体实现；6 轮冲刺见 2026-11-07/08 各条，终版复验数字取自 sprint 6）

六期**交互深化**（契约先行 `c753f93`——CONTRACT 2.11 五条执行模型约定 + openapi + 迁移 `00008_phase6.sql`；实现分三批：`a0d395a` 审批卡命令编辑/门限选择器/深度思考组件 → `32a76b7` 门限四档/命令改写/回放后端 → `9307a8c` 回放播放器与全站复制，`5085432` 补契约遗留；打磨经 6 轮冲刺 `a9cd6f3` → `34e0511`，每轮门禁复验全绿后收轮，**有明确评分的三轮为 68 → 77 → 83，单轮最高 83/100，未达 100**；第 4–6 轮工作记录未给出总分，如实不臆造）。

- **审批门限四档**：`internal/approvals/policy.go` 是唯一策略实现——`NeedsApproval({mode, auto_exec_enabled, level, severity})` 六行写死矩阵 + `EffectiveMode`（会话覆盖 > 全局）。`all`（全审批）/`standard`（一至五期现行行为，全局缺省）/`high`（warning 级免审）/`none`（全自动执行仍落审计）。落库：`settings["approval.mode"]`（00008 幂等插入 `"standard"`）、`conversations.approval_mode`（可空=跟随全局）、`approvals.approval_mode`（创建时快照）。**服务器级硬保护**：`auto_exec_enabled=false` 时非只读命令任何档位都审批。五条执行路径全部改走本函数：对话 `ssh_exec`、`container_action` 工具、`batchx` 逐台、REST 容器 action（四期「控制台恒审批」裁决被取代，响应改 `ContainerActionResponse` wrapper）、诊断 fix（五期「修复无免审批路径」在 none/high+auto_exec 放宽为直执行）。新增 `GET/PUT /api/settings`（公开键白名单 + `settings.update` 审计）、`PATCH /api/conversations/:id` 兼 `approval_mode`、`/settings` 页与会话头部选择器。
- **审批时命令改写**：`POST /api/approvals/:id/decide` 增可选 `edited_command`（仅 approve 合法、1..2000 字符、拒 NUL、`server_id` 为空不允许改写）。服务端按目标分册**重新分类**（网络设备 `ClassifyNet`），改写后 forbidden → 403 `COMMAND_FORBIDDEN` 且审批保持 pending；否则 `edited_*` 与决策状态同一 UPDATE 落库，原 `command/risk_level/rule_id/reason` 不动。执行使用改写后命令（chat 路径 runner / console+batch 路径 decide handler），SSE `approval_resolved` 携带 `edited_command`。**审计双记录**：`approval.approved` 与 `command.approved` 的 detail 带 `original_command`/`edited_command`/双风险级别；`command.executed` 带实际执行 `command` 与改写时 `original_command`。前端抽 `ApprovalCommandEditor` 供对话审批卡、容器弹窗、诊断修复抽屉、批量详情四处复用（编辑态实时 `preview-risk`；**空草稿禁用保存**——此前空串被当「未改写」按原命令执行）。
- **会话执行回放**：`GET /api/conversations/:id/replay` 从 `messages`/`tool_calls`/`approvals` 重建（不建表），稳定排序写死 `(timestamp, kind 权重, 来源键)` 后重编 `seq=1..N`；step 全集含 user_message/assistant_text/reasoning/tool_call/approval/execution，execution 仅 `ssh_exec`/`container_action`，从 result JSON 取 exit_code/output/duration_ms；失败且无 result 的通道失败也生成 `failed` execution 步。**播放器**（`ReplayViewer.tsx`）kind 分色、播放/暂停/上一步/下一步/进度条/1x·2x·4x（800/400/200ms）、自动播放高亮滚动；`user_message` 剥内部上下文前缀、`assistant_text` 走 Markdown、`reasoning` 复用 `ReasoningBlock`（带服务端耗时）、`approval` 展示来源与「用户改写」双行命令、`execution` 输出默认折叠可复制。两个入口：对话页头部「回放」（流式中禁用）与审计页展开行「回放」。截断两因分报：步数 20000（`steps`）/ 审批满 200 条（`approvals`），响应带 `truncated_reason`。
- **全站复制**：`CopyButton` 成为复制清单唯一实现——`navigator.clipboard` → `execCommand` 两级降级 → 可选中文本面板（自动全选）+ toast 报错；成功切 √ + 1.5s 冷却。落位覆盖契约 8.7 清单 ①–⑬：服务器（主机/`host:port`/ID，含编辑弹窗只读态）、数据库（连接串/ID）、模型（Base URL/模型名含下拉选项）、审计（detail 逐值）、容器（镜像/名/ID + 日志抽屉整段）、监控（指标当前值）、告警（规则/事件 ID）、巡检报告（命令列，新列 `inspection_report_items.command` 迁移 00011）、批量详情（每台命令与输出）、终端（当前输入行常显）、对话（消息气泡/工具卡/代码块/审批命令/思考全文）、回放（命令/输出）。`rg -n "<CopyButton" web/src` 实测 40 处调用点、20 个源文件。
- **深度思考**：供应商侧解析 `reasoning_content`（OpenAI 兼容）/ `thinking`（Anthropic）为 `reasoning` SSE 事件；`ReasoningBlock` 默认收起「深度思考 · 耗时 Ns」、流式中自动展开（首个 reasoning delta）与回折叠（首个正文 token）、展开可复制全文、折叠用 `display:none` 不丢内容。耗时：流式中前端测量，刷新后取服务端 `messages.reasoning_duration_ms`（迁移 00009，`agent` 在 reasoning 首/尾 delta 打点，与 assistant 消息同一条 INSERT 落库）；`0` → 「耗时 0s」、`null` → 「耗时 —」。`reasoning` 与正文同为持久化字段并作为独立步骤进入回放；不回灌给模型（`buildMessages` 只发 content 与工具结果）。
- **六期冲刺（走查评分，最高 83/100）**：sprint 1 `a9cd6f3`（68/100，对话批量取全局门限 / 回放 reasoning 耗时 / 输出折叠 / 复制清单 4 处 / 三方标题 / 门限徽标持久化 00010 / 审计实际执行命令 / 免审批徽标）；sprint 2 `d42ded0`（77/100，空命令审批拦截 / 采集失联哨兵规则 / 回放免审批标注 / 回放中文化与失败步 / 剥离上下文前缀 / 手写复制收敛 / 单步进度 NaN）；sprint 3 `24f867d`（83/100，批量直执行分类快照 / 取消审批收敛 tool_call / 批量审计命令 / 回放「已过期」/ 三入口补编辑 / 可访问名 / 失败执行步空块）；sprint 4 `d3f577b`（1 high 3 medium 3 low，私钥空口令短路 / 审计动作补全 / 折叠内复制不连带开合 / 侧栏末项 / 过期措辞 / 告警中文化 / 截断横幅）；sprint 5 `c72547a`（5 medium 3 low，`command.approved` 落地 / 命令摘要 / 批量改写列 / 回放来源中文 / 工具卡改写展示 / 门限文案 / 标题 tooltip / chat 页头）；sprint 6 `34e0511`（1 high 2 medium 5 low，网络速率去 8 倍 / 容器弹窗门限文案 / 回放 Markdown / 拒绝审计 / TTL 文案 / 直执行 `approval_mode` / 巡检色阶 / 过期逐条审计）。
- **终版复验（全部通过，2026-11-08 sprint 6 实测）**：`gofmt -l .` 无输出；`go vet ./...` 退出码 0；`go test -count=1 ./...`（TEST_DATABASE_URL 指向本地 65432）**27 包 ok、0 FAIL**；覆盖率 **69.9%**；`go run ./cmd/smoke` `SMOKE_OK`（S02 p95 23.03ms）；前端 typecheck / lint（`--max-warnings=0`）/ 单测 **79 文件 1305 项** / build + 体积门禁 **124,348 B ≈ 121.4 KiB PASS**；E2E **205 passed（11.6m，49 spec）**；四目标 fuzz 各 10s 无崩溃（FuzzClassify 2,779,736 / FuzzWinClassify 1,794,121 / FuzzNetClassify 174,476 / FuzzCheck 201,902）；`loadgen -mode=selftest` **PASS**（chats=811 / approvals=815 / 5xx=0 / sse p95=279ms / RSS +4.4%）；真 guacd 集成 `ok`（2.011s）；部署验证镜像构建 + 容器 Healthy + `/healthz` `database=ok`。
- **本次文档同步（六期终版）**：README 补六期简介、`/settings` 页面、对话页六期交互说明，新增 **4.17 审批门限四档 / 4.18 命令改写 / 4.19 执行回放 / 4.20 全站复制与深度思考** 四小节，更新 4.5/4.12/4.14/5.3/5.4/6.1/6.2/6.3 的六期口径，8.1 门禁表改为「六期终版 + 七期复验」双列，8.4 补六期专项 E2E（7 文件 44 条，本轮实跑 44 passed / 2.3m），8.5 换 2026-11-09 的 selftest/soak 产物，8.7 补六期首屏 121.4 KiB，8.8 增六期 6 轮评分表（68 → 77 → 83，第 4–6 轮无总分如实标注），已知限制新增 39（回放粒度/复制降级/门限 none 风险/思考大小）与 40（六期最高 83/100），8.9 补六期未验证项；`reports/selftest.*` 与 `reports/soak.*` 刷新为 2026-11-09 运行产物。

## 2026-10-27 — 五期交付：监控告警、AI 自诊断、AI 定时巡检与批量操作（含 6 轮冲刺与终版复验）

五期 AI 自动化（契约先行 `e6c358c`——CONTRACT 2.10 六条执行模型约定 + openapi + 迁移 `00007_phase5.sql`；实现 `49255df` 携 M2 诊断+巡检本体与 M3 共享文件改动，M1 监控+M3 批量本体与 M4 前端四页随 sprint 2 `164e4b2` 入库；打磨经 6 轮冲刺 `14c77ff` → `c12e37f`，每轮走查评分驱动、门禁复验全绿后收轮，**单轮最高 96/100，未达 100**）。**核心差异化：四大新能力全部双入口——REST 页面 + 注册为 AI 工具（11→18 个），对话中一句自然语言可直接发起诊断、巡检与批量操作**：

- **监控告警（M1，`internal/monitor`）**：固定模板采集六指标（Linux `/proc` 分段 / Windows 单条 PowerShell JSON；`SAFE_COLLECT`/`WIN_SAFE_COLLECT` 整串精确匹配，不经审批同 `server_facts`）经现有 SSH/WinRM 通道直连执行；进程内环形缓冲 1440 点/台（≤1024 台）+ 批量写库 ≤500 行/批 + `server_metrics_latest` 物化表 UPSERT + 30 天保留每日清理；采集间隔 `settings["monitor.collector"]`（缺省 60s，30..3600）页面可改。告警评估每 30s tick（采集轮后追加）：`metric × gt/lt × threshold × duration_windows(1..30) × cooldown` 六指标规则、部分唯一索引兜底并发、resolved 自动收敛；**防风暴**：冷却期 + 静默（`silent_until` 照常入库不投递）+ 同一 firing 只投一次。**四渠道通知**：webhook（HMAC-SHA256）/飞书（自建应用 webhook+签名）/钉钉（加签 URL）/email（SMTP ≤50 收件人），渠道配置 cryptox 加密落库，投递尽力而为（缓冲 256/worker 2/重试 3 次退避 1s/5s/15s）。连续失败 5 次暂停 10 分钟 + 系统告警事件，恢复自动重采。REST 15 路由（monitor overview、collector GET/PUT、servers/:id/metrics、alert-rules/events CRUD、notification-channels CRUD+test）。
- **AI 自诊断（M2，`internal/diagnosis`）**：**独立受控只读会话**（不落用户对话、不 import agent）——装配上下文（告警+最新指标+facts）→ mini-loop ≤6 轮，每轮模型候选命令过 `risk.Classify` **仅 safe 执行**（needs_approval/forbidden 拒绝并把原因回给模型，代码强制）→ 报告 JSON（原因/证据 ≤20×4KB/建议 ≤10 条/64KB 截断、forbidden 建议过滤）；状态机 `pending→diagnosing→report→awaiting_fix→done|expired|failed`，worker 2 + 队列 100（满 429 `DIAGNOSIS_BUSY`）、会话 300s、awaiting_fix 30m 过期。三入口：页面手动 / critical 告警自动（`monitor.auto_diagnose`，缺省关）/ 对话 `run_diagnosis` 工具（120s 轮询至终态）。**修复无免审批路径**：fix 端点逐字节校验建议命令建 console 审批，对话执行走 `ssh_exec` 完整链。
- **AI 定时巡检（M2，`internal/inspection`）**：五段 cron（UTC，NextRun 逐分钟推进 + 4 年上限有界终止）+ 每分钟 tick 调度（同 job 互斥、全局并发 2）+ `basic`/`full` 固定模板（`SAFE_INSPECT`/`WIN_SAFE_INSPECT` 精确匹配、执行前逐条 Classify 断言）+ 写死评分规则（失败服务/磁盘双档/负载双档/内存/journal/活跃告警逐项扣分）+ `previous_scores` 趋势对比；**巡检不用 LLM**（执行=模板+规则评分；「AI 解读」由对话 `get_inspection_report` 完成）。目标 tags 匹配 ≤200 台、单台 60s/64KB。**cron job 只经 REST 创建**——定时任务变更不交给模型。
- **批量操作（M3，`internal/batchx`）**：`(description 自然语言原文, command, server_ids ≤50)` → **逐台** `risk.Classify`（网络设备按厂商分册）——forbidden 跳过落审计、safe+auto_exec 直执行（池 8）、其余**逐台 `source=batch` 审批**（approvals 扩枚举、command 逐字节）；decide 批准后同步执行并按 `approval_id` 反查回填（exit_code/output/duration），全部终态归并 done/partial。对话 `batch_execute` 工具立即返回分派概要**不等审批**；进度走 `GET /api/batch/jobs/:id` 轮询（前端 2s），不新增 SSE 事件。过期扫描 30s/轮收敛 awaiting_approval 台。
- **前端 M4 四页**：`/monitor`（概要四卡+采集配置+指标趋势）、`/alerts`（规则/事件/渠道三 Tab+诊断抽屉）、`/inspections`（job CRUD+立即巡检+报告抽屉趋势对比）、`/batch`（命令+目标多选+实时风险预览+详情抽屉 per-server 审批/进度）；五期工具卡深链新标签、状态 Tag 与批量页同中文标签。
- 五期冲刺（走查评分，最高 96/100，各轮要点见 README 8.8 五期表）：巡检嵌套 detail 与五期视图出折叠面板（1）、服务入口装配 monitor.Manager 四循环+越限→firing e2e+评估回退查库锁死（2）、诊断 fix 直返 Approval 修首点 500 与 void mutateAsync 全站（3）、批量通道错误码回落通用映射（4）、decide 响应补 exit_code=-1 与 e2e 竞态修复（5）、监控概要补装 last_inspection+规则重名 409 中文（6）。
- **终版复验（全部通过，2026-10-27 文档同步时实际执行）**：
  - 门禁：`go vet` 退出码 0；`gofmt -l` **首跑发现 `internal/httpapi/monitor_handlers.go` 一处 struct 对齐未格式化，`gofmt -w` 修复后复验无输出**（随本文档提交入库）；Go 全量 **27 包 ok**（新增 monitor/diagnosis/inspection/batchx 四包；注：与前端构建并行的一次全量中 `batchx TestMaxTargets` 出现一次时间窗竞态 flaky，单独 `-count=3` 与无并行负载全量重跑均 PASS，未改代码）；覆盖率 **69.4%**（四期 72.1%，五期提交（契约+实现+测试，`060383b..c12e37f`，+34,150/-2,590 行）并入后回落；auth 96.3% / sqlguard 95.2% / approvals 95.0% / risk 89.8% / agent 77.0% / httpapi 72.7%；新包 monitor 66.1% / diagnosis 62.0% / inspection 71.5% / batchx 69.9%）；前端 typecheck / lint（`--max-warnings=0`）/ 单测 **68 文件 1159 项** / build + 体积门禁（123,812 B ≈ **120.9 KiB** PASS）；HTTP 冒烟 `SMOKE_OK`（S02 p95 13.75ms）；E2E **162 passed（9.0m，42 spec）**；真 guacd 集成 PASS（2.0s）。
  - 模糊：四目标各 30s 无失败（FuzzClassify 6,754,392 / FuzzWinClassify 2,789,942 / FuzzNetClassify 225,287 / FuzzCheck 475,358 次执行；与 E2E 并行跑，执行数偏低）。
  - 负载：selftest 重跑 PASS（855 聊天轮、SSE p95 270ms、报告刷新）；浸泡 15m0s / 并发 8 **PASS**（2026-10-27T23:08，10,095 完成聊天轮、SSE p95 390ms、RSS 峰 74MB 增幅 5.5%、0 错误，即当前 `reports/soak.md`）。
  - 部署：`docker compose build app` 成功（`aiops-app:latest` **88.7MB**，较四期 67.8MB 增大约 21MB，来自五期四包与前端四页）；`up -d --wait app` 后 `127.0.0.1:18081/healthz` 200（`database: ok`）、SPA 首页 200。
  - **本提交（文档同步）**：README 五期终版文档（简介五期、界面四页、功能 4.13–4.16 四小节、5.3 补五期模板豁免段、4.5 补五期审计动作、技术栈/项目结构更新、已知限制新增 36/37/38 并更新 3/4/11/18/19/30、第 8 节全部换五期实测数字）；selftest/soak 报告刷新。
  - 已知缺口（如实记录，见 README 已知限制 18/36/38）：**告警的「真实高负载→采集→firing→通知外发」全链路未实测**——评估侧经 E2E 真实链路（直插越限点断言 firing），采集解析为假执行桩单测，四渠道通知未对真实飞书/钉钉/SMTP 外发；**巡检 cron 未做多天期真实墙钟到期验证**（解析与调度约束为注入假时钟单测）；批量/诊断执行段未对真实多机并发实测；真实 Windows 主机 / 网络设备 / SQL Server / Redis / MongoDB / RDP 服务端 / 容器 daemon / 真实 LLM 仍未接入验证（见 README 8.9）。

## 2026-10-27 — 四期交付：容器管理（含 6 轮冲刺与终版复验）

四期容器管理（契约先行 `e0ac009`，实现 C1 后端 `23be434` + C2 前端 `9e3fda2`，打磨经 6 轮冲刺 `f2462d7` → `f67f0ee`，每轮走查评分驱动、门禁复验全绿后收轮，**单轮最高 99/100，未达 100**）：

- **容器是服务器能力（C1）**：不建表不加资产形态；`servers.container_runtime`（''|none|docker|podman|containerd）由 `server_facts`/`POST test`/容器接口在 `''` 时同步探测（15s）写回——探测通道错误不落库（保持 `''` 待重探，评审修订：一次瞬态失败不把服务器钉死 none），探测确认的结论才写回；containerd 只探测展示。执行走现有 SSH/WinRM 通道调 CLI（无 Docker SDK）：新增 `internal/containerx`（零 internal 依赖，`ExecFunc` 注入；docker JSONL 与 podman JSON 数组两套解析分册、`Names` 字符串 vs 数组、镜像 `Size` 人类可读串 1000 进制解析、CID 白名单先验、CLIError/NotFound 语义区分、256KB 截断）。
- **REST 5 路由与 console 审批**：`GET /api/servers/:id/containers[/:cid[/logs]]`、`GET /api/servers/:id/images`、`POST .../containers/:cid/action`——action 不直接执行，创建 `source=console` 审批（`command` 固化为按运行时拼好的逐字节命令如 `docker stop <cid>`，迁移 `00006_phase4.sql` 给 approvals 加 `source` 列并放空 `conversation_id`/`tool_call_id`），批准时 decide handler 同步逐字节执行并返回 `execution`；start/stop/restart 统一走审批流（不做 safe 直通）。审计新增 `container.action`（detail 携带 runtime/container_id/action/exit_code/source）。
- **AI 工具 6→11**：`container_list`/`container_inspect`/`container_logs`/`image_list` 固定只读模板恒 safe 无审批；`container_action` start=safe、stop/restart=needs_approval（`CONTAINER_CHANGE`），`auto_exec_enabled` 语义同 `ssh_exec`；不提供 exec/run 工具。
- **容器风险分册重写（先修规则再产品化）**：`classifyContainer` 按契约 7.3D 重写——`logs`/`compose logs` 由 safe 改 needs_approval（`CONTAINER_LOGS`，日志可能含凭据进入 LLM 上下文）；`-H`/`--context`/`--tls*`/`--remote`/`--url` 全局选项前置检查（旧实现 `docker -H tcp://x ps` 误判 safe）；`container`/`image` 前缀别名与 `nerdctl`/`compose` 归一；`run` 逃逸判定 `CONTAINER_ESCAPE`（`--privileged`/`--pid=host`/宿主敏感挂载 forbidden）与 exec/run shell `-c` 载荷递归取最高；`prune`/`save`/`load`/`cp`/`login` 显式分级；Windows 分册补 `WIN_CONTAINER_*`（此前完全无容器规则）；fuzz 种子补容器样本。
- **前端 C2**：`/containers` 页（服务器选择含运行时徽标、`none`/`containerd` 禁用提示、容器表格 id 截断/state 徽标/ports、镜像 Tab、日志抽屉 tail 100/200/500/1000 缺省 200 + 刷新/复制/截断提示）、操作三阶段弹窗（confirm→审批卡（命令/服务器/风险/原因/倒计时）→decide 执行结果回填 exit_code/duration/error_code）、服务器列表运行时徽标；4 个纯逻辑单测文件。
- 四期冲刺（走查评分，最高 99/100，各轮要点见 README 8.8 四期表）：容器错误码中文映射与横幅语义化（sprint 1）、审计 container.action 标签与审批弹窗 pending 态措辞（2）、探测写回失效 servers 同步徽标（3）、none 重探走 POST test 与 exited 行禁用停止（4）、弹窗命令区三阶段文案（5）、SSH 不可达映射 SSH_EXEC_FAILED 不误报 daemon 未运行（6）；另有 `c55713f` prettier 全量格式化。
- **终版复验（全部通过，2026-10-27）**：
  - 门禁：`go vet`、`gofmt -l`（无输出）、Go 全量 **23 包 ok**（新增 containerx）、覆盖率 **72.1%**（三期 71.5%；sqlguard 95.2% / approvals 95.0% / auth 96.3% / risk 89.8% / agent 85.5% / httpapi 81.9%；新包 containerx 84.9%）；前端 typecheck / lint（`--max-warnings=0`）/ 单测 **64 文件 1108 项** / build + 体积门禁（123,000 B ≈ **120.1 KiB** PASS）；HTTP 冒烟 `SMOKE_OK`（S02 p95 23.6ms）；E2E **130 passed（4.5m，35 spec）**；真 guacd 集成 PASS（2.0s）。
  - 模糊：四目标各 30s 无失败（FuzzClassify 6,468,421 / FuzzWinClassify 2,111,334 / FuzzNetClassify 336,695 / FuzzCheck 513,137 次执行；与 E2E 并行跑，执行数偏低）。
  - 负载：selftest 重跑 PASS（SSE p95 274ms，报告刷新）；浸泡 15m0s / 并发 8 **PASS**（2026-10-27T06:56，10,197 聊天轮、SSE p95 390ms、RSS 峰 68MB 增幅 6.9%、0 错误，即当前 `reports/soak.md`）。
  - 部署：`docker compose build app` 成功（`aiops-app:latest` **67.8MB**，较三期 +0.3MB 来自 containerx 与容器 handler/前端容器页）；`up -d --wait app` 后 `127.0.0.1:18081/healthz` 200（`database: ok`）、SPA 首页 200。
  - **本提交（文档同步）**：README 四期终版文档（功能清单 4.12 容器管理、容器分册摘要入 5.3、技术栈/项目结构更新、已知限制新增 34/35 并更新 3/4/11/18/19/20/30、第 8 节全部换四期实测数字）；**修正三期文档失实**——smoke 实际为 S01–S18（`cmd/smoke` 自一期后未改，二/三期文档所写「S19/S20 路径」不存在，已在 8.2 按代码事实更正）；selftest/soak 报告刷新。
  - 已知缺口（如实记录，见 README 已知限制 18/34/35）：**容器管理未对真实 Docker/Podman 守护进程做端到端验证**——本机装有 Docker 但测试桩不连它（`containerx` 用注入 `ExecFunc` 假桩、E2E 用假服务器与 mock 数据；探测判据的 daemon 行为是调研期对本机 CLI 的实测结论）；真实 Windows 主机 / 网络设备 / SQL Server / Redis / MongoDB / RDP 服务端仍未接入验证（见 README 8.9）。

## 2026-10-27 — 四期契约先行：容器管理（文档/schema/迁移，不含实现）

四期范围定稿（契约 `docs/CONTRACT.md` 2.9/5.2C/7.3D 等 + `api/openapi.yaml` + 迁移 `00006_phase4.sql` + 前端类型再生成；负责人 C1 容器后端 / C2 容器前端）：

- **容器是服务器能力，不是新实体**：不建表不加资产形态；`servers.container_runtime`（''|none|docker|podman|containerd）由 `server_facts`/`POST test`/容器接口探测写回（判据 `docker version --format '{{.Server.Version}}'` daemon 不可达 exit 1、`podman info` 本地 store 即成功——四期调研实测）；containerd 只探测展示，管理面仅 docker/podman。
- **执行走现有通道调 CLI，无 Docker SDK**（调研：`sshx`/`winrmx` 已合并 stdout+stderr 并取 ExitCode；docker `logs` 输出全在 stderr）；新增 `internal/containerx`（零 internal 依赖，`ExecFunc` 注入；docker JSONL 与 podman JSON 数组两套解析分册写死——`Names` 字符串 vs 数组、`CreatedAt` 时间串 vs 人类可读、镜像 `Size` "67.5MB" 1000 进制 vs 整数字节）。
- **AI 工具 6→11**：`container_list`/`container_inspect`/`container_logs`/`image_list` 固定只读模板恒 safe；`container_action`（start=safe、stop/restart=needs_approval）动作集封闭，不提供 exec/run 工具。
- **REST 5 路由**：`GET /api/servers/:id/containers[/:cid[/logs]]`、`GET /api/servers/:id/images`、`POST .../action`（创建 **console 审批**——approvals 加 `source` 列且 `conversation_id`/`tool_call_id` 改可空，批准时 decide handler 逐字节执行固化命令并返回 `execution`；响应 schema 容器 id/name/image/state/status/ports/created、镜像 repo/tag/size_bytes/created）。
- **容器风险分册重写（7.3D，先修规则再产品化）**：修正现有 `classifyContainer` 两处 safe 误放（`docker logs`/`docker compose logs`——日志可能含凭据进入 LLM 上下文，与三期 `show log` 归 `CONFIG_DUMP` 同一裁决）、全局选项绕过（`docker -H tcp://x ps` 曾被选项剥离误判 safe → `CONTAINER_REMOTE`）、`container`/`image` 前缀别名与 nerdctl 归一、`run` 逃逸判定（`--privileged`/宿主敏感挂载 → `CONTAINER_ESCAPE` forbidden）、exec/run shell `-c` 载荷递归；Windows 分册补齐 `WIN_CONTAINER_*` 五行（此前完全无容器规则）。
- **配套**：错误码 `CONTAINER_UNSUPPORTED`/`CONTAINER_NOT_FOUND`/`CONTAINER_CLI_FAILED`（openapi ErrorCode 同步补齐三期遗漏的 `NETCLI_*` 两码）；审计动作仅增 `container.action`；测试 G27–G29/S21；前端 `/containers` 页（C2）+ 服务器列表运行时徽标。

## 2026-10-26/27 — 三期交付：网络设备接入与 MongoDB（含 6 轮冲刺与终版复验）

三期两块能力（契约先行 `1893d92`，实现与打磨经 6 轮冲刺 `f95c947` → `61926c1`——实现本体约 9,700 行随 sprint 3 `3379d69` 入库，每轮走查评分驱动、门禁复验全绿后收轮，**单轮最高 99/100，未达 100**；评审修复见上一条目）：

- **网络设备接入（N1）**：`servers` 新增 `device_kind=network_device` 形态（与 `os='network'` 双向绑定），五品牌 `netdev_brand`（cisco/huawei/h3c/junos/generic）×双通道 `access_proto`（ssh/telnet）。执行层为 `internal/netdev`（契约 5.2B 定名 netcli，实现包名 netdev）：SSH 通道在 PTY 内逐行 expect（连接/TOFU 复用 `sshx`），Telnet 通道为**包内自研 `internal/netdev/telnetx`**（原契约指定的 `ziutek/telnet` 自 2016 年停止维护，三期定稿改自研——裸 TCP IAC 清洗、4MiB 缓冲上限、明文告警单一事实源）。固定流程：登录交互 → 提示符 → 分页关闭 → 命令 → 清洗（去回显/去 `--More--`，分页自动续读）→ `exit`，每命令独立连接（每设备 2 并发/全局 32）。`enable_secret`（enable/super 特权密码）独立加密存储，仅对 `enable`/`super` 命令自动应答。新增**网络设备五分册**（`rules_netdev.go`，`NET_CISCO_`/`NET_HUAWEI_`/`NET_H3C_`/`NET_JUNIPER_`/`NET_GENERIC_` 同构规则、generic 为四家保守并集、Cisco 保留字前缀缩写归一）：reload/write erase/zeroize/boot 变更 forbidden，配置导出/enable/配置模式/保存/外发/抓包 needs_approval，只读白名单+ping/tracert+facts 模板 safe，shell 元字符按注入 forbidden，未知 DEFAULT_ASK；`ssh_exec` 工具名不变、一次一条 CLI 命令。`server_facts` 五品牌固定模板；连接测试走品牌探针（`server_version` 取版本串）；`access_proto=ssh` 可开 Web 终端（termx 直用 sshx PTY），telnet 400 `TERM_UNSUPPORTED`、网络设备 RDP 400 `RDP_UNSUPPORTED`。
- **MongoDB（N2）**：`databases.engine=mongodb`（第六种引擎，`go.mongodb.org/mongo-driver` v1.17.4）：host/port（缺省 27017）/目标库/用户名密码成对（同空=无认证）/`auth_db`（AuthSource 缺省 admin）；ssl_mode 映射 TLS 三态。查询输入是**单条 JSON 命令文档**（`db_query` 的 `sql` 字段承载，与 MongoDB database command 同形）；守卫为**六层白名单**（`sqlguard/mongo.go`：严格单一 JSON 对象 → 五命令白名单 find/aggregate/count/distinct/estimatedDocumentCount → 每命令请求参数白名单 → 聚合管道阶段白名单（$facet/$unionWith 递归）→ 全文档递归拒 `$where`/`$function`/`$accumulator`/`$out`/`$merge` → maxTimeMS≤60s/limit≤1000）；执行 `readPreference=primary` + ctx/SetMaxTime 双超时，结果按第一行 BSON 键序映射（ObjectID/DateTime/Decimal128 字符串化、二进制 base64），1000 行/1MB 截断；连接只经 `SetHosts` 结构化传递（不走 ApplyURI，防 URI 保留字符解析面）。只读同一、二期裁决：写路径不存在、不创建审批。
- 基础设施：迁移 `00005_phase3.sql`（servers `device_kind`/`netdev_brand`/`access_proto`/`enable_secret_enc`、`os` 枚举扩 `network`；databases `auth_db` 与 mongodb 引擎；无新表新索引）；fuzz 目标新增 `FuzzNetClassify`；smoke 新增 S20 三期路径；E2E 新增 `polish-p3round6.spec.ts`（8 用例）。
- 三期冲刺（走查评分，最高 99/100，各轮要点见 README 8.8 三期表）：网络设备连接测试弹窗通道化与中文分类（sprint 1）、RDP 下拉隐藏网络设备与审计标签去 SQL 化（2）、实现本体入库与 Mongo/Telnet 表单打磨（3）、Telnet 明文告警中文化（4）、netdev 错误分类短语与终端页常驻说明（5）、失败 reason 剥包名哨兵链只留最终原因（6）；另有 `4207cd8` 修 E2E 数据库列表分页溢出 flaky。
- **终版复验（全部通过，2026-10-26/27）**：
  - 门禁：`go vet`、`gofmt -l`（无输出）、Go 全量 **22 包 ok**、覆盖率 **71.5%**（二期 71.9% 基本持平；approvals 100% / auth 96.3% / sqlguard 95.2% / risk 90.7% / agent 89.4% / httpapi 82.1%；新包 netdev 85.6% / telnetx 90.3%）；前端 typecheck / lint（`--max-warnings=0`）/ 单测 **58 文件 1044 项** / build + 体积门禁（120,962 B ≈ **118.1 KiB** PASS）；HTTP 冒烟 `SMOKE_OK`（S02 p95 24.9ms，含 S20 三期路径）；E2E **118 passed（3.5m，30 spec）**。
  - 模糊：四目标各 30s 无失败（FuzzClassify 8,354,815 / FuzzWinClassify 3,560,584 / FuzzNetClassify 202,499 / FuzzCheck 595,406 次执行）。
  - 负载：selftest 重跑 PASS（SSE p95 264ms）；浸泡 15m0s / 并发 8 **PASS**（2026-10-26T23:12，10,177 聊天轮、SSE p95 389ms、RSS 峰 64MB 增幅 1.2%、0 错误，即当前 `reports/soak.md`）。
  - 部署：`docker compose build app` 成功（`aiops-app:latest` **67.5MB**，较二期 +4MB 来自 netdev/telnetx 与 mongo-driver）；`up -d --wait app` 后 `127.0.0.1:18081/healthz` 200（`database: ok`）、SPA 首页 200。
  - **本提交（文档同步）**：README 三期终版文档（功能清单两块 4.10/4.11、网络设备分册摘要入 5.3、mongodb 六层白名单入 5.5、技术栈/项目结构更新、已知限制新增 32/33 并更新 3/4/11/12/18/26/30、第 8 节全部换三期实测数字）；selftest 报告刷新。
  - 已知缺口（如实记录，见 README 已知限制 18/29/32）：真实网络设备（五品牌真机）与真实 MongoDB 实例未接入验证（fake 设备 TCP server / 纯逻辑守卫 + build tag 集成测试）；`.env.example` 仍缺二期环境变量（`AIOPS_GUACD_ADDR`、`AIOPS_TERM_*`、`AIOPS_RDP_*` 等，三期无新增环境变量，也未补二期缺口）。

## 2026-10-26 — 三期评审修复（含 Telnet 通道选型定稿）

三期评审 12 项结论全数处置（1 verified high + 6 medium + 5 low，无误报）：

- **Telnet 通道选型定稿（契约修订）**：原契约 2.8/5.2B 指定的 `github.com/ziutek/telnet` 自 2016 年停止维护（无 context/超时支持、同步 expect 模型与池化并发不匹配），三期实现改为包内自研 `internal/netdev/telnetx`（~300 行裸 TCP IAC 清洗层，stream 4MiB 缓冲上限、明文告警单一事实源）。契约五处同步标注「选型修订」（路由表、5.2B、包边界、依赖表、目录树）。
- 修复 telnetx IAC DONT/WONT 跨 TCP 块时选项字节混入数据流（与 do/will 同构的 pending 暂存，补跨块回归测试）。
- termx/rdpx 修复两组 -race 实测数据竞争：`liveSession.rec` 改 atomic.Pointer；`senderWG/ioWG` 的 Add 移入注册临界区（sync 语义要求正增量 Add 先于 Wait），失败路径哑 goroutine 冲销预加计数。
- Telnet 明文告警透传链补齐：server.create/update 审计 `detail.confirm_telnet_cleartext=true`（契约 9.7）、连接测试响应与执行结果携带 `cleartext_warning`、连接测试弹窗展示告警（契约 2.6「UI 必须显示」）。
- MongoDB 连接去掉 ApplyURI：host 仅经 SetHosts 结构化传递，host 含 URI 保留字符（?/&）不再有被驱动解析为查询选项的面。
- 前端：编辑无认证 MongoDB 的表单死锁（verified high，校验器抽出 `mongoAuthUsernameError` 纯函数）；网络设备「自动执行」开关按终态传 telnet 确认；`netdev_brand:""` 形态切换（后端空串按清除处理，openapi/store 既有语义）；chat 工具卡 NETCLI_* 中文映射；NET_* 风险规则标签（五前缀 × 同构后缀）；服务器搜索 haystack 常量污染（「通用」「ssh」不再匹配全部服务器行）；Mongo/Redis 超时去 SQL 化文案。

## 2026-10-26 — 二期交付：Windows 执行通道、SQL Server/Redis、Web 终端与 RDP 桌面（含 6 轮冲刺与终版复验）

二期四块能力（契约先行 `5528352`，实现与打磨经 6 轮冲刺 `5b32eb1` → `aa4b357`，每轮走查评分驱动、门禁复验全绿后收轮，**单轮最高 95/100，未达 100**）：

- **Windows 服务器（W1）**：`server.os` 双轨执行——`password`/`private_key` 走 Windows OpenSSH Server（复用 `sshx`，包装 `powershell -NoProfile -NonInteractive -Command`）；`password_ntlm`/`password_basic` 走 WinRM（新增 `internal/winrmx`，`masterzen/winrm`，`winrm_port` 缺省 5985，Basic 明文提示仅内网/HTTPS 5986）。新增 **PowerShell/cmd 风险分册**（`internal/risk/rules_win.go`，`WIN_` 前缀规则对照 MITRE ATT&CK：凭据转储/防御规避/编码执行/LOLBIN/持久化/远程执行均 forbidden，未知 `WIN_DEFAULT_ASK`、解析失败 `WIN_OBFUSCATION`，失败关闭），与 Linux 分册互不混用；`server_facts` 按 os 用固定 PowerShell 模板。
- **SQL Server 与 Redis（W2）**：`databases.engine` 新增 `mssql`（`go-mssqldb`，`instance_name` 与端口互斥）与 `redis`（`go-redis/v9`，`redis_db` 0..1023）。守卫：mssql 词法分类（只放单条 SELECT/WITH…SELECT，拒 `SELECT INTO`/`FOR BROWSE`/`OPENROWSET`/`EXEC sp_*`/多语句/BACKUP）；redis **只读命令白名单**（写命令/管道/`MULTI`/未知命令全拒，`CONFIG GET` 凭据参数与通配符拒，`KEYS` 放行但 UI 提示 `SCAN`，`SELECT` 不在白名单）。与一期同一裁决：只读、写路径不存在、不创建审批。
- **堡垒机式 Web 终端与录制回放（W3/F1）**：`/term` 页 xterm.js + WebSocket（子协议 `opsmind-term-v1`）+ 后端 PTY（`internal/termx`，`sshx` 新增 pty.go）；asciinema v2 全程录制（`.cast`，0600，≤64 事件/1s 落盘，结束封盘回填大小）；`/recordings` 页浏览器内回放（自研 castParser + xterm，静默段进度插值）与下载。治理：同服务器 1 会话/全局默认 4、空闲 10m/时长 8h、输入摘要审计 `session.input_summary`（逐行截 512B、脱敏）、`session.start/end` 审计。**终端不经命令级风控**（治理靠录制与审计）。
- **浏览器内 RDP 远程桌面（W4/F1）**：`/rdp` 页经 WebSocket（子协议 `guacamole`，文本帧）→ `internal/rdpx`（纯字节透传，不解析 guacamole 指令）→ **guacd 边车**（`guacamole/guacd:1.5.0`，compose 服务可整体移除，`AIOPS_GUACD_ADDR` 空 = RDP 禁用 503）。凭据服务端注入（明文只在握手内存，不落日志/WS/审计）；`.guac` 录制写共享卷；分辨率创建后不可变；**v1 回放仅元数据 + 下载**（浏览器内播放不支持）。
- 基础设施：迁移 `00004_phase2.sql`（servers `os`/`winrm_port`、databases mssql/redis 字段、`terminal_sessions`/`rdp_sessions` 表）；`gorilla/websocket`、`@xterm/xterm`、`guacamole-common-js` 等依赖；E2E 基建补假 guacd（18082）与假 sshd（`cmd/e2esshd`，SSH 18083/控制 18084）支撑终端/RDP **真链路**用例。

二期冲刺（走查评分，最高 95/100，见 README 8.8 二期表）：装配与五处终端交互修复（sprint 1）、guacamole 子协议与回放插值（2）、RDP 文本帧与真链路 e2e（3）、409 场景化与会话列表乐观结束（4）、WS 握手期排队 resize 修 PTY 尺寸同步（5）、输入摘要跨帧缓冲修复（6）。

**终版复验（发现 1 项未通过并已修复；其余全部通过，2026-10-26）**：

- **未通过项**：fuzz 门禁脚本用无锚点 `-fuzz FuzzClassify` 跑 Linux 分册时匹配到二期新增的 `FuzzClassifyWindows`，Go 拒绝执行多目标（exit 1）。**修复**：Windows 分册目标改名 `FuzzWinClassify`（命名注意已写入 `fuzz_test.go` 注释），门禁模式锚定 `FuzzClassify$`；修复后三目标各 30s 实测无失败（8,225,591 / 3,400,169 / 545,205 次执行）。
- 门禁：`go vet`、`gofmt -l`（无输出）、Go 全量 21 包 ok、覆盖率 **71.9%**（一期 69.5%；approvals 100% / auth 96.3% / sqlguard 95.5% / agent 90.8% / risk 90.2% / httpapi 86.0%；新包 winrmx 83.4% / termx 82.3% / rdpx 80.3%）；前端 typecheck / lint（`--max-warnings=0`）/ 单测 **55 文件 969 项** / build + 体积门禁（120,839 B ≈ **118.0 KiB** PASS）；HTTP 冒烟 `SMOKE_OK`（S02 p95 20.1ms，含二期 S19）；E2E **109 passed（3.6m，29 spec）**。
- 负载：selftest 重跑 PASS（SSE p95 270ms）；浸泡 15m0s / 并发 8 **PASS**（2026-10-26T08:07，9,953 聊天轮、SSE p95 403ms、RSS 峰 61MB 增幅 2.1%、0 错误，即当前 `reports/soak.md`）。
- 部署：`docker compose build app` 成功（`aiops-app:latest` **63.5MB**，较一期 +7.4MB 来自二期模块与驱动依赖）；`up -d --wait app`（连带 guacd 边车）后 `127.0.0.1:18081/healthz` 200（`database: ok`）、SPA 首页 200。
- 可选真 guacd 集成：`AIOPS_REAL_GUACD=127.0.0.1:14822 go test ./internal/rdpx/ -run RealGuacd` 对 `guacamole/guacd:1.5.0` 容器握手 **PASS**（非 CI 门禁）。
- **本提交（文档同步 + 修复）**：README 二期终版文档（功能清单四块 4.6–4.9、Windows 分册摘要入 5.3、mssql/redis 守卫入 5.5、终端/RDP WS 反代 Upgrade 头配置入 3.3、guacd 边车入 3.1、已知限制新增 24–31、第 8 节全部换二期实测数字）；fuzz 目标改名（`FuzzClassifyWindows` → `FuzzWinClassify`）；selftest/soak 报告刷新。
- 已知缺口（如实记录，见 README 已知限制 29）：`.env.example` 尚未补二期环境变量（`AIOPS_GUACD_ADDR`、`AIOPS_TERM_*`、`AIOPS_RDP_*` 等）；真实 Windows 主机 / SQL Server / Redis / RDP 服务端仍未接入验证（见 README 8.9）。

## 2026-10-25（晚）— 测试覆盖强化、成品度冲百循环（10 轮）与终版复验

- **测试覆盖强化**（两个提交）：
  - `3d0b583`：后端语句覆盖率 65.6% → 69.2%（approvals 100% / risk 95.9% / sqlguard 95.5% / auth 96.3% / httpapi 93.7% / agent 91.4%）。
  - `41b6a7f`：前端单测与 E2E 扩充——新增 17 个单测文件（apiError/format/meta/queryKeys 矩阵、审批状态机、SSE 解析与流缓冲矩阵、composerStore、pageTitle 等）与 `frontend-coverage.spec.ts`；配合其后冲刺轮次补的 3 个单测文件与 5 个专项 E2E spec，前端单测现为 **43 文件 803 项**、E2E **91 项 / 24 spec**（终版复验实测）。
  - 同批 `1adf99b`：文档与当日负载报告同步（README/CHANGELOG + selftest/soak 报告刷新）。
- **成品度冲百循环**（走查评分量化：high −10 / medium −4 / low −1，**无可做项才计 100**；每轮采纳项完成后整组门禁复验，全绿才收轮）：共 10 轮冲刺 `d7089b5` → `bdd8804`，累计采纳并完成 **22 项**；评分曲线 `99 → 98 → 93 → 93 → 97 → 99 → 90 → 86 → 99 → 97`，**最高 99/100（第 1/6/9 轮），未达 100**——每轮走查仍存在 1–4 项可做项（扣 1–14 分），第 10 轮 3 项完成、门禁全绿后收尾，未再复评。各轮评分/提交/要点对照表见 README 8.8。除冲刺 3（即上文「sprint 打磨第 3 轮」条目）外：
  - 冲刺 1 `d7089b5`：工具卡耗时剔除审批等待段，denied/expired 不再显示 45s 级误读耗时。
  - 冲刺 2 `b58576b`：改密失败提示场景化「旧密码不正确」；E2E 实证审计动作筛选可关键词过滤。
  - 冲刺 4 `189886a`：停止回答即时作废未决审批卡、数据库重名 409 具体化、404 页换自定义插画、审计 UUID 筛选归一化小写。
  - 冲刺 5 `3fa0b3d`：登录错密提示改「管理员密码错误」、侧栏搜索无匹配联动加载更早会话、审计 UUID 提示中英文补空格。
  - 冲刺 6 `1c16a66`：会话侧栏新建/更多纯图标按钮补 aria-label 可访问名称。
  - 冲刺 7 `920f1d7`：登出/改密后登录页不再把会话结束误报成 INTERNAL 服务器故障横幅。
  - 冲刺 8 `44c11d9`：SSH 22 端口拨号地址不再被 knownhosts.Normalize 剥掉端口；工具失败底层原因落库 error_message 供历史会话展示。
  - 冲刺 9 `6521a81`：SQL 查询抽屉空语句不再静默无响应——执行按钮禁用，Ctrl+Enter 给出「请输入要执行的 SQL」提示。
  - 冲刺 10 `bdd8804`：审计动作补改密标签、服务器搜索含端口、双窗口审批后补偿刷新。
- **终版复验（全部通过，2026-10-25 晚）**：
  - 门禁：`go vet`；`gofmt`（修复 `3d0b583` 引入的 2 个测试文件注释对齐：`internal/approvals/service_edges_test.go`、`internal/risk/rules_edges_test.go`）；Go 全量测试 15 包 ok（退出码 0）；前端 typecheck / lint（`--max-warnings=0`）/ 单测 **803 项** / build + 体积门禁（120,510 B ≈ **117.7 KiB**，PASS）；HTTP 冒烟 `SMOKE_OK`（S02 p95 23.9ms）；E2E **91 passed（3.2m）**。
  - 覆盖率复测：**69.5%**（`3d0b583` 的 69.2% 之后冲刺又有后端增量）；关键包 approvals 100% / auth 96.3% / risk 95.9% / sqlguard 95.5% / httpapi 93.8% / agent 91.4%。
  - 浸泡：15m0s / 并发 8 **PASS**（2026-10-25T19:51，9,976 聊天轮、SSE p95 402ms、RSS 峰 57MB 增幅 3.4%、goroutine 增幅 11.7%、0 错误，即当前 `reports/soak.md`）；selftest 重跑 PASS（2026-10-25T20:19，SSE p95 277ms，报告已刷新）。
  - 模糊：FuzzClassify 30s **8,616,851** 次执行、FuzzCheck 30s **666,476** 次执行，均无失败。
  - 镜像：`docker compose build app` 成功（`aiops-app:latest` 56.1MB）；启动后 `127.0.0.1:18081/healthz` 200（`database: ok`）、SPA 首页 200。
- **文档同步（本提交）**：README 8.1/8.3–8.7/8.9 数字更新（覆盖率 69.5%、单测 43 文件 803 项、E2E 91 项 / 3.2m、fuzz 新执行计数、selftest/soak 终版报告、镜像与探针复验、体积 117.7 KiB），新增 8.8「成品度冲百循环」；已知限制 11/19/20 更新、新增 23（冲百循环未达 100 分）。

## 2026-10-25（第三轮）— sprint 打磨第 3 轮（含同轮 E2E flaky 修复）

采纳 4 项，全部完成，七层门禁全绿后提交（两个提交：4 项打磨 + 竞态修复二次迭代）：

- **快上游流式工具卡间歇性消失（竞态修复，两次迭代定稿）**：done 处理里 messages 与 toolCalls 的 invalidate 是并发 refetch、存在毫秒级落地间隙——messages 先落地即撤 live 时，toolCalls 未落地让 toolsByTurn 没有本轮分组，工具卡从页面消失（无 token 间隔的 LLM 下 13 轮实测 3 轮复现，重进会话才恢复）。第一版修复（等 toolCalls 覆盖 live.tools 再撤 live）在审批超时 E2E 全量并行下暴露新问题：间隙内 live 条目与持久化 assistant 消息渲染两份正文，`getByText(RISKY_FINAL)` 撞进窗口 strict mode 双元素失败（单跑通过、全量必现，trace 网络日志实证 messages 3ms / tool-calls 14ms 的落地错峰）。最终机制拆成两个互补判定：`computeLiveSettled`（`useChatStream.ts`，ChatPage liveSettled 本体）done 分支**只看 messages**（正文切换无窗口）+ `liveToolsFallbackTurnId`（MessageList 回退渲染）在该 turn 的持久化 toolCalls 分组未落地时用内存中的 `live.tools` 补渲染工具卡（卡片不消失，分组落地后回退自动失效；后端先落库再 emit，无永久等待）。MessageList 的 tools 条目统一为 ToolCallView，live 审批索引提升至列表层并与持久化审批合并。回归：`__tests__/liveSettledRace.test.ts` 12 项（撤除判定 + 回退判定矩阵，含既有 error/停止轮语义守恒）；全量 E2E 连续三轮 81 passed 验证稳定。
- **审计目标 ID 筛选接入 uuidFilter**（已知限制 21）：目标 ID 输入框改走 `applyUuidFilter`，与会话/服务器 ID 一致——非法 UUID 提示「必须是合法的 UUID」且不发请求，不再静默按 200 空结果误导。E2E：`e2e/audit.spec.ts` 新增用例（非法拦截断言 toast 且 `resource_id=not-an-uuid` 从未发出；合法 UUID 筛出自种 database.create 记录）。
- **首屏 JS gzip 回预算内 + CI 体积门禁**（已知限制 19）：`AntdAppRoot`（ConfigProvider 壳 + `antd/locale/zh_CN`）下沉到懒加载边界（`src/App.tsx`，模块求值即预取、挂起期纯 CSS Spinner），antd 壳 chunk 与 icons-vendor 退出首屏，实测 229,513 B ≈ 224 KiB → **120,290 B ≈ 117.5 KiB ≤ 150KB**。新增 `web/scripts/check-bundle-size.mjs`（`pnpm -C web check:bundle-size`，与 README 8.7 测量命令同口径）并在 CI frontend job build 后强制执行；eslint 配置为 `scripts/**/*.mjs` 启用 node globals。
- **`.env.example` 补 `AIOPS_PPROF`**（已知限制 22）：按 README 2.3 补注释与默认值（`false`），置于运行时配置组末尾。
- **文档同步**：README 已知限制 11/19/21/22、8.1 门禁表（前端单测 40 文件 760 项、E2E 81 项 / 2.6m、构建 117.5 KiB 达标）、8.7（新构成表、修复记录、CI 门禁）、8.5/8.8 数字更新。
- **门禁实测**（全部通过，flaky 修复后重跑）：`go vet ./...`；`go test -p 2 ./...`（15 包 ok）；`pnpm -C web typecheck`/`lint`（含 `--max-warnings=0`）/`test`（760 项）/`build` + `check:bundle-size` PASS；`go run ./cmd/smoke`（SMOKE_OK）；`pnpm -C web test:e2e` **连续三轮 81 passed**（修复前全量在审批超时用例必失败）。

## 2026-10-25 — sprint 打磨、第二批六轮产品自迭代与最终复验

- **两轮 sprint 打磨**：
  - `ff1cf56`（sprint 1）：供应商连接测试失败透出底层原因，裸短语不再拼进中文标题。
  - `23d9c04`（sprint 2）：会话列表游标分页 + 侧栏「加载更多」、限流提示去英文、CI 补前端单测、README 数字同步。
- **45 分钟档浸泡实测通过**（无独立提交，报告曾入库后被确认性浸泡覆盖，可查 git 历史 `reports/soak.md`）：`loadgen -mode=soak -duration=45m0s -concurrency=8`（2026-10-25T01:35，PASS）——29,918 聊天轮、29,920 审批自动批准、1,795,080 token 事件、SSE p95 403ms、RSS 峰 60MB（增幅 1.6%）、goroutine 增幅 0.0%、0 错误。
- **第二批六轮产品自迭代打磨**（每轮：自评审打分 → 采纳若干项 → 门禁全绿复验）：
  - 第 1 轮 `7652936`（成品度 88/100，采纳 2 项）：假 LLM 清单回复按真实资产动态生成；新增管理员改密（校验旧密码 + 吊销全部会话 + 审计）与顶栏修改密码弹窗。
  - 第 2 轮 `78412a7`（84/100，采纳 3 项）：流式空闲看门狗兜底代理挂死并给悬挂轮重新生成入口；输入框 showCount；端口去静默 clamp。
  - 第 3 轮 `ae37114`（90/100，采纳 2 项）：SQL 查询失败透出底层原因并中文化（只读拒绝/超时），页面标题随路由区分。
  - 第 4 轮 `0d6b56a`（78/100，采纳 4 项）：输入框自动聚焦、antd 全局 zh_CN 中文化、纯流式停止单击直停（pending 审批仍确认）、会话侧栏标题搜索。⚠️ **代价**：根部 `AntdAppRoot` 引入 `antd/locale/zh_CN` 后 ConfigProvider 依赖链进入首屏，首屏 JS gzip 由 116.5 KiB 涨至 **224 KiB，超出 150KB 预算**（根因与候选方向见 README 8.7，优化未做）。
  - 第 5 轮（92/100，主干体验无阻断，无值得做的项）：62 项既有 E2E 全绿；9 项人工自由走查全部通过（双窗口审批实时同步、390px 移动端布局、console 全程收集无真实前端错误等）；`make seed` 播种/幂等/`CHAT=1` 演示链路实测走通。遗留低影响细节：审计目标 ID 筛选校验不一致、`.env.example` 缺 `AIOPS_PPROF`（见 README 已知限制 21/22）。
  - 第 6 轮 `3a07455`（90/100，采纳 1 项）：审计操作者列显示「管理员 admin」——`/api/audit` 按 actor_id 批量装配 actor_name，完整 UUID 移入 Tooltip。
- **循环后最终复验**（2026-10-25）：门禁全绿、确认性浸泡达标（15m0s / 并发 8，2026-10-25T07:31，PASS，即当前 `reports/soak.md`）、模糊无崩溃、镜像探针通过（`127.0.0.1:18081/healthz` 200）。
- **本提交（文档与修复）**：
  - README「测试与验证」更新为本日实测：E2E **63 passed**（2.2m）、前端单测 21 文件 **208 项**、后端语句覆盖率 **65.6%**、两个 fuzz 目标各 30s 无崩溃（FuzzClassify 10,198,675 次执行 / FuzzCheck 713,101 次）、loadgen selftest PASS（SSE p95 274ms）、部署探针 200 与镜像 56.1MB；补 45m 浸泡结论与 soak 报告双档说明。
  - README 8.7 如实记录首屏 JS gzip 超标（229,347 B ≈ 224 KiB > 150KB）及根因（第 4 轮 zh_CN 全局化）；已知限制 19–22 更新（预算超标、45m 已测、审计目标 ID 筛选、`.env.example` 缺 `AIOPS_PPROF`）。
  - 修复：`gofmt` 补齐 2 个测试文件的注释对齐（`internal/httpapi/approvals_audit_ops_test.go`、`internal/risk/risk_test.go`），恢复 `gofmt -l .` 无输出。

## 2026-10-24（深夜）— 测试与 E2E 扩充

- `cc513d0`：后端测试覆盖率 46.5% → 65.2%（本日文档更新时复测为 65.6%）。
- `1b2d8ad`：前端单测与 E2E 扩充——新增 markdownBlocksMore / sseParserEdge / streamBufferMore / useChatStreamMore 等单测（单测由 15 文件 163 项增至 21 文件 208 项），E2E 由 47 项增至 62 项。

## 2026-10-24（晚）— 验证补齐收尾 + 文档与 CHANGELOG

- **验证补齐**（无独立提交，成果分布在其后各轮提交与本日验证记录）：
  - Playwright 端到端（`pnpm -C web test:e2e`，globalSetup 全自动编排 postgres/后端/假 LLM/vite preview）；
  - Go 原生模糊测试 `FuzzClassify`（`internal/risk/fuzz_test.go`）与 `FuzzCheck`（`internal/sqlguard/fuzz_test.go`），固化种子与回归语料入仓；
  - 自包含负载/浸泡工具 `cmd/loadgen`（selftest/soak/stress 三模式，固定阈值判定，报告落 `reports/`）；
  - 生产镜像与 compose 部署验证（多阶段 Dockerfile + distroless nonroot，宿主机约定端口 `127.0.0.1:18081` 健康探针）；
  - 前端分包优化（rolldown `codeSplitting.groups`：react-vendor / icons-vendor / markdown-vendor，重组件随懒加载路由分块，首屏 JS gzip 降至 ~119KB）。
- **六轮产品自迭代打磨**（每轮：自评审打分 → 采纳若干项 → 门禁全绿复验）：
  - 第 1 轮 `c270b97`（成品度 78/100，采纳 5 项）：消息/代码块复制、审批风险中文化、演示数据播种（`make seed`）、登录密码指引、失败消息重新生成。
  - 第 2 轮 `406acbf`（成品度 72/100，采纳 5 项）：消息加载失败防崩、流式刷新离开确认、停止/取消文案纠偏、409 删除弹窗自关、修复 `leaveInProgressRef` 用例定位。
  - 第 3 轮 `8d758b5`（成品度 88/100，采纳 3 项）：品牌云 favicon、fake-llm `/v1/models` 路由修复让“测试连接”可通、未知路径 404 页替代静默重定向，补 4 项 E2E。
  - 第 4 轮 `75d7e3c`（成品度 87/100，采纳 5 项）：流式期间输入框可预输入、服务器重名 409 返回具体原因、审计展开行 CodeBlock 一键复制、新建会话改草稿页不落空库、审批终态隐藏按钮组。
  - 第 5 轮 `6bd13a9`（成品度 88/100，采纳 3 项）：断连 502 规范为 `NETWORK_ERROR` 中文提示并聚合横幅、错误码降级为小字不再当标题、README 补 E2E 现状，新增 6 项单测与 4 项 E2E。
  - 第 6 轮 `4788605`（成品度 88/100，采纳 3 项）：连接/执行失败透出底层原因、登录与工具卡失败提示全中文化。
- **循环后最终复验**：门禁全绿、确认性浸泡达标（`reports/soak.md`，15m0s / 并发 8，PASS）、模糊无崩溃、镜像探针通过（`127.0.0.1:18081/healthz` 200）。
- **文档**（本轮提交）：README 新增「测试与验证」一节（冒烟/单测/模糊/端到端/浸泡压测/部署验证的命令与结果）、loadgen 用法与阈值、模糊续跑与 E2E 复现指引、首屏 JS gzip 性能预算、部署一节补充镜像构建与 18081 健康检查端口；新增本 CHANGELOG。

## 2026-10-24（午）— 模糊测试硬化

- `33603c0`：fuzz 硬化——修复挂起/漏判 6 类缺陷并固化种子回归：
  - 挂起类：`readRedirectTarget` 零消费返回导致分类器游标原地踏步（7 字节输入即可挂死）；
  - 内存类：长词逐字节拼接 O(n²) 分配（16KB 词 ~256MB）、NFKC 膨胀输入驱动巨量分配；
  - 漏判类：算术展开内命令替换 `$(( $(...) ))`、here-string / 输入重定向目标中的替换、算术空体 `$(($(())))` 被当纯算术；
  - 误报类：重定向目标被 `path.Base` 误提取为命令名（`cat</rm -rf`）、转义分隔符（`ls 0\&shutdown`）；
  - 修复后全部失败关闭或正确分级，引爆样本入 `testdata/fuzz` 回归语料（risk/sqlguard 各 66 个）。

## 2026-10-24（凌晨）— 首个功能版本

- `eb1be27`：云枢 OpsMind AI 运维系统首次完整交付——单管理员控制台、对话式运维（6 工具循环 + SSE 流式）、SSH 资产与 TOFU 主机密钥、外部数据库只读守卫（PG AST + MySQL/SQLite 词法）、9 预设模型供应商、高风险命令人工审批（120s TTL）、只追加审计哈希链、AES-256-GCM 凭据加密、Origin 校验与登录限流、docker compose 部署、OpenAPI 契约先行（`api/openapi.yaml` + `docs/CONTRACT.md`）。

## 2026-10-23 — 脚手架

- `7ff9ace`：Go + React 项目骨架（模块布局、CI 工作流、开发环境约定）。
