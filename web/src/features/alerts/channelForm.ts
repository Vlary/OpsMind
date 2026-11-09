// M4 五期：通知渠道配置的纯逻辑（CONTRACT.md 5.15 四种 kind 的 config
// schema——webhook/飞书/钉钉 url+可选 secret；email smtp 全家桶）。
// 凭据永不回填（8.7 渠道表单）；留空=不修改（PATCH）。

export type ChannelKind = 'webhook' | 'feishu' | 'dingtalk' | 'email';

export const CHANNEL_KIND_LABELS: Record<ChannelKind, string> = {
  webhook: 'Webhook',
  feishu: '飞书',
  dingtalk: '钉钉',
  email: '邮件',
};

export interface UrlChannelFormValues {
  url: string;
  secret?: string;
}

export interface EmailChannelFormValues {
  smtp_host: string;
  smtp_port: number;
  username: string;
  password: string;
  from: string;
  to: string;
  use_tls: boolean;
}

export function isUrlKind(kind: ChannelKind): boolean {
  return kind === 'webhook' || kind === 'feishu' || kind === 'dingtalk';
}

function isHttpUrl(value: string): boolean {
  try {
    const parsed = new URL(value.trim());
    return parsed.protocol === 'http:' || parsed.protocol === 'https:';
  } catch {
    return false;
  }
}

/** url 类渠道配置校验/构造：url 必填且 http(s)；secret 可选。 */
export function buildUrlConfig(
  values: UrlChannelFormValues,
): { ok: true; config: Record<string, unknown> } | { ok: false; error: string } {
  if (!isHttpUrl(values.url)) {
    return { ok: false, error: '请输入合法的 http(s) URL' };
  }
  const config: Record<string, unknown> = {
    url: values.url.trim(),
    secret: values.secret?.trim() ?? '',
  };
  return { ok: true, config };
}

/** email 渠道配置校验/构造：host/from 必填、收件人 ≥1、端口 1..65535。 */
export function buildEmailConfig(
  values: EmailChannelFormValues,
): { ok: true; config: Record<string, unknown> } | { ok: false; error: string } {
  if (values.smtp_host.trim() === '') {
    return { ok: false, error: '请输入 SMTP 服务器地址' };
  }
  const port = Number(values.smtp_port);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    return { ok: false, error: 'SMTP 端口应为 1..65535' };
  }
  if (values.from.trim() === '' || !values.from.includes('@')) {
    return { ok: false, error: '请输入合法的发件人邮箱' };
  }
  const recipients = splitRecipients(values.to);
  if (recipients.length === 0) {
    return { ok: false, error: '请至少填写一个收件人' };
  }
  const config: Record<string, unknown> = {
    smtp_host: values.smtp_host.trim(),
    smtp_port: port,
    username: values.username.trim(),
    password: values.password,
    from: values.from.trim(),
    to: recipients,
    use_tls: values.use_tls,
  };
  return { ok: true, config };
}

/** 收件人多值输入：按逗号/分号/换行拆分、去空白去重保序。 */
export function splitRecipients(input: string): string[] {
  const seen = new Set<string>();
  const result: string[] = [];
  for (const raw of input.split(/[,;\n]/)) {
    const trimmed = raw.trim();
    if (trimmed !== '' && !seen.has(trimmed)) {
      seen.add(trimmed);
      result.push(trimmed);
    }
  }
  return result;
}

/** 测试投递结果文案（成功耗时 / 失败错误码中文）。 */
export function describeTestResult(
  result: { ok: boolean; error_code?: string; duration_ms: number },
  codeText: (code: string) => string,
): string {
  if (result.ok) {
    return `测试投递成功（${result.duration_ms} ms）`;
  }
  const code = result.error_code ?? 'NOTIFICATION_TEST_FAILED';
  return `测试投递失败：${codeText(code)}（${code}）`;
}
