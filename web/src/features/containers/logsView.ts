// C2 四期：容器日志抽屉的纯逻辑（契约 8.7 容器页 / 5.2C logs 模板）。
// tail 取值 1..1000（0 视为缺省 200，与后端一致）；UI 固定 4 档。

export const TAIL_CHOICES = [100, 200, 500, 1000] as const;

export const DEFAULT_TAIL = 200;

/** tail 合法化：非有限数 / 越界（1..1000）回落缺省 200（后端同语义）。 */
export function normalizeTail(value: number | null | undefined): number {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    return DEFAULT_TAIL;
  }
  const rounded = Math.round(value);
  if (rounded < 1 || rounded > 1000) {
    return DEFAULT_TAIL;
  }
  return rounded;
}

/**
 * 日志原文按行拆分渲染（契约 8.7「输出按行渲染」）。空串返回 []（空态）；
 * 末尾换行不产生多余空行；中间空行保留（日志原文语义）。
 */
export function splitLogLines(output: string): string[] {
  if (output === '') {
    return [];
  }
  const lines = output.split('\n');
  if (lines.length > 0 && lines[lines.length - 1] === '') {
    lines.pop();
  }
  return lines;
}
