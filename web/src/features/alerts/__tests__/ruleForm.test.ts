// M4 五期：告警规则表单纯逻辑测试（payload 转换 / 阈值域 / 清空语义）。
import { describe, expect, it } from 'vitest';

import {
  METRIC_LABELS,
  SUSPEND_RULE_NAME,
  clampThreshold,
  isPercentMetric,
  metricThresholdBounds,
  ruleDisplayName,
  toCreatePayload,
  toUpdatePayload,
  type RuleFormValues,
} from '../ruleForm';
import { describePhase5Failure, describeResultCode, PHASE5_CODE_MESSAGES } from '../phase5Codes';
import {
  buildEmailConfig,
  buildUrlConfig,
  describeTestResult,
  splitRecipients,
} from '../channelForm';

const baseValues: RuleFormValues = {
  name: '  CPU 高  ',
  metric: 'cpu_pct',
  comparator: 'gt',
  threshold: 90,
  duration_windows: 3,
  cooldown_seconds: 300,
  severity: 'warning',
  silent_until: null,
  clear_silent_until: false,
  notify_channel_id: null,
  clear_notify_channel: false,
  enabled: true,
};

describe('metricThresholdBounds / clampThreshold', () => {
  it('百分比指标 0..100，其他 ≥0 无上限', () => {
    expect(metricThresholdBounds('cpu_pct')).toEqual({ min: 0, max: 100 });
    expect(metricThresholdBounds('disk_pct').max).toBe(100);
    expect(metricThresholdBounds('load_1')).toEqual({ min: 0, max: null });
    expect(metricThresholdBounds('net_in_bps')).toEqual({ min: 0, max: null });
    expect(isPercentMetric('mem_pct')).toBe(true);
    expect(isPercentMetric('load_1')).toBe(false);
  });

  it('指标切换时越界阈值钳回合法域', () => {
    expect(clampThreshold('cpu_pct', 120)).toBe(100);
    expect(clampThreshold('cpu_pct', -5)).toBe(0);
    expect(clampThreshold('load_1', 120)).toBe(120);
    expect(clampThreshold('load_1', Number.NaN)).toBe(0);
  });

  it('六个指标全有中文可读名', () => {
    for (const label of Object.values(METRIC_LABELS)) {
      expect(label.length).toBeGreaterThan(0);
    }
    expect(Object.keys(METRIC_LABELS)).toHaveLength(6);
  });
});

describe('toCreatePayload', () => {
  it('名称去首尾空白；未绑定渠道/未静默时不携带字段', () => {
    const payload = toCreatePayload(baseValues);
    expect(payload.name).toBe('CPU 高');
    expect(payload.notify_channel_id).toBeUndefined();
    expect(payload.silent_until).toBeUndefined();
    expect(payload.metric).toBe('cpu_pct');
    expect(payload.enabled).toBe(true);
  });

  it('绑定渠道与静默时间时携带', () => {
    const payload = toCreatePayload({
      ...baseValues,
      notify_channel_id: 'ch-1',
      silent_until: '2026-11-01T00:00:00Z',
    });
    expect(payload.notify_channel_id).toBe('ch-1');
    expect(payload.silent_until).toBe('2026-11-01T00:00:00Z');
  });
});

describe('toUpdatePayload', () => {
  it('清空静默走 clear_silent_until 布尔', () => {
    const payload = toUpdatePayload({ ...baseValues, clear_silent_until: true });
    expect(payload.clear_silent_until).toBe(true);
    expect(payload.silent_until).toBeUndefined();
  });

  it('清空渠道走 clear_notify_channel 布尔', () => {
    const payload = toUpdatePayload({ ...baseValues, clear_notify_channel: true });
    expect(payload.clear_notify_channel).toBe(true);
    expect(payload.notify_channel_id).toBeUndefined();
  });

  it('选择渠道时直接带 id；两者都不动时不携带', () => {
    expect(toUpdatePayload({ ...baseValues, notify_channel_id: 'ch-2' }).notify_channel_id).toBe(
      'ch-2',
    );
    expect(toUpdatePayload(baseValues).notify_channel_id).toBeUndefined();
  });

  it('静默与渠道可同时清空', () => {
    const payload = toUpdatePayload({
      ...baseValues,
      clear_silent_until: true,
      clear_notify_channel: true,
    });
    expect(payload.clear_silent_until).toBe(true);
    expect(payload.clear_notify_channel).toBe(true);
  });
});

describe('phase5Codes', () => {
  it('五期错误码命中中文映射', () => {
    expect(PHASE5_CODE_MESSAGES['CRON_INVALID']).toContain('cron');
    expect(PHASE5_CODE_MESSAGES['BATCH_TOO_MANY_TARGETS']).toContain('50');
    expect(PHASE5_CODE_MESSAGES['DIAGNOSIS_BUSY']).toBeTruthy();
  });

  it('诊断引擎终态错误码有中文映射（抽屉错误列不直显英文原码）', () => {
    // 五期打磨 sprint 2：DIAGNOSIS_MAX_ROUNDS / DIAGNOSIS_TIMEOUT 曾缺失，
    // 诊断抽屉错误列直显英文原码（走查 chat-diagnosis-drawer.png）。
    expect(PHASE5_CODE_MESSAGES['DIAGNOSIS_MAX_ROUNDS']).toContain('轮数');
    expect(PHASE5_CODE_MESSAGES['DIAGNOSIS_TIMEOUT']).toContain('超时');
    expect(describeResultCode('DIAGNOSIS_MAX_ROUNDS')).not.toBe('DIAGNOSIS_MAX_ROUNDS');
  });

  it('批量通道错误码不直显英文原码（sprint 4：抽屉错误列与 decide toast）', () => {
    // 回落通用 CODE_MESSAGES（一期口径全站一致，不复制文案防漂移）。
    expect(describeResultCode('SSH_EXEC_FAILED')).toBe('远程命令执行失败');
    expect(describeResultCode('SERVER_NOT_FOUND')).toBe('服务器不存在');
    expect(describeResultCode('SSH_TIMEOUT')).toBe('SSH 连接超时');
    expect(describeResultCode('SSH_HOST_KEY_CHANGED')).toBe('主机密钥已变化，连接被阻止');
    expect(describeResultCode('COMMAND_FORBIDDEN')).toBe('该命令被风险规则禁止');
    // 通用表没有的批量专属码（internal/batchx service.go errCode* 集合）。
    expect(describeResultCode('NETCLI_EXEC_FAILED')).toContain('网络设备命令执行失败');
    expect(describeResultCode('EXEC_INTERRUPTED')).toContain('中断');
    // 真未知码仍原样返回（不吞后端语义）。
    expect(describeResultCode('SOME_FUTURE_CODE')).toBe('SOME_FUTURE_CODE');
    expect(describeResultCode(null)).toBe('');
  });

  it('describePhase5Failure 未知码回落通用文案', () => {
    const text = describePhase5Failure({
      code: 'SOME_NEW_CODE',
      message: 'boom',
      requestId: '',
      details: null,
      status: null,
    });
    expect(text).toBe('boom');
  });

  it('describePhase5Failure 已知通用码走 CODE_MESSAGES 中文', () => {
    const text = describePhase5Failure({
      code: 'AUTH_REQUIRED',
      message: 'x',
      requestId: '',
      details: null,
      status: null,
    });
    expect(text).toContain('登录');
  });
});

describe('channelForm', () => {
  it('url 渠道校验 http(s) 且构造 config', () => {
    expect(buildUrlConfig({ url: 'https://a.example.com/hook', secret: ' s ' })).toEqual({
      ok: true,
      config: { url: 'https://a.example.com/hook', secret: 's' },
    });
    expect(buildUrlConfig({ url: 'ftp://x', secret: '' }).ok).toBe(false);
    expect(buildUrlConfig({ url: 'not a url', secret: '' }).ok).toBe(false);
  });

  it('email 渠道端口/发件人/收件人校验', () => {
    const ok = buildEmailConfig({
      smtp_host: 'smtp.example.com',
      smtp_port: 465,
      username: 'u',
      password: 'p',
      from: 'ops@example.com',
      to: 'a@example.com, b@example.com',
      use_tls: true,
    });
    expect(ok.ok).toBe(true);
    if (ok.ok) {
      expect(ok.config['to']).toEqual(['a@example.com', 'b@example.com']);
      expect(ok.config['use_tls']).toBe(true);
    }
    expect(
      buildEmailConfig({
        smtp_host: '',
        smtp_port: 465,
        username: '',
        password: '',
        from: '',
        to: '',
        use_tls: false,
      }).ok,
    ).toBe(false);
    expect(
      buildEmailConfig({
        smtp_host: 'h',
        smtp_port: 70000,
        username: '',
        password: '',
        from: 'ops@example.com',
        to: 'a@example.com',
        use_tls: false,
      }).ok,
    ).toBe(false);
  });

  it('收件人拆分：多分隔符、去重、去空白、保序', () => {
    expect(splitRecipients('a@x.com, b@x.com; c@x.com\n a@x.com ')).toEqual([
      'a@x.com',
      'b@x.com',
      'c@x.com',
    ]);
    expect(splitRecipients('   ')).toEqual([]);
  });

  it('测试结果文案：成功耗时 / 失败错误码中文', () => {
    expect(describeTestResult({ ok: true, duration_ms: 120 }, (code) => code)).toContain('120 ms');
    const fail = describeTestResult(
      { ok: false, error_code: 'NOTIFICATION_TEST_FAILED', duration_ms: 50 },
      (code) => PHASE5_CODE_MESSAGES[code] ?? code,
    );
    expect(fail).toContain('测试投递失败');
    expect(fail).toContain('通知渠道测试投递失败');
  });
});

// 六期打磨 sprint 4（走查 low）：「采集不可达」系统哨兵规则的英文原名在
// 告警页规则行/事件行直出（monitor.collector.unreachable），是全站中文 UI
// 里唯一直出的英文业务文案。规则名保持英文（数据库键 + 前端识别），
// 展示名由 ruleDisplayName 映射；未知规则名原样返回。
describe('ruleDisplayName（系统哨兵规则中文展示名，p6 sprint 4）', () => {
  it('系统哨兵规则映射「采集不可达（系统）」', () => {
    expect(ruleDisplayName(SUSPEND_RULE_NAME)).toBe('采集不可达（系统）');
    expect(SUSPEND_RULE_NAME).toBe('monitor.collector.unreachable');
  });

  it('用户自定义规则名原样返回（不吞用户命名）', () => {
    expect(ruleDisplayName('CPU 过高')).toBe('CPU 过高');
    expect(ruleDisplayName('web-1 disk full')).toBe('web-1 disk full');
    expect(ruleDisplayName('')).toBe('');
  });

  it('展示名与英文原名不同（Tooltip 有内容可显示）', () => {
    expect(ruleDisplayName(SUSPEND_RULE_NAME)).not.toBe(SUSPEND_RULE_NAME);
  });
});
