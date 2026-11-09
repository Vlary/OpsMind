import { LockOutlined, MenuOutlined, PlayCircleOutlined, RobotOutlined } from '@ant-design/icons';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Alert,
  AutoComplete,
  Button,
  Drawer,
  Grid,
  Modal,
  Select,
  Space,
  Spin,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link, useLocation, useNavigate, useParams, useBlocker } from 'react-router-dom';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import ApprovalModeSelect from '../../components/ApprovalModeSelect';
import { effectiveModeText } from '../../components/approvalMode';
import ErrorBanner from '../../components/ErrorBanner';
import { useComposerStore } from '../../stores/composerStore';
// 三期 p3 sprint 3：服务器上下文下拉与终端页共用选项文案口径。
import { termServerOptionLabel } from '../term/serverOptions';
import {
  useGlobalApprovalMode,
  useUpdateConversationApprovalMode,
} from '../settings/useApprovalSettings';
import Composer from './Composer';
import ConversationSidebar from './ConversationSidebar';
import { stripContextPrefix } from './markdownBlocks';
import {
  computeLiveSettled,
  describeError,
  isUnauthorizedError,
  toApiErrorInfo,
  useChatStream,
  type ApiErrorInfo,
} from './useChatStream';

// 消息列表（含虚拟滚动 @tanstack/react-virtual）仅在已有会话时需要，
// 拆为动态 import，不进首屏入口链。
const MessageList = lazy(() => import('./MessageList'));

// FE-P2（六期）：执行回放播放器（Modal）。仅点击「回放」时才加载。
const ReplayViewer = lazy(() => import('./ReplayViewer'));

type Provider = components['schemas']['Provider'];
type Server = components['schemas']['Server'];
type Database = components['schemas']['Database'];
type Message = components['schemas']['Message'];

interface PendingMessage {
  text: string;
  providerId: string;
  model: string;
  serverIds: string[];
  databaseIds: string[];
}

type LeaveTarget = { kind: 'chat' } | { kind: 'conversation'; id: string };

const EXAMPLES = [
  '列出所有已配置的服务器',
  '检查 web-1 的磁盘使用率和负载',
  '查看 nginx 服务最近的日志',
];

// ChatRequest.content 的契约上限（api/openapi.yaml）。
const CHAT_CONTENT_LIMIT = 16000;

// export 仅作单测钩子（__tests__/chatContext.test.ts）：纯函数，无行为变化。
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function buildContextContent(
  text: string,
  serverIds: string[],
  databaseIds: string[],
  servers: Server[],
  databases: Database[],
): string {
  if (serverIds.length === 0 && databaseIds.length === 0) {
    return text;
  }
  const lines = ['<!--opsmind-context'];
  if (serverIds.length > 0) {
    const names = serverIds.map((id) => {
      const server = servers.find((item) => item.id === id);
      return server ? `${server.name} (id=${server.id})` : id;
    });
    lines.push(`servers: ${names.join(', ')}`);
  }
  if (databaseIds.length > 0) {
    const names = databaseIds.map((id) => {
      const database = databases.find((item) => item.id === id);
      return database ? `${database.name} (id=${database.id})` : id;
    });
    lines.push(`databases: ${names.join(', ')}`);
  }
  lines.push('-->');
  return `${lines.join('\n')}\n${text}`;
}

function providerName(providers: Provider[], id: string | null | undefined): string {
  if (!id) {
    return '';
  }
  return providers.find((provider) => provider.id === id)?.name ?? id;
}

export default function ChatPage() {
  const { conversationId } = useParams<{ conversationId: string }>();
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const screens = Grid.useBreakpoint();
  const isMobile = screens.md === false;

  // 消息列表 chunk 预取：组件挂载时触发（而非模块顶层副作用）——顶层
  // void import 会随任何单测导入 ChatPage 的纯函数钩子一起执行，惰性
  // 链在 Vitest teardown 后 resolve 产生未处理拒绝（评审实测偶发 exit 1）。
  useEffect(() => {
    void import('./MessageList');
  }, []);

  const [drawerOpen, setDrawerOpen] = useState(false);
  const [leaveTarget, setLeaveTarget] = useState<LeaveTarget | null>(null);
  const [providerId, setProviderId] = useState('');
  const [model, setModel] = useState('');
  const [serverIds, setServerIds] = useState<string[]>([]);
  const [databaseIds, setDatabaseIds] = useState<string[]>([]);
  const [sendError, setSendError] = useState<string | null>(null);
  // FE-P2（六期）：执行回放入口开关（契约 8.4——流式进行中禁用，先等 done）。
  const [replayOpen, setReplayOpen] = useState(false);

  const onUnauthorized = useCallback(() => {
    const next = `${window.location.pathname}${window.location.search}`;
    navigate(`/login?next=${encodeURIComponent(next)}`, { replace: true });
  }, [navigate]);

  const setDraft = useComposerStore((state) => state.setDraft);
  const handleSendRejected = useCallback(
    (content: string) => {
      // content 是携带 <!--opsmind-context …--> 前缀的完整发送体；恢复草稿
      // 必须剥离前缀，否则用户重发时 buildContextContent 会叠加第二层前缀。
      setDraft(conversationId ?? 'new', stripContextPrefix(content));
    },
    [conversationId, setDraft],
  );

  const providersQuery = useQuery({
    queryKey: qk.providers,
    queryFn: async () => {
      const { data, error } = await api.GET('/api/providers');
      if (error) {
        throw error;
      }
      return data.items;
    },
  });

  const serversQuery = useQuery({
    queryKey: qk.servers,
    queryFn: async () => {
      const { data, error } = await api.GET('/api/servers');
      if (error) {
        throw error;
      }
      return data.items;
    },
  });

  const databasesQuery = useQuery({
    queryKey: qk.databases,
    queryFn: async () => {
      const { data, error } = await api.GET('/api/databases');
      if (error) {
        throw error;
      }
      return data.items;
    },
  });

  const conversationQuery = useQuery({
    queryKey: qk.conversation(conversationId ?? 'none'),
    queryFn: async () => {
      if (!conversationId) {
        throw new Error('missing conversation id');
      }
      const { data, error } = await api.GET('/api/conversations/{id}', {
        params: { path: { id: conversationId } },
      });
      if (error) {
        throw error;
      }
      return data;
    },
    enabled: Boolean(conversationId),
  });

  // 六期 FE-P1：会话头部门限覆盖（PATCH /api/conversations/:id）与全局
  // 门限（「跟随全局」项展示当前全局档）。变更即时生效并 toast；对流式
  // 进行中修改不禁止——门限只对「创建之后」的审批生效（契约 2.11 约定 1）。
  const approvalModeMutation = useUpdateConversationApprovalMode(conversationId);
  const globalApprovalModeQuery = useGlobalApprovalMode();

  const messagesQuery = useInfiniteQuery({
    queryKey: qk.messages(conversationId ?? 'none'),
    queryFn: async ({ pageParam }) => {
      if (!conversationId) {
        throw new Error('missing conversation id');
      }
      const { data, error } = await api.GET('/api/conversations/{id}/messages', {
        params: {
          path: { id: conversationId },
          query: {
            limit: 100,
            ...(pageParam ? { before_seq: pageParam } : {}),
          },
        },
      });
      if (error) {
        throw error;
      }
      return data;
    },
    initialPageParam: 0,
    // 首次加载即 error（后端重启/网络闪断窗口）时 react-query 会在无成功
    // 数据的渲染路径以 undefined 调用本函数；不做防御会让整页崩进
    // RouteErrorBoundary。undefined 直接视为无更多页。
    getNextPageParam: (lastPage) =>
      lastPage && lastPage.has_more && lastPage.next_before_seq !== null
        ? lastPage.next_before_seq
        : undefined,
    enabled: Boolean(conversationId),
  });

  const toolCallsQuery = useQuery({
    queryKey: qk.toolCalls(conversationId ?? 'none'),
    queryFn: async () => {
      if (!conversationId) {
        throw new Error('missing conversation id');
      }
      const { data, error } = await api.GET('/api/conversations/{id}/tool-calls', {
        params: { path: { id: conversationId }, query: { limit: 200 } },
      });
      if (error) {
        throw error;
      }
      return data;
    },
    enabled: Boolean(conversationId),
  });

  const approvalsQuery = useQuery({
    queryKey: qk.approvals(conversationId ?? 'none'),
    queryFn: async () => {
      if (!conversationId) {
        throw new Error('missing conversation id');
      }
      const { data, error } = await api.GET('/api/approvals', {
        params: { query: { conversation_id: conversationId, limit: 200 } },
      });
      if (error) {
        throw error;
      }
      return data;
    },
    enabled: Boolean(conversationId),
  });

  const stream = useChatStream(conversationId, {
    onUnauthorized,
    onSendRejected: handleSendRejected,
  });
  const streamSend = stream.send;
  const streamStop = stream.stop;
  const streamRetry = stream.retry;
  const streamApplyApproval = stream.applyApproval;
  const streamClearError = stream.clearError;

  useEffect(() => {
    const errors = [
      providersQuery.error,
      serversQuery.error,
      databasesQuery.error,
      conversationQuery.error,
      messagesQuery.error,
      toolCallsQuery.error,
      approvalsQuery.error,
    ];
    if (errors.some(isUnauthorizedError)) {
      onUnauthorized();
    }
  }, [
    providersQuery.error,
    serversQuery.error,
    databasesQuery.error,
    conversationQuery.error,
    messagesQuery.error,
    toolCallsQuery.error,
    approvalsQuery.error,
    onUnauthorized,
  ]);

  const providers = useMemo(() => providersQuery.data ?? [], [providersQuery.data]);
  const servers = useMemo(() => serversQuery.data ?? [], [serversQuery.data]);
  const databases = useMemo(() => databasesQuery.data ?? [], [databasesQuery.data]);
  const conversation = conversationQuery.data;

  const messages = useMemo<Message[]>(() => {
    const pages = messagesQuery.data?.pages ?? [];
    const items: Message[] = [];
    for (let index = pages.length - 1; index >= 0; index -= 1) {
      const page = pages[index];
      if (page) {
        items.push(...page.items);
      }
    }
    return items;
  }, [messagesQuery.data]);

  // useMemo 固定引用：data 未变时 items 引用稳定，避免下游 memo/依赖
  // 因 `?? []` 每帧新建数组而失效。
  const toolCalls = useMemo(() => toolCallsQuery.data?.items ?? [], [toolCallsQuery.data]);
  const approvals = approvalsQuery.data?.items ?? [];

  const streaming = stream.phase === 'connecting' || stream.phase === 'streaming';
  const live = stream.live;
  // 停止含 pending 审批的流会作废审批（不可恢复），仅此场景二次确认；
  // 纯流式停止单击直停（有重新生成与悬挂轮重发兜底）。SSE 实时审批
  // （live.approvals）与落库列表（approvalsQuery，SSE 审批事件触发
  // invalidate）合并判定，避免新建审批尚未 refetch 的窗口漏判。
  const hasPendingApproval =
    (live?.approvals.some((approval) => approval.status === 'pending') ?? false) ||
    approvals.some((approval) => approval.status === 'pending');
  // liveSettled 判定本体在 useChatStream.computeLiveSettled（纯函数，
  // 矩阵回归见 __tests__/liveSettledRace.test.ts）。done 分支只看
  // messages：撤 live 若还要等 toolCalls，两个并发 refetch 的落地间隙
  // 会同时渲染 live 条目与持久化消息两份正文（审批超时 E2E 全量并行下
  // 实测撞进该间隙）。间隙内的工具卡由 MessageList 的
  // liveToolsFallbackTurnId 回退渲染，卡片不消失。
  const liveSettled = useMemo(() => computeLiveSettled(live, messages), [live, messages]);
  const liveVisible = Boolean(live) && !liveSettled;
  const liveStreaming = streaming && live !== null && !live.done && !live.error && !live.stopped;

  const boundProviderId = conversation?.provider_id ?? null;
  const bound = Boolean(boundProviderId);
  const boundProviderName = providerName(providers, boundProviderId);

  // 未绑定会话使用本地选择；默认第一个供应商及其默认模型，允许手填模型。
  const explicitProvider =
    providerId && providers.some((provider) => provider.id === providerId) ? providerId : '';
  const effectiveProviderId = bound
    ? (boundProviderId ?? '')
    : explicitProvider || providers[0]?.id || '';
  const selectedProvider = providers.find((provider) => provider.id === effectiveProviderId);
  const effectiveModel = bound
    ? (conversation?.model ?? '')
    : model || selectedProvider?.default_model || selectedProvider?.models[0] || '';

  const modelOptions = useMemo(() => {
    const values = new Set<string>();
    if (selectedProvider) {
      if (selectedProvider.default_model) {
        values.add(selectedProvider.default_model);
      }
      for (const item of selectedProvider.models) {
        values.add(item);
      }
    }
    return Array.from(values).map((value) => ({ value }));
  }, [selectedProvider]);

  const handleProviderChange = useCallback(
    (id: string) => {
      setProviderId(id);
      const provider = providers.find((item) => item.id === id);
      setModel(provider?.default_model ?? provider?.models[0] ?? '');
    },
    [providers],
  );

  const createMutation = useMutation({
    mutationFn: async (input: { title: string }) => {
      const { data, error } = await api.POST('/api/conversations', {
        params: { header: { Origin: window.location.origin } },
        body: { title: input.title },
      });
      if (error) {
        throw error;
      }
      return data;
    },
  });

  const handleSendFromEmpty = useCallback(
    async (text: string): Promise<boolean> => {
      if (providers.length === 0 || providersQuery.isError) {
        return false;
      }
      const pending: PendingMessage = {
        text,
        providerId: effectiveProviderId,
        model: effectiveModel,
        serverIds,
        databaseIds,
      };
      try {
        const created = await createMutation.mutateAsync({
          title: text.slice(0, 30) || '新会话',
        });
        void queryClient.invalidateQueries({ queryKey: qk.conversations });
        navigate(`/c/${created.id}`, { state: { pendingMessage: pending } });
        return true;
      } catch {
        // 错误由 createMutation.error 展示；保留草稿以便重试。
        return false;
      }
    },
    [
      createMutation,
      databaseIds,
      effectiveModel,
      effectiveProviderId,
      navigate,
      providers.length,
      providersQuery.isError,
      queryClient,
      serverIds,
    ],
  );

  const handleSend = useCallback(
    async (text: string): Promise<boolean> => {
      if (!conversationId) {
        return handleSendFromEmpty(text);
      }
      if (!bound && (providers.length === 0 || providersQuery.isError)) {
        return false;
      }
      const content = buildContextContent(text, serverIds, databaseIds, servers, databases);
      if ([...content].length > CHAT_CONTENT_LIMIT) {
        setSendError(`消息过长：加上上下文后超过 ${CHAT_CONTENT_LIMIT} 字符上限。`);
        return false;
      }
      setSendError(null);
      void streamSend({
        content,
        ...(!bound && effectiveProviderId ? { providerId: effectiveProviderId } : {}),
        ...(!bound && effectiveModel ? { model: effectiveModel } : {}),
      });
      return true;
    },
    [
      bound,
      conversationId,
      databaseIds,
      databases,
      effectiveModel,
      effectiveProviderId,
      handleSendFromEmpty,
      providers.length,
      providersQuery.isError,
      servers,
      serverIds,
      streamSend,
    ],
  );

  // 路由状态里的待发送消息只消费一次。
  const pendingHandledRef = useRef(false);
  const lastConversationRef = useRef<string | undefined>(undefined);
  // 离开确认的「放行一次」标记：置 true 后当次导航不再被 useBlocker 拦截，
  // 到达新会话时在上面的 effect 里复位（否则跨会话切换一次后弹窗永久失效）。
  const leaveInProgressRef = useRef(false);
  useEffect(() => {
    if (lastConversationRef.current !== conversationId) {
      lastConversationRef.current = conversationId;
      pendingHandledRef.current = false;
      // 离开确认的「放行一次」标记只对当次导航生效：/c/a 与 /c/b 命中同一条
      // 路由（不 remount），到达新会话后必须复位，否则此后流式进行中的
      // 离开确认弹窗永久失效（违反 CONTRACT 8.6.8）。
      leaveInProgressRef.current = false;
    }
  }, [conversationId]);

  useEffect(() => {
    if (!conversationId || pendingHandledRef.current) {
      return;
    }
    const state = location.state as { pendingMessage?: PendingMessage } | null;
    const pending = state?.pendingMessage;
    if (!pending) {
      return;
    }
    // 延迟到 StrictMode 的二次 effect 之后，避免开发模式重复发送。
    const timer = setTimeout(() => {
      if (pendingHandledRef.current) {
        return;
      }
      pendingHandledRef.current = true;
      navigate(location.pathname, { replace: true, state: null });
      const content = buildContextContent(
        pending.text,
        pending.serverIds,
        pending.databaseIds,
        servers,
        databases,
      );
      void streamSend({
        content,
        ...(pending.providerId ? { providerId: pending.providerId } : {}),
        ...(pending.model ? { model: pending.model } : {}),
      });
    }, 0);
    return () => clearTimeout(timer);
  }, [conversationId, databases, location.pathname, location.state, navigate, servers, streamSend]);

  // 经确认弹窗后的离开不再触发 useBlocker 二次拦截（见 leaveInProgressRef）。

  const performLeave = useCallback(
    (target: LeaveTarget) => {
      leaveInProgressRef.current = true;
      streamStop();
      // 「新建会话」复用 /chat 草稿页：首条消息发送时才真正创建会话
      // （handleSendFromEmpty），避免点一下就落一条空「新会话」。
      if (target.kind === 'chat') {
        navigate('/chat');
        return;
      }
      navigate(`/c/${target.id}`);
    },
    [navigate, streamStop],
  );

  // CONTRACT.md 8.6.8：流式进行（含审批等待）时离开 /c/:id 必须先弹窗确认。
  // useBlocker 覆盖会话侧栏之外的全部离开路径：顶部导航菜单、浏览器后退/前进
  // 等 SPA 与 popstate 导航。经确认弹窗（performLeave）或登录过期跳转
  // （/login）的导航直接放行，不二次拦截。
  const blocker = useBlocker(
    ({ currentLocation, nextLocation }) =>
      streaming &&
      !leaveInProgressRef.current &&
      nextLocation.pathname !== '/login' &&
      currentLocation.pathname !== nextLocation.pathname,
  );

  // useBlocker 只能拦 SPA 导航；刷新/关标签会直接断开 SSE，后端随之取消
  // 未决审批并作废本轮（internal/agent/runner.go 流断开即 Cancel）。流式
  // 进行（含审批等待）期间挂 beforeunload，把「刷新会作废审批」的选择权
  // 交给用户；流结束即注销，不影响日常刷新。
  useEffect(() => {
    if (!streaming) {
      return;
    }
    const handleBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      // Chromium 需要 returnValue 触发原生确认框。
      event.returnValue = '';
    };
    window.addEventListener('beforeunload', handleBeforeUnload);
    return () => window.removeEventListener('beforeunload', handleBeforeUnload);
  }, [streaming]);

  const requestLeave = useCallback(
    (target: LeaveTarget) => {
      setDrawerOpen(false);
      if (streaming) {
        setLeaveTarget(target);
        return;
      }
      performLeave(target);
    },
    [performLeave, streaming],
  );

  const handleSidebarNavigate = useCallback(
    (id: string) => {
      if (id === conversationId) {
        setDrawerOpen(false);
        return;
      }
      requestLeave({ kind: 'conversation', id });
    },
    [conversationId, requestLeave],
  );

  const handleDeleted = useCallback(
    (id: string) => {
      if (id === conversationId) {
        leaveInProgressRef.current = true;
        streamStop();
        navigate('/chat', { replace: true });
      }
    },
    [conversationId, navigate, streamStop],
  );

  const handleApprovalUpdated = useCallback(
    (approval: components['schemas']['Approval']) => {
      streamApplyApproval(approval);
    },
    [streamApplyApproval],
  );

  // 历史失败回合的重新生成：取该轮 user 消息原文（含上下文前缀）按既有
  // send 语义重新发起一轮；新一轮会新增一条 user 消息，与流式失败横幅的
  // retry（仅未落库时原位重发）互补。
  const handleRegenerate = useCallback(
    (messageId: string) => {
      const index = messages.findIndex((item) => item.id === messageId);
      const failed = index >= 0 ? messages[index] : undefined;
      if (!failed) {
        return;
      }
      const sameTurnUser = messages.find(
        (item) => item.role === 'user' && item.turn_id === failed.turn_id,
      );
      let userContent = sameTurnUser?.content ?? '';
      if (!userContent) {
        for (let i = index - 1; i >= 0; i -= 1) {
          const item = messages[i];
          if (item?.role === 'user') {
            userContent = item.content;
            break;
          }
        }
      }
      if (!userContent) {
        return;
      }
      void streamSend({ content: userContent });
    },
    [messages, streamSend],
  );

  const conversationError = conversationQuery.error
    ? toApiErrorInfo(conversationQuery.error)
    : null;
  const messagesError = messagesQuery.error ? toApiErrorInfo(messagesQuery.error) : null;
  const providersError = providersQuery.error ? toApiErrorInfo(providersQuery.error) : null;
  const serversError = serversQuery.error ? toApiErrorInfo(serversQuery.error) : null;
  const databasesError = databasesQuery.error ? toApiErrorInfo(databasesQuery.error) : null;
  // 历史工具调用与审批卡片同样需要显式错误提示；静默回退空数组会让卡片
  // “消失”且无从重试，与同页其他查询的处理不一致。
  const toolCallsError = toolCallsQuery.error ? toApiErrorInfo(toolCallsQuery.error) : null;
  const approvalsError = approvalsQuery.error ? toApiErrorInfo(approvalsQuery.error) : null;
  const createError = createMutation.error ? toApiErrorInfo(createMutation.error) : null;

  // 上下文类查询（服务器/数据库/工具调用/审批卡片）失败聚合成一条横幅：
  // 后端断连/网络闪断会同时打挂全部查询，逐条展示会堆出多条几乎相同的
  // 错误条；重试时一并 refetch 所有失败的查询。
  const contextErrorEntries: { info: ApiErrorInfo | null; retry: () => void }[] = [
    { info: serversError, retry: () => void serversQuery.refetch() },
    { info: databasesError, retry: () => void databasesQuery.refetch() },
    { info: toolCallsError, retry: () => void toolCallsQuery.refetch() },
    { info: approvalsError, retry: () => void approvalsQuery.refetch() },
  ];
  const contextQueryErrors = contextErrorEntries.filter(
    (entry): entry is { info: ApiErrorInfo; retry: () => void } => entry.info !== null,
  );
  const firstContextError = contextQueryErrors[0]?.info ?? null;

  const providersUnavailable =
    !bound && (providersQuery.isLoading || providersQuery.isError || providers.length === 0);
  const composerDisabled =
    providersUnavailable ||
    Boolean(conversationError) ||
    conversationQuery.isLoading ||
    createMutation.isPending;
  const composerHint = providersQuery.isError ? (
    <span>模型供应商加载失败，请重试或前往模型管理。</span>
  ) : providersQuery.isLoading && !bound ? (
    <span>正在加载模型供应商…</span>
  ) : !bound && providers.length === 0 ? (
    <span>
      尚未配置模型供应商，请先前往
      <Link to="/providers">模型管理</Link>
      添加。
    </span>
  ) : conversationQuery.isLoading ? (
    <span>正在加载会话…</span>
  ) : conversationError ? (
    <span>
      会话不可用，请返回
      <Link to="/chat">新会话</Link>。
    </span>
  ) : null;

  // 六期打磨 sprint 5（走查 low）：顶栏标题超出 260px 即省略号截断
  // （global.css .chat-topbar-title），此前无 title/Tooltip，用户无处查看
  // 完整标题（侧栏同名条目有 title）。topbarTitle 同时用作文本与 Tooltip。
  const topbarTitle = conversation?.title ?? (conversationId ? '会话' : '新会话');

  // 上下文注释会拼在正文前，输入框可用长度相应扣减。
  const contextPrefixLength = useMemo(
    () => [...buildContextContent('', serverIds, databaseIds, servers, databases)].length,
    [databaseIds, databases, serverIds, servers],
  );
  const composerMaxLength = Math.max(1, CHAT_CONTENT_LIMIT - contextPrefixLength);

  const handleNew = useCallback(() => requestLeave({ kind: 'chat' }), [requestLeave]);

  const sidebar = (
    <ConversationSidebar
      activeId={conversationId}
      onNavigate={handleSidebarNavigate}
      onNew={handleNew}
      onDeleted={handleDeleted}
      creating={createMutation.isPending}
    />
  );

  return (
    <div className="chat-layout">
      {!isMobile ? sidebar : null}

      <div className="chat-main">
        <div className="chat-topbar">
          <Space size={8} wrap>
            {isMobile ? (
              <Button
                type="text"
                icon={<MenuOutlined />}
                onClick={() => setDrawerOpen(true)}
                aria-label="打开会话列表"
              />
            ) : null}
            {/* 六期打磨 sprint 5（走查 low）：/chat 是唯一没有页头的页面——
                导航「对话」、标签页 title「对话」、页面 h3 却是空态标语
                「开始新的运维对话」，三方不一致。这里补一个视觉弱化的
                页头 h3「对话」（15px、次级色），与全站 nav = title = 页头
                的口径对齐；会话标题紧随其后，仍是用户阅读的主角。 */}
            <Typography.Title level={3} className="chat-topbar-page-title">
              对话
            </Typography.Title>
            <span className="chat-topbar-page-divider" aria-hidden="true">
              /
            </span>
            {/* 与侧栏 ConversationSidebar 同口径：原生 title 属性给出完整
                标题（CSS 仍按 260px 省略号截断，hover 可看全文）。 */}
            <Typography.Text strong className="chat-topbar-title" title={topbarTitle}>
              {topbarTitle}
            </Typography.Text>
          </Space>

          <div className="chat-topbar-spacer" />

          <Space size={8} wrap>
            {conversationId ? (
              <Tooltip
                title={
                  streaming
                    ? '回答进行中，结束后可回放本轮执行过程'
                    : '按时间轴回放本会话的用户消息、工具调用、审批与执行'
                }
              >
                <Button
                  size="small"
                  icon={<PlayCircleOutlined />}
                  aria-label="回放"
                  disabled={streaming}
                  onClick={() => setReplayOpen(true)}
                >
                  回放
                </Button>
              </Tooltip>
            ) : null}
            {bound ? (
              <Space size={6} wrap>
                <Tag icon={<LockOutlined />} color="blue">
                  {boundProviderName || '已绑定供应商'}
                </Tag>
                <Tag>{conversation?.model || '默认模型'}</Tag>
              </Space>
            ) : providersQuery.isError ? (
              <Button size="small" danger onClick={() => void providersQuery.refetch()}>
                重试加载供应商
              </Button>
            ) : providersQuery.isLoading ? (
              <Spin size="small" />
            ) : providers.length === 0 ? (
              <Link to="/providers">
                <Button size="small" type="primary" ghost>
                  配置模型
                </Button>
              </Link>
            ) : (
              <>
                <Select
                  size="small"
                  value={effectiveProviderId || null}
                  placeholder="供应商"
                  style={{ minWidth: 130 }}
                  options={providers.map((provider) => ({
                    value: provider.id,
                    label: provider.name,
                  }))}
                  onChange={handleProviderChange}
                />
                <AutoComplete
                  size="small"
                  value={effectiveModel}
                  options={modelOptions}
                  placeholder="模型"
                  style={{ width: 200 }}
                  onChange={(value: string) => setModel(value)}
                />
              </>
            )}
            {/* 六期 FE-P1：会话审批门限覆盖下拉（会话标题/模型选择器旁，
                契约 8.2/8.7）。显示生效门限与来源（effectiveModeText）；
                「跟随全局」提交 approval_mode:null；保存即时生效并 toast。 */}
            {conversationId && conversation ? (
              <Tooltip
                title={`会话审批门限：${effectiveModeText(
                  conversation.approval_mode,
                  conversation.effective_approval_mode,
                )}。变更即时生效，只影响之后新建的审批。`}
              >
                <Space size={4} wrap>
                  <ApprovalModeSelect
                    size="small"
                    allowFollowGlobal
                    ariaLabel="会话审批门限"
                    globalMode={
                      globalApprovalModeQuery.data?.approval_mode ??
                      conversation.effective_approval_mode
                    }
                    value={conversation.approval_mode}
                    disabled={approvalModeMutation.isPending}
                    onChange={(next) => approvalModeMutation.mutate(next)}
                  />
                  <Typography.Text type="secondary" className="chat-topbar-mode-source">
                    生效：
                    {effectiveModeText(
                      conversation.approval_mode,
                      conversation.effective_approval_mode,
                    )}
                  </Typography.Text>
                </Space>
              </Tooltip>
            ) : null}
            <Select
              size="small"
              mode="multiple"
              allowClear
              value={serverIds}
              placeholder="服务器上下文"
              style={{ minWidth: 150 }}
              maxTagCount={1}
              // p3 sprint 3：与终端页同口径（termServerOptionLabel）——
              // 此前仅裸 server.name，网络设备无品牌/协议/类型标识，同名
              // 时无法与服务器区分（实测走查 sprint 3）。经共享纯函数模块
              // 引入，不把 TermPage（xterm 链）拉进对话 chunk。
              options={servers.map((server) => ({
                value: server.id,
                label: termServerOptionLabel(server),
              }))}
              onChange={(value: string[]) => setServerIds(value)}
              // 同 TermPage：资产量级无需虚拟化，虚拟列表只渲染可视区
              // 选项，底部选项缺 DOM 会让无障碍/E2E 定位失效。
              virtual={false}
            />
            <Select
              size="small"
              mode="multiple"
              allowClear
              value={databaseIds}
              placeholder="数据库上下文"
              style={{ minWidth: 150 }}
              maxTagCount={1}
              options={databases.map((database) => ({
                value: database.id,
                label: database.name,
              }))}
              onChange={(value: string[]) => setDatabaseIds(value)}
              virtual={false}
            />
          </Space>
        </div>

        {firstContextError ? (
          <div className="chat-stream-error">
            <ErrorBanner
              code={firstContextError.code}
              message={
                contextQueryErrors.length > 1
                  ? `${contextQueryErrors.length} 项数据加载失败：${describeError(firstContextError.code, firstContextError.message)}`
                  : describeError(firstContextError.code, firstContextError.message)
              }
              requestId={firstContextError.requestId}
              onRetry={() => contextQueryErrors.forEach((entry) => entry.retry())}
            />
          </div>
        ) : null}

        {conversationId ? (
          conversationQuery.isLoading ? (
            <div className="chat-messages chat-messages--center">
              <Spin />
            </div>
          ) : conversationError ? (
            <div className="chat-messages chat-messages--center chat-error-area">
              <ErrorBanner
                code={conversationError.code}
                message={describeError(conversationError.code, conversationError.message)}
                requestId={conversationError.requestId}
                onRetry={() => void conversationQuery.refetch()}
              />
            </div>
          ) : messagesError ? (
            <div className="chat-messages chat-messages--center chat-error-area">
              <ErrorBanner
                code={messagesError.code}
                message={describeError(messagesError.code, messagesError.message)}
                requestId={messagesError.requestId}
                onRetry={() => void messagesQuery.refetch()}
              />
            </div>
          ) : (
            <Suspense
              fallback={
                <div className="chat-messages chat-messages--center">
                  <Spin />
                </div>
              }
            >
              <MessageList
                conversationId={conversationId}
                messages={messages}
                toolCalls={toolCalls}
                approvals={approvals}
                live={live}
                liveVisible={liveVisible}
                streaming={liveStreaming}
                loading={messagesQuery.isLoading}
                hasOlder={Boolean(messagesQuery.hasNextPage)}
                loadingOlder={messagesQuery.isFetchingNextPage}
                onLoadOlder={() => void messagesQuery.fetchNextPage()}
                onApprovalUpdated={handleApprovalUpdated}
                onRegenerate={handleRegenerate}
              />
            </Suspense>
          )
        ) : (
          <div className="chat-messages chat-messages--center">
            <div className="chat-empty">
              <RobotOutlined className="chat-empty-icon" />
              <Typography.Title level={3} style={{ margin: '8px 0' }}>
                开始新的运维对话
              </Typography.Title>
              <Typography.Paragraph type="secondary" style={{ maxWidth: 560 }}>
                描述目标，OpsMind 会读取服务器与数据库信息并给出结论。高风险命令会先进入审批，
                未经批准不会执行。
              </Typography.Paragraph>
              <Space wrap>
                {EXAMPLES.map((example) => (
                  <Button
                    key={example}
                    onClick={() => handleSendFromEmpty(example)}
                    disabled={
                      providersQuery.isLoading ||
                      providersQuery.isError ||
                      providers.length === 0 ||
                      createMutation.isPending
                    }
                  >
                    {example}
                  </Button>
                ))}
              </Space>
              {providersQuery.isError ? (
                <div style={{ marginTop: 20, width: '100%', maxWidth: 560 }}>
                  <ErrorBanner
                    code={providersError?.code ?? 'UNKNOWN'}
                    message={describeError(
                      providersError?.code ?? 'UNKNOWN',
                      providersError?.message,
                    )}
                    requestId={providersError?.requestId ?? ''}
                    onRetry={() => void providersQuery.refetch()}
                  />
                </div>
              ) : providersQuery.isLoading ? (
                <div style={{ marginTop: 20 }}>
                  <Spin size="small" />{' '}
                  <Typography.Text type="secondary">正在加载模型供应商…</Typography.Text>
                </div>
              ) : providers.length === 0 ? (
                <Alert
                  style={{ marginTop: 20, maxWidth: 560 }}
                  type="warning"
                  showIcon
                  message="尚未配置模型供应商"
                  description="请先在模型管理中添加供应商与模型，再开始对话。"
                  action={
                    <Link to="/providers">
                      <Button size="small" type="primary">
                        去模型管理
                      </Button>
                    </Link>
                  }
                />
              ) : null}
            </div>
          </div>
        )}

        {live?.error ? (
          <div className="chat-stream-error">
            <ErrorBanner
              code={live.error.code}
              message={live.error.message}
              requestId={live.error.requestId}
              {...(live.error.retryable && !live.persisted ? { onRetry: () => streamRetry() } : {})}
            />
            <Button type="text" size="small" onClick={streamClearError}>
              忽略
            </Button>
          </div>
        ) : null}

        {createError ? (
          <div className="chat-stream-error">
            <ErrorBanner
              code={createError.code}
              message={describeError(createError.code, createError.message)}
              requestId={createError.requestId}
            />
          </div>
        ) : null}

        {sendError ? (
          <div className="chat-stream-error">
            <Alert
              type="error"
              showIcon
              closable
              message={sendError}
              onClose={() => setSendError(null)}
            />
          </div>
        ) : null}

        <Composer
          conversationId={conversationId ?? 'new'}
          streaming={streaming}
          disabled={composerDisabled}
          disabledHint={composerHint}
          maxLength={composerMaxLength}
          onSend={handleSend}
          onStop={streamStop}
          stopRequiresConfirm={hasPendingApproval}
          placeholder={
            conversationId
              ? '描述你的运维需求，或直接粘贴命令…'
              : '描述你的运维需求，发送后自动创建会话…'
          }
        />
      </div>

      {isMobile ? (
        <Drawer
          open={drawerOpen}
          placement="left"
          width={300}
          title="会话"
          onClose={() => setDrawerOpen(false)}
          styles={{ body: { padding: 0 } }}
        >
          {sidebar}
        </Drawer>
      ) : null}

      {/* FE-P2（六期）：执行回放播放器。流式进行中入口禁用（上方的按钮
          已按 streaming 屏蔽），打开时每次重取（契约 8.4）。 */}
      {conversationId && replayOpen ? (
        <Suspense fallback={null}>
          <ReplayViewer
            key={conversationId}
            open={replayOpen}
            conversationId={conversationId}
            title={conversation?.title}
            onClose={() => setReplayOpen(false)}
          />
        </Suspense>
      ) : null}

      {/* 离开确认文案按是否真有会话内 pending 审批区分：仅流式进行时离开
          只会停止本轮回答（batch/诊断审批落库独立于会话流，跳走不受影响），
          「审批将失效」只对会话内工具（ssh_exec 等）的未决审批成立——统一
          恐吓式文案会误导用户（五期走查：工具卡引导跳 /batch 被本弹窗拦截，
          实际点「停止并离开」后批量页审批完全正常）。拦截与二次确认本身
          不变（契约 8.6.8 流式进行离开必弹窗）。 */}
      <Modal
        open={leaveTarget !== null}
        title="当前回答仍在进行"
        okText="停止并切换"
        cancelText="继续等待"
        okButtonProps={{ danger: true }}
        onOk={() => {
          const target = leaveTarget;
          setLeaveTarget(null);
          if (target) {
            performLeave(target);
          }
        }}
        onCancel={() => setLeaveTarget(null)}
      >
        {hasPendingApproval
          ? '当前回答将停止，未处理的审批将失效。'
          : '当前回答将停止；批量任务与诊断会话的审批不受影响，可放心离开。'}
      </Modal>

      <Modal
        open={blocker.state === 'blocked'}
        title="当前回答仍在进行"
        okText="停止并离开"
        cancelText="继续等待"
        okButtonProps={{ danger: true }}
        onOk={() => {
          leaveInProgressRef.current = true;
          streamStop();
          blocker.proceed?.();
        }}
        onCancel={() => blocker.reset?.()}
      >
        {hasPendingApproval
          ? '当前回答将停止，未处理的审批将失效。'
          : '当前回答将停止；批量任务与诊断会话的审批不受影响，可放心离开。'}
      </Modal>
    </div>
  );
}
