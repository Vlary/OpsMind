// 审批上下文抽屉（RBAC 走查 sprint 2，契约 12.3 第 23 条）。
//
// 审批者此前在审批队列只看得到命令、风险与目标服务器，看不到发起这场
// 对话的用户原话和工具参数凭据（截图走查 medium）。后端早已提供有界
// 上下文接口 GET /api/approvals/{id}/context（审批 + tool_call + 资产 +
// 审批前最多 20 条消息，审批后新消息绝不返回），前端从未调用。本组件
// 只做只读展示，不新增任何授权面：入口按 approvals:read 收敛，真正的
// 跨用户读取审计仍由后端写 approval.context_viewed。

import { useQuery } from '@tanstack/react-query';
import { Alert, Descriptions, Drawer, Empty, Space, Spin, Tag, Typography } from 'antd';

import { api } from '../api/client';
import { qk } from '../api/queryKeys';
import type { components } from '../api/schema';
import ApiErrorAlert from './admin/ApiErrorAlert';
import { ApiRequestError, failureFromError, toApiFailure } from './admin/apiError';
import CodeBlock from './CodeBlock';
import RiskBadge from './RiskBadge';

type Message = components['schemas']['Message'];

const MESSAGE_ROLE_LABELS: Record<Message['role'], string> = {
  user: '用户',
  assistant: '助手',
};

/** 消息角色中文标签（未知值原样保留，避免丢排查信息）。 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function messageRoleLabel(role: string): string {
  return MESSAGE_ROLE_LABELS[role as Message['role']] ?? role;
}

/**
 * 工具参数格式化为可复制的 JSON。
 *
 * 参数为空（无 tool_call 或 arguments 为空对象）时返回空串，由调用方
 * 决定展示「无工具参数」而不是一个空的代码块。序列化失败（循环引用等
 * 异常数据）同样返回空串，抽屉降级为纯文本提示，不抛错整页崩。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function formatToolArguments(args: Record<string, unknown> | null | undefined): string {
  if (!args || Object.keys(args).length === 0) {
    return '';
  }
  try {
    return JSON.stringify(args, null, 2);
  } catch {
    return '';
  }
}

interface ApprovalContextDrawerProps {
  approvalId: string;
  open: boolean;
  onClose: () => void;
}

export default function ApprovalContextDrawer({
  approvalId,
  open,
  onClose,
}: ApprovalContextDrawerProps) {
  const contextQuery = useQuery({
    queryKey: qk.approvalContext(approvalId),
    // 只在抽屉打开时请求：审批队列轮询不额外放大上下文读取（跨用户读取
    // 会写审计），关闭后保留缓存供再次打开即时展示。
    enabled: open,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/approvals/{id}/context', {
        params: { path: { id: approvalId } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
  });

  const context = contextQuery.data;
  const toolArgs = context?.tool_call ? formatToolArguments(context.tool_call.arguments) : '';

  return (
    <Drawer
      open={open}
      onClose={onClose}
      size={640}
      title="审批上下文"
      styles={{ body: { paddingTop: 16 } }}
    >
      <Space orientation="vertical" size={12} style={{ width: '100%' }}>
        <Alert
          type="info"
          showIcon
          title="审批创建前的上下文"
          description="最多展示审批前 20 条消息，不含审批后的新消息。查看他人发起的审批会写入审计。"
        />

        {contextQuery.isPending ? (
          <div className="app-center">
            <Spin />
          </div>
        ) : null}

        {contextQuery.isError ? (
          <ApiErrorAlert
            failure={failureFromError(contextQuery.error)}
            onRetry={() => void contextQuery.refetch()}
          />
        ) : null}

        {context ? (
          <>
            <Descriptions size="small" column={1} bordered>
              <Descriptions.Item label="发起人">
                {context.approval.requested_by_name || context.approval.requested_by || '未知'}
              </Descriptions.Item>
              <Descriptions.Item label="目标服务器">
                {context.server
                  ? `${context.server.name}（${context.server.host}）`
                  : context.approval.server_name || '未知'}
              </Descriptions.Item>
              <Descriptions.Item label="风险">
                <Space size={8} wrap>
                  <RiskBadge level={context.approval.risk_level} />
                  <Typography.Text>{context.approval.reason || '—'}</Typography.Text>
                </Space>
              </Descriptions.Item>
            </Descriptions>

            <div>
              <Typography.Text strong>工具参数</Typography.Text>
              {context.tool_call ? (
                <Space orientation="vertical" size={6} style={{ width: '100%', marginTop: 6 }}>
                  <Space size={8} wrap>
                    <Tag color="blue">{context.tool_call.name}</Tag>
                    <Typography.Text type="secondary">{context.tool_call.status}</Typography.Text>
                  </Space>
                  {toolArgs ? (
                    <CodeBlock code={toolArgs} language="json" />
                  ) : (
                    <Typography.Text type="secondary">无工具参数</Typography.Text>
                  )}
                </Space>
              ) : (
                <div style={{ marginTop: 6 }}>
                  <Typography.Text type="secondary">
                    该审批没有关联工具调用（如终端授权或策略变更）。
                  </Typography.Text>
                </div>
              )}
            </div>

            <div>
              <Typography.Text strong>审批前消息（{context.messages.length} 条）</Typography.Text>
              {context.messages.length === 0 ? (
                <div style={{ marginTop: 8 }}>
                  <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有可展示的历史消息" />
                </div>
              ) : (
                <Space orientation="vertical" size={10} style={{ width: '100%', marginTop: 8 }}>
                  {context.messages.map((message) => (
                    <div key={message.id} className="opsmind-approval-context-message">
                      <Space size={8} wrap>
                        <Tag color={message.role === 'user' ? 'blue' : 'default'}>
                          {messageRoleLabel(message.role)}
                        </Tag>
                        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                          #{message.seq}
                        </Typography.Text>
                      </Space>
                      <Typography.Paragraph style={{ margin: '6px 0 0', whiteSpace: 'pre-wrap' }}>
                        {message.content || '（空消息）'}
                      </Typography.Paragraph>
                    </div>
                  ))}
                </Space>
              )}
            </div>
          </>
        ) : null}
      </Space>
    </Drawer>
  );
}
