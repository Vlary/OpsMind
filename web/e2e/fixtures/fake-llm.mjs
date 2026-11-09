// 无依赖的假 OpenAI 兼容 SSE 服务，供 Playwright 端到端测试使用。
//
// 行为由最后一条 role=user 消息中的标记决定：
// - 含 __scenario_list__ ：
//     1) 首次调用（尚无工具结果）→ 返回 list_servers 的 tool_calls；
//     2) 收到 list_servers 的 tool 结果后 → 解析 JSON 里的真实资产动态
//        生成中文清单文案，逐 token 流式返回（数量/名称与库中资产一致）。
// - 含 __scenario_risky__ ：
//     1) 首次调用 → 返回 list_servers 的 tool_calls（ssh_exec 需要真实
//        server_id，先从资产清单取）；
//     2) 收到 list_servers 结果 → 返回 ssh_exec 的 tool_calls
//        （command = "rm -rf /tmp/aiops-e2e"，风险分级 needs_approval，
//        会触发审批卡片）；
//     3) 收到 ssh_exec 结果（批准后 SSH 连接失败 / 拒绝后 APPROVAL_DENIED，
//        均属预期）→ 逐 token 流式回最终文本。
// - 含 __scenario_metrics__ （五期工具链）：
//     1) 首次调用 → list_servers；2) 拿到 server_id → get_metrics
//     （server_id + minutes=60）；3) 收到指标序列 → 最终文案书挡。
// - 含 __scenario_alerts__ （五期工具链）：直接 list_alerts（无参数）→
//     按事件条数生成最终文案书挡。
// - 含 __scenario_batch__ （五期工具链）：list_servers → batch_execute
//     （command="uptime"，safe 但资产 auto_exec=false → 逐台 awaiting_
//     approval，正好覆盖工具卡专属视图与「查看批量任务」深链）→ 书挡。
// - 含 __scenario_diagnosis__ （五期工具链）：list_servers →
//     run_diagnosis（server_id）→ 按工具结果的 status 动态生成最终文案
//     （成功=报告已生成可审批，失败=如实说明终态与错误码，不再无视
//     status 固定宣称成功——五期打磨 sprint 2）。
// - 诊断引擎协议请求（无 scenario 标记，messages[0] 是含「服务器故障
//     诊断引擎」的 system 提示——internal/diagnosis/prompt.go 的协议提示
//     词）：按轮次回协议 JSON——首轮 {"action":"probe",…}，收到
//     probe_result 后 {"action":"report",…}，使诊断会话真实到达
//     awaiting_fix（此前回默认流式文本，引擎 6 轮全部解析失败落
//     DIAGNOSIS_MAX_ROUNDS，而工具卡与文案仍宣称成功）。
// - 含 __scenario_code__ ：逐 token 流式回一段带 bash 代码块的文本，
//     用于验证代码块复制按钮。
// - 含 __scenario_reasoning__ （六期深度思考）：先按段（每段
//     REASONING_SEGMENT_DELAY_MS）回 delta.reasoning_content（模拟思考
//     流，前端 ReasoningBlock 自动展开显示「思考中…」），再逐 token 回
//     正文 delta.content（正文首帧后回折叠）——e2e 断言展开/回折叠/复制
//     全文/折叠不丢内容（此前该功能只有组件单测与回放 fake 数据覆盖，
//     流式全链路 LLM→SSE→折叠 无回归保护）。
// - 含 __scenario_error__ ：前 ERROR_FAIL_CALLS 次调用返回 HTTP 500（上游
//     故障，后端落库 assistant error 消息），之后恢复正常文本，用于验证
//     历史失败消息的「重新生成」。失败次数必须不低于后端单轮的最大尝试
//     次数（internal/llm/http.go 的 maxAttempts=3，5xx 会整轮重试），否则
//     重试的第 2 次尝试就拿到成功文本，error 轮永远不会发生。
// - 含 __scenario_error_alt__ ：与 __scenario_error__ 同构，但使用独立的
//     失败计数器（frontend-coverage.spec.ts 的「忽略错误横幅」用例用），
//     两个 error 场景并行/先后执行时互不消耗对方的失败次数。
// - 含 __scenario_slow__ ：按 SLOW_TOKEN_DELAY_MS（默认 80ms）逐 token 回
//     一段长文本（>200 字符），提供 ~15s 的稳定流式窗口，供「停止生成」
//     等需要中途干预的用例。
// - GET /e2e/last-user ：返回最近一次 /chat/completions 请求中最后一条
//     role=user 消息的原文（含 <!--opsmind-context--> 上下文前缀），供
//     「服务器/数据库上下文选择随请求发送」类用例断言请求体内容。
// - 其余（含空消息列表）→ 按小延迟逐 token 流式回默认文本。
//
// 用法：node fake-llm.mjs [port]   （默认 18086，与 global-setup 的 FAKE_LLM_PORT 一致）
// 也可 import { startFakeLLM } 后以库方式启动（返回 { server, close }）。

import http from 'node:http';

const PORT = Number(process.argv[2] ?? process.env.FAKE_LLM_PORT ?? 18086);
const TOKEN_DELAY_MS = 25;

const SCENARIO_LIST = '__scenario_list__';
const SCENARIO_RISKY = '__scenario_risky__';
const SCENARIO_METRICS = '__scenario_metrics__';
const SCENARIO_ALERTS = '__scenario_alerts__';
const SCENARIO_BATCH = '__scenario_batch__';
const SCENARIO_DIAGNOSIS = '__scenario_diagnosis__';
const SCENARIO_CODE = '__scenario_code__';
const SCENARIO_REASONING = '__scenario_reasoning__';
const SCENARIO_ERROR = '__scenario_error__';
const SCENARIO_ERROR_ALT = '__scenario_error_alt__';
const SCENARIO_SLOW = '__scenario_slow__';
const RISKY_COMMAND = 'rm -rf /tmp/aiops-e2e';

// list 场景的最终文本按 list_servers 工具结果动态生成（不再硬编码资产
// 数量/名称）：cmd/seed 播种 demo 资产后走同一条链路，回答必须与库里的
// 真实资产一致，否则演示会话自相矛盾。开头/结尾书挡供 E2E 断言。
const LIST_FINAL_PREFIX = '资产清单核对完毕：当前共';
const LIST_FINAL_SUFFIX = '以上即当前全部服务器资产。';
const LIST_FINAL_FALLBACK = '资产清单核对完毕：资产接口返回内容无法解析，请稍后重试。';

const RISKY_FINAL =
  '命令已按流程处理完毕。目标服务器 e2e-fake-host 在测试环境中不可达，' +
  '因此没有产生任何真实执行输出；审批与执行路径均已走通。';

const CODE_FINAL =
  '排查磁盘占满时可以按下面的顺序执行：\n\n' +
  '```bash\nsystemctl status nginx\njournalctl -u nginx -n 100 --no-pager\n```\n\n' +
  '先确认服务状态，再看最近的日志。';

// reasoning 场景的思考段（按段一帧发送，段间延迟大于常规 token 节奏，
// 给 e2e 留出可捕捉的「思考中…」展开窗口）；拼接全文（segments.join('')）
// 与 helpers.ts 的 REASONING_FINAL 保持一致（e2e 断言复制全文）。
const REASONING_SEGMENTS = [
  '用户在排查磁盘占满问题。',
  '先确认资产清单锁定目标主机，',
  '再对目标主机执行 df -h 采集使用率证据，',
  '结合最近日志判断是日志膨胀还是数据增长，',
  '最后按使用率给出清理建议。',
];
const REASONING_BODY_FINAL = '思考结论：磁盘排查按先清单后采集的顺序执行即可。';
const REASONING_SEGMENT_DELAY_MS = 350;

const ERROR_FINAL = '上游服务已恢复，这是重新生成后的正常回答。';

const SLOW_FINAL =
  '这是一段刻意放慢的流式输出，用于验证停止生成与重新生成链路。'.repeat(4) +
  '慢速流的结尾标记：全部 token 发送完毕。';

const SLOW_TOKEN_DELAY_MS = 80;

// 五期四条对话工具链的最终文案书挡（e2e 断言用，helpers.ts 同步导出；
// 中段动态生成，仅首尾固定）。
const METRICS_FINAL_PREFIX = '指标查询完毕：';
const METRICS_FINAL_SUFFIX = '以上为该服务器的指标查询结论。';
const ALERTS_FINAL_PREFIX = '告警查询完毕：';
const ALERTS_FINAL_SUFFIX = '以上为当前告警事件查询结论。';
const BATCH_FINAL_PREFIX = '批量任务已创建：';
const BATCH_FINAL_SUFFIX = '以上为批量分派结论。';
const DIAGNOSIS_FINAL_PREFIX = '诊断会话已结束：';
const DIAGNOSIS_FINAL_SUFFIX = '以上为本次诊断结论。';
const NO_SERVER_FINAL = '未找到可用的服务器资产，无法继续。';
const BATCH_COMMAND = 'uptime';

// 诊断引擎协议应答（无 scenario 标记的协议请求按轮次返回）。
// probe 用 safe 只读命令（对不可达目标执行失败也会作为 probe_result
// 回给引擎继续）；report 的 schema 必须过 diagnosis.ParseReport 的严格
// 校验（未知字段/枚举越界均拒绝），suggestions 交给引擎 Normalize 重算
// risk_level（模型自报不可信）。
const DIAGNOSIS_SYSTEM_MARKER = '服务器故障诊断引擎';
const DIAGNOSIS_PROBE_JSON = '{"action":"probe","command":"uptime"}';
const DIAGNOSIS_REPORT_JSON = JSON.stringify({
  action: 'report',
  report: {
    summary:
      '目标主机 SSH 端口不可达（连接被拒绝），未能采集运行期证据；' +
      '基于通道建立失败的事实判定该资产当前无法远程管理，建议人工核实。',
    severity: 'warning',
    root_cause: 'SSH 拨号被目标拒绝（connect: connection refused），只读采集通道无法建立。',
    evidence: [
      {
        source: 'command',
        command: 'uptime',
        output_excerpt: 'execution failed: dial tcp 127.0.0.1:1: connect: connection refused',
        note: '采集命令未送达，通道建立失败',
      },
    ],
    suggestions: [
      {
        command: 'systemctl status sshd',
        reason: '登录目标主机确认 sshd 运行状态与监听端口（需审批执行）',
      },
    ],
    confidence: 'medium',
  },
});

const DEFAULT_FINAL =
  '这是一条来自假 LLM 的流式回复，用于验证 token 逐段渐进上屏的渲染链路。' +
  '每个 token 之间带少量延迟，方便端到端测试捕捉中间状态。';

function chunkFrame(payload) {
  return `data: ${JSON.stringify(payload)}\n\n`;
}

function usageFrame() {
  return chunkFrame({
    choices: [],
    usage: { prompt_tokens: 128, completion_tokens: 64, total_tokens: 192 },
  });
}

function doneFrame() {
  return 'data: [DONE]\n\n';
}

function textDelta(content) {
  return chunkFrame({
    choices: [{ index: 0, delta: { content }, finish_reason: null }],
  });
}

// OpenAI 兼容的思考增量（deepseek 风格 reasoning_content；后端
// internal/llm/openai.go 的 Delta 解析同名字段并转 SSE reasoning 事件）。
function reasoningDelta(content) {
  return chunkFrame({
    choices: [{ index: 0, delta: { reasoning_content: content }, finish_reason: null }],
  });
}

function toolCallChunks(callId, name, args) {
  return [
    chunkFrame({
      choices: [
        {
          index: 0,
          delta: {
            role: 'assistant',
            tool_calls: [
              {
                index: 0,
                id: callId,
                type: 'function',
                function: { name, arguments: '' },
              },
            ],
          },
          finish_reason: null,
        },
      ],
    }),
    chunkFrame({
      choices: [
        {
          index: 0,
          delta: {
            tool_calls: [{ index: 0, function: { arguments: JSON.stringify(args) } }],
          },
          finish_reason: null,
        },
      ],
    }),
    chunkFrame({
      choices: [{ index: 0, delta: {}, finish_reason: 'tool_calls' }],
    }),
  ];
}

function lastUserMessage(messages) {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    if (messages[i]?.role === 'user') {
      return messages[i];
    }
  }
  return null;
}

function parseServerId(toolContent) {
  try {
    const parsed = JSON.parse(toolContent);
    const id = parsed?.servers?.[0]?.id;
    return typeof id === 'string' && id.length > 0 ? id : null;
  } catch {
    return null;
  }
}

// describeServer 把单台服务器元数据渲染为中文短语；字段缺失时给占位值，
// 保证异常数据下文案仍完整。
function describeServer(server) {
  const name = typeof server?.name === 'string' && server.name ? server.name : '未命名服务器';
  const host = typeof server?.host === 'string' && server.host ? server.host : 'unknown';
  const port = Number.isInteger(server?.port) ? server.port : 22;
  const tags =
    Array.isArray(server?.tags) && server.tags.length > 0 ? `，标签 ${server.tags.join('、')}` : '';
  const auto = server?.auto_exec_enabled ? '已开启自动执行' : '未开启自动执行';
  return `${name}（${host}:${port}${tags}，${auto}）`;
}

// buildListFinal 按 list_servers 工具结果的 JSON（{"servers":[…]}）动态
// 生成中文清单文案；解析失败返回固定兜底文案（不提具体资产数，避免说错）。
function buildListFinal(toolContent) {
  let servers = null;
  try {
    servers = JSON.parse(toolContent ?? '')?.servers;
  } catch {
    servers = null;
  }
  if (!Array.isArray(servers)) {
    return LIST_FINAL_FALLBACK;
  }
  if (servers.length === 0) {
    return `${LIST_FINAL_PREFIX} 0 台服务器，尚未配置任何资产。${LIST_FINAL_SUFFIX}`;
  }
  const items = servers.map((server) => describeServer(server)).join('；');
  return `${LIST_FINAL_PREFIX} ${servers.length} 台服务器：${items}。${LIST_FINAL_SUFFIX}`;
}

// isDiagnosisProtocolRequest 识别诊断引擎的协议请求：messages[0] 是
// internal/diagnosis/prompt.go 的 system 提示（含固定标记「服务器故障
// 诊断引擎」）。此类请求无 scenario 标记，必须在 scenario 分发前处理，
// 否则落默认流式文本、引擎解析失败跑满轮数。
function isDiagnosisProtocolRequest(messages) {
  const first = messages[0];
  return (
    first?.role === 'system' &&
    typeof first.content === 'string' &&
    first.content.includes(DIAGNOSIS_SYSTEM_MARKER)
  );
}

// diagnosisProtocolOutcome 按轮次回协议 JSON：收到过 probe_result 即证据
// 阶段结束，下一轮输出最终报告（报告 → 引擎置 awaiting_fix）。
function diagnosisProtocolOutcome(messages) {
  const seenProbeResult = messages.some(
    (m) =>
      m?.role === 'user' &&
      typeof m.content === 'string' &&
      m.content.includes('"action":"probe_result"'),
  );
  return seenProbeResult
    ? { kind: 'text', text: DIAGNOSIS_REPORT_JSON }
    : { kind: 'text', text: DIAGNOSIS_PROBE_JSON };
}

// buildDiagnosisFinal 按 run_diagnosis 工具结果的 status 动态生成最终
// 文案（五期打磨 sprint 2：此前无视 status 固定宣称「报告与修复建议已
// 生成」，而引擎实际落 failed(DIAGNOSIS_MAX_ROUNDS)——文案与抽屉自相
// 矛盾）。书挡（首尾）不变，e2e 断言兼容。
function buildDiagnosisFinal(toolContent) {
  let status = null;
  let errorCode = null;
  try {
    const parsed = JSON.parse(toolContent ?? '');
    status = typeof parsed?.status === 'string' ? parsed.status : null;
    errorCode = typeof parsed?.error_code === 'string' ? parsed.error_code : null;
  } catch {
    status = null;
  }
  if (status === 'awaiting_fix' || status === 'done') {
    return `${DIAGNOSIS_FINAL_PREFIX}诊断已产出报告与修复建议，可在告警页诊断抽屉查看并审批修复命令。${DIAGNOSIS_FINAL_SUFFIX}`;
  }
  const why = errorCode ? `（错误码 ${errorCode}）` : '';
  return `${DIAGNOSIS_FINAL_PREFIX}诊断未成功：会话终态为 ${status ?? 'unknown'}${why}，请到告警页诊断抽屉查看详情。${DIAGNOSIS_FINAL_SUFFIX}`;
}

// decide 返回 { kind: 'text', text } | { kind: 'tool', name, args } |
// { kind: 'reasoning', segments, text } | { kind: 'http-error' }。
// errorCalls / errorAltCalls 分别计数两个 error 场景的失败次数：覆盖后端
// 整轮重试（maxAttempts=3）后恢复，各自验证「重新生成」/「忽略横幅」链路。
const ERROR_FAIL_CALLS = 3;
let errorCalls = 0;
let errorAltCalls = 0;

// 最近 50 条 /chat/completions 的最后一条 user 消息（含上下文前缀原文），
// 供 E2E 经 GET /e2e/last-user 断言前端实际发出的请求体。保留历史而非
// 仅末条：并行 worker 的请求会交错，仅末条会让断言错过目标请求。
const lastUserHistory = [];
let lastUserContent = '';

function decide(messages) {
  // 诊断引擎的协议请求优先于 scenario 分发（协议请求无 scenario 标记）。
  if (isDiagnosisProtocolRequest(messages)) {
    return diagnosisProtocolOutcome(messages);
  }

  const last = messages[messages.length - 1] ?? null;
  const user = lastUserMessage(messages);
  const marker = typeof user?.content === 'string' ? user.content : '';
  lastUserContent = marker;
  lastUserHistory.push(marker);
  if (lastUserHistory.length > 50) {
    lastUserHistory.shift();
  }

  if (marker.includes(SCENARIO_LIST)) {
    if (last?.role === 'tool' && last.name === 'list_servers') {
      return { kind: 'text', text: buildListFinal(last.content ?? '') };
    }
    return { kind: 'tool', name: 'list_servers', args: {} };
  }

  if (marker.includes(SCENARIO_RISKY)) {
    if (last?.role === 'tool' && last.name === 'ssh_exec') {
      return { kind: 'text', text: RISKY_FINAL };
    }
    if (last?.role === 'tool' && last.name === 'list_servers') {
      const serverId = parseServerId(last.content ?? '');
      if (serverId) {
        return {
          kind: 'tool',
          name: 'ssh_exec',
          args: { server_id: serverId, command: RISKY_COMMAND, timeout_seconds: 10 },
        };
      }
      return { kind: 'text', text: '未找到可用的服务器资产，无法继续执行命令。' };
    }
    return { kind: 'tool', name: 'list_servers', args: {} };
  }

  if (marker.includes(SCENARIO_METRICS)) {
    if (last?.role === 'tool' && last.name === 'get_metrics') {
      return {
        kind: 'text',
        text: `${METRICS_FINAL_PREFIX}已取回该服务器 60 分钟内的指标序列，可在监控页查看折线。${METRICS_FINAL_SUFFIX}`,
      };
    }
    if (last?.role === 'tool' && last.name === 'list_servers') {
      const serverId = parseServerId(last.content ?? '');
      if (serverId) {
        return { kind: 'tool', name: 'get_metrics', args: { server_id: serverId, minutes: 60 } };
      }
      return { kind: 'text', text: NO_SERVER_FINAL };
    }
    return { kind: 'tool', name: 'list_servers', args: {} };
  }

  if (marker.includes(SCENARIO_ALERTS)) {
    if (last?.role === 'tool' && last.name === 'list_alerts') {
      let count = null;
      try {
        const parsed = JSON.parse(last.content ?? '');
        const items = parsed?.events ?? parsed?.items ?? null;
        count = Array.isArray(items) ? items.length : null;
      } catch {
        count = null;
      }
      return {
        kind: 'text',
        text: `${ALERTS_FINAL_PREFIX}当前共 ${count ?? '若干'} 条事件，详见告警页。${ALERTS_FINAL_SUFFIX}`,
      };
    }
    return { kind: 'tool', name: 'list_alerts', args: {} };
  }

  if (marker.includes(SCENARIO_BATCH)) {
    if (last?.role === 'tool' && last.name === 'batch_execute') {
      return {
        kind: 'text',
        text: `${BATCH_FINAL_PREFIX}目标服务器的分派结果如上，请到批量页逐台审批。${BATCH_FINAL_SUFFIX}`,
      };
    }
    if (last?.role === 'tool' && last.name === 'list_servers') {
      const serverId = parseServerId(last.content ?? '');
      if (serverId) {
        return {
          kind: 'tool',
          name: 'batch_execute',
          args: {
            server_ids: [serverId],
            command: BATCH_COMMAND,
            description: '对话批量执行（e2e）',
          },
        };
      }
      return { kind: 'text', text: NO_SERVER_FINAL };
    }
    return { kind: 'tool', name: 'list_servers', args: {} };
  }

  if (marker.includes(SCENARIO_DIAGNOSIS)) {
    if (last?.role === 'tool' && last.name === 'run_diagnosis') {
      return { kind: 'text', text: buildDiagnosisFinal(last.content ?? '') };
    }
    if (last?.role === 'tool' && last.name === 'list_servers') {
      const serverId = parseServerId(last.content ?? '');
      if (serverId) {
        return { kind: 'tool', name: 'run_diagnosis', args: { server_id: serverId } };
      }
      return { kind: 'text', text: NO_SERVER_FINAL };
    }
    return { kind: 'tool', name: 'list_servers', args: {} };
  }

  if (marker.includes(SCENARIO_CODE)) {
    return { kind: 'text', text: CODE_FINAL };
  }

  if (marker.includes(SCENARIO_REASONING)) {
    return { kind: 'reasoning', segments: REASONING_SEGMENTS, text: REASONING_BODY_FINAL };
  }

  if (marker.includes(SCENARIO_ERROR)) {
    if (errorCalls < ERROR_FAIL_CALLS) {
      errorCalls += 1;
      return { kind: 'http-error' };
    }
    return { kind: 'text', text: ERROR_FINAL };
  }

  if (marker.includes(SCENARIO_ERROR_ALT)) {
    if (errorAltCalls < ERROR_FAIL_CALLS) {
      errorAltCalls += 1;
      return { kind: 'http-error' };
    }
    return { kind: 'text', text: ERROR_FINAL };
  }

  if (marker.includes(SCENARIO_SLOW)) {
    return { kind: 'text', text: SLOW_FINAL, slow: true };
  }

  return { kind: 'text', text: DEFAULT_FINAL };
}

async function writeWithDelay(res, parts, delayMs) {
  for (const part of parts) {
    await new Promise((resolve) => setTimeout(resolve, delayMs));
    res.write(part);
  }
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export function startFakeLLM(port = PORT) {
  let callCounter = 0;

  const server = http.createServer((req, res) => {
    const url = new URL(req.url ?? '/', `http://127.0.0.1:${port}`);

    // base_url 允许带 /v1 前缀（e2e seed 的 provider 即如此）：OpenAI 适配器
    // 把 base 与 /models 直拼，/v1/models 与 /models 都要命中。
    if (req.method === 'GET' && /^\/(v1\/)?models$/.test(url.pathname)) {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ data: [{ id: 'fake-model' }] }));
      return;
    }

    // E2E 断言辅助：最近一次 chat/completions 的最后一条 user 消息原文
    // 与最近 50 条历史（并行用例交错时从 history 中定位目标请求）。
    if (req.method === 'GET' && url.pathname === '/e2e/last-user') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ content: lastUserContent, history: lastUserHistory }));
      return;
    }

    if (req.method === 'POST' && url.pathname.endsWith('/chat/completions')) {
      let body = '';
      req.on('data', (chunk) => {
        body += chunk;
      });
      req.on('end', () => {
        let messages = [];
        try {
          messages = JSON.parse(body)?.messages ?? [];
        } catch {
          messages = [];
        }
        callCounter += 1;
        console.log(`[fake-llm] call #${callCounter}: ${messages.length} messages`);

        const outcome = decide(messages);
        if (outcome.kind === 'http-error') {
          res.writeHead(500, { 'Content-Type': 'application/json' });
          res.end(JSON.stringify({ error: { message: 'fake upstream failure' } }));
          return;
        }

        res.writeHead(200, {
          'Content-Type': 'text/event-stream; charset=utf-8',
          'Cache-Control': 'no-cache',
          Connection: 'keep-alive',
        });

        if (outcome.kind === 'tool') {
          const chunks = toolCallChunks(
            `call_${callCounter}_${outcome.name}`,
            outcome.name,
            outcome.args,
          );
          void writeWithDelay(res, chunks, TOKEN_DELAY_MS).then(async () => {
            await sleep(TOKEN_DELAY_MS);
            res.write(usageFrame());
            res.write(doneFrame());
            res.end();
          });
          return;
        }

        // reasoning 场景：先逐段回思考增量（段间慢节奏留出「思考中…」
        // 观察窗口），再按常规 token 节奏回正文，最后正常收尾。
        if (outcome.kind === 'reasoning') {
          const thinkFrames = outcome.segments.map((segment) => reasoningDelta(segment));
          const bodyFrames = [...outcome.text].map((token) => textDelta(token));
          void (async () => {
            await writeWithDelay(res, thinkFrames, REASONING_SEGMENT_DELAY_MS);
            await writeWithDelay(res, bodyFrames, TOKEN_DELAY_MS);
            await sleep(TOKEN_DELAY_MS);
            res.write(
              chunkFrame({
                choices: [{ index: 0, delta: {}, finish_reason: 'stop' }],
              }),
            );
            res.write(usageFrame());
            res.write(doneFrame());
            res.end();
          })();
          return;
        }

        const tokens = [...outcome.text];
        const frames = tokens.map((token) => textDelta(token));
        const delay = outcome.slow ? SLOW_TOKEN_DELAY_MS : TOKEN_DELAY_MS;
        void writeWithDelay(res, frames, delay).then(async () => {
          await sleep(TOKEN_DELAY_MS);
          res.write(
            chunkFrame({
              choices: [{ index: 0, delta: {}, finish_reason: 'stop' }],
            }),
          );
          res.write(usageFrame());
          res.write(doneFrame());
          res.end();
        });
      });
      return;
    }

    res.writeHead(404, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: { message: 'not found' } }));
  });

  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, '127.0.0.1', () => {
      resolve({
        server,
        port: server.address().port,
        close: () => new Promise((done) => server.close(() => done())),
      });
    });
  });
}

// 作为脚本直接运行时启动服务。
if (process.argv[1] && process.argv[1].endsWith('fake-llm.mjs')) {
  startFakeLLM(PORT)
    .then(({ port }) => {
      console.log(`[fake-llm] listening on http://127.0.0.1:${port}`);
    })
    .catch((error) => {
      console.error('[fake-llm] failed to start:', error);
      process.exit(1);
    });
}
