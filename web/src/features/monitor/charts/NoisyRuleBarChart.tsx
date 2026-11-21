// 最吵的规则 Top 8。
//
// 数据：GET /api/alert-events/stats 的 top_rules[]。
// 交互：hover/聚焦显示影响服务器数与最近触发时间；点击（或 Enter/空格）
// 把 rule_id 交给页面写入事件表筛选（与时段筛选可叠加、各自可清除）。

import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';

import { ruleDisplayName } from '../../alerts/ruleForm';
import ChartFrame from './ChartFrame';
import ChartTooltip from './ChartTooltip';
import {
  countScale,
  SEVERITY_LABELS,
  sortNoisyRules,
  toCount,
  truncateRuleNameToWidth,
  type ChartWindow,
  type NoisyRuleRow,
  type SeverityKey,
} from './chartModel';
import { activateOnKey } from './chartA11y';
import { useChartWidth } from './useChartWidth';

export interface NoisyRuleSelection {
  rule_id: string;
  rule_name: string;
}

export interface NoisyRuleBarChartProps {
  /** stats.top_rules。 */
  rules: readonly NoisyRuleRow[];
  /**
   * 窗口内有触发记录的规则总数（stats.distinct_rules）。top_rules 被后端
   * LIMIT 8 截断，不能拿 rules.length 当全量写进 aria 文案。
   */
  totalRules?: number;
  limit?: number;
  window?: ChartWindow;
  onRuleSelect?: (selection: NoisyRuleSelection) => void;
  selectedRuleId?: string | null;
  loading?: boolean;
  error?: ReactNode;
  emptyMessage?: ReactNode;
  height?: number;
}

const SEVERITY_COLORS: Record<SeverityKey, string> = {
  critical: 'var(--color-danger)',
  warning: 'var(--color-warning)',
  info: 'var(--color-info)',
};

const SEVERITY_KEYS: readonly SeverityKey[] = ['critical', 'warning', 'info'];

function isSeverityKey(value: string): value is SeverityKey {
  return value === 'critical' || value === 'warning' || value === 'info';
}

function severityColor(value: string): string {
  return isSeverityKey(value) ? SEVERITY_COLORS[value] : 'var(--color-primary)';
}

function severityLabel(value: string): string {
  return isSeverityKey(value) ? SEVERITY_LABELS[value] : value;
}

const FALLBACK_WIDTH = 560;
const PADDING_TOP = 26;
// 底部留两行：刻度数字 + 「触发次数（次）」轴标题（走查第 15 轮 low）。
const PADDING_BOTTOM = 34;

const timeFormatter = new Intl.DateTimeFormat('zh-CN', {
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
});

function formatTime(value: string | null | undefined): string {
  if (!value) {
    return '—';
  }
  const time = Date.parse(value);
  return Number.isFinite(time) ? timeFormatter.format(time) : '时间未知';
}

export default function NoisyRuleBarChart({
  rules,
  totalRules: totalRulesProp,
  limit = 8,
  window = '24h',
  onRuleSelect,
  selectedRuleId,
  loading = false,
  error,
  emptyMessage = '该时间窗内暂无规则触发记录',
  height = 280,
}: NoisyRuleBarChartProps) {
  const [containerRef, measuredWidth] = useChartWidth();
  const [hovered, setHovered] = useState<number | null>(null);

  const cappedLimit = Math.max(1, Math.min(8, Math.floor(limit)));
  const ranked = useMemo(() => sortNoisyRules(rules, cappedLimit), [rules, cappedLimit]);

  const width = measuredWidth > 0 ? measuredWidth : FALLBACK_WIDTH;
  const padLeft = width < 520 ? 88 : 124;
  const padRight = width < 520 ? 38 : 48;
  const plotWidth = Math.max(0, width - padLeft - padRight);
  const plotHeight = Math.max(0, height - PADDING_TOP - PADDING_BOTTOM);
  const rowHeight = ranked.length > 0 ? Math.min(28, Math.max(20, plotHeight / ranked.length)) : 26;
  const groupTop = PADDING_TOP + Math.max(0, (plotHeight - rowHeight * ranked.length) / 2);

  const maxCount = ranked.reduce((max, rule) => Math.max(max, toCount(rule.count)), 0);
  const scale = countScale(maxCount);

  const reportedTotal =
    totalRulesProp !== undefined && Number.isFinite(totalRulesProp)
      ? Math.floor(totalRulesProp)
      : rules.length;
  const totalRules = Math.max(ranked.length, reportedTotal);
  const ariaLabel = `最吵的规则 Top ${ranked.length}，近 ${
    window === '7d' ? '7 天' : '24 小时'
  }，按触发次数降序，共 ${totalRules} 条有触发记录的规则${
    totalRules > ranked.length ? `（图中显示触发最多的 ${ranked.length} 条）` : ''
  }`;

  const hoveredRule = hovered !== null ? ranked[hovered] : undefined;
  const hoveredRawName = hoveredRule ? hoveredRule.rule_name || hoveredRule.rule_id : '';
  const hoveredDisplayName = ruleDisplayName(hoveredRawName);
  const selectable = Boolean(onRuleSelect);

  function selectRule(index: number): void {
    const rule = ranked[index];
    if (!rule) {
      return;
    }
    onRuleSelect?.({ rule_id: rule.rule_id, rule_name: rule.rule_name });
  }

  return (
    <ChartFrame
      loading={loading}
      error={error}
      empty={ranked.length === 0}
      emptyMessage={emptyMessage}
      height={height}
      subtitle={
        selectable
          ? // 走查第 15 轮 low：副标题此前只有点击说明，x 轴 0/20/40/60
            // 没有「次」，第一次看的人无法判断柱长是触发次数。副标题与
            // 轴标题同时写明口径。
            '柱长为窗口内触发次数（按次数降序）；点击规则把它写入下方事件表筛选，可与时段筛选叠加、各自清除。'
          : '柱长为窗口内触发次数（按次数降序），颜色为规则严重度。'
      }
    >
      <div ref={containerRef} style={{ position: 'relative', width: '100%' }}>
        <svg
          width={width}
          height={height}
          style={{ display: 'block', width: '100%' }}
          role="img"
          aria-label={ariaLabel}
        >
          <title>{ariaLabel}</title>

          {SEVERITY_KEYS.map((severity, index) => (
            <g key={severity} transform={`translate(${padLeft + index * 72}, 12)`}>
              <rect x={0} y={-6} width={9} height={9} rx={2} fill={SEVERITY_COLORS[severity]} />
              <text x={14} y={2} fontSize={10} fill="var(--color-text-secondary)">
                {SEVERITY_LABELS[severity]}
              </text>
            </g>
          ))}

          {scale.ticks.map((tick) => {
            const x = padLeft + (tick / scale.max) * plotWidth;
            return (
              <g key={tick}>
                <line
                  x1={x}
                  x2={x}
                  y1={PADDING_TOP}
                  y2={PADDING_TOP + plotHeight}
                  stroke="var(--color-border)"
                  strokeWidth={1}
                />
                <text
                  x={x}
                  y={PADDING_TOP + plotHeight + 15}
                  textAnchor="middle"
                  fontSize={10}
                  fill="var(--color-text-tertiary)"
                >
                  {tick}
                </text>
              </g>
            );
          })}

          {/* x 轴标题（走查第 15 轮 low）：刻度 0/20/40/60 单独出现时
              读不出是「次」还是别的计数。 */}
          <text
            x={padLeft + plotWidth / 2}
            y={height - 5}
            textAnchor="middle"
            fontSize={10}
            fill="var(--color-text-tertiary)"
            data-testid="noisy-rule-x-title"
          >
            触发次数（次）
          </text>

          {ranked.map((rule, index) => {
            const rowTop = groupTop + index * rowHeight;
            const centerY = rowTop + rowHeight / 2;
            const value = toCount(rule.count);
            const barWidth = scale.max > 0 ? (value / scale.max) * plotWidth : 0;
            const selected = selectedRuleId === rule.rule_id;
            // 与事件表/规则表同口径：系统哨兵规则显示中文展示名，避免
            // 「monitor.coll…」与同页「采集不可达（系统）」对不上。
            const rawName = rule.rule_name || rule.rule_id;
            const displayName = ruleDisplayName(rawName);
            // 走查第 6 轮 medium：固定 8 个码点在 1024/390 下仍宽 99px，
            // 而左列只有 80px，标签左缘被裁成负坐标。改为按可用像素宽度
            // 截断（CJK 双宽），保证整段落在 padLeft 左侧。
            // 走查第 17 轮 low：规则名差异集中在前缀（网络入/网络出），
            // 中段截断会同时吞掉入/出；改用保留前缀的截断，并给系统规则
            // 保留「（系统）」后缀。
            const label = truncateRuleNameToWidth(displayName, padLeft - 12, 11);
            return (
              <g key={rule.rule_id}>
                {selected ? (
                  <rect
                    x={0}
                    y={rowTop}
                    width={width}
                    height={rowHeight}
                    fill="var(--color-bg-selected)"
                    rx={3}
                  />
                ) : null}
                <text
                  x={padLeft - 8}
                  y={centerY + 4}
                  textAnchor="end"
                  fontSize={11}
                  fill="var(--color-text-secondary)"
                >
                  {label}
                </text>
                <rect
                  x={padLeft}
                  y={centerY - Math.max(4, rowHeight * 0.28)}
                  width={Math.max(0, barWidth)}
                  height={Math.max(7, rowHeight * 0.56)}
                  rx={3}
                  fill={severityColor(rule.severity)}
                />
                <text
                  x={Math.min(padLeft + barWidth + 5, width - 30)}
                  y={centerY + 4}
                  fontSize={10}
                  fill="var(--color-text-secondary)"
                >
                  {value}
                </text>
                <rect
                  x={0}
                  y={rowTop}
                  width={width}
                  height={rowHeight}
                  fill="transparent"
                  tabIndex={0}
                  role={selectable ? 'button' : 'img'}
                  aria-label={`第 ${index + 1} 名规则 ${displayName}${
                    displayName === rawName ? '' : `（原始名 ${rawName}）`
                  }，严重度 ${severityLabel(rule.severity)}，触发 ${value} 次，影响 ${toCount(
                    rule.server_count,
                  )} 台服务器，最近触发 ${formatTime(rule.last_fired_at)}${
                    selectable ? '；按 Enter 筛选该规则' : ''
                  }`}
                  onMouseEnter={() => {
                    setHovered(index);
                  }}
                  onMouseLeave={() => {
                    setHovered(null);
                  }}
                  onFocus={() => {
                    setHovered(index);
                  }}
                  onBlur={() => {
                    setHovered(null);
                  }}
                  onClick={() => {
                    selectRule(index);
                  }}
                  onKeyDown={(event) => {
                    activateOnKey(event, () => {
                      selectRule(index);
                    });
                  }}
                  style={{ cursor: selectable ? 'pointer' : 'default' }}
                />
              </g>
            );
          })}
        </svg>

        {hoveredRule ? (
          <ChartTooltip
            x={padLeft + (scale.max > 0 ? (toCount(hoveredRule.count) / scale.max) * plotWidth : 0)}
            containerWidth={width}
            top={Math.max(4, groupTop + (hovered ?? 0) * rowHeight)}
            testId="noisy-rule-tooltip"
          >
            <div style={{ fontWeight: 600 }}>{hoveredDisplayName}</div>
            {hoveredDisplayName !== hoveredRawName ? (
              <div style={{ color: 'var(--color-text-tertiary)', fontSize: 11 }}>
                原始规则名 {hoveredRawName}
              </div>
            ) : null}
            <div style={{ color: 'var(--color-text-tertiary)' }}>
              {severityLabel(hoveredRule.severity)} · 触发 {toCount(hoveredRule.count)} 次
            </div>
            <div>影响 {toCount(hoveredRule.server_count)} 台服务器</div>
            <div style={{ color: 'var(--color-text-tertiary)' }}>
              最近触发 {formatTime(hoveredRule.last_fired_at)}
            </div>
            {selectable ? (
              <div style={{ color: 'var(--color-text-tertiary)' }}>点击筛选该规则</div>
            ) : null}
          </ChartTooltip>
        ) : null}
      </div>
    </ChartFrame>
  );
}
