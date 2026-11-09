// M4 五期：巡检报告详情抽屉（CONTRACT.md 8.7——avg_score 大数字 + per-server
// 表：score 色阶、异常项展开、previous_score 对比箭头、raw_output 折叠面板
// 与截断标记）。

import { useQuery } from '@tanstack/react-query';
import { Collapse, Descriptions, Drawer, Space, Spin, Table, Tag, Tooltip, Typography } from 'antd';
import type { TableColumnsType } from 'antd';
import { Alert as AntAlert } from 'antd';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import { QueryErrorState } from '../../components/admin/AdminStates';
import { ApiRequestError, toApiFailure } from '../../components/admin/apiError';
import { formatDateTime, formatDuration } from '../../components/admin/format';
import CopyButton from '../../components/CopyButton';

import { TRIGGER_LABELS, describeInspectionError, scoreColor, scoreColorVar, scoreTrend } from './inspectionView';

type InspectionReportDetail = components['schemas']['InspectionReportDetail'];
type InspectionReportItem = components['schemas']['InspectionReportItem'];
type InspectionReport = components['schemas']['InspectionReport'];

export interface ReportDrawerProps {
  open: boolean;
  reportId: string | null;
  onClose: () => void;
}

const anomalySeverityColor: Record<string, string> = {
  critical: 'red',
  warning: 'gold',
  info: 'blue',
};

const ANOMALY_ITEM_LABELS: Record<string, string> = {
  collect: '采集失败',
  service: '异常服务',
  disk: '磁盘使用率',
  load: '系统负载',
  memory: '内存使用率',
  journal: 'journal 错误',
};

export default function ReportDrawer({ open, reportId, onClose }: ReportDrawerProps) {
  const detailQuery = useQuery({
    queryKey: qk.inspectionReport(reportId ?? ''),
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/inspection/reports/{id}', {
        params: { path: { id: reportId ?? '' } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    enabled: open && reportId !== null,
    // 进行中的报告 10s 轮询（抽屉内 AntAlert 承诺「报告 10 秒自动刷新」
    // ——五期评审发现此前无轮询，数据停留打开时快照）。
    refetchInterval: (query) => (query.state.data?.report.finished_at == null ? 10_000 : false),
  });

  const detail: InspectionReportDetail | undefined = detailQuery.data;

  const columns: TableColumnsType<InspectionReportItem> = [
    {
      title: '服务器',
      dataIndex: 'server_name',
      render: (name: string, item) => name || item.server_id.slice(0, 8),
    },
    {
      title: '评分',
      dataIndex: 'score',
      width: 90,
      sorter: (a, b) => a.score - b.score,
      render: (score: number) => <Tag color={scoreColor(score)}>{score}</Tag>,
    },
    {
      title: '较上次',
      key: 'trend',
      width: 90,
      render: (_, item) => {
        const previous =
          detail?.previous_scores && item.server_id in detail.previous_scores
            ? detail.previous_scores[item.server_id]
            : null;
        const trend = scoreTrend(item.score, previous);
        if (trend.direction === 'none') {
          return <Typography.Text type="secondary">—</Typography.Text>;
        }
        if (trend.direction === 'up') {
          return (
            <Typography.Text style={{ color: 'var(--color-success)' }}>
              ↑ {trend.text}
            </Typography.Text>
          );
        }
        if (trend.direction === 'down') {
          return (
            <Typography.Text style={{ color: 'var(--color-danger)' }}>
              ↓ {trend.text}
            </Typography.Text>
          );
        }
        return <Typography.Text type="secondary">→ {trend.text}</Typography.Text>;
      },
    },
    {
      title: '异常项',
      dataIndex: 'anomaly_count',
      width: 80,
      render: (count: number, item) =>
        count > 0 ? (
          <Space size={4} wrap>
            {item.anomalies.slice(0, 2).map((anomaly, index) => (
              <Tag key={index} color={anomalySeverityColor[anomaly.severity] ?? 'default'}>
                {ANOMALY_ITEM_LABELS[anomaly.item] ?? anomaly.item}
              </Tag>
            ))}
            {count > 2 ? <Tag>+{count - 2}</Tag> : null}
          </Space>
        ) : (
          <Typography.Text type="secondary">0</Typography.Text>
        ),
    },
    {
      title: '命令',
      dataIndex: 'command',
      width: 130,
      // 契约 8.7 ⑧（六期打磨 sprint 1）：报告「命令列」复制——模板展开后
      // 的实际命令清单（inspection_report_items.command）。此前报告明细只有
      // raw_output，「命令」无处可复制（实机仅命中「复制原始输出」1 个）。
      // 旧行（00011 之前）command 为空串 → 不渲染按钮。
      render: (command: string) => {
        if (!command) {
          return <Typography.Text type="secondary">—</Typography.Text>;
        }
        const lines = command.split('\n').filter((line) => line.length > 0);
        return (
          <Tooltip title={<pre style={{ margin: 0, whiteSpace: 'pre-wrap' }}>{command}</pre>}>
            <Space size={4} wrap>
              <span className="opsmind-admin-meta">{lines.length} 条</span>
              <CopyButton text={command} label="复制命令" />
            </Space>
          </Tooltip>
        );
      },
    },
    {
      title: '耗时',
      dataIndex: 'duration_ms',
      width: 90,
      render: (value: number) => formatDuration(value),
    },
    {
      title: '错误',
      dataIndex: 'error_code',
      width: 110,
      // 巡检 runner 落库的是错误链原文（≤128B）：中文分类后展示，未知
      // 形态回落原文（五期打磨 sprint 2，走查 inspections-runnow-report.png）。
      render: (code: string) => (code ? describeInspectionError(code) : '—'),
    },
  ];

  return (
    <Drawer title="巡检报告" open={open} onClose={onClose} width={760} destroyOnHidden>
      {detailQuery.isLoading ? (
        <div style={{ textAlign: 'center', padding: 32 }}>
          <Spin />
        </div>
      ) : detailQuery.isError ? (
        <QueryErrorState
          error={detailQuery.error}
          onRetry={() => {
            void detailQuery.refetch();
          }}
        />
      ) : detail ? (
        <ReportBody report={detail.report} items={detail?.items ?? []} columns={columns} />
      ) : null}
    </Drawer>
  );
}

function ReportBody({
  report,
  items,
  columns,
}: {
  report: InspectionReport;
  items: InspectionReportItem[];
  columns: TableColumnsType<InspectionReportItem>;
}) {
  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 24 }}>
        <div>
          {/* 六期打磨 sprint 6（走查 low）：大字按分档着色（此前恒为绿色，
              30 分的低分报告也显示为绿色），与列表 Tag 的 scoreColor 同源
              （85+ 绿 / 60-84 黄 / 0-59 红）；七期起色值为语义变量引用。 */}
          <Typography.Title
            style={{ margin: 0, fontSize: 44, color: scoreColorVar(report.avg_score) }}
            copyable={false}
          >
            {report.avg_score}
          </Typography.Title>
          <Typography.Text type="secondary">平均评分</Typography.Text>
        </div>
        <Descriptions size="small" column={1} style={{ flex: 1 }}>
          <Descriptions.Item label="任务">
            {report.job_name || '即时巡检'}（{TRIGGER_LABELS[report.trigger] ?? report.trigger}）
          </Descriptions.Item>
          <Descriptions.Item label="覆盖 / 异常">
            {report.server_count} 台 · 异常项 {report.anomaly_count} 个
          </Descriptions.Item>
          <Descriptions.Item label="时间">
            {formatDateTime(report.started_at)}
            {report.finished_at ? ` → ${formatDateTime(report.finished_at)}` : '（进行中）'}
          </Descriptions.Item>
        </Descriptions>
      </div>

      {report.finished_at === null ? (
        <AntAlert type="info" showIcon message="巡检进行中：报告 10 秒自动刷新。" />
      ) : null}

      <Table<InspectionReportItem>
        rowKey="id"
        size="small"
        columns={columns}
        dataSource={items}
        pagination={false}
        expandable={{
          rowExpandable: (item) => item.anomaly_count > 0 || item.raw_output !== '',
          expandedRowRender: (item) => (
            <Space direction="vertical" size={8} style={{ width: '100%' }}>
              {item.anomalies.length > 0 ? (
                <Space direction="vertical" size={4} style={{ width: '100%' }}>
                  {item.anomalies.map((anomaly, index) => (
                    <div key={index}>
                      <Tag color={anomalySeverityColor[anomaly.severity] ?? 'default'}>
                        {ANOMALY_ITEM_LABELS[anomaly.item] ?? anomaly.item}
                      </Tag>
                      <Typography.Text type="secondary">{anomaly.detail}</Typography.Text>{' '}
                      {/* 契约 8.7 ⑧（FE-P2 六期）：报告条目值复制（异常详情）。 */}
                      <CopyButton text={anomaly.detail} label="复制异常详情" />
                    </div>
                  ))}
                </Space>
              ) : null}
              {item.raw_output !== '' ? (
                <Collapse
                  items={[
                    {
                      key: 'raw',
                      label: (
                        <Space size={6}>
                          <span>{`原始采集输出${item.truncated ? '（已截断）' : ''}`}</span>
                          {/* 契约 8.7 ⑧（FE-P2 六期）：报告条目输出复制——统一
                              CopyButton 取代 antd Typography copyable（8.2 唯一
                              实现）。六期打磨 sprint 4：按钮在 Collapse label
                              区内，冒泡会让点击复制连带开合面板，故外层 span
                              拦下 click/keydown。 */}
                          <span
                            onClick={(event) => event.stopPropagation()}
                            onKeyDown={(event) => event.stopPropagation()}
                          >
                            <CopyButton text={item.raw_output} label="复制原始输出" />
                          </span>
                        </Space>
                      ),
                      children: (
                        <Typography.Paragraph style={{ whiteSpace: 'pre-wrap', marginBottom: 0 }}>
                          <div
                            style={{
                              contentVisibility: 'auto',
                              containIntrinsicSize: 'auto 500px',
                            }}
                          >
                            {item.raw_output}
                          </div>
                        </Typography.Paragraph>
                      ),
                    },
                  ]}
                />
              ) : null}
            </Space>
          ),
        }}
      />
    </Space>
  );
}
