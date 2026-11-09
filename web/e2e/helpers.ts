import { expect, type Page } from '@playwright/test';

// 端到端测试共享常量与工具。globalSetup（e2e/global-setup.ts）使用同一
// ADMIN_PASSWORD 创建管理员。

export const ADMIN_PASSWORD = 'e2e-admin-pass-123';
export const AUTH_STATE_FILE = 'e2e/.auth/user.json';

export const SCENARIO_LIST = '__scenario_list__';
export const SCENARIO_RISKY = '__scenario_risky__';
export const SCENARIO_CODE = '__scenario_code__';
export const SCENARIO_ERROR = '__scenario_error__';
// 独立失败计数器的 error 场景（fake-llm 内 errorAltCalls）：与 SCENARIO_ERROR
// 的用例并行/先后执行时互不消耗对方的失败次数。
export const SCENARIO_ERROR_ALT = '__scenario_error_alt__';
export const SCENARIO_SLOW = '__scenario_slow__';
export const RISKY_COMMAND = 'rm -rf /tmp/aiops-e2e';
// 五期四条对话工具链场景（fake-llm：list_servers → get_metrics / list_alerts /
// batch_execute / run_diagnosis 两步链 + 最终文案书挡）。
export const SCENARIO_METRICS = '__scenario_metrics__';
export const SCENARIO_ALERTS = '__scenario_alerts__';
export const SCENARIO_BATCH = '__scenario_batch__';
export const SCENARIO_DIAGNOSIS = '__scenario_diagnosis__';
// 六期深度思考场景（fake-llm：先逐段 reasoning_content 再正文）。
export const SCENARIO_REASONING = '__scenario_reasoning__';

// 假 LLM（e2e/fixtures/fake-llm.mjs）list 场景的最终文本按 list_servers 工具
// 结果动态生成（数量/名称取决于当时库里的资产），无法用固定全文断言；
// 这里导出文案的开头/结尾书挡，必须与 fixture 保持一致。
export const LIST_FINAL_PREFIX = '资产清单核对完毕：当前共';
export const LIST_FINAL_SUFFIX = '以上即当前全部服务器资产。';
// 以下场景的最终文本是固定文案，必须与 fixture 保持一致。
export const RISKY_FINAL =
  '命令已按流程处理完毕。目标服务器 e2e-fake-host 在测试环境中不可达，' +
  '因此没有产生任何真实执行输出；审批与执行路径均已走通。';
export const CODE_FINAL =
  '排查磁盘占满时可以按下面的顺序执行：\n\n' +
  '```bash\nsystemctl status nginx\njournalctl -u nginx -n 100 --no-pager\n```\n\n' +
  '先确认服务状态，再看最近的日志。';
export const CODE_BLOCK_CONTENT = 'systemctl status nginx\njournalctl -u nginx -n 100 --no-pager';
export const ERROR_FINAL = '上游服务已恢复，这是重新生成后的正常回答。';
export const SLOW_FINAL =
  '这是一段刻意放慢的流式输出，用于验证停止生成与重新生成链路。'.repeat(4) +
  '慢速流的结尾标记：全部 token 发送完毕。';
// 五期工具链场景的最终文案书挡（中段动态，仅首尾固定，与 fixture 保持一致）。
export const METRICS_FINAL_PREFIX = '指标查询完毕：';
export const METRICS_FINAL_SUFFIX = '以上为该服务器的指标查询结论。';
export const ALERTS_FINAL_PREFIX = '告警查询完毕：';
export const ALERTS_FINAL_SUFFIX = '以上为当前告警事件查询结论。';
export const BATCH_FINAL_PREFIX = '批量任务已创建：';
export const BATCH_FINAL_SUFFIX = '以上为批量分派结论。';
export const DIAGNOSIS_FINAL_PREFIX = '诊断会话已结束：';
export const DIAGNOSIS_FINAL_SUFFIX = '以上为本次诊断结论。';
// reasoning 场景固定文案（与 fixture 保持一致）：思考全文（拼接后）与
// 正文书挡。
export const REASONING_FINAL = [
  '用户在排查磁盘占满问题。',
  '先确认资产清单锁定目标主机，',
  '再对目标主机执行 df -h 采集使用率证据，',
  '结合最近日志判断是日志膨胀还是数据增长，',
  '最后按使用率给出清理建议。',
].join('');
export const REASONING_BODY_PREFIX = '思考结论：';
// global-setup 启动后端时通过 AIOPS_APPROVAL_TTL 收紧的审批有效期。
export const APPROVAL_TTL_SECONDS = 45;
// fake-llm（e2e/fixtures/fake-llm.mjs）的 HTTP 端口，供测试直接查询其记录。
// 与 global-setup.ts 的 FAKE_LLM_PORT（18086）保持一致：避开冒烟模块
// 常驻 aiops-server 的 18081（门禁序列中两者先后运行，端口隔离防冲突）。
export const FAKE_LLM_URL = 'http://127.0.0.1:18086';
export const DEFAULT_FINAL =
  '这是一条来自假 LLM 的流式回复，用于验证 token 逐段渐进上屏的渲染链路。' +
  '每个 token 之间带少量延迟，方便端到端测试捕捉中间状态。';

export async function loginViaUI(page: Page, password: string) {
  await page.goto('/login');
  const input = page.getByPlaceholder('请输入管理员密码');
  await expect(input).toBeVisible();
  await input.fill(password);
  // antd 对纯双字文本按钮自动插空格（accessible name “登 录”），正则兼容。
  await page.getByRole('button', { name: /登\s*录/ }).click();
}

/** 等待 composer 就绪并发送一条消息。 */
export async function sendMessage(page: Page, text: string) {
  const box = page.getByLabel('消息输入框');
  await expect(box).toBeVisible();
  await expect(box).toBeEnabled();
  await box.fill(text);
  await page.getByRole('button', { name: '发送' }).click();
}

/** 从当前 /c/:id URL 提取会话 ID。 */
export function conversationIdOf(page: Page): string {
  const match = /\/c\/([0-9a-f-]+)/.exec(page.url());
  if (!match) {
    throw new Error(`not on a conversation page: ${page.url()}`);
  }
  return match[1];
}

/**
 * 安装渐进上屏采样器：记录 assistant 消息正文的 Markdown 容器
 * （.chat-message--assistant .opsmind-markdown，见 MessageItem/MessageList 的
 * LazyMarkdownView）出现过的所有 textContent 长度。测正文容器而非整个气泡：
 * 气泡内还含 “OpsMind” 等固定 meta 文本，会让长度恒大于正文。
 * 观察 document.body 而非 .chat-messages：发送消息后路由从 /chat 跳到
 * /c/:id，React 会渲染新的消息容器 DOM，旧容器上的 observer 会失效。
 * 返回值需在页面 reload 前读取（evaluate 内的窗口状态不跨导航保留）。
 */
export async function installProgressSampler(page: Page) {
  await page.evaluate(() => {
    const w = window as unknown as { __e2eLens?: Set<number> };
    w.__e2eLens = new Set<number>();
    const observer = new MutationObserver(() => {
      for (const el of document.querySelectorAll('.chat-message--assistant .opsmind-markdown')) {
        const len = (el.textContent ?? '').trim().length;
        if (len > 0) {
          w.__e2eLens?.add(len);
        }
      }
    });
    observer.observe(document.body, { subtree: true, childList: true, characterData: true });
  });
}

/** 读取渐进上屏采样结果（不同文本长度的个数与最大长度）。 */
export async function readProgressSamples(page: Page): Promise<{ count: number; max: number }> {
  return page.evaluate(() => {
    const w = window as unknown as { __e2eLens?: Set<number> };
    const lens = [...(w.__e2eLens ?? [])];
    return { count: lens.length, max: lens.length ? Math.max(...lens) : 0 };
  });
}
