// FE2：统一 API 错误解析、错误码中文映射与表单字段错误映射。
// 错误体契约见 api/openapi.yaml ErrorResponse；错误码表见 docs/CONTRACT.md 9.6。

export interface ApiFailure {
  code: string;
  message: string;
  requestId: string;
  details: Record<string, unknown> | null;
  status: number | null;
}

export class ApiRequestError extends Error {
  readonly failure: ApiFailure;

  constructor(failure: ApiFailure) {
    super(failure.message);
    this.name = 'ApiRequestError';
    this.failure = failure;
  }
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export function makeFailure(code: string, message: string): ApiFailure {
  return { code, message, requestId: '', details: null, status: null };
}

// 把 openapi-fetch 的 error 与 HTTP 响应归一化为 ApiFailure。
export function toApiFailure(error: unknown, response: Response | null): ApiFailure {
  const headerRequestId = response?.headers.get('X-Request-ID') ?? '';
  const status = response?.status ?? null;

  if (isRecord(error) && isRecord(error.error)) {
    const body = error.error;
    const code = typeof body.code === 'string' && body.code !== '' ? body.code : 'INTERNAL';
    const message =
      typeof body.message === 'string' && body.message !== '' ? body.message : 'request failed';
    const requestId =
      typeof body.request_id === 'string' && body.request_id !== ''
        ? body.request_id
        : headerRequestId;
    const details = isRecord(body.details) ? body.details : null;
    return { code, message, requestId, details, status };
  }

  return {
    code: 'INTERNAL',
    message: error instanceof Error && error.message !== '' ? error.message : 'request failed',
    requestId: headerRequestId,
    details: null,
    status,
  };
}

// 冲刺 7 打磨：识别「前端兜底合成的 INTERNAL」——empty response 兜底
// （makeFailure）与 failureFromError 对非契约错误值（请求中止、网络层
// 异常等）的兜底都不带 requestId；后端真实 INTERNAL 经 toApiFailure 必带
// request_id 或 X-Request-ID 头（api/openapi.yaml ErrorResponse）。调用方
// （如登录页的 me 探测）据此把这类「无法归因于服务端」的失败视为预期态
// 而非服务器故障，不再渲染成 INTERNAL 错误横幅。
export function isSyntheticInternalFailure(failure: ApiFailure): boolean {
  return failure.code === 'INTERNAL' && failure.requestId === '';
}

export function failureFromError(error: unknown): ApiFailure {
  if (error instanceof ApiRequestError) {
    return error.failure;
  }
  if (isRecord(error) && typeof error.code === 'string' && typeof error.message === 'string') {
    return {
      code: error.code,
      message: error.message,
      requestId: typeof error.requestId === 'string' ? error.requestId : '',
      details: isRecord(error.details) ? error.details : null,
      status: typeof error.status === 'number' ? error.status : null,
    };
  }
  return makeFailure('INTERNAL', error instanceof Error ? error.message : '请求失败');
}

// 五期 sprint 4 起导出：phase5Codes 的 describeResultCode 对批量通道落库
// 的 error_code（SSH_EXEC_FAILED / SERVER_NOT_FOUND 等）回落本表，复用一期
// 中文口径（仅加 export，映射内容与一期一致，不改行为）。
export const CODE_MESSAGES: Record<string, string> = {
  AUTH_REQUIRED: '登录已过期，请重新登录',
  AUTH_INVALID: '用户名或密码错误',
  AUTH_RATE_LIMITED: '登录尝试过于频繁，请稍后再试',
  ORIGIN_FORBIDDEN: '请求来源校验失败',
  VALIDATION_FAILED: '表单校验未通过',
  NOT_FOUND: '资源不存在',
  CONFLICT: '资源冲突或当前状态不允许该操作',
  INTERNAL: '服务器内部错误',
  SERVICE_UNAVAILABLE: '服务暂不可用，请稍后重试',
  // 反向代理（vite dev / Nginx）在后端不可达时返回非 JSON 的 502/503，
  // 由 api/client.ts 的中间件规范化为此错误码（见 toContractErrorResponse）。
  NETWORK_ERROR: '网络异常，请检查连接后重试',
  NO_PROVIDER_CONFIGURED: '尚未配置模型供应商',
  PROVIDER_NOT_FOUND: '供应商不存在',
  PROVIDER_TEST_FAILED: '供应商连接测试失败',
  PROVIDER_MODELS_FAILED: '拉取模型列表失败',
  SERVER_NOT_FOUND: '服务器不存在',
  // 二期起 WinRM 通道测试失败同码（契约 2.7），文案不再绑定 SSH。
  SERVER_TEST_FAILED: '服务器连接测试失败',
  SSH_HOST_KEY_CHANGED: '主机密钥已变化，连接被阻止',
  SSH_TIMEOUT: 'SSH 连接超时',
  SSH_EXEC_FAILED: '远程命令执行失败',
  DATABASE_NOT_FOUND: '数据库不存在',
  DATABASE_TEST_FAILED: '数据库连接测试失败',
  SQL_NOT_READONLY: '仅支持只读查询，写操作不会执行',
  SQL_TIMEOUT: 'SQL 执行超时',
  SQL_QUERY_FAILED: 'SQL 执行失败',
  AUDIT_WRITE_FAILED: '审计日志写入失败',
  TOO_MANY_STREAMS: '并发对话流过多，请稍后重试',
  STREAM_ACTIVE: '该会话已有进行中的回答',
  LLM_UPSTREAM_ERROR: '上游模型服务错误',
  LLM_TIMEOUT: '模型服务超时',
  LLM_STREAM_ERROR: '模型流式响应解析失败',
  COMMAND_FORBIDDEN: '该命令被风险规则禁止',
  COMMAND_INVALID: '命令不合法',
  COMMAND_TOO_LARGE: '命令超出大小限制',
  CONVERSATION_NOT_FOUND: '会话不存在',
  CONVERSATION_MODEL_LOCKED: '会话已绑定其他供应商/模型',
  APPROVAL_NOT_FOUND: '审批不存在',
  APPROVAL_NOT_PENDING: '审批已被处理或已过期',
  APPROVAL_DENIED: '审批被拒绝，命令未执行',
  APPROVAL_TIMEOUT: '审批超时，命令未执行',
  APPROVAL_CANCELLED: '回答流已中断，本轮审批已取消，命令未执行',
  TOOL_NOT_FOUND: '模型调用了未知的工具',
  TOOL_ARGS_INVALID: '工具参数不合法',
  AGENT_MAX_ITERATIONS: '工具调用轮次达到上限',
  LLM_OUTPUT_TRUNCATED: '模型输出被截断且无可用文本',
  STREAM_TIMEOUT: '回答流超时',
  SERVER_SHUTTING_DOWN: '服务正在关闭',
  APPROVAL_STREAM_CANCELLED: '连接已断开，审批随之失效',
  CLIENT_DISCONNECTED: '客户端已断开连接',
  // 二期新增（契约 9.6）：终端 / RDP / 录制 / guacd。
  TERM_SESSION_NOT_FOUND: '终端会话不存在',
  TERM_SESSION_EXISTS: '该服务器已有活动终端会话',
  TERM_SESSION_LIMIT: '活动终端会话数已达上限',
  TERM_CONNECT_FAILED: '终端连接建立失败',
  TERM_UNSUPPORTED: '该服务器认证方式不支持终端（WinRM 无交互终端）',
  TERM_WS_PROTOCOL: '终端 WebSocket 子协议缺失或不匹配',
  RECORDING_NOT_FOUND: '会话没有可用的录制文件',
  RECORDING_UNAVAILABLE: '录制文件缺失或不可读',
  RDP_SESSION_NOT_FOUND: 'RDP 会话不存在',
  RDP_SESSION_EXISTS: '该服务器已有活动 RDP 会话',
  RDP_SESSION_LIMIT: '活动 RDP 会话数已达上限',
  RDP_CONNECT_FAILED: 'RDP 连接建立失败',
  RDP_UNSUPPORTED: '仅 Windows 服务器支持 RDP',
  RDP_NO_CREDENTIALS: '该服务器没有可用的密码凭据',
  GUACD_UNAVAILABLE: 'RDP 网关服务未配置或不可达',
};

// 后端通用 store 错误映射（internal/httpapi writeStoreError）透传的 message
// 只是错误码的小写同义词（conflict / not found / …），没有额外信息量；
// 这类兜底不拼进文案，避免出现「资源冲突…（conflict）」这种无效后缀。
// 具体冲突如 "服务器名称已存在"（servers 重名，p3 sprint 1 起中文）不在
// 集合内，会拼给用户。
const GENERIC_STORE_MESSAGES = new Set([
  'conflict',
  'not found',
  'invalid argument',
  'internal server error',
  'request failed',
  // AUTH_INVALID 的固定 message（后端 auth 处理器），无额外信息量；
  // 中文映射 CODE_MESSAGES.AUTH_INVALID 已完整表达。
  'invalid credentials',
  // AUTH_RATE_LIMITED 的固定 message（后端 auth 处理器），同理：
  // 中文映射已完整表达，且错误码已在 description 小字保留。
  'too many login attempts',
  // 连接/命令失败的兜底短语：后端修复后正常会带 ": <原因>" 后缀，
  // 仅当底层错误为空时才会是裸短语，此时不拼进文案。
  'ssh command failed',
  'ssh connection test failed',
  'winrm connection test failed',
  'database connection test failed',
  // 供应商测试/拉取模型同构兜底：后端修复后正常带 ": <原因>" 后缀
  // （dial 拒绝 / no such host / status 500 等），仅裸短语时不拼进文案。
  'provider request failed',
  'provider request timed out',
  // 中文裸短语同理（databases 查询超时固定 message）：
  // 中文映射 CODE_MESSAGES.SQL_TIMEOUT 已完整表达，拼进括号只会重复。
  '查询超时',
]);

export function describeFailure(failure: ApiFailure): string {
  const message = failure.message.trim();
  let localized = CODE_MESSAGES[failure.code];
  // 二期 2.7：连接测试按 os/auth_method 路由 SSH 或 WinRM，两通道同用
  // SERVER_TEST_FAILED（错误码不变，契约 9.6）；但弹窗文案按通道区分——
  // SSH 通道沿用一期打磨固化的「SSH 连接测试失败」（polish-round5/6 e2e
  // 锁定），WinRM 通道用「WinRM 连接测试失败」。后端 message 携带通道
  // 前缀（"ssh connection test failed: …" / "winrm connection test
  // failed: …"），据此选择通道化标题；无前缀时退回通用文案。
  // p2 sprint 3：识别到通道前缀后将其从括号内容中剥掉——标题已表达
  // 通道，保留前缀会出现「SSH 连接测试失败（ssh connection test
  // failed: 无法连接 …）」的重复；后端 message 已升级为中文分类短语
  // （无法连接 host:port：连接被拒绝…）开头，剥离后括号内直接从
  // 分类短语/底层原因开始。
  let detail = message;
  if (failure.code === 'SERVER_TEST_FAILED') {
    const lower = message.toLowerCase();
    if (lower.startsWith('ssh connection test failed')) {
      localized = 'SSH 连接测试失败';
      detail = message.slice('ssh connection test failed'.length);
    } else if (lower.startsWith('winrm connection test failed')) {
      localized = 'WinRM 连接测试失败';
      detail = message.slice('winrm connection test failed'.length);
    } else if (lower.startsWith('network device connection test failed')) {
      // 三期 p3 sprint 1：网络设备（device_kind=network_device）连接测试
      // 同用 SERVER_TEST_FAILED，后端 message 前缀为 "network device
      // connection test failed: …"（netcli 探针，servers.go
      // describeConnTestFailure），据此给出「网络设备连接测试失败」标题
      // 并剥离前缀——不剥离会出现「服务器连接测试失败（network device
      // connection test failed: 无法连接 …）」的中英重复（对齐 ssh/winrm
      // 的 sprint 3 处理）。
      localized = '网络设备连接测试失败';
      detail = message.slice('network device connection test failed'.length);
    }
    detail = detail.replace(/^[\s:：]+/, '');
  } else if (failure.code === 'DATABASE_TEST_FAILED') {
    // p2 sprint 5：后端 message 已升级为中文分类短语开头（无法连接
    // host:port：连接被拒绝…），剥离英文通道前缀——保留会出现「数据库
    // 连接测试失败（database connection test failed: 无法连接 …）」的
    // 中英重复（对齐 SERVER_TEST_FAILED 的 sprint 3 处理）。
    const prefix = 'database connection test failed';
    if (message.toLowerCase().startsWith(prefix)) {
      detail = message.slice(prefix.length).replace(/^[\s:：]+/, '');
    }
  }
  const informative = detail !== '' && !GENERIC_STORE_MESSAGES.has(detail.toLowerCase());
  if (!localized) {
    return informative ? detail : '请求失败';
  }
  return informative ? `${localized}（${detail}）` : localized;
}

// 冲刺 2 第 2 轮打磨：改密弹窗（ChangePasswordModal）只有旧/新/确认三个
// 密码字段，没有用户名输入；该场景下 AUTH_INVALID（"invalid credentials"）
// 必然来自旧密码校验失败（internal/auth 改密处理器），CODE_MESSAGES 的
// 「用户名或密码错误」会让用户去找不存在的用户名字段。这里按场景改写，
// 与登录页的 describeLoginFailure 互不影响。
export function describeChangePasswordFailure(failure: ApiFailure): string {
  if (failure.code !== 'AUTH_INVALID') {
    return describeFailure(failure);
  }
  const message = failure.message.trim();
  const informative = message !== '' && !GENERIC_STORE_MESSAGES.has(message.toLowerCase());
  return informative ? `旧密码不正确（${message}）` : '旧密码不正确';
}

// 冲刺 5 打磨：登录页（LoginPage）的用户名输入框 disabled 固定为内置
// admin，用户唯一可输入的凭据是管理员密码；该场景下 AUTH_INVALID 同样
// 只可能是密码错误，通用映射「用户名或密码错误」会让用户去找并不存在
// 的可编辑用户名字段。按场景改写为「管理员密码错误」，其余错误码走
// describeFailure 原映射。
export function describeLoginFailure(failure: ApiFailure): string {
  if (failure.code !== 'AUTH_INVALID') {
    return describeFailure(failure);
  }
  const message = failure.message.trim();
  const informative = message !== '' && !GENERIC_STORE_MESSAGES.has(message.toLowerCase());
  return informative ? `管理员密码错误（${message}）` : '管理员密码错误';
}

function normalizeFieldName(name: string): string {
  return name.replace(/[A-Z]/g, (char) => `_${char.toLowerCase()}`);
}

// 支持三种后端 details 形态：
// 1. { fields: { host: "..." } }  2. { host: "..." }  3. { field: "host", message: "..." }
export function fieldErrorsFrom(details: Record<string, unknown> | null): Record<string, string> {
  if (!details) {
    return {};
  }
  const source = isRecord(details.fields) ? details.fields : details;
  const result: Record<string, string> = {};
  for (const [rawKey, value] of Object.entries(source)) {
    const key = normalizeFieldName(rawKey);
    if (typeof value === 'string' && value.trim() !== '') {
      result[key] = value;
    } else if (Array.isArray(value) && value.length > 0) {
      const messages = value.filter((item): item is string => typeof item === 'string');
      if (messages.length === value.length) {
        result[key] = messages.join('；');
      }
    } else if (isRecord(value) && typeof value.message === 'string') {
      result[key] = value.message;
    }
  }
  if (
    typeof details.field === 'string' &&
    typeof details.message === 'string' &&
    !result[normalizeFieldName(details.field)]
  ) {
    result[normalizeFieldName(details.field)] = details.message;
  }
  return result;
}

export function applyFieldErrors(
  details: Record<string, unknown> | null,
  fields: readonly string[],
  setFields: (items: { name: string; errors: string[] }[]) => void,
): boolean {
  const errors = fieldErrorsFrom(details);
  const items = Object.entries(errors)
    .filter(([name]) => fields.includes(name))
    .map(([name, message]) => ({ name, errors: [message] }));
  if (items.length === 0) {
    return false;
  }
  setFields(items);
  return true;
}

// 写操作必须带 Origin（CONTRACT.md 9.6 ORIGIN_FORBIDDEN）。浏览器会以真实
// Origin 覆盖该头；这里传入是为了满足生成类型中 header 的必填约束。
export function writeHeaders(): { Origin: string } {
  return { Origin: window.location.origin };
}
