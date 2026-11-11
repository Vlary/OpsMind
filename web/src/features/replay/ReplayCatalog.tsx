// FE-R（八期）：回放页左栏会话目录（契约 2.12 约定 4/5，8.7 ⑧）。
//
// 数据来自八期新增的 `GET /api/replays`（ReplayCatalogPage：有界聚合、游标分页、
// q 与服务端过滤、has_actions 开关）。目录只读计数与时间，不渲染正文——
// 因此 100 个会话也只有一次聚合查询，不会像「前端逐会话取 /replay」那样
// 产生 100 次 20000 步重建（契约 5.19.1 的理由）。
//
// 选中会话来自 URL（`/replays/:conversationId`），本组件只上报点击；
// 不得把「当前回放会话」放进任何 store（8.5：刷新/分享必须可复原）。

import { useInfiniteQuery } from '@tanstack/react-query';
import { Button, Input, Space, Switch, Tag, Typography } from 'antd';
import { useEffect, useMemo, useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import EmptyState from '../../components/EmptyState';
import { AdminLoading, QueryErrorState } from '../../components/admin/AdminStates';
import { formatDateTime } from '../../components/admin/format';
import { Link } from 'react-router-dom';

type ReplayCatalogItem = components['schemas']['ReplayCatalogItem'];
type ReplayCatalogCursor = { last_message_at: string; id: string };

export interface ReplayCatalogProps {
  /** 当前选中的会话 ID（URL 参数）；null 表示未选中。 */
  activeId: string | null;
  /** 选中一行（页面负责 navigate 到 /replays/<id>）。 */
  onSelect: (conversationId: string) => void;
}

/** 目录页大小：后端缺省 50、上限 200（契约 5.19.1）。 */
const CATALOG_LIMIT = 50;

/**
 * 目录展平后的硬上限（防 OOM 铁律第 1 条：任何累积必须有写入上限）。
 *
 * 「加载更早」由用户逐次点击驱动，但长目录仍会一路累积；1000 行约等于
 * 20 页，足以覆盖真实使用，同时给内存一个写死的上界。触顶后不再渲染
 * 「加载更早」按钮（后端游标仍可用，刷新页面即可从最新页重新开始）。
 */
const MAX_CATALOG_ROWS = 1000;

/** 搜索防抖（契约 8.7 ⑧：300ms）。 */
const SEARCH_DEBOUNCE_MS = 300;

/**
 * q 输入 → 防抖后的服务端查询值。
 *
 * 300ms 内的连续输入不切 query key（否则每个字符都会发起一次聚合查询）；
 * 定时器在卸载/下一次输入时清理，无悬挂回调。
 */
function useDebouncedValue(value: string, delayMs: number): string {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = window.setTimeout(() => setDebounced(value), delayMs);
    return () => window.clearTimeout(timer);
  }, [value, delayMs]);
  return debounced;
}

export default function ReplayCatalog({ activeId, onSelect }: ReplayCatalogProps) {
  const [keyword, setKeyword] = useState('');
  const [hasActions, setHasActions] = useState(false);
  const query = useDebouncedValue(keyword.trim(), SEARCH_DEBOUNCE_MS);

  const catalogQuery = useInfiniteQuery({
    // 契约 8.4：q / has_actions 变化直接切 key（不失效旧 key）；
    // staleTime 30s、窗口聚焦重取（目录页可能长时间停留）。
    queryKey: qk.replays({ q: query, has_actions: hasActions }),
    initialPageParam: undefined as ReplayCatalogCursor | undefined,
    queryFn: async ({ pageParam }) => {
      const params: {
        limit: number;
        q?: string;
        has_actions?: boolean;
        before_last_message_at?: string;
        before_id?: string;
      } = { limit: CATALOG_LIMIT };
      if (query !== '') {
        params.q = query;
      }
      if (hasActions) {
        params.has_actions = true;
      }
      if (pageParam) {
        params.before_last_message_at = pageParam.last_message_at;
        params.before_id = pageParam.id;
      }
      const { data, error } = await api.GET('/api/replays', { params: { query: params } });
      if (error) {
        throw error;
      }
      return data;
    },
    getNextPageParam: (lastPage) =>
      lastPage.has_more && lastPage.next_cursor ? lastPage.next_cursor : undefined,
    staleTime: 30_000,
    refetchOnWindowFocus: true,
  });

  // 跨页展平（每页 ≤50 行，页数由用户点「加载更早」驱动），并钳制到
  // MAX_CATALOG_ROWS（写死上限，防无限累积）。
  const items = useMemo<ReplayCatalogItem[]>(() => {
    const pages = catalogQuery.data?.pages ?? [];
    const flat: ReplayCatalogItem[] = [];
    for (const page of pages) {
      for (const item of page.items) {
        if (flat.length >= MAX_CATALOG_ROWS) {
          return flat;
        }
        flat.push(item);
      }
    }
    return flat;
  }, [catalogQuery.data]);

  const body = catalogQuery.isPending ? (
    <div className="replay-catalog-list">
      <AdminLoading rows={6} />
    </div>
  ) : catalogQuery.isError ? (
    <div className="replay-catalog-list">
      <QueryErrorState error={catalogQuery.error} onRetry={() => void catalogQuery.refetch()} />
    </div>
  ) : items.length === 0 ? (
    <div className="replay-catalog-list">
      <EmptyState
        description={query !== '' || hasActions ? '没有符合条件的历史会话' : '暂无可回放的会话'}
      >
        <Link to="/chat">
          <Button size="small">去对话页</Button>
        </Link>
      </EmptyState>
    </div>
  ) : (
    <div className="replay-catalog-list" role="group" aria-label="可回放会话">
      {items.map((item) => {
        const active = item.conversation_id === activeId;
        return (
          <button
            key={item.conversation_id}
            type="button"
            aria-current={active ? 'true' : undefined}
            className={`replay-catalog-item${active ? ' replay-catalog-item--active' : ''}`}
            onClick={() => onSelect(item.conversation_id)}
          >
            <span className="replay-catalog-title" title={item.title || '（未命名会话）'}>
              {item.title || '（未命名会话）'}
            </span>
            <span className="replay-catalog-meta">
              <span>{formatDateTime(item.last_message_at)}</span>
              <Tag>工具 {item.tool_count}</Tag>
              <Tag>审批 {item.approval_count}</Tag>
              {item.has_actions ? <Tag color="green">有动作</Tag> : null}
              {/* 目录侧只是步数上限预判；进入播放器后以真实
                  truncated_reason 为准（契约 5.19.1）。 */}
              {item.truncated ? <Tag color="orange">仅显示最近步</Tag> : null}
            </span>
          </button>
        );
      })}
    </div>
  );

  return (
    <div className="opsmind-replay-catalog">
      <div className="replay-catalog-toolbar">
        <Input
          allowClear
          value={keyword}
          onChange={(event) => setKeyword(event.target.value)}
          placeholder="搜索会话标题"
          aria-label="搜索回放会话"
        />
        <Space size={8} align="center">
          <Switch
            size="small"
            checked={hasActions}
            onChange={setHasActions}
            aria-label="只看有动作的会话"
          />
          <Typography.Text style={{ fontSize: 12 }}>只看有动作的会话</Typography.Text>
        </Space>
      </div>

      {body}

      {catalogQuery.hasNextPage && items.length < MAX_CATALOG_ROWS ? (
        <div className="replay-catalog-footer">
          <Button
            size="small"
            loading={catalogQuery.isFetchingNextPage}
            onClick={() => void catalogQuery.fetchNextPage()}
          >
            加载更早
          </Button>
        </div>
      ) : null}
    </div>
  );
}
