import { ReloadOutlined, SearchOutlined } from '@ant-design/icons';
import { useQuery } from '@tanstack/react-query';
import {
  Button,
  Descriptions,
  Flex,
  Input,
  Select,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { TableColumnsType } from 'antd';
import { message } from '../../components/antdMessage';
import { useState } from 'react';
import { useNavigate } from 'react-router-dom';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import CodeBlock from '../../components/CodeBlock';
import CopyButton from '../../components/CopyButton';
import EmptyState from '../../components/EmptyState';
import PageHeader from '../../components/PageHeader';
import { AdminLoading, QueryErrorState } from '../../components/admin/AdminStates';
import {
  ApiRequestError,
  isRecord,
  makeFailure,
  toApiFailure,
} from '../../components/admin/apiError';
import {
  formatDateTime,
  formatDuration,
  prettyJson,
  readBoolean,
  readNumber,
  readString,
  shortId,
  truncate,
} from '../../components/admin/format';
import {
  AUDIT_ACTIONS,
  RESOURCE_TYPE_OPTIONS,
  actionLabel,
  actorTypeLabel,
  resourceTypeLabel,
} from '../../components/admin/meta';
import '../../styles/admin.css';
import { isValidUUID, normalizeUUID } from './uuidFilter';

type AuditLog = components['schemas']['AuditLog'];

const PAGE_SIZES = [20, 50, 100, 200];

interface OutcomeTag {
  label: string;
  color: string;
}

const FAILED_ACTION = /(_failed|_rejected|_denied|_forbidden|_cancelled|_timeout)$/;
const SUCCEEDED_ACTION =
  /(login_succeeded|host_key_trusted|\.finished$|\.approved$|\.executed$|\.create$|\.update$|\.delete$|\.test$|\.models_refresh$|\.result$)/;

function auditOutcome(record: AuditLog): OutcomeTag | null {
  const detail = record.detail;
  const ok = readBoolean(detail, ['ok']);
  const exitCode = readNumber(detail, ['exit_code']);
  const errorValue = detail.error;
  const errorCode =
    readString(detail, ['error_code']) ??
    (isRecord(errorValue) ? readString(errorValue, ['code']) : null);
  const failedAction =
    FAILED_ACTION.test(record.action) ||
    record.action === 'auth.login_failed' ||
    record.action === 'auth.rate_limited';

  if (ok === false || errorCode !== null || (exitCode !== null && exitCode !== 0) || failedAction) {
    return { label: '失败', color: 'red' };
  }
  if (
    ok === true ||
    (exitCode !== null && exitCode === 0) ||
    SUCCEEDED_ACTION.test(record.action)
  ) {
    return { label: '成功', color: 'green' };
  }
  return null;
}

function commandSummary(detail: Record<string, unknown>): string | null {
  return readString(detail, ['command', 'sql', 'normalized_command']);
}

// 完整命令/SQL 的高亮语言：sql/normalized_command 字段按 SQL 渲染，其余按 bash。
function commandLanguage(detail: Record<string, unknown>): string {
  return readString(detail, ['sql', 'normalized_command']) !== null ? 'sql' : 'bash';
}

function auditDuration(record: AuditLog): number | null {
  return readNumber(record.detail, ['duration_ms', 'latency_ms']);
}

/** 展开态单值 + 复制按钮（契约 8.7 ④：detail 中 ID 逐值复制）。 */
function CopyableValue({ value, label }: { value: string | null | undefined; label: string }) {
  const text = value ?? '';
  return (
    <Space size={4}>
      <span className="opsmind-admin-mono">{text || '—'}</span>
      {text ? <CopyButton text={text} label={label} /> : null}
    </Space>
  );
}

function AuditDetail({
  record,
  onReplay,
}: {
  record: AuditLog;
  onReplay: (conversationId: string) => void;
}) {
  const command = commandSummary(record.detail);
  const output = readString(record.detail, ['output', 'stdout', 'raw_output', 'combined']) ?? null;
  const stderr = readString(record.detail, ['stderr']);
  const items = [
    {
      key: 'actor',
      label: '操作者',
      children: (
        <Space size={4}>
          <span>
            {actorTypeLabel(record.actor_type)}
            {record.actor_name ? ` ${record.actor_name}` : ''} / {record.actor_id || '—'}
          </span>
          {record.actor_id ? <CopyButton text={record.actor_id} label="复制操作者 ID" /> : null}
        </Space>
      ),
    },
    {
      key: 'request',
      label: 'request_id',
      children: <CopyableValue value={record.request_id} label="复制 request_id" />,
    },
    {
      key: 'target',
      label: '目标',
      children: (
        <Space size={4}>
          <span>
            {resourceTypeLabel(record.resource_type)} / {record.resource_id || '—'}
          </span>
          {record.resource_id ? <CopyButton text={record.resource_id} label="复制目标 ID" /> : null}
        </Space>
      ),
    },
    {
      key: 'ip',
      label: '来源 IP',
      children: <CopyableValue value={record.ip} label="复制来源 IP" />,
    },
    {
      key: 'conversation',
      label: '会话 ID',
      children: record.conversation_id ? (
        <Space size={4}>
          <CopyableValue value={record.conversation_id} label="复制会话 ID" />
          {/* 八期 FE-R（契约 2.12 约定 2）：审计行「回放」→ 同标签页 SPA
              跳转 `/replays/<conversationId>`（不再打开 Modal）。 */}
          <Button
            type="link"
            size="small"
            aria-label="回放"
            onClick={() => onReplay(record.conversation_id ?? '')}
          >
            回放
          </Button>
        </Space>
      ) : (
        <Typography.Text type="secondary">—</Typography.Text>
      ),
    },
    {
      key: 'server',
      label: '服务器 ID',
      children: <CopyableValue value={record.server_id} label="复制服务器 ID" />,
    },
    {
      key: 'prev',
      label: '前序哈希',
      children: (
        <Tooltip title={record.prev_hash}>
          <span className="opsmind-admin-mono">{shortId(record.prev_hash, 20)}</span>
        </Tooltip>
      ),
    },
    {
      key: 'hash',
      label: '当前哈希',
      children: (
        <Tooltip title={record.hash}>
          <span className="opsmind-admin-mono">{shortId(record.hash, 20)}</span>
        </Tooltip>
      ),
    },
  ];

  return (
    <div className="opsmind-admin-detail">
      <Descriptions size="small" column={2} bordered items={items} />
      {command ? (
        <div>
          <Typography.Text strong>完整命令 / SQL</Typography.Text>
          <div style={{ marginTop: 6 }}>
            <CodeBlock code={command} language={commandLanguage(record.detail)} />
          </div>
        </div>
      ) : null}
      {output ? (
        <div>
          <Typography.Text strong>完整输出</Typography.Text>
          <div style={{ marginTop: 6 }}>
            <CodeBlock code={output} />
          </div>
        </div>
      ) : null}
      {stderr && stderr !== output ? (
        <div>
          <Typography.Text strong>标准错误</Typography.Text>
          <div style={{ marginTop: 6 }}>
            <CodeBlock code={stderr} />
          </div>
        </div>
      ) : null}
      <div>
        <Typography.Text strong>原始 detail</Typography.Text>
        <div style={{ marginTop: 6 }}>
          <CodeBlock code={prettyJson(record.detail)} language="json" />
        </div>
      </div>
    </div>
  );
}

export default function AuditPage() {
  const [action, setAction] = useState('');
  const [resourceType, setResourceType] = useState('');
  const [resourceIdInput, setResourceIdInput] = useState('');
  const [resourceId, setResourceId] = useState('');
  const [conversationIdInput, setConversationIdInput] = useState('');
  const [conversationId, setConversationId] = useState('');
  const [serverIdInput, setServerIdInput] = useState('');
  const [serverId, setServerId] = useState('');
  const [pageSize, setPageSize] = useState(50);
  const [cursors, setCursors] = useState<Array<number | undefined>>([undefined]);
  const [pageIndex, setPageIndex] = useState(0);
  const navigate = useNavigate();

  const beforeId = cursors[pageIndex];

  const resetPaging = () => {
    setCursors([undefined]);
    setPageIndex(0);
  };

  // 提交 uuid 筛选：空值清除筛选；非空必须是合法 uuid，否则提示并保留原筛选值。
  // 提交前归一化（trim + 小写）：后端资源 ID 恒小写，大写粘贴只是形式差异，
  // 不归一化会得到 0 行且无提示（第 4 轮打磨）。
  const applyUuidFilter = (raw: string, set: (value: string) => void, label: string) => {
    const value = normalizeUUID(raw);
    if (value !== '' && !isValidUUID(value)) {
      message.warning(`${label} 必须是合法的 UUID`);
      return;
    }
    set(value);
    resetPaging();
  };

  const auditQuery = useQuery({
    queryKey: qk.audit({
      action,
      resource_type: resourceType,
      resource_id: resourceId,
      conversation_id: conversationId,
      server_id: serverId,
      before_id: beforeId ?? '',
      limit: pageSize,
    }),
    queryFn: async () => {
      const query: {
        before_id?: number;
        action?: string;
        resource_type?: string;
        resource_id?: string;
        conversation_id?: string;
        server_id?: string;
        limit?: number;
      } = { limit: pageSize };
      if (beforeId !== undefined) {
        query.before_id = beforeId;
      }
      if (action !== '') {
        query.action = action;
      }
      if (resourceType !== '') {
        query.resource_type = resourceType;
      }
      if (resourceId !== '') {
        query.resource_id = resourceId;
      }
      if (conversationId !== '') {
        query.conversation_id = conversationId;
      }
      if (serverId !== '') {
        query.server_id = serverId;
      }
      const { data, error, response } = await api.GET('/api/audit', {
        params: { query },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      if (!data) {
        throw new ApiRequestError(makeFailure('INTERNAL', 'empty audit response'));
      }
      return data;
    },
  });

  const records = auditQuery.data?.items ?? [];
  const hasMore = auditQuery.data?.has_more ?? false;
  const nextCursor = auditQuery.data?.next_before_id ?? null;

  const goNext = () => {
    if (nextCursor === null) {
      return;
    }
    setCursors([...cursors.slice(0, pageIndex + 1), nextCursor]);
    setPageIndex(pageIndex + 1);
  };

  const goPrevious = () => {
    setPageIndex(Math.max(0, pageIndex - 1));
  };

  const columns: TableColumnsType<AuditLog> = [
    {
      title: '时间',
      dataIndex: 'created_at',
      key: 'created_at',
      width: 175,
      render: (_value, record) => (
        <span className="opsmind-admin-mono">{formatDateTime(record.created_at)}</span>
      ),
    },
    {
      title: '操作者',
      key: 'actor',
      width: 150,
      render: (_value, record) => (
        <div>
          {/* 有用户名时主行显示「管理员 admin」，完整 UUID 放入下方 Tooltip。 */}
          <div>
            {actorTypeLabel(record.actor_type)}
            {record.actor_name ? ` ${record.actor_name}` : ''}
          </div>
          <Tooltip title={record.actor_id}>
            <div className="opsmind-admin-cell-sub">{shortId(record.actor_id, 14)}</div>
          </Tooltip>
        </div>
      ),
    },
    {
      title: '动作',
      dataIndex: 'action',
      key: 'action',
      width: 200,
      render: (_value, record) => (
        <div>
          <div>{actionLabel(record.action)}</div>
          <div className="opsmind-admin-cell-sub">{record.action}</div>
        </div>
      ),
    },
    {
      title: '目标',
      key: 'target',
      width: 170,
      render: (_value, record) => (
        <div>
          <div>{resourceTypeLabel(record.resource_type)}</div>
          <Tooltip title={record.resource_id}>
            <div className="opsmind-admin-cell-sub">
              {record.resource_id ? shortId(record.resource_id, 12) : '—'}
            </div>
          </Tooltip>
        </div>
      ),
    },
    {
      title: '命令摘要',
      key: 'command',
      width: 260,
      render: (_value, record) => {
        const summary = commandSummary(record.detail);
        if (!summary) {
          return <Typography.Text type="secondary">—</Typography.Text>;
        }
        return (
          <Tooltip title={summary}>
            <div className="opsmind-admin-clamp">{truncate(summary.replace(/\s+/g, ' '), 80)}</div>
          </Tooltip>
        );
      },
    },
    {
      title: '结果',
      key: 'outcome',
      width: 90,
      render: (_value, record) => {
        const outcome = auditOutcome(record);
        return outcome ? (
          <Tag color={outcome.color}>{outcome.label}</Tag>
        ) : (
          <Typography.Text type="secondary">—</Typography.Text>
        );
      },
    },
    {
      title: '耗时',
      key: 'duration',
      width: 100,
      render: (_value, record) => (
        <span className="opsmind-admin-mono">{formatDuration(auditDuration(record))}</span>
      ),
    },
  ];

  return (
    <div className="opsmind-admin-page">
      <PageHeader
        title="审计"
        description="只追加审计日志，按 id 倒序游标分页"
        extra={
          <Button
            icon={<ReloadOutlined />}
            loading={auditQuery.isFetching}
            onClick={() => void auditQuery.refetch()}
          >
            刷新
          </Button>
        }
      />

      <Flex wrap gap={12} align="center" className="opsmind-admin-toolbar">
        <Select
          style={{ width: 260 }}
          allowClear
          showSearch
          optionFilterProp="label"
          placeholder="按动作筛选"
          value={action === '' ? null : action}
          options={AUDIT_ACTIONS}
          onChange={(value: string | undefined) => {
            setAction(value ?? '');
            resetPaging();
          }}
        />
        <Select
          style={{ width: 180 }}
          allowClear
          showSearch
          optionFilterProp="label"
          placeholder="按目标类型筛选"
          value={resourceType === '' ? null : resourceType}
          options={RESOURCE_TYPE_OPTIONS}
          onChange={(value: string | undefined) => {
            setResourceType(value ?? '');
            resetPaging();
          }}
        />
        <Input.Search
          style={{ width: 300 }}
          allowClear
          prefix={<SearchOutlined />}
          placeholder="目标 ID（资源 UUID）"
          value={resourceIdInput}
          onChange={(event) => setResourceIdInput(event.target.value)}
          onSearch={(value) =>
            applyUuidFilter(
              value,
              (v) => {
                setResourceId(v);
              },
              '目标 ID',
            )
          }
          enterButton="筛选"
        />
        <Input.Search
          style={{ width: 330 }}
          allowClear
          prefix={<SearchOutlined />}
          placeholder="会话 ID（conversation UUID）"
          value={conversationIdInput}
          onChange={(event) => setConversationIdInput(event.target.value)}
          onSearch={(value) =>
            applyUuidFilter(
              value,
              (v) => {
                setConversationId(v);
              },
              '会话 ID',
            )
          }
          enterButton="筛选"
        />
        <Input.Search
          style={{ width: 330 }}
          allowClear
          prefix={<SearchOutlined />}
          placeholder="服务器 ID（server UUID）"
          value={serverIdInput}
          onChange={(event) => setServerIdInput(event.target.value)}
          onSearch={(value) =>
            applyUuidFilter(
              value,
              (v) => {
                setServerId(v);
              },
              '服务器 ID',
            )
          }
          enterButton="筛选"
        />
        <Select
          style={{ width: 140 }}
          value={pageSize}
          options={PAGE_SIZES.map((size) => ({ value: size, label: `${size} 条 / 页` }))}
          onChange={(value: number) => {
            setPageSize(value);
            resetPaging();
          }}
        />
        <Typography.Text type="secondary">第 {pageIndex + 1} 页</Typography.Text>
      </Flex>

      {auditQuery.isPending ? <AdminLoading rows={8} /> : null}
      {auditQuery.isError ? (
        <QueryErrorState error={auditQuery.error} onRetry={() => void auditQuery.refetch()} />
      ) : null}
      {auditQuery.isSuccess ? (
        <>
          <Table<AuditLog>
            rowKey="id"
            columns={columns}
            dataSource={records}
            size="middle"
            loading={auditQuery.isFetching}
            scroll={{ x: 1200 }}
            pagination={false}
            expandable={{
              expandedRowRender: (record) => (
                <AuditDetail record={record} onReplay={(id) => navigate(`/replays/${id}`)} />
              ),
            }}
            locale={{
              emptyText: <EmptyState description="没有符合条件的审计记录" />,
            }}
          />
          <Flex justify="space-between" align="center" style={{ marginTop: 16 }} wrap gap={12}>
            <Typography.Text type="secondary">
              {hasMore ? '还有更多记录' : '已到最后一页'} · 每页 {pageSize} 条
            </Typography.Text>
            <Space>
              <Button disabled={pageIndex === 0} onClick={goPrevious}>
                上一页
              </Button>
              <Button disabled={!hasMore || nextCursor === null} onClick={goNext}>
                下一页
              </Button>
            </Space>
          </Flex>
        </>
      ) : null}

      {/* 八期 FE-R（契约 2.12 约定 2）：Modal 回放形态下线——审计行「回放」
          直接 SPA 跳转 `/replays/<conversationId>`（见 AuditDetail）。 */}
    </div>
  );
}
