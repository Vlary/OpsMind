// 图表组件四态冒烟（node 环境用 react-dom/server 静态渲染，不需要 jsdom）：
// 空态、加载态、错误态、单点态都必须出确定文案且不抛异常；根 svg 带
// role="img" 与描述范围/口径的 aria-label（本轮约束 3、4）。
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import MetricChart from '../../MetricChart';
import {
  estimateAxisLabelWidth,
  formatBps,
  formatNumericAxisTick,
  formatSignificant,
} from '../../chartMath';
import AlertSeverityStackedBarChart from '../AlertSeverityStackedBarChart';
import ClusterTrendChart from '../ClusterTrendChart';
import { estimateTextWidth } from '../chartModel';
import InspectionScoreTrendChart from '../InspectionScoreTrendChart';
import NoisyRuleBarChart from '../NoisyRuleBarChart';
import TopServerBarChart from '../TopServerBarChart';

const SINGLE_SERVER = {
  server_id: 'srv-1',
  server_name: 'web-01',
  cpu_pct: 87.5,
  mem_pct: 62,
  disk_pct: 71,
  created_at: '2026-11-19T10:00:00Z',
};

describe('TopServerBarChart 四态', () => {
  it('空态、加载态、错误态都有明确占位', () => {
    expect(renderToStaticMarkup(<TopServerBarChart servers={[]} />)).toContain('chart-empty');
    expect(renderToStaticMarkup(<TopServerBarChart servers={[]} loading />)).toContain(
      'chart-loading',
    );
    expect(renderToStaticMarkup(<TopServerBarChart servers={[]} error="加载失败" />)).toContain(
      'chart-error',
    );
  });

  it('窄屏（fallback 420px）同前缀机器保留尾部编号，不再截成 batch-0…', () => {
    const rows = ['batch-04', 'batch-05'].map((name, index) => ({
      server_id: `srv-${index}`,
      server_name: name,
      cpu_pct: 90 - index,
      mem_pct: 60,
      disk_pct: 50,
      created_at: '2026-11-19T10:00:00Z',
    }));
    const html = renderToStaticMarkup(<TopServerBarChart servers={rows} />);
    // 可见刻度文本必须是完整名（aria-label 本来就含全名，不能只断言它）。
    expect(html).toContain('>batch-04</text>');
    expect(html).toContain('>batch-05</text>');
    expect(html).not.toContain('batch-0…');
  });

  it('窄屏 11 码点下 prod-cache-01 / prod-idle-01 显示为不同标签', () => {
    // 走查第 4 轮 medium：9 码点时两者都截成「prod…e-01」，运维无法区分。
    // 第 12 轮窄屏左列加宽到 88px（非过期预算 76px → 12 个单位），13 字符
    // 名保留 6 字符尾部，cache/idle 仍可区分。
    const rows = ['prod-cache-01', 'prod-idle-01'].map((name, index) => ({
      server_id: `srv-${index}`,
      server_name: name,
      cpu_pct: 90 - index,
      mem_pct: 60,
      disk_pct: 50,
      created_at: '2026-11-19T10:00:00Z',
    }));
    const html = renderToStaticMarkup(<TopServerBarChart servers={rows} />);
    expect(html).toContain('>prod-…che-01</text>');
    expect(html).toContain('>prod-idle-01</text>');
    expect(html).not.toContain('>prod…e-01</text>');
  });

  it('中文长名按像素宽度截断，fallback 420 下左缘不再越界', () => {
    // 走查第 6 轮 medium：11 个汉字宽 121px，而窄屏左列只有 68px。
    // fallback 420 → padLeft 76、可用 66px；尾部区域整段优先保留。
    const html = renderToStaticMarkup(
      <TopServerBarChart
        servers={[
          {
            server_id: 'srv-cjk',
            server_name: '生产数据库主节点-华东一区',
            cpu_pct: 92,
            mem_pct: 60,
            disk_pct: 50,
            created_at: '2026-11-20T02:00:00Z',
          },
        ]}
      />,
    );
    expect(html).toContain('>生…华东一区</text>');
    expect(estimateTextWidth('生…华东一区')).toBeLessThanOrEqual(66);
    expect(html).not.toContain('生产数据库主节点-华东一区</text>');
  });

  it('单点态可渲染，aria-label 含 Top、指标与 0–100 轴', () => {
    const html = renderToStaticMarkup(<TopServerBarChart servers={[SINGLE_SERVER]} />);
    expect(html).toContain('<svg');
    expect(html).toContain('aria-label="最紧张服务器 Top 1');
    expect(html).toContain('CPU 使用率');
    expect(html).toContain('0 到 100%');
    expect(html).toContain('87.5%');
    expect(html).toContain('tabindex="0"');
  });

  it('外部过期阈值（按采集间隔推导）进入副文案', () => {
    const html = renderToStaticMarkup(
      <TopServerBarChart servers={[SINGLE_SERVER]} staleAfterMs={1_200_000} now={Date.now()} />,
    );
    expect(html).toContain('采样超 20 分钟标为过期');
  });
});

describe('ClusterTrendChart 四态', () => {
  it('空态/加载态/错误态', () => {
    expect(renderToStaticMarkup(<ClusterTrendChart points={[]} />)).toContain('chart-empty');
    expect(renderToStaticMarkup(<ClusterTrendChart points={[]} loading />)).toContain(
      'chart-loading',
    );
    expect(renderToStaticMarkup(<ClusterTrendChart points={[]} error="加载失败" />)).toContain(
      'chart-error',
    );
  });

  it('满载 99.25% 的百分比轴封顶 100%，不再扩到 200%', () => {
    const html = renderToStaticMarkup(
      <ClusterTrendChart
        points={[
          {
            bucket_start: '2026-11-19T11:00:00Z',
            cpu_p95: 99.25,
            cpu_avg: 42.5,
            cpu_max: 100,
            servers: 3,
            samples: 180,
          },
        ]}
      />,
    );
    expect(html).toContain('纵轴 0 到 100%');
    expect(html).toContain('100%');
    expect(html).not.toContain('150%');
    expect(html).not.toContain('200%');
  });

  it('全部桶为 null（采集停摆）时显示「窗口内暂无样本」空态', () => {
    const html = renderToStaticMarkup(
      <ClusterTrendChart
        points={[
          {
            bucket_start: '2026-11-19T10:00:00Z',
            cpu_p95: null,
            cpu_avg: null,
            cpu_max: null,
            servers: 0,
            samples: 0,
          },
          {
            bucket_start: '2026-11-19T11:00:00Z',
            cpu_p95: null,
            cpu_avg: null,
            cpu_max: null,
            servers: 0,
            samples: 0,
          },
        ]}
      />,
    );
    expect(html).toContain('chart-empty');
    expect(html).toContain('窗口内暂无样本');
    expect(html).not.toContain('<svg');
  });

  it('单桶可渲染，aria-label 含窗口、桶宽与桶数', () => {
    const html = renderToStaticMarkup(
      <ClusterTrendChart
        points={[
          {
            bucket_start: '2026-11-19T10:00:00Z',
            cpu_p95: 55,
            cpu_avg: 40,
            cpu_max: 70,
            servers: 3,
            samples: 180,
          },
        ]}
      />,
    );
    expect(html).toContain('<svg');
    expect(html).toContain('aria-label="集群压力分桶趋势');
    expect(html).toContain('近 24 小时');
    expect(html).toContain('每桶 1 小时');
    expect(html).toContain('共 1 个桶');
    expect(html).toContain('tabindex="0"');
    // 第 11 轮 medium：根 svg 按触点 x 取最近桶；几何标记覆盖整张绘图区
    // 且不拦截 pointer，每桶矩形保留键盘/读屏能力。
    expect(html).toMatch(/<rect data-testid="cluster-trend-hit-surface"[^>]*pointer-events="none"/);
    expect(html).toMatch(/<rect[^>]*tabindex="0"(?![^>]*pointer-events="none")/);
  });

  // 走查第 16 轮 medium：后端已按服务端 now 排除超前机器的未来样本，
  // 副标题必须说明有几台未参与，避免趋势台数与 Top 12 对不上。
  it('时钟超前机器数非零时说明未参与趋势统计，为零时不提示', () => {
    const point = {
      bucket_start: '2026-11-19T10:00:00Z',
      cpu_p95: 55,
      cpu_avg: 40,
      cpu_max: 70,
      servers: 6,
      samples: 180,
    };
    const html = renderToStaticMarkup(
      <ClusterTrendChart points={[point]} futureSkewedServers={1} />,
    );
    expect(html).toContain('data-testid="cluster-trend-future-note"');
    expect(html).toContain('1 台采样时间超前服务端时钟，未参与趋势统计（与 Top 12 同口径）');

    const none = renderToStaticMarkup(<ClusterTrendChart points={[point]} />);
    expect(none).not.toContain('cluster-trend-future-note');
  });
});

/** 取集群趋势 x 轴标签的 x/锚点/文案。 */
function clusterAxisTicks(html: string): Array<{ x: number; anchor: string; text: string }> {
  const axis = /<g data-testid="cluster-trend-x-axis">([\s\S]*?)<\/g>/.exec(html)?.[1] ?? '';
  return [...axis.matchAll(/<text ([^>]*)>([^<]+)<\/text>/g)].map((match) => {
    const attrs = match[1] ?? '';
    return {
      x: Number(/x="([\d.]+)"/.exec(attrs)?.[1] ?? Number.NaN),
      anchor: /text-anchor="([^"]+)"/.exec(attrs)?.[1] ?? '',
      text: match[2] ?? '',
    };
  });
}

function trendBuckets(count: number, startMs: number, bucketMs: number, nonEmptyFrom = -1) {
  return Array.from({ length: count }, (_, index) => {
    const empty = nonEmptyFrom >= 0 && index < nonEmptyFrom;
    return {
      bucket_start: new Date(startMs + index * bucketMs).toISOString(),
      cpu_p95: empty ? null : 40 + index,
      cpu_avg: empty ? null : 30 + index,
      cpu_max: empty ? null : 50 + index,
      servers: empty ? 0 : 2,
      samples: empty ? 0 : 120,
    };
  });
}

describe('ClusterTrendChart 末刻度贴边（第 15 轮 low）', () => {
  it('24 桶 1440 下：末标签改 end 锚点，右缘不越出 SVG', () => {
    // 走查第 15 轮证据：1440 视图 svgWidth=554，最后标签 x=533.5、宽 54，
    // 右缘 561 被裁掉。fallback 560 下末标签同样放不下，必须贴边。
    const start = Date.parse('2026-11-20T02:00:00Z');
    const html = renderToStaticMarkup(
      <ClusterTrendChart points={trendBuckets(24, start, 3_600_000)} window="24h" />,
    );
    const ticks = clusterAxisTicks(html);
    const first = ticks[0];
    const last = ticks[ticks.length - 1];
    expect(first?.anchor).toBe('start');
    expect(first?.x).toBeCloseTo(40, 5);
    expect(last?.anchor).toBe('end');
    expect(last?.x).toBeCloseTo(560 - 10, 5);
    // end 锚点的 x 就是文本右缘；所有刻度都不能越出 560。
    for (const tick of ticks) {
      const width = estimateTextWidth(tick.text, 10);
      let left = tick.x;
      let right = tick.x;
      if (tick.anchor === 'end') {
        left = tick.x - width;
      } else if (tick.anchor === 'middle') {
        left = tick.x - width / 2;
        right = tick.x + width / 2;
      } else {
        right = tick.x + width;
      }
      expect(left).toBeGreaterThanOrEqual(0);
      expect(right).toBeLessThanOrEqual(560);
    }
  });
});

describe('ClusterTrendChart 部分空桶覆盖提示（第 15 轮 low）', () => {
  it('7d 的 28 桶里只有 7 桶有样本：数据跨度用实际首末采样（1.5 天，不是桶跨度 1.8 天）', () => {
    // 走查第 17 轮证据：7 个 6 小时桶的桶跨度是 1.75 天（显示 1.8 天），
    // 但实际样本只跨 1 天 12:40:20（1.53 天）。接口附带 first_sample /
    // last_sample 后必须展示真实跨度。
    const start = Date.parse('2026-11-14T02:00:00Z');
    const points = trendBuckets(28, start, 6 * 3_600_000, 21);
    const firstSample = new Date(start + 21 * 6 * 3_600_000 + 30 * 60_000).toISOString();
    const lastSample = new Date(
      Date.parse(firstSample) + (36 * 60 + 40) * 60_000 + 20_000,
    ).toISOString();
    const html = renderToStaticMarkup(
      <ClusterTrendChart
        points={points}
        window="7d"
        firstSample={firstSample}
        lastSample={lastSample}
      />,
    );
    expect(html).toContain('data-testid="cluster-trend-coverage-note"');
    expect(html).toContain('有样本 7/28 桶');
    expect(html).toContain('数据跨度 1.5 天');
    expect(html).not.toContain('1.8 天');
    expect(html).toContain('空桶可能是服务刚上线');
  });

  it('缺少 first_sample/last_sample 时退回桶跨度并写明首末桶未满', () => {
    // 走查第 15 轮证据：7d API 返回 {n:28,nn:6}，图上只有右侧 6 桶有
    // 折线，其余 22 个空桶没有任何解释，值班员无法区分「刚上线」与
    // 「停采 6 天」。
    const start = Date.parse('2026-11-14T02:00:00Z');
    const points = trendBuckets(28, start, 6 * 3_600_000, 22);
    const html = renderToStaticMarkup(<ClusterTrendChart points={points} window="7d" />);
    expect(html).toContain('data-testid="cluster-trend-coverage-note"');
    expect(html).toContain('有样本 6/28 桶');
    // 6 个 6 小时桶 = 36 小时 → formatCoverageDuration 跨天读作 1.5 天。
    expect(html).toContain('覆盖桶跨度约 1.5 天（首末桶未满）');
    expect(html).not.toContain('数据跨度');
    expect(html).toContain('空桶可能是服务刚上线');
  });

  it('全部 28 桶有样本时不显示覆盖提示', () => {
    const start = Date.parse('2026-11-14T02:00:00Z');
    const html = renderToStaticMarkup(
      <ClusterTrendChart points={trendBuckets(28, start, 6 * 3_600_000)} window="7d" />,
    );
    expect(html).not.toContain('cluster-trend-coverage-note');
  });

  it('全空仍走空态，不叠加 0/28 覆盖提示', () => {
    const start = Date.parse('2026-11-14T02:00:00Z');
    const html = renderToStaticMarkup(
      <ClusterTrendChart points={trendBuckets(28, start, 6 * 3_600_000, 28)} window="7d" />,
    );
    expect(html).toContain('chart-empty');
    expect(html).not.toContain('cluster-trend-coverage-note');
  });
});

describe('TopServerBarChart 过期零值（第 8 轮 medium）', () => {
  const now = Date.parse('2026-11-20T08:55:00Z');
  const rows = [
    {
      server_id: 'idle-id',
      server_name: 'idle-01',
      cpu_pct: 0,
      mem_pct: 0,
      disk_pct: 0,
      // 采样过期约 9 小时 15 分钟。
      created_at: '2026-11-19T23:39:47Z',
    },
    {
      server_id: 'batch-id',
      server_name: 'batch-04',
      cpu_pct: 0,
      mem_pct: 0,
      disk_pct: 0,
      // 刚刚采到（恰好 5 分钟，未严格超过阈值）。
      created_at: '2026-11-20T08:50:00Z',
    },
  ];

  it('0.0% 的过期机器仍有固定大小的过期标记与 warning 色文字', () => {
    const html = renderToStaticMarkup(<TopServerBarChart servers={rows} now={now} />);
    // 两行柱体宽度都是 0，旧版唯一能区分过期与否的柱体样式完全不可见。
    expect([...html.matchAll(/<rect[^>]*width="0"/g)]).toHaveLength(2);
    // 固定大小的行首标记只出现在过期行。
    expect([...html.matchAll(/data-testid="top-server-stale-marker"/g)]).toHaveLength(1);
    expect(html).toMatch(/<text[^>]*fill="var\(--color-warning\)"[^>]*>idle-01<\/text>/);
    expect(html).toMatch(/<text[^>]*fill="var\(--color-text-secondary\)"[^>]*>batch-04<\/text>/);
    // 读屏文案仍带过期时长。
    expect(html).toContain('已过期 9 小时');
  });

  it('未过期机器不显示过期标记与 warning 色文字', () => {
    const fresh = rows.filter((row) => row.server_name === 'batch-04');
    const html = renderToStaticMarkup(<TopServerBarChart servers={fresh} now={now} />);
    expect(html).not.toContain('top-server-stale-marker');
    expect(html).not.toMatch(/<text[^>]*fill="var\(--color-warning\)"/);
  });

  it('过期行标签再让 12px 给行首圆点，11 字符名不压到 3–9px', () => {
    // 走查第 12 轮 low：390 宽下 web-prod-01 的 66.05px 标签左缘 1.95px，
    // 与 cx=6/r=3 的过期圆点重叠。过期行预算 88-12-12=64px → 10 个单位，
    // 未过期行 88-12=76px → 12 个单位（完整显示）。
    const html = renderToStaticMarkup(
      <TopServerBarChart
        servers={[
          {
            server_id: 'stale-web',
            server_name: 'web-prod-01',
            cpu_pct: 0,
            mem_pct: 0,
            disk_pct: 0,
            created_at: '2026-11-19T23:39:47Z',
          },
          {
            server_id: 'fresh-web',
            server_name: 'web-prod-02',
            cpu_pct: 0,
            mem_pct: 0,
            disk_pct: 0,
            created_at: '2026-11-20T08:50:00Z',
          },
        ]}
        now={now}
      />,
    );
    expect(html).toMatch(/<text[^>]*fill="var\(--color-warning\)"[^>]*>web-…od-01<\/text>/);
    expect(html).toMatch(/<text[^>]*fill="var\(--color-text-secondary\)"[^>]*>web-prod-02<\/text>/);
    // 读屏/触屏 tooltip 仍能拿到完整机器名。
    expect(html).toContain('web-prod-01');
  });
});

describe('MetricChart 百分比轴', () => {
  const points = [
    { t: Date.parse('2026-11-19T10:00:00Z'), v: 42 },
    { t: Date.parse('2026-11-19T11:00:00Z'), v: 99.25 },
  ];

  it('percentAxis 时 y 轴封顶 100%（单机 CPU/内存/磁盘卡）', () => {
    const html = renderToStaticMarkup(<MetricChart points={points} percentAxis />);
    expect(html).toContain('100.0%');
    expect(html).not.toContain('200.0%');
  });

  it('非百分比指标（负载/网络）保持自动量纲', () => {
    const html = renderToStaticMarkup(
      <MetricChart
        points={[
          { t: Date.parse('2026-11-19T10:00:00Z'), v: 120 },
          { t: Date.parse('2026-11-19T11:00:00Z'), v: 130 },
        ]}
      />,
    );
    // 130*1.1=143 → niceCeil 200：无上界序列仍允许扩张。
    expect(html).toContain('200.0%');
  });

  it('20 GB/s 的 y 轴标签左缘不为负（不再把 20 裁成 0）', () => {
    const html = renderToStaticMarkup(
      <MetricChart
        label="网络入"
        formatValue={formatBps}
        points={[
          { t: Date.parse('2026-11-19T10:00:00Z'), v: 1_000_000_000 },
          { t: Date.parse('2026-11-19T10:01:00Z'), v: 9_876_543_210 },
        ]}
      />,
    );
    const ticks = [
      ...html.matchAll(
        /<text x="([\d.]+)"[^>]*data-testid="metric-chart-y-tick"[^>]*>([^<]+)<\/text>/g,
      ),
    ].map((match) => ({ x: Number(match[1]), label: match[2] ?? '' }));
    expect(ticks.length).toBe(5);
    // 走查第 7 轮 low：轴与卡头共用带 B 的完整单位（不再丢 B 成 20G/s）。
    expect(ticks.map((tick) => tick.label)).toContain('20.00 GB/s');
    expect(ticks.map((tick) => tick.label)).not.toContain('20G/s');
    for (const tick of ticks) {
      // x 是 textAnchor=end 的右缘；左缘 = x - 估算宽，必须 >= 0。
      expect(tick.x - estimateAxisLabelWidth(tick.label)).toBeGreaterThanOrEqual(0);
    }
  });

  it('svg 可聚焦，aria-label/title 含指标、窗口与最新/最小/最大', () => {
    const html = renderToStaticMarkup(
      <MetricChart label="CPU 使用率" points={points} percentAxis />,
    );
    expect(html).toContain('role="img"');
    expect(html).toContain('tabindex="0"');
    expect(html).toContain('aria-label="CPU 使用率折线图');
    expect(html).toContain('最新 99.3%');
    expect(html).toContain('最小 42.0%');
    expect(html).toContain('最大 99.3%');
    expect(html).toContain('左右方向键逐点读数');
    expect(html).toMatch(/<title>CPU 使用率折线图/);
    // 逐点读数的 aria-live 区域恒存在（键盘导航时写入最新读数）。
    expect(html).toContain('data-testid="metric-chart-announcement"');
  });

  it('24h 跨度渲染 x 轴时间刻度，标签带月/日', () => {
    const html = renderToStaticMarkup(
      <MetricChart
        percentAxis
        points={[
          { t: Date.parse('2026-11-19T00:00:00Z'), v: 10 },
          { t: Date.parse('2026-11-20T00:00:00Z'), v: 20 },
        ]}
      />,
    );
    expect(html).toContain('data-testid="metric-chart-time-axis"');
    expect(html).toMatch(/\d{2}\/\d{2} \d{2}:\d{2}/);
  });

  it('loading 且无数据时渲染同高度占位，不显示「暂无采集数据」', () => {
    // 走查第 5 轮 medium：弱网切换服务器/时间窗时，新 query 尚无数据，
    // 旧实现直接进空态，用户会误以为该窗口没有历史数据。
    const html = renderToStaticMarkup(<MetricChart label="CPU 使用率" points={[]} loading />);
    expect(html).toContain('data-testid="metric-chart-loading"');
    expect(html).toContain('aria-busy="true"');
    expect(html).not.toContain('暂无采集数据');
  });

  it('已有数据时 loading 不遮挡图表（如 30s 轮询刷新）', () => {
    const html = renderToStaticMarkup(
      <MetricChart
        points={[
          { t: 1, v: 42 },
          { t: 2, v: 43 },
        ]}
        loading
      />,
    );
    expect(html).toContain('<svg');
    expect(html).not.toContain('metric-chart-loading');
  });

  it('aria 按接口原始点数报数，降采样时注明绘制点数', () => {
    // 走查第 5 轮 low：24h 接口返回 1440 点、图上只画 500 点，读屏文案
    // 不能报「共 500 个采样点」。
    const base = Date.parse('2026-11-19T00:00:00Z');
    const points = Array.from({ length: 1_200 }, (_, index) => ({
      t: base + index * 60_000,
      v: 20 + (index % 60),
    }));
    const html = renderToStaticMarkup(
      <MetricChart label="CPU 使用率" points={points} percentAxis />,
    );
    expect(html).toContain('共 1200 个采样点（绘制 500）');
    expect(html).not.toContain('共 500 个采样点');
  });

  it('未降采样时只报原始点数，不出现「绘制」', () => {
    const html = renderToStaticMarkup(
      <MetricChart
        points={[
          { t: Date.parse('2026-11-19T10:00:00Z'), v: 42 },
          { t: Date.parse('2026-11-19T11:00:00Z'), v: 99.25 },
        ]}
      />,
    );
    expect(html).toContain('共 2 个采样点，');
    expect(html).not.toContain('绘制');
  });
});

/** 从静态 HTML 里取折线的 x 坐标（fallback 320 布局）。 */
function polylineXs(html: string): number[] {
  // 属性顺序不固定（断档分段后 polyline 多了 data-testid），按任意顺序提取。
  const points = /<polyline[^>]*\spoints="([^"]*)"/.exec(html)?.[1] ?? '';
  return points
    .split(' ')
    .filter(Boolean)
    .map((pair) => Number(pair.split(',')[0]));
}

/** 取 x 轴刻度文案及锚点。 */
function timeAxisTicks(html: string): Array<{ x: number; anchor: string; text: string }> {
  const axis = /<g data-testid="metric-chart-time-axis">([\s\S]*?)<\/g>/.exec(html)?.[1] ?? '';
  return [...axis.matchAll(/<text ([^>]*)>([^<]+)<\/text>/g)].map((match) => {
    const attrs = match[1] ?? '';
    return {
      x: Number(/x="([\d.]+)"/.exec(attrs)?.[1] ?? Number.NaN),
      anchor: /text-anchor="([^"]+)"/.exec(attrs)?.[1] ?? '',
      text: match[2] ?? '',
    };
  });
}

describe('MetricChart 选中窗口（第 7 轮 medium）', () => {
  const windowEnd = Date.UTC(2026, 10, 20, 6, 43);
  const windowStart = windowEnd - 24 * 3_600_000;
  const sparsePoints = [
    { t: windowEnd - 2 * 3_600_000, v: 10 },
    { t: windowEnd - 3_600_000, v: 20 },
    { t: windowEnd, v: 30 },
  ];

  it('24h 窗口内只有 2h 数据：折线贴着窗口右端，不再拉满整宽', () => {
    const html = renderToStaticMarkup(
      <MetricChart
        label="CPU 使用率"
        points={sparsePoints}
        percentAxis
        windowStart={windowStart}
        windowEnd={windowEnd}
      />,
    );
    const xs = polylineXs(html);
    expect(xs).toHaveLength(3);
    // fallback 320：paddingLeft 44、plotWidth 268。22h/24h ≈ 289.7，
    // 旧版按首/末采样点铺满会从 x=44 起步。
    expect(xs[0]).toBeGreaterThan(44 + 268 * 0.8);
    expect(xs[2]).toBeCloseTo(44 + 268, 0);

    const labels = timeAxisTicks(html);
    // 24h 窗口在 320px 下取 3 个刻度：首尾贴绘图区边缘，中间按窗口时间。
    expect(labels).toHaveLength(3);
    expect(labels[0]).toMatchObject({ x: 44, anchor: 'start' });
    expect(labels[2]).toMatchObject({ x: 312, anchor: 'end' });
    expect(labels[1]?.x).toBeCloseTo(44 + 268 / 2, 0);
    for (const label of labels) {
      expect(label.text).toMatch(/^\d{2}\/\d{2} \d{2}:\d{2}$/);
    }
  });

  it('覆盖不足时给出明文提示与 aria 口径（数据仅覆盖 2 小时）', () => {
    const html = renderToStaticMarkup(
      <MetricChart
        label="CPU 使用率"
        points={sparsePoints}
        percentAxis
        windowStart={windowStart}
        windowEnd={windowEnd}
      />,
    );
    expect(html).toContain('data-testid="metric-chart-coverage-note"');
    expect(html).toContain('数据仅覆盖 2 小时（24 小时窗口内其余时段无采样）');
    expect(html).toContain('数据仅覆盖 2 小时');
  });

  it('不传窗口时保持旧行为（首/末采样点铺满，无覆盖提示）', () => {
    const html = renderToStaticMarkup(<MetricChart points={sparsePoints} percentAxis />);
    const xs = polylineXs(html);
    expect(xs[0]).toBe(44);
    expect(xs[2]).toBeCloseTo(312, 0);
    expect(html).not.toContain('metric-chart-coverage-note');
  });

  it('数据覆盖接近整窗时不显示覆盖提示', () => {
    const html = renderToStaticMarkup(
      <MetricChart
        points={[
          { t: windowStart, v: 10 },
          { t: windowEnd, v: 20 },
        ]}
        percentAxis
        windowStart={windowStart}
        windowEnd={windowEnd}
      />,
    );
    expect(html).not.toContain('metric-chart-coverage-note');
  });
});

// 走查第 16 轮 low：末个采样距窗口终点 11 分钟时，覆盖时长曾被四舍五入
// 成「24 小时」，与「24 小时窗口」同值，看不出到底缺了多少。
describe('MetricChart 覆盖提示分钟精度（第 16 轮 low）', () => {
  const windowEnd = Date.UTC(2026, 10, 20, 12, 0);
  const windowStart = windowEnd - 24 * 3_600_000;
  const lastSample = windowEnd - 11 * 60_000;

  it('23h49m 覆盖写分钟精度并明说缺 11 分钟', () => {
    const points: Array<{ t: number; v: number }> = [];
    for (let t = windowStart; t <= lastSample; t += 60_000) {
      points.push({ t, v: 42 });
    }
    const html = renderToStaticMarkup(
      <MetricChart
        label="CPU 使用率"
        points={points}
        percentAxis
        windowStart={windowStart}
        windowEnd={windowEnd}
      />,
    );
    expect(html).toContain('data-testid="metric-chart-coverage-note"');
    expect(html).toContain('数据仅覆盖 23 小时 49 分钟（24 小时窗口内缺 11 分钟采样）');
    // 不能再说「覆盖 24 小时」——它与窗口同值，等于把缺口藏起来。
    expect(html).not.toContain('数据仅覆盖 24 小时');
  });
});

describe('MetricChart 窗口外采样（第 13 轮 medium）', () => {
  const windowEnd = Date.UTC(2026, 10, 20, 6, 43);
  const windowStart = windowEnd - 3_600_000;

  it('窗口外的点直接丢弃，不再钳到绘图区边缘', () => {
    // 60 个窗内点 + 2 个远在窗口之后的点：旧实现会把后两点钳到右缘。
    const points = Array.from({ length: 60 }, (_, index) => ({
      t: windowStart + index * 60_000,
      v: 20 + (index % 10),
    }));
    points.push({ t: windowEnd + 3_600_000, v: 99 }, { t: windowEnd + 7_200_000, v: 99 });
    const html = renderToStaticMarkup(
      <MetricChart
        label="CPU 使用率"
        points={points}
        percentAxis
        windowStart={windowStart}
        windowEnd={windowEnd}
      />,
    );
    const xs = polylineXs(html);
    expect(xs).toHaveLength(60);
    expect(Math.max(...xs)).toBeLessThanOrEqual(312);
    expect(html).toContain('共 60 个采样点');
  });

  it('全部点在窗口外：显示「窗口内无采样」而不是笼统的「暂无采集数据」', () => {
    const html = renderToStaticMarkup(
      <MetricChart
        label="CPU 使用率"
        points={[{ t: windowEnd + 7_200_000, v: 42 }]}
        percentAxis
        windowStart={windowStart}
        windowEnd={windowEnd}
      />,
    );
    expect(html).toContain('窗口内无采样（采集可能已暂停）');
    expect(html).not.toContain('暂无采集数据');
  });
});

describe('MetricChart 截断与单采样点（第 10 轮）', () => {
  const windowEnd = new Date(2026, 10, 20, 13, 0, 0).getTime();
  const windowStart = windowEnd - 24 * 3_600_000;

  it('truncated 时写「仅返回最近 X 小时」，不把未返回历史说成无采样', () => {
    // 30s 采集的 24h 窗口被旧上限截成最近 1440 点（≈12h）。
    const points = Array.from({ length: 1440 }, (_, index) => ({
      t: windowEnd - (1439 - index) * 30_000,
      v: 20 + (index % 60),
    }));
    const html = renderToStaticMarkup(
      <MetricChart
        label="CPU 使用率"
        points={points}
        percentAxis
        windowStart={windowStart}
        windowEnd={windowEnd}
        truncated
      />,
    );
    expect(html).toContain('data-testid="metric-chart-coverage-note"');
    expect(html).toContain('仅返回最近 12 小时（更早的点未返回）');
    expect(html).not.toContain('无采样');
  });

  it('单点按窗口坐标画圆点，并给出时间与值（不再显示「点数不足」）', () => {
    const pointTime = new Date(2026, 10, 20, 11, 1, 0).getTime();
    const html = renderToStaticMarkup(
      <MetricChart
        label="CPU 使用率"
        points={[{ t: pointTime, v: 42.5 }]}
        percentAxis
        windowStart={windowStart}
        windowEnd={windowEnd}
      />,
    );
    const dot = /<circle[^>]*data-testid="metric-chart-series-dot"[^>]*\scx="([\d.]+)"/.exec(html);
    expect(dot).not.toBeNull();
    // 窗口 13:00→次日 13:00，点 11:01 距右端 1h59m：落在绘图区右侧。
    expect(Number(dot?.[1])).toBeGreaterThan(240);
    expect(html).toContain('data-testid="metric-chart-single-point-note"');
    expect(html).toContain('仅 1 个采样点（11/20 11:01），值 42.5%');
    expect(html).toContain('共 1 个采样点');
    expect(html).not.toContain('点数不足');
    expect(html).not.toContain('暂无采集数据');
  });

  it('真正 0 点时仍显示「暂无采集数据」', () => {
    const html = renderToStaticMarkup(
      <MetricChart
        label="CPU 使用率"
        points={[]}
        windowStart={windowStart}
        windowEnd={windowEnd}
      />,
    );
    expect(html).toContain('暂无采集数据');
    expect(html).not.toContain('metric-chart-single-point-note');
  });
});

describe('MetricChart 断档切段（第 8 轮 high）', () => {
  const end = Date.UTC(2026, 10, 20, 12, 0);
  const start = end - 24 * 3_600_000;
  const STEP = 5 * 60_000;

  /** 5 分钟采样，跳过 (skipFrom, skipTo) 之间的点（两端保留）。 */
  function pointsWithGap(skipFrom: number, skipTo: number) {
    const points = [];
    for (let t = start; t <= end; t += STEP) {
      if (t > skipFrom && t < skipTo) {
        continue;
      }
      points.push({ t, v: 20 + ((t / STEP) % 10) });
    }
    return points;
  }

  function segmentCount(html: string): number {
    return [...html.matchAll(/data-testid="metric-chart-series-segment"/g)].length;
  }

  it('24h 内断采 3 小时：折线断成两段、缺口处提示、覆盖率按实际样本算', () => {
    const points = pointsWithGap(start + 13 * 3_600_000, start + 16 * 3_600_000);
    const html = renderToStaticMarkup(
      <MetricChart
        label="CPU 使用率"
        points={points}
        percentAxis
        windowStart={start}
        windowEnd={end}
      />,
    );
    // 旧版只有一条 polyline（253 点跨过 3 小时空白期）。
    expect(segmentCount(html)).toBe(2);
    expect(html).toContain('data-testid="metric-chart-gap-band"');
    expect(html).toContain('data-testid="metric-chart-gap-note"');
    expect(html).toContain('无采样断档');
    expect(html).toContain('（3 小时）');
    // 21h 实际覆盖 < 24h*90%，不再把断档算成已覆盖。
    expect(html).toContain('数据仅覆盖 21 小时');
    expect(html).toContain('数据仅覆盖 21 小时（24 小时窗口内其余时段无采样）');
    // 走查第 15 轮 low：提示行必须在 position:relative 定位容器之外，
    // 否则 tooltip 的 top:PADDING_TOP 会从容器顶部算起、直接盖住提示。
    const noteAt = html.indexOf('metric-chart-gap-note');
    const plotAt = html.indexOf('position:relative');
    expect(noteAt).toBeGreaterThan(-1);
    expect(plotAt).toBeGreaterThan(noteAt);
    expect(html.indexOf('</div>', noteAt)).toBeLessThan(plotAt);
  });

  it('缺口足够宽时在带内写「缺失 12 小时」', () => {
    const points = pointsWithGap(start + 6 * 3_600_000, start + 18 * 3_600_000);
    const html = renderToStaticMarkup(
      <MetricChart
        label="CPU 使用率"
        points={points}
        percentAxis
        windowStart={start}
        windowEnd={end}
      />,
    );
    expect(segmentCount(html)).toBe(2);
    expect(html).toContain('data-testid="metric-chart-gap-label"');
    expect(html).toContain('缺失 12 小时');
  });

  it('正常 5 分钟采样保持单段，不出现缺口提示', () => {
    const points = pointsWithGap(end + STEP, end + 2 * STEP);
    const html = renderToStaticMarkup(
      <MetricChart
        label="CPU 使用率"
        points={points}
        percentAxis
        windowStart={start}
        windowEnd={end}
      />,
    );
    expect(segmentCount(html)).toBe(1);
    expect(html).not.toContain('metric-chart-gap-note');
    expect(html).not.toContain('metric-chart-gap-band');
    expect(html).not.toContain('metric-chart-coverage-note');
  });
});

describe('MetricChart 网络单位一致性（第 7 轮 low）', () => {
  it('轴刻度与卡头同格式（保留 B），0 刻度仍是 0 B/s', () => {
    const html = renderToStaticMarkup(
      <MetricChart
        label="网络入"
        formatValue={formatBps}
        points={[
          { t: Date.parse('2026-11-19T10:00:00Z'), v: 20_000_000 },
          { t: Date.parse('2026-11-19T10:01:00Z'), v: 50_000_000 },
        ]}
      />,
    );
    const labels = [
      ...html.matchAll(/data-testid="metric-chart-y-tick"[^>]*>([^<]+)<\/text>/g),
    ].map((match) => match[1] ?? '');
    expect(labels).toContain('0 B/s');
    expect(labels).toContain('25.00 MB/s');
    expect(labels.some((label) => label.endsWith('M/s'))).toBe(false);
  });

  it('极端值轴用 EB/s，不出现 5000000000G/s', () => {
    const html = renderToStaticMarkup(
      <MetricChart
        label="网络入"
        formatValue={formatBps}
        points={[
          { t: Date.parse('2026-11-19T10:00:00Z'), v: 2 ** 63 - 1 },
          { t: Date.parse('2026-11-19T10:01:00Z'), v: 8_000_000_000_000_000_000 },
        ]}
      />,
    );
    const ticks = [
      ...html.matchAll(
        /<text x="([\d.]+)"[^>]*data-testid="metric-chart-y-tick"[^>]*>([^<]+)<\/text>/g,
      ),
    ].map((match) => ({ x: Number(match[1]), label: match[2] ?? '' }));
    expect(ticks.length).toBe(5);
    expect(ticks.some((tick) => tick.label.includes('EB/s'))).toBe(true);
    expect(ticks.some((tick) => tick.label.includes('G/s'))).toBe(false);
    for (const tick of ticks) {
      expect(tick.x - estimateAxisLabelWidth(tick.label)).toBeGreaterThanOrEqual(0);
    }
  });
});

describe('AlertSeverityStackedBarChart 四态', () => {
  it('空态/加载态/错误态', () => {
    expect(renderToStaticMarkup(<AlertSeverityStackedBarChart buckets={[]} />)).toContain(
      'chart-empty',
    );
    expect(renderToStaticMarkup(<AlertSeverityStackedBarChart buckets={[]} loading />)).toContain(
      'chart-loading',
    );
    expect(
      renderToStaticMarkup(<AlertSeverityStackedBarChart buckets={[]} error="加载失败" />),
    ).toContain('chart-error');
  });

  it('零告警（24 个全零桶）走 ChartFrame 空态，不渲染 24 个可聚焦热区', () => {
    // 走查第 9 轮 low：后端固定返回 24 桶，旧条件只看 stacked.length，
    // 键盘用户要 Tab 24 次才能到事件表。
    const buckets = Array.from({ length: 24 }, (_, index) => ({
      bucket_start: new Date(Date.UTC(2026, 10, 19, index)).toISOString(),
      info: 0,
      warning: 0,
      critical: 0,
    }));
    const html = renderToStaticMarkup(
      <AlertSeverityStackedBarChart buckets={buckets} onBucketSelect={() => {}} />,
    );
    expect(html).toContain('chart-empty');
    expect(html).toContain('该时间窗内暂无告警事件');
    expect(html).not.toContain('<svg');
    expect(html).not.toContain('tabindex="0"');
    expect(html).not.toContain('窗口内暂无告警事件');
  });

  it('单桶可渲染并带「按 Enter 筛选」的可聚焦热区', () => {
    const html = renderToStaticMarkup(
      <AlertSeverityStackedBarChart
        buckets={[{ bucket_start: '2026-11-19T13:00:00Z', info: 1, warning: 2, critical: 3 }]}
        onBucketSelect={() => {}}
      />,
    );
    expect(html).toContain('<svg');
    expect(html).toContain('aria-label="告警发生分布');
    expect(html).toContain('共 1 个桶、6 条告警');
    expect(html).toContain('按 Enter 筛选该时段');
    expect(html).toContain('role="button"');
    // 第 11 轮 medium：根 svg 按触点 x 取最近桶；几何标记覆盖整张绘图区
    // （fallback 560-36-10=514）且不拦截 pointer，每桶矩形保留键盘/读屏。
    expect(html).toMatch(
      /<rect data-testid="alert-severity-hit-surface"[^>]*width="514"[^>]*pointer-events="none"/,
    );
    expect(html).toMatch(/<rect[^>]*tabindex="0"(?![^>]*pointer-events="none")/);
  });

  it('7 天视图 x 轴 6 个标签，首尾 start/end 贴绘图区边缘', () => {
    const buckets = Array.from({ length: 28 }, (_, index) => ({
      bucket_start: new Date(Date.UTC(2026, 10, 13, index * 6)).toISOString(),
      info: 1,
      warning: 0,
      critical: 0,
    }));
    const html = renderToStaticMarkup(
      <AlertSeverityStackedBarChart buckets={buckets} window="7d" />,
    );
    const axis = /<g data-testid="alert-severity-x-axis">([\s\S]*?)<\/g>/.exec(html)?.[1] ?? '';
    const labels = [...axis.matchAll(/<text ([^>]*)>([^<]+)<\/text>/g)].map((match) => ({
      attrs: match[1] ?? '',
      label: match[2] ?? '',
    }));
    expect(labels).toHaveLength(6);
    expect(labels[0]?.attrs).toContain('text-anchor="start"');
    expect(labels[0]?.attrs).toContain('x="36"');
    expect(labels[5]?.attrs).toContain('text-anchor="end"');
    // fallback 宽 560 - PADDING_RIGHT 10 = 550：最后标签右缘不再越界。
    expect(labels[5]?.attrs).toContain('x="550"');
  });
});

describe('NoisyRuleBarChart 四态', () => {
  it('空态/加载态/错误态', () => {
    expect(renderToStaticMarkup(<NoisyRuleBarChart rules={[]} />)).toContain('chart-empty');
    expect(renderToStaticMarkup(<NoisyRuleBarChart rules={[]} loading />)).toContain(
      'chart-loading',
    );
    expect(renderToStaticMarkup(<NoisyRuleBarChart rules={[]} error="加载失败" />)).toContain(
      'chart-error',
    );
  });

  it('单条规则可渲染并带键盘筛选提示', () => {
    const html = renderToStaticMarkup(
      <NoisyRuleBarChart
        rules={[
          {
            rule_id: 'rule-1',
            rule_name: 'CPU 过高',
            severity: 'critical',
            count: 9,
            server_count: 4,
            last_fired_at: '2026-11-19T09:00:00Z',
          },
        ]}
        onRuleSelect={() => {}}
      />,
    );
    expect(html).toContain('<svg');
    expect(html).toContain('aria-label="最吵的规则 Top 1');
    expect(html).toContain('按 Enter 筛选该规则');
    expect(html).toContain('role="button"');
    // 走查第 15 轮 low：副标题与 x 轴标题都要说明柱长是触发次数，
    // 不能只写点击说明、让 0/20/40/60 刻度无单位。
    expect(html).toContain('柱长为窗口内触发次数（按次数降序）');
    expect(html).toContain('data-testid="noisy-rule-x-title"');
    expect(html).toContain('>触发次数（次）</text>');
  });

  it('系统哨兵规则用中文展示名，原始英文名保留在 aria 供排查', () => {
    const html = renderToStaticMarkup(
      <NoisyRuleBarChart
        rules={[
          {
            rule_id: 'rule-sentinel',
            rule_name: 'monitor.collector.unreachable',
            severity: 'warning',
            count: 4,
            server_count: 2,
            last_fired_at: '2026-11-19T09:00:00Z',
          },
        ]}
      />,
    );
    expect(html).toContain('>采集不可达（系统）</text>');
    expect(html).toContain('原始名 monitor.collector.unreachable');
    expect(html).not.toContain('>monitor.collector.unreachable</text>');
  });

  it('aria 使用 distinct_rules 全量总数，不把 Top 8 当全量', () => {
    const rules = Array.from({ length: 8 }, (_, index) => ({
      rule_id: `rule-${index}`,
      rule_name: `规则 ${index + 1}`,
      severity: 'warning' as const,
      count: 10 - index,
      server_count: 1,
      last_fired_at: '2026-11-19T09:00:00Z',
    }));
    const html = renderToStaticMarkup(<NoisyRuleBarChart rules={rules} totalRules={9} />);
    expect(html).toContain('共 9 条有触发记录的规则');
    expect(html).toContain('图中显示触发最多的 8 条');
  });
});

describe('InspectionScoreTrendChart 四态', () => {
  it('空态/加载态/错误态', () => {
    expect(renderToStaticMarkup(<InspectionScoreTrendChart points={[]} />)).toContain(
      'chart-empty',
    );
    expect(renderToStaticMarkup(<InspectionScoreTrendChart points={[]} loading />)).toContain(
      'chart-loading',
    );
    expect(
      renderToStaticMarkup(<InspectionScoreTrendChart points={[]} error="加载失败" />),
    ).toContain('chart-error');
  });

  it('单份报告可渲染；jobId=null 是「全部任务」而不是未选任务', () => {
    // 走查第 6 轮 high：即时巡检（job_id 为空）不属于任何任务，此前
    // jobId=null 被当成未选任务直接不出图；现在应渲染并标注全部任务。
    const points = [
      {
        report_id: 'rep-1',
        created_at: '2026-11-19T10:00:00Z',
        avg_score: 88,
        server_count: 5,
        anomaly_count: 1,
        trigger: 'manual',
      },
    ];
    const html = renderToStaticMarkup(
      <InspectionScoreTrendChart
        points={points}
        jobs={[
          { id: 'job-1', name: '基础巡检' },
          { id: 'job-2', name: '数据库巡检' },
        ]}
        jobId={null}
      />,
    );
    expect(html).toContain('aria-label="巡检均分走势');
    expect(html).toContain('全部任务');
    expect(html).toContain('最近 1 份报告');
    expect(html).not.toContain('请选择巡检任务');
    expect(html).not.toContain('chart-empty');

    const single = renderToStaticMarkup(
      <InspectionScoreTrendChart points={points} jobs={[{ id: 'job-1', name: '基础巡检' }]} />,
    );
    expect(single).toContain('aria-label="巡检均分走势');
    expect(single).toContain('最近 1 份报告');
    expect(single).toContain('0 到 100 分');
    expect(single).toContain('tabindex="0"');
  });

  it('选中任务无报告时显示「该任务暂无报告」，全部任务空态保持原文案', () => {
    const jobs = [{ id: 'job-1', name: '基础巡检' }];
    const selected = renderToStaticMarkup(
      <InspectionScoreTrendChart points={[]} jobs={jobs} jobId="job-1" />,
    );
    expect(selected).toContain('该任务暂无报告');
    expect(selected).not.toContain('暂无巡检报告');

    const all = renderToStaticMarkup(
      <InspectionScoreTrendChart points={[]} jobs={jobs} jobId={null} />,
    );
    expect(all).toContain('暂无巡检报告');
    expect(all).not.toContain('该任务暂无报告');

    // 没有定时任务（jobs 为空）时也走「全部任务」，空态不提示选任务。
    const noJobs = renderToStaticMarkup(<InspectionScoreTrendChart points={[]} />);
    expect(noJobs).toContain('暂无巡检报告');
    expect(noJobs).not.toContain('请选择巡检任务');
  });

  it('跨度 >24h 时 x 轴标签带时:分，同一天的多份报告不再同名', () => {
    // 14 份 2 小时间隔报告：11/18 两份、11/19 两份（旧版只显示 MM/DD）。
    const points = Array.from({ length: 14 }, (_, index) => ({
      report_id: `rep-${index}`,
      created_at: new Date(Date.UTC(2026, 10, 18, index * 2)).toISOString(),
      avg_score: 80 + index,
      server_count: 5,
      anomaly_count: 1,
      trigger: 'cron' as const,
    }));
    const html = renderToStaticMarkup(
      <InspectionScoreTrendChart points={points} jobs={[{ id: 'job-1', name: '基线巡检' }]} />,
    );
    const axis = /<g data-testid="inspection-trend-x-axis">([\s\S]*?)<\/g>/.exec(html)?.[1] ?? '';
    const labels = [...axis.matchAll(/<text[^>]*>([^<]+)<\/text>/g)].map((match) => match[1] ?? '');
    expect(labels.length).toBeGreaterThanOrEqual(3);
    for (const label of labels) {
      expect(label).toMatch(/^\d{2}\/\d{2} \d{2}:\d{2}$/);
    }
    expect(new Set(labels).size).toBe(labels.length);
  });

  it('x 轴首/末标签用 start/end 贴边，末标签不越出 SVG 右缘', () => {
    // 走查第 4 轮 low：末标签 middle 锚点右缘 319.6px > SVG 316px，
    // 「11/20 02:13」被裁成「11/20 02:1」。
    const points = Array.from({ length: 14 }, (_, index) => ({
      report_id: `rep-${index}`,
      created_at: new Date(Date.UTC(2026, 10, 18, index * 2)).toISOString(),
      avg_score: 80 + index,
      server_count: 5,
      anomaly_count: 1,
      trigger: 'cron' as const,
    }));
    const html = renderToStaticMarkup(
      <InspectionScoreTrendChart points={points} jobs={[{ id: 'job-1', name: '基线巡检' }]} />,
    );
    const axis = /<g data-testid="inspection-trend-x-axis">([\s\S]*?)<\/g>/.exec(html)?.[1] ?? '';
    const labels = [...axis.matchAll(/<text ([^>]*)>([^<]+)<\/text>/g)];
    expect(labels.length).toBeGreaterThanOrEqual(3);
    expect(labels[0]?.[1]).toContain('text-anchor="start"');
    // fallback 宽 560、PADDING_LEFT 38：首标签左缘贴绘图区。
    expect(labels[0]?.[1]).toContain('x="38"');
    expect(labels[labels.length - 1]?.[1]).toContain('text-anchor="end"');
    // 560 - PADDING_RIGHT(12) = 548：末标签右缘不再越过 SVG。
    expect(labels[labels.length - 1]?.[1]).toContain('x="548"');
  });
});

/** 取巡检图的数据点 cx（排除图例中 cy=-2 的圆）。 */
function pointCxs(html: string): number[] {
  return [...html.matchAll(/<circle cx="([\d.-]+)" cy="([\d.-]+)" r="[\d.]+"/g)]
    .filter((match) => Number(match[2]) > 0)
    .map((match) => Number(match[1]));
}

/** 取巡检图 x 轴标签的 x/锚点/文案。 */
function inspectionAxisTicks(html: string): Array<{ x: number; anchor: string; text: string }> {
  const axis = /<g data-testid="inspection-trend-x-axis">([\s\S]*?)<\/g>/.exec(html)?.[1] ?? '';
  return [...axis.matchAll(/<text ([^>]*)>([^<]+)<\/text>/g)].map((match) => {
    const attrs = match[1] ?? '';
    return {
      x: Number(/x="([\d.]+)"/.exec(attrs)?.[1] ?? Number.NaN),
      anchor: /text-anchor="([^"]+)"/.exec(attrs)?.[1] ?? '',
      text: match[2] ?? '',
    };
  });
}

function inspectionPoints(count: number) {
  return Array.from({ length: count }, (_, index) => ({
    report_id: `rep-${index}`,
    created_at: new Date(Date.UTC(2026, 10, 20, 2, 13 + index)).toISOString(),
    avg_score: 80 + index,
    server_count: 5,
    anomaly_count: 1,
    trigger: 'cron' as const,
  }));
}

describe('InspectionScoreTrendChart 小样本轴标签（第 7 轮 medium）', () => {
  it('单份报告：标签与圆点同为绘图区中心，不再贴最左缘', () => {
    // 走查第 7 轮 medium：单报告圆点 cx=293（fallback 560 下居中），
    // 旧版标签 x=38 贴左缘，日期与报告无法对应。
    const html = renderToStaticMarkup(
      <InspectionScoreTrendChart
        points={inspectionPoints(1)}
        jobs={[{ id: 'job-1', name: '基线巡检' }]}
      />,
    );
    const circles = pointCxs(html);
    const labels = inspectionAxisTicks(html);
    expect(circles).toHaveLength(1);
    expect(labels).toHaveLength(1);
    expect(labels[0]?.anchor).toBe('middle');
    expect(labels[0]?.x).toBeCloseTo(circles[0] as number, 1);
    expect(labels[0]?.x).toBeCloseTo(38 + 510 / 2, 1);
    expect(labels[0]?.x).not.toBe(38);
  });

  it('3 份报告：每个日期标签都对齐各自的点（不再首末贴边）', () => {
    const html = renderToStaticMarkup(
      <InspectionScoreTrendChart
        points={inspectionPoints(3)}
        jobs={[{ id: 'job-1', name: '基线巡检' }]}
      />,
    );
    const circles = pointCxs(html);
    const labels = inspectionAxisTicks(html);
    expect(circles).toHaveLength(3);
    expect(labels).toHaveLength(3);
    for (let index = 0; index < 3; index++) {
      expect(labels[index]?.anchor).toBe('middle');
      expect(labels[index]?.x).toBeCloseTo(circles[index] as number, 1);
    }
    // 首/末标签不再钉在 38 / 548。
    expect(labels[0]?.x).toBeGreaterThan(38 + 10);
    expect(labels[2]?.x).toBeLessThan(548 - 10);
  });

  it('4 份报告仍居中；密集样本（14 份）继续首末贴边防裁切', () => {
    const four = renderToStaticMarkup(
      <InspectionScoreTrendChart
        points={inspectionPoints(4)}
        jobs={[{ id: 'job-1', name: '基线巡检' }]}
      />,
    );
    const fourLabels = inspectionAxisTicks(four);
    const fourCircles = pointCxs(four);
    expect(fourLabels).toHaveLength(4);
    for (let index = 0; index < fourLabels.length; index++) {
      expect(fourLabels[index]?.x).toBeCloseTo(fourCircles[index] as number, 1);
    }

    const dense = renderToStaticMarkup(
      <InspectionScoreTrendChart
        points={inspectionPoints(14)}
        jobs={[{ id: 'job-1', name: '基线巡检' }]}
      />,
    );
    const denseLabels = inspectionAxisTicks(dense);
    expect(denseLabels[0]).toMatchObject({ x: 38, anchor: 'start' });
    expect(denseLabels[denseLabels.length - 1]).toMatchObject({ x: 548, anchor: 'end' });
  });
});

describe('InspectionScoreTrendChart 同日标签与 5 份对齐（第 9 轮 low）', () => {
  it('跨度 <24h 但日标签重复时自动补 HH:mm', () => {
    // 业务巡检 11/19 08:51 与 14:51（span 6h）：只写 MM/DD 时两个点
    // 都叫「11/19」，无法区分先后（走查第 9 轮 low 证据）。
    const points = [
      {
        report_id: 'rep-morning',
        created_at: '2026-11-19T08:51:17',
        avg_score: 88,
        server_count: 5,
        anomaly_count: 1,
        trigger: 'cron' as const,
      },
      {
        report_id: 'rep-afternoon',
        created_at: '2026-11-19T14:51:17',
        avg_score: 92,
        server_count: 5,
        anomaly_count: 0,
        trigger: 'cron' as const,
      },
    ];
    const html = renderToStaticMarkup(
      <InspectionScoreTrendChart points={points} jobs={[{ id: 'job-1', name: '业务巡检' }]} />,
    );
    const labels = inspectionAxisTicks(html).map((tick) => tick.text);
    expect(labels).toEqual(['11/19 08:51', '11/19 14:51']);
  });

  it('5 份跨天报告：短日期标签放得下就跟随分带中心，与点重合', () => {
    // 走查第 9 轮 low：旧规则 ≥5 份首/末强制贴边，首点 cx=147.6、
    // 首标签 x=38，相差约半个分带（1440 视图实测）。fallback 560 下
    // 首点 cx=89、末点 cx=497，标签都是「MM/DD」应居中。
    const days = ['2026-11-16', '2026-11-17', '2026-11-18', '2026-11-19', '2026-11-20'];
    const points = days.map((day, index) => ({
      report_id: `rep-${index}`,
      created_at: `${day}T10:00:00`,
      avg_score: 80 + index,
      server_count: 5,
      anomaly_count: 0,
      trigger: 'cron' as const,
    }));
    const html = renderToStaticMarkup(
      <InspectionScoreTrendChart points={points} jobs={[{ id: 'job-1', name: '夜间巡检' }]} />,
    );
    const labels = inspectionAxisTicks(html);
    const circles = pointCxs(html);
    expect(labels).toHaveLength(5);
    expect(circles).toHaveLength(5);
    for (let index = 0; index < labels.length; index++) {
      expect(labels[index]?.anchor).toBe('middle');
      expect(labels[index]?.x).toBeCloseTo(circles[index] as number, 1);
    }
    // 首/末不再钉在绘图区左右缘（距边缘至少半个分带）。
    expect(labels[0]?.x).toBeCloseTo(38 + 510 / 10, 1);
    expect(labels[4]?.x).toBeCloseTo(38 + 510 - 510 / 10, 1);
  });
});

describe('MetricChart 负载轴统一记法（第 9 轮 low）', () => {
  it('轴 max≥1e6 时 0/5.00e5/1.00e6 不再混排普通数字与科学计数', () => {
    const now = Date.now();
    const points = [
      { t: now - 60_000, v: 800_000 },
      { t: now, v: 1_000_000 },
    ];
    const html = renderToStaticMarkup(
      <MetricChart
        points={points}
        label="负载（1m）"
        formatValue={(value) => formatSignificant(value, 3)}
        formatAxisTick={formatNumericAxisTick}
      />,
    );
    // 轴 0..2e6：每一档都必须是同一种记法。
    expect(html).toContain('>0</text>');
    expect(html).toContain('>5.00e5</text>');
    expect(html).toContain('>1.00e6</text>');
    expect(html).toContain('>1.50e6</text>');
    expect(html).toContain('>2.00e6</text>');
    expect(html).not.toContain('>500000</text>');
    expect(html).not.toContain('>1000000</text>');
  });

  it('未传 formatAxisTick 时保持旧行为（其他卡片/调用方不受影响）', () => {
    const now = Date.now();
    const html = renderToStaticMarkup(
      <MetricChart
        points={[
          { t: now - 60_000, v: 10 },
          { t: now, v: 20 },
        ]}
        formatValue={(value) => `${value.toFixed(1)}%`}
      />,
    );
    expect(html).toContain('>25.0%</text>');
  });
});

describe('走查第 14 轮：覆盖率容差与空态分级', () => {
  const windowEnd = Date.parse('2026-11-20T01:00:00Z');
  const windowStart = windowEnd - 3_600_000;

  it('600s 采样、1h 窗口完整采集：覆盖 50 分钟不再提示缺采（第 6 项）', () => {
    // 首个采样在窗口开始后 10 分钟、末个正好在窗口末尾：实际覆盖 50 分钟，
    // 旧版按 90% 阈值误报「数据仅覆盖 50 分钟」。
    const points = Array.from({ length: 6 }, (_, index) => ({
      t: windowStart + (index + 1) * 600_000,
      v: 42,
    }));
    const html = renderToStaticMarkup(
      <MetricChart
        points={points}
        sampleIntervalMs={600_000}
        windowStart={windowStart}
        windowEnd={windowEnd}
      />,
    );
    expect(html).not.toContain('metric-chart-coverage-note');
    expect(html).not.toContain('其余时段无采样');
  });

  it('真实 30 分钟缺口仍提示覆盖不足（容差不吞掉真断档）', () => {
    const points = [10, 20, 50, 60].map((offset) => ({
      t: windowStart + offset * 60_000,
      v: 42,
    }));
    const html = renderToStaticMarkup(
      <MetricChart
        points={points}
        sampleIntervalMs={60_000}
        windowStart={windowStart}
        windowEnd={windowEnd}
      />,
    );
    expect(html).toContain('metric-chart-coverage-note');
    expect(html).toContain('数据仅覆盖 20 分钟');
  });

  it('窗口内 0 点但存在 latest：空态写「最近一次采样 N 前」而不是「暂无采集数据」（第 3 项）', () => {
    const stopped = renderToStaticMarkup(
      <MetricChart
        points={[]}
        windowStart={windowStart}
        windowEnd={windowEnd}
        hasAnySample
        latestSampleSummary="72 小时前"
      />,
    );
    expect(stopped).toContain('窗口内无采样（最近一次采样 72 小时前）');
    expect(stopped).not.toContain('暂无采集数据');

    const never = renderToStaticMarkup(
      <MetricChart points={[]} windowStart={windowStart} windowEnd={windowEnd} />,
    );
    expect(never).toContain('暂无采集数据');
  });
});

describe('走查第 14 轮：时钟超前的样本不参与 Top12 排名', () => {
  it('未来 20 分钟的 99% 机器被排除，正常机器排第一并给出提示（第 2 项）', () => {
    const now = Date.parse('2026-11-20T15:02:00Z');
    const html = renderToStaticMarkup(
      <TopServerBarChart
        servers={[
          {
            server_id: 'normal',
            server_name: 'normal-01',
            cpu_pct: 80,
            mem_pct: 40,
            disk_pct: 30,
            created_at: '2026-11-20T15:01:30Z',
          },
          {
            server_id: 'skew',
            server_name: 'skew-08',
            cpu_pct: 99,
            mem_pct: 40,
            disk_pct: 30,
            created_at: '2026-11-20T15:23:00Z',
          },
        ]}
        now={now}
      />,
    );
    expect(html).toContain('aria-label="最紧张服务器 Top 1');
    expect(html).toContain('80.0%');
    expect(html).not.toContain('99.0%');
    expect(html).toContain('1 台采样时间超前服务端时钟，未参与排名');
  });

  it('600s 采集间隔下超前 9 分钟的 100% 机器仍被排除（第 17 轮 medium）', () => {
    // 走查第 17 轮证据：采集间隔调到 600s 后，+9 分钟的未来样本落在
    // 「一个采集间隔」的旧容差内，被当作当前值排第 1、显示「1 秒前」；
    // 趋势 SQL 已按服务端 now 排除它。现在 Top12 也用 1 秒容差。
    const now = Date.parse('2026-11-20T15:02:00Z');
    const html = renderToStaticMarkup(
      <TopServerBarChart
        servers={[
          {
            server_id: 'normal',
            server_name: 'normal-01',
            cpu_pct: 80,
            mem_pct: 40,
            disk_pct: 30,
            created_at: '2026-11-20T14:57:00Z',
          },
          {
            server_id: 'skew',
            server_name: 'future-agent-11',
            cpu_pct: 100,
            mem_pct: 40,
            disk_pct: 30,
            created_at: '2026-11-20T15:11:00Z',
          },
        ]}
        now={now}
      />,
    );
    expect(html).toContain('aria-label="最紧张服务器 Top 1');
    expect(html).toContain('80.0%');
    expect(html).not.toContain('100.0%');
    expect(html).toContain('1 台采样时间超前服务端时钟，未参与排名');
    expect(html).not.toContain('1 秒前');
  });
});

describe('走查第 14 轮：24h 横轴补日期与未走完桶', () => {
  const longFormatter = new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  });
  const expectedLong = (iso: string) => longFormatter.format(Date.parse(iso));

  it('集群趋势 24h 首桶带月/日（第 4 项）', () => {
    const html = renderToStaticMarkup(
      <ClusterTrendChart
        points={[
          {
            bucket_start: '2026-11-19T23:00:00Z',
            cpu_p95: 50,
            cpu_avg: 40,
            cpu_max: 60,
            servers: 2,
            samples: 120,
          },
          {
            bucket_start: '2026-11-20T00:00:00Z',
            cpu_p95: 55,
            cpu_avg: 45,
            cpu_max: 65,
            servers: 2,
            samples: 120,
          },
        ]}
      />,
    );
    expect(html).toContain(`>${expectedLong('2026-11-19T23:00:00Z')}</text>`);
    expect(html).not.toContain('>23:00</text>');
  });

  it('集群趋势最后桶 end > now 标「进行中」（第 5 项）', () => {
    const html = renderToStaticMarkup(
      <ClusterTrendChart
        points={[
          {
            bucket_start: '2026-11-20T22:00:00Z',
            cpu_p95: 50,
            cpu_avg: 40,
            cpu_max: 60,
            servers: 2,
            samples: 40,
          },
        ]}
        now={Date.parse('2026-11-20T22:40:00Z')}
      />,
    );
    expect(html).toMatch(/data-in-progress="true"/);
    expect(html).toContain('（进行中）');
    expect(html).toContain('最后一桶进行中');
  });

  it('告警分布 24h 横轴带月/日，最后桶标「进行中」（第 4/5 项）', () => {
    const html = renderToStaticMarkup(
      <AlertSeverityStackedBarChart
        buckets={[
          { bucket_start: '2026-11-19T23:00:00Z', info: 1, warning: 2, critical: 3 },
          { bucket_start: '2026-11-20T00:00:00Z', info: 1, warning: 0, critical: 0 },
        ]}
        now={Date.parse('2026-11-20T00:40:00Z')}
      />,
    );
    const axis = /<g data-testid="alert-severity-x-axis">([\s\S]*?)<\/g>/.exec(html)?.[1] ?? '';
    const labels = [...axis.matchAll(/<text[^>]*>([^<]+)<\/text>/g)].map((match) => match[1] ?? '');
    expect(labels).toContain(expectedLong('2026-11-19T23:00:00Z'));
    expect(html).toMatch(/data-in-progress="true"/);
    expect(html).toContain('（进行中）');
    expect(html).toContain('最后一桶进行中');
  });
});

describe('MetricChart 稀疏断档（第 9 轮 low）', () => {
  it('3 个稀疏点（间隔 60s 与 3h）：3h 缺口画出断档带并给出说明', () => {
    // 走查证据：three-point-01 的 3 个点只有 60s 与 3h 两个间隔，旧版
    // 中位 1.5h → 阈值 3h，delta 不满足 > threshold，折线直接跨过空白。
    const now = Date.now();
    const hour = 3_600_000;
    const html = renderToStaticMarkup(
      <MetricChart
        points={[
          { t: now - 23 * hour, v: 10 },
          { t: now - 23 * hour + 60_000, v: 11 },
          { t: now - 20 * hour, v: 12 },
        ]}
        sampleIntervalMs={60_000}
        windowStart={now - 24 * hour}
        windowEnd={now}
      />,
    );
    expect(html).toContain('metric-chart-gap-note');
    expect(html).toContain('无采样断档');
    expect(html).toContain('3 小时');
    expect(html).toContain('metric-chart-gap-band');
  });
});
