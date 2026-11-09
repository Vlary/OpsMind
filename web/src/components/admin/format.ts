// FE2：资源管理页共用的纯展示格式化函数。

const dateTimeFormatter = new Intl.DateTimeFormat('zh-CN', {
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
});

export function formatDateTime(value: string | null | undefined): string {
  if (!value) {
    return '—';
  }
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) {
    return value;
  }
  return dateTimeFormatter.format(parsed);
}

export function formatDuration(milliseconds: number | null | undefined): string {
  if (milliseconds === null || milliseconds === undefined) {
    return '—';
  }
  if (milliseconds < 1000) {
    return `${milliseconds} ms`;
  }
  return `${(milliseconds / 1000).toFixed(2)} s`;
}

export function truncate(value: string, maxLength = 60): string {
  return value.length > maxLength ? `${value.slice(0, maxLength)}…` : value;
}

export function shortId(value: string | null | undefined, length = 8): string {
  if (!value) {
    return '—';
  }
  return value.length > length ? value.slice(0, length) : value;
}

export function formatCell(value: unknown): string {
  if (value === null || value === undefined) {
    return 'NULL';
  }
  if (typeof value === 'string') {
    return value;
  }
  if (typeof value === 'number' || typeof value === 'boolean' || typeof value === 'bigint') {
    return String(value);
  }
  try {
    const serialized = JSON.stringify(value);
    return serialized === undefined ? String(value) : serialized;
  } catch {
    return String(value);
  }
}

export function prettyJson(value: unknown): string {
  try {
    const serialized = JSON.stringify(value, null, 2);
    return serialized === undefined ? String(value) : serialized;
  } catch {
    return String(value);
  }
}

export function readString(
  source: Record<string, unknown> | null,
  keys: readonly string[],
): string | null {
  if (!source) {
    return null;
  }
  for (const key of keys) {
    const value = source[key];
    if (typeof value === 'string' && value !== '') {
      return value;
    }
  }
  return null;
}

export function readNumber(
  source: Record<string, unknown> | null,
  keys: readonly string[],
): number | null {
  if (!source) {
    return null;
  }
  for (const key of keys) {
    const value = source[key];
    if (typeof value === 'number' && Number.isFinite(value)) {
      return value;
    }
  }
  return null;
}

export function readBoolean(
  source: Record<string, unknown> | null,
  keys: readonly string[],
): boolean | null {
  if (!source) {
    return null;
  }
  for (const key of keys) {
    const value = source[key];
    if (typeof value === 'boolean') {
      return value;
    }
  }
  return null;
}
