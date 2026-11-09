// M4 五期：巡检页纯逻辑（评分色阶 0-59 红 / 60-84 黄 / 85+ 绿、与上次
// 巡检的趋势对比箭头文案；CONTRACT.md 8.7 巡检页）。node 直测。

import { describeResultCode } from '../alerts/phase5Codes';

export type ScoreColor = 'red' | 'gold' | 'green';

/** 评分色阶（85+ 绿 / 60-84 黄 / 0-59 红）。 */
export function scoreColor(score: number): ScoreColor {
  if (!Number.isFinite(score)) {
    return 'red';
  }
  if (score >= 85) {
    return 'green';
  }
  if (score >= 60) {
    return 'gold';
  }
  return 'red';
}

/**
 * 评分色阶 → 主题色变量（报告抽屉的 44px 大数字用；与 Tag 的
 * `scoreColor` 同源，避免大字恒绿色）。
 *
 * 六期打磨 sprint 6（走查 low）：报告详情抽屉的平均评分此前恒为绿色
 *（`--color-success`），30 分的低分报告也显示为绿色，与列表 Tag 的
 * >=85 绿 / >=60 黄 / 其余红分档矛盾，误导健康度判断。
 *
 * 七期：返回值改成语义变量的 `var(--color-…)` 引用——取值随主题走，
 * 组件内不再出现十六进制字面量（CONTRACT.md 8.3.1 / 8.3.9）。
 */
export const SCORE_COLOR_VAR: Record<ScoreColor, string> = {
  green: 'var(--color-success)',
  gold: 'var(--color-warning)',
  red: 'var(--color-danger)',
};

/** 评分 → 大数字主题色（`scoreColor` 的色值投影）。 */
export function scoreColorVar(score: number): string {
  return SCORE_COLOR_VAR[scoreColor(score)];
}

export interface ScoreTrend {
  /** 与上次比较：升 / 降 / 持平 / 无上次数据。 */
  direction: 'up' | 'down' | 'flat' | 'none';
  /** 展示文案（含符号），如 “+7（较上次）”。 */
  text: string;
}

/** 与上次评分对比（previous 为 null/undefined 表示无上次数据）。 */
export function scoreTrend(current: number, previous: number | null | undefined): ScoreTrend {
  if (previous === null || previous === undefined || !Number.isFinite(previous)) {
    return { direction: 'none', text: '—' };
  }
  const delta = current - previous;
  if (delta > 0) {
    return { direction: 'up', text: `+${delta}` };
  }
  if (delta < 0) {
    return { direction: 'down', text: `${delta}` };
  }
  return { direction: 'flat', text: '±0' };
}

export const TRIGGER_LABELS: Record<string, string> = {
  cron: '定时',
  manual: '手动',
  chat: '对话',
};

export const TEMPLATE_LABELS: Record<string, string> = {
  basic: '基础模板',
  full: '完整模板',
};

/** cron 表达式五段粗校验（后端 400 CRON_INVALID 前的前端即时反馈）。 */
export function looksLikeCron(value: string): boolean {
  const trimmed = value.trim();
  if (trimmed === '') {
    return false;
  }
  return trimmed.split(/\s+/).length === 5;
}

/**
 * 巡检明细「错误」列的中文文案（五期打磨 sprint 2）。
 *
 * 巡检 runner 落库的 error_code 是底层错误链原文（≤128 字节，如
 * "sshx: dial 127.0.0.1:1: dial tcp 127.0.0.1:1: connect: connection
 * refused"），此前直显英文链。与三期连接测试/查询失败的中文分类同口径
 * （termx/dialerrors.go 的特征匹配：只按错误文本特征归类，不依赖具体
 * 包名前缀）：
 * 1. 结构化错误码（NOTIFICATION_* 等）先走 describeResultCode 命中映射；
 * 2. 连接失败按特征归类为中文短语（附「巡检目标不可达」语境）；
 * 3. 未知形态回落原文（不吞后端语义）。
 */
export function describeInspectionError(code: string | null | undefined): string {
  if (!code) {
    return '';
  }
  const known = describeResultCode(code);
  if (known !== code) {
    return known;
  }
  const msg = code.toLowerCase();
  // 凭据解密失败（六期打磨 sprint 4）：后端各自动化路径的错误链形如
  // "inspection: decrypt passphrase failed" / "monitor: decrypt private key
  // failed" / "diagnosis: decrypt password failed"，包名前缀随调用方变化。
  // 走查实测曾把 `inspection: decrypt passphrase failed` 原文直出错误列。
  // 兜底为一句中文，指向「重新录入凭据」这一可行动作；具体是哪一项
  // （password / private key / passphrase）不展开，用户无需分辩。
  if (/decrypt\b.*\bfailed/.test(msg)) {
    return '凭据解密失败（请重新录入该服务器的登录凭据）';
  }
  if (msg.includes('unable to authenticate') || msg.includes('authentication failed')) {
    return 'SSH 认证失败（用户名或密码不正确）';
  }
  if (msg.includes('connection refused')) {
    return '巡检目标不可达（连接被拒绝，端口未开放或被防火墙拦截）';
  }
  if (msg.includes('no route to host') || msg.includes('unreachable')) {
    return '巡检目标不可达（无到目标主机的路由）';
  }
  if (msg.includes('no such host') || msg.includes('lookup ')) {
    return '域名解析失败';
  }
  if (msg.includes('timeout') || msg.includes('deadline exceeded')) {
    return '连接超时（目标未在时限内完成连接）';
  }
  return code;
}
