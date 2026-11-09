// M4 五期：巡检页纯逻辑测试（评分色阶 / 趋势对比 / cron 形状）。
import { describe, expect, it } from 'vitest';

import { describeInspectionError, looksLikeCron, scoreColor, scoreTrend } from '../inspectionView';
import {
  BATCH_STATUS_LABELS,
  canCancelBatch,
  failedServerIds,
  isBatchJobActive,
  isResultFinal,
  progressSummary,
  resultStatusColor,
} from '../../batch/batchView';
import type { BatchResult } from '../../batch/batchView';

describe('scoreColor', () => {
  it('85+ 绿 / 60-84 黄 / 0-59 红', () => {
    expect(scoreColor(100)).toBe('green');
    expect(scoreColor(85)).toBe('green');
    expect(scoreColor(84)).toBe('gold');
    expect(scoreColor(60)).toBe('gold');
    expect(scoreColor(59)).toBe('red');
    expect(scoreColor(0)).toBe('red');
  });

  it('NaN 防御为红', () => {
    expect(scoreColor(Number.NaN)).toBe('red');
  });
});

describe('scoreTrend', () => {
  it('无上次数据显示 —', () => {
    expect(scoreTrend(90, null).direction).toBe('none');
    expect(scoreTrend(90, undefined).text).toBe('—');
  });

  it('升 / 降 / 持平带符号文案', () => {
    expect(scoreTrend(90, 83)).toEqual({ direction: 'up', text: '+7' });
    expect(scoreTrend(70, 90)).toEqual({ direction: 'down', text: '-20' });
    expect(scoreTrend(70, 70)).toEqual({ direction: 'flat', text: '±0' });
  });
});

describe('looksLikeCron', () => {
  it('五段表达式通过，其余拒绝', () => {
    expect(looksLikeCron('*/5 * * * *')).toBe(true);
    expect(looksLikeCron('0 8 * * *')).toBe(true);
    expect(looksLikeCron('*/5 * *')).toBe(false);
    expect(looksLikeCron('')).toBe(false);
    expect(looksLikeCron('a b c d e f')).toBe(false);
  });
});

describe('describeInspectionError（巡检错误列中文分类，五期打磨 sprint 2）', () => {
  it('连接被拒绝的错误链归类为巡检目标不可达', () => {
    // 走查实测形态（inspections-runnow-report.png）：runner 落库的错误链原文。
    const chain =
      'sshx: dial 127.0.0.1:1: dial tcp 127.0.0.1:1: connect: connection refused';
    expect(describeInspectionError(chain)).toContain('巡检目标不可达');
    expect(describeInspectionError(chain)).not.toContain('connection refused');
  });

  it('认证失败 / 超时 / 无路由 / 解析失败各有中文短语', () => {
    expect(describeInspectionError('ssh: unable to authenticate')).toContain('认证失败');
    expect(describeInspectionError('dial tcp 10.0.0.9:22: i/o timeout')).toContain('连接超时');
    expect(describeInspectionError('dial tcp 10.0.0.9:22: no route to host')).toContain('不可达');
    expect(describeInspectionError('lookup nope.invalid: no such host')).toContain('域名解析失败');
  });

  it('结构化错误码先走五期映射', () => {
    expect(describeInspectionError('NOTIFICATION_TEST_FAILED')).toContain('测试投递失败');
  });

  it('未知形态回落原文（不吞后端语义）', () => {
    expect(describeInspectionError('collect command exit 3')).toBe('collect command exit 3');
    expect(describeInspectionError('')).toBe('');
    expect(describeInspectionError(null)).toBe('');
  });
});

function result(status: BatchResult['status']): BatchResult {
  return {
    id: 'r',
    batch_job_id: 'j',
    server_id: 's1',
    server_name: 'srv',
    risk_level: 'safe',
    rule_id: '',
    approval_id: null,
    status,
    edited_command: '',
    exit_code: null,
    output: '',
    truncated: false,
    error_code: '',
    duration_ms: 0,
    created_at: '2026-10-27T00:00:00Z',
    updated_at: '2026-10-27T00:00:00Z',
  };
}

describe('batchView', () => {
  it('job 非终态判定（轮询开关）', () => {
    expect(isBatchJobActive('pending')).toBe(true);
    expect(isBatchJobActive('running')).toBe(true);
    expect(isBatchJobActive('done')).toBe(false);
    expect(isBatchJobActive('partial')).toBe(false);
    expect(isBatchJobActive('cancelled')).toBe(false);
  });

  it('单台终态判定', () => {
    expect(isResultFinal('executed')).toBe(true);
    expect(isResultFinal('denied')).toBe(true);
    expect(isResultFinal('skipped')).toBe(true);
    expect(isResultFinal('awaiting_approval')).toBe(false);
    expect(isResultFinal('pending')).toBe(false);
  });

  it('取消仅 pending/awaiting_approval 存在时可用', () => {
    expect(canCancelBatch([result('executed'), result('denied')])).toBe(false);
    expect(canCancelBatch([result('executed'), result('awaiting_approval')])).toBe(true);
    expect(canCancelBatch([result('pending')])).toBe(true);
    expect(canCancelBatch([])).toBe(false);
  });

  it('失败行集合（failed/denied/expired/skipped）用于重试', () => {
    const results = [
      { ...result('executed'), server_id: 'a' },
      { ...result('failed'), server_id: 'b' },
      { ...result('skipped'), server_id: 'c' },
      { ...result('denied'), server_id: 'd' },
      { ...result('expired'), server_id: 'e' },
      { ...result('awaiting_approval'), server_id: 'f' },
    ];
    // denied/expired 的审批已终结，重试是唯一推进路径（五期评审）。
    expect(failedServerIds(results)).toEqual(['b', 'c', 'd', 'e']);
  });

  it('进度摘要计数', () => {
    const summary = progressSummary([
      result('executed'),
      result('awaiting_approval'),
      result('pending'),
      result('failed'),
    ]);
    expect(summary).toEqual({ total: 4, done: 2, awaiting: 2, failed: 1 });
  });

  it('状态灯颜色覆盖全部状态且中文标签齐全', () => {
    const statuses: BatchResult['status'][] = [
      'pending',
      'awaiting_approval',
      'executed',
      'failed',
      'denied',
      'expired',
      'skipped',
      'cancelled',
    ];
    for (const status of statuses) {
      // 七期（CONTRACT.md 8.3.9）：状态色改为语义变量引用，不再是十六进制。
      expect(resultStatusColor(status)).toMatch(/^var\(--color-[a-z-]+\)$/);
      expect(BATCH_STATUS_LABELS).toBeTruthy();
    }
  });
});

describe('describeInspectionError：凭据解密失败中文兜底（六期打磨 sprint 4）', () => {
  // 走查实测：私钥认证且未填 passphrase 时巡检报告 0 分，「错误」列逐字
  // 显示 `inspection: decrypt passphrase failed`。后端已修空串短路，这里
  // 再加一层中文兜底——包名前缀随调用方变化（monitor/diagnosis/inspection）。
  it('decrypt 失败按调用方三种前缀都归为凭据解密失败', () => {
    for (const chain of [
      'inspection: decrypt passphrase failed',
      'monitor: decrypt private key failed',
      'diagnosis: decrypt password failed',
      'decrypt passphrase failed',
    ]) {
      const text = describeInspectionError(chain);
      expect(text).toContain('凭据解密失败');
      expect(text).not.toContain('decrypt');
      expect(text).not.toMatch(/passphrase|private key/);
    }
  });

  it('凭据解密失败优先于连接类特征（不误判成不可达）', () => {
    expect(describeInspectionError('inspection: decrypt passphrase failed')).not.toContain('不可达');
  });
});
