import { describe, expect, it } from 'vitest';

// p2 sprint 4 打磨锁定：断开后会话列表的乐观更新（终端与 RDP 共用
// markSessionEnded）。修复前断开只 invalidateQueries 一次，服务端收尾
// （实测约 2.3s）前的 refetch 拿回 active 行，列表「进行中/录制中…」
// 与页头「已断开/已结束」矛盾残留 2-3 秒。
// p2 sprint 5 追加：乐观行的 bytes_written 置 BYTES_FINALIZING 占位——
// 缓存旧值 0 会让「录制大小」列显示误导性的「0 B」（走查实测 2-3 秒
// 后才被真实大小覆盖，用户误以为没录上）。
import { markSessionEnded, BYTES_FINALIZING, SESSION_END_REFETCH_DELAY_MS } from '../sessionList';

interface Row {
  id: string;
  server_name: string;
  status: 'active' | 'closed';
  has_recording: boolean;
  bytes_written: number;
  ended_at: string | null;
}

function rowOf(overrides: Partial<Row> = {}): Row {
  return {
    id: 's-1',
    server_name: 'win-e2e',
    status: 'active',
    has_recording: false,
    bytes_written: 0,
    ended_at: null,
    ...overrides,
  };
}

describe('markSessionEnded（断开乐观更新）', () => {
  it('把目标行置 closed 并写入结束时刻，录制标记原样保留', () => {
    const updated = markSessionEnded([rowOf()], 's-1', '2026-10-26T05:52:52.000Z');
    expect(updated).toHaveLength(1);
    expect(updated[0]).toMatchObject({
      id: 's-1',
      status: 'closed',
      ended_at: '2026-10-26T05:52:52.000Z',
      has_recording: false,
    });
  });

  it('bytes_written 置 BYTES_FINALIZING 占位（p2 sprint 5：不再显示过时的 0 B）', () => {
    const updated = markSessionEnded([rowOf()], 's-1', '2026-10-26T05:52:52.000Z');
    expect(updated[0]).toMatchObject({ bytes_written: BYTES_FINALIZING });
    expect(BYTES_FINALIZING).toBeLessThan(0);
  });

  it('只影响目标会话行，其余行（含其他 active 行）原样保留', () => {
    const updated = markSessionEnded(
      [rowOf({ id: 'other', server_name: 'lin-2' }), rowOf()],
      's-1',
      '2026-10-26T00:00:00Z',
    );
    expect(updated[0]).toMatchObject({ id: 'other', status: 'active', ended_at: null });
    expect(updated[1]).toMatchObject({ id: 's-1', status: 'closed' });
  });

  it('不修改入参数组（React Query 缓存不可变更新）', () => {
    const original = [rowOf()];
    markSessionEnded(original, 's-1', '2026-10-26T00:00:00Z');
    expect(original[0]).toMatchObject({ status: 'active', ended_at: null });
  });

  it('未命中（空列表或异 id）时返回内容不变的新数组', () => {
    const original: Row[] = [rowOf()];
    expect(markSessionEnded([], 's-1', 'now')).toEqual([]);
    expect(markSessionEnded(original, 'missing', 'now')).toEqual(original);
  });
});

describe('SESSION_END_REFETCH_DELAY_MS（延迟补偿刷新）', () => {
  it('不早于服务端收尾实测耗时（rdpx bridge 2s 超时 + 关行 ≈2.3s）', () => {
    // 若小于收尾耗时，refetch 拿回 active 行会盖掉乐观值、矛盾重现。
    expect(SESSION_END_REFETCH_DELAY_MS).toBeGreaterThanOrEqual(2500);
  });
});
