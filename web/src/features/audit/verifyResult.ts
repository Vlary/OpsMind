// 审计页「校验哈希链」的纯函数：解析 GET /api/audit/verify 的成功响应与
// 409 AUDIT_CHAIN_BROKEN 的 error.details（两者字段同名），并生成中文结论。
// 抽成独立模块便于单测（审计页本身没有 DOM 测试环境）。

export interface AuditVerifySummary {
  checked: number;
  legacy: number;
  headId: number;
  headHash: string;
  brokenAt: number | null;
  /**
   * 本次校验覆盖的最小审计 id（空区间/旧后端缺省时为 null）。
   *
   * 第 2 轮走查 low：/api/audit/verify 的窗口写死 200 条，页面却只写
   * 「已校验 N 条」——499 行的库显示「已校验 200 条」会被读成全链通过。
   * 有了 first_id/window，文案可以如实写出覆盖区间与「更早记录未校验」。
   */
  firstId: number | null;
  /** 本次生效的窗口上限（旧后端缺省时按 checked 推断）。 */
  window: number | null;
}

function readNumber(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null;
}

// parseVerifySummary 从成功响应对象或 error.details 读取校验摘要；
// 缺少 checked/head_id/head_hash 时返回 null（视为无法解析，不臆造 0）。
export function parseVerifySummary(source: unknown): AuditVerifySummary | null {
  if (typeof source !== 'object' || source === null || Array.isArray(source)) {
    return null;
  }
  const record = source as Record<string, unknown>;
  const checked = readNumber(record.checked);
  const headId = readNumber(record.head_id);
  const headHash = typeof record.head_hash === 'string' ? record.head_hash : '';
  if (checked === null || headId === null || headHash === '') {
    return null;
  }
  const firstId = readNumber(record.first_id);
  const window = readNumber(record.window);
  return {
    checked,
    legacy: readNumber(record.legacy) ?? 0,
    headId,
    headHash,
    brokenAt: readNumber(record.broken_at),
    // 0 表示空区间（没有可覆盖的行），与「字段缺失」区分开：true 的
    // 「覆盖区间」文案只应在 firstId>0 时出现。
    firstId: firstId === null ? null : firstId,
    window,
  };
}

/**
 * coverageText 返回本次校验的覆盖区间说明（形如「起始 id–链头 id」的区间）。
 * 空区间（firstId<=0）或旧后端未返回 first_id 时返回 null——
 * 不臆造区间。
 */
export function verifyCoverageText(summary: AuditVerifySummary): string | null {
  if (summary.firstId === null || summary.firstId <= 0) {
    return null;
  }
  return `#${summary.firstId}–#${summary.headId}`;
}

/**
 * earlierUnverified 报告是否存在更早、本次未覆盖的审计行。
 *
 * 判据：first_id > 1 即说明链上还有更早的行（审计 id 从 1 连续递增）。
 * 旧后端（无 first_id）返回 undefined，调用方据此不声称任何一侧。
 */
export function hasEarlierUnverified(summary: AuditVerifySummary): boolean | null {
  if (summary.firstId === null) {
    return null;
  }
  return summary.firstId > 1;
}

// verifyOkText 是链完整时的结论；legacy 行必须如实说明「只校验链接」，
// 不能把迁移前的存量行说成已复算通过。窗口有界时必须写出覆盖区间与
// 「更早记录未在本次范围内」，否则「已校验 200 条」会被读成全链通过。
export function verifyOkText(summary: AuditVerifySummary): string {
  const coverage = verifyCoverageText(summary);
  const earlier = hasEarlierUnverified(summary);
  let base: string;
  if (coverage) {
    base = `哈希链校验通过：已校验 ${summary.checked} 条（${coverage}），链头 #${summary.headId}`;
    if (earlier === true) {
      base += `；本次只是单次窗口（上限 ${summary.window ?? summary.checked} 条），更早记录（#1–#${(summary.firstId ?? 1) - 1}）未在本次校验范围内`;
    }
  } else {
    base = `哈希链校验通过：已校验 ${summary.checked} 条，链头 #${summary.headId}`;
  }

  if (summary.legacy > 0) {
    return `${base}；其中 ${summary.legacy} 条为迁移前历史记录，按 legacy 规则仅校验链接`;
  }
  return base;
}

// verifyBrokenText 是链断裂时的结论：断点 id 必须露出来（审计员据此定位）。
export function verifyBrokenText(summary: AuditVerifySummary | null): string {
  if (summary === null) {
    return '哈希链已破坏（服务端未返回断点信息）';
  }
  const at = summary.brokenAt === null ? '未知位置' : `#${summary.brokenAt}`;
  const coverage = verifyCoverageText(summary);
  const scope = coverage ? `（校验范围 ${coverage}）` : '';
  return `哈希链已破坏：断点 ${at}，已校验 ${summary.checked} 条${scope}`;
}
