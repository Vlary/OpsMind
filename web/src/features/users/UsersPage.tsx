import { PlusOutlined, ReloadOutlined } from '@ant-design/icons';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Flex, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { TableColumnsType } from 'antd';
import { useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
import type { components } from '../../api/schema';
import EmptyState from '../../components/EmptyState';
import PageHeader from '../../components/PageHeader';
import { AdminLoading, QueryErrorState } from '../../components/admin/AdminStates';
import ApiErrorAlert from '../../components/admin/ApiErrorAlert';
import ConfirmActionModal from '../../components/admin/ConfirmActionModal';
import {
  ApiRequestError,
  failureFromError,
  makeFailure,
  type ApiFailure,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import { formatDateTime } from '../../components/admin/format';
import { message } from '../../components/antdMessage';
import { useMe } from '../auth/useMe';
import { hasAnyPermission, PERMS, roleLabel, roleTagColor } from '../../lib/rbacPermissions';
import '../../styles/admin.css';
import UserCreateModal from './UserCreateModal';
import UserRoleModal from './UserRoleModal';
import {
  adminGuardHint,
  canChangeRole,
  canDisableUser,
  canEnableUser,
  canResetPassword,
  canResendInvite,
  canRevokeSessions,
  isInvitePending,
  userStatus,
  type GuardResult,
  type User,
} from './usersView';

type CredentialReset = components['schemas']['CredentialResetResponse'];

type UserAction =
  | { kind: 'toggle'; user: User }
  | { kind: 'reset'; user: User }
  | { kind: 'resend'; user: User }
  | { kind: 'revoke'; user: User };

interface ActionButtonProps {
  label: string;
  disabled: boolean;
  reason?: string | undefined;
  danger?: boolean;
  onClick: () => void;
}

/** 行内操作按钮：禁用原因用 Tooltip 说明（护栏提示不只靠点击后的 409）。 */
function ActionButton({ label, disabled, reason, danger = false, onClick }: ActionButtonProps) {
  return (
    <Tooltip title={reason}>
      <span>
        <Button type="link" size="small" danger={danger} disabled={disabled} onClick={onClick}>
          {label}
        </Button>
      </span>
    </Tooltip>
  );
}

function denyReason(
  guard: GuardResult,
  permitted: boolean,
  permissionHint: string,
): string | undefined {
  if (!permitted) {
    return permissionHint;
  }
  return guard.allowed ? undefined : guard.reason;
}

/**
 * 用户管理（00012，CONTRACT.md 12.6）。
 *
 * 列表/创建/改角色/启用停用/重置密码。前端护栏与服务端一致：
 * 不能停用自己、不能停用或降级最后一个启用中的管理员、不能重置自己
 * （12.3 第 14/15 条）；并发窗口下的最终判定仍在服务端（409
 * LAST_ADMIN / SELF_OPERATION）。
 */
export default function UsersPage() {
  const queryClient = useQueryClient();
  const meQuery = useMe();
  const permissions = meQuery.data?.permissions ?? [];
  const currentUserId = meQuery.data?.user.id ?? '';
  const canWrite = hasAnyPermission(permissions, [PERMS.usersWrite]);
  const canReset = hasAnyPermission(permissions, [PERMS.usersReset]);

  const [createOpen, setCreateOpen] = useState(false);
  const [roleTarget, setRoleTarget] = useState<User | null>(null);
  const [toggleTarget, setToggleTarget] = useState<User | null>(null);
  const [resetTarget, setResetTarget] = useState<User | null>(null);
  const [resendTarget, setResendTarget] = useState<User | null>(null);
  const [revokeTarget, setRevokeTarget] = useState<User | null>(null);
  const [actionFailure, setActionFailure] = useState<ApiFailure | null>(null);

  const usersQuery = useQuery({
    queryKey: qk.users,
    queryFn: async () => {
      const { data, error, response } = await api.GET('/api/users', {
        params: { query: { limit: 200 } },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      if (!data) {
        throw new ApiRequestError(makeFailure('INTERNAL', 'empty user list response'));
      }
      return data.items;
    },
  });

  const users = usersQuery.data ?? [];

  const invalidate = () => void queryClient.invalidateQueries({ queryKey: qk.users });

  const actionMutation = useMutation({
    mutationFn: async (action: UserAction) => {
      const { user } = action;
      if (action.kind === 'resend') {
        // 重发邀请走 POST /api/users 同名路径：服务端作废旧 invite、重新
        // 签发并投递，不会重复创建用户（第 5 轮走查 medium）。
        const { data, error, response } = await api.POST('/api/users', {
          body: {
            username: user.username,
            email: user.email,
            role: user.role,
            ...(user.display_name ? { display_name: user.display_name } : {}),
          },
          params: { header: writeHeaders() },
        });
        if (error) {
          throw new ApiRequestError(toApiFailure(error, response));
        }
        if (!data) {
          throw new ApiRequestError(makeFailure('INTERNAL', 'empty user response'));
        }
        return { kind: 'resend' as const, user, result: data as User };
      }
      if (action.kind === 'toggle') {
        const { error, response } = await api.PATCH('/api/users/{id}', {
          params: { path: { id: user.id }, header: writeHeaders() },
          body: { enabled: !user.enabled },
        });
        if (error) {
          throw new ApiRequestError(toApiFailure(error, response));
        }
        return action;
      }
      if (action.kind === 'reset') {
        const { data, error, response } = await api.POST('/api/users/{id}/password', {
          params: { path: { id: user.id }, header: writeHeaders() },
        });
        if (error) {
          throw new ApiRequestError(toApiFailure(error, response));
        }
        if (!data) {
          throw new ApiRequestError(makeFailure('INTERNAL', 'empty credential reset response'));
        }
        return { kind: 'reset' as const, user, result: data as CredentialReset };
      }
      const { error, response } = await api.POST('/api/users/{id}/revoke-sessions', {
        params: { path: { id: user.id }, header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return action;
    },
    onSuccess: (result) => {
      setActionFailure(null);
      setToggleTarget(null);
      setResetTarget(null);
      setResendTarget(null);
      setRevokeTarget(null);
      if (result.kind === 'reset') {
        const delivered = result.result.delivered;
        message.success(
          delivered
            ? `重置链接已发送到 ${result.user.email || '目标邮箱'}，有效期至 ${formatDateTime(result.result.expires_at)}`
            : `重置令牌已签发，但投递状态未知，请检查通知通道（有效期至 ${formatDateTime(result.result.expires_at)}）`,
        );
      } else if (result.kind === 'resend') {
        // 以服务端返回的行为准：同名重发会把账号邮箱同步为本次请求的地址。
        message.success(
          `邀请已重新发送到 ${result.result.email || result.user.email || '目标邮箱'}，旧邀请链接已失效`,
        );
      } else if (result.kind === 'toggle') {
        message.success(
          result.user.enabled
            ? `已停用 ${result.user.username}，其会话已吊销`
            : `已启用 ${result.user.username}`,
        );
      } else {
        message.success(`已吊销 ${result.user.username} 的全部会话`);
      }
      invalidate();
    },
    onError: (error) => {
      // 关闭确认框，让页面顶部的错误横幅可见（并发窗口下的服务端判定，
      // 如 409 LAST_ADMIN / SELF_OPERATION / DELIVERY_UNAVAILABLE）。
      setToggleTarget(null);
      setResetTarget(null);
      setResendTarget(null);
      setRevokeTarget(null);
      setActionFailure(failureFromError(error));
    },
  });

  const columns: TableColumnsType<User> = [
    {
      title: '用户名',
      dataIndex: 'username',
      key: 'username',
      width: 220,
      render: (_, user) => (
        <Space direction="vertical" size={0}>
          <Space size={6}>
            <Typography.Text strong>{user.username}</Typography.Text>
            {user.id === currentUserId ? <Tag color="blue">我</Tag> : null}
          </Space>
          {user.display_name ? (
            <Typography.Text type="secondary" className="opsmind-admin-cell-sub">
              {user.display_name}
            </Typography.Text>
          ) : null}
        </Space>
      ),
    },
    {
      title: '角色',
      dataIndex: 'role',
      key: 'role',
      width: 120,
      render: (_, user) => <Tag color={roleTagColor(user.role)}>{roleLabel(user.role)}</Tag>,
    },
    {
      title: '状态',
      dataIndex: 'enabled',
      key: 'status',
      width: 130,
      render: (_, user) => {
        const status = userStatus(user);
        return <Tag color={status.color}>{status.label}</Tag>;
      },
    },
    {
      title: '邮箱',
      dataIndex: 'email',
      key: 'email',
      ellipsis: true,
      render: (_, user) => user.email || '—',
    },
    {
      title: '最后登录',
      dataIndex: 'last_login_at',
      key: 'last_login_at',
      width: 180,
      render: (_, user) => formatDateTime(user.last_login_at),
    },
    {
      title: '操作',
      key: 'actions',
      width: 280,
      render: (_, user) => {
        const roleGuard = canChangeRole(user, currentUserId, users);
        const invitePending = isInvitePending(user);
        const toggleGuard = user.enabled
          ? canDisableUser(user, currentUserId, users)
          : invitePending
            ? canResendInvite(user)
            : canEnableUser(user);
        const toggleLabel = user.enabled ? '停用' : invitePending ? '重发邀请' : '启用';
        // 待接受邀请（含过期）的账号没有可用密码：重置只会换密码、不会
        // 启用账号，还会让未使用的邀请链接失效，因此由 canResetPassword
        // 直接拒绝（服务端同样返回 409 INVITE_PENDING）。
        const resetGuard = canResetPassword(user, currentUserId);
        const revokeGuard = canRevokeSessions(user, currentUserId);
        const resetDisabled = !canReset || !resetGuard.allowed || user.email.trim() === '';
        const resetReason = !canReset
          ? '需要签发密码重置令牌的权限'
          : user.email.trim() === '' && resetGuard.allowed
            ? '该账号未配置邮箱，无法投递重置链接'
            : resetGuard.reason;

        return (
          <Space size={0} wrap className="opsmind-admin-actions">
            <ActionButton
              label="改角色"
              disabled={!canWrite || !roleGuard.allowed}
              reason={denyReason(roleGuard, canWrite, '需要用户管理权限')}
              onClick={() => {
                setActionFailure(null);
                setRoleTarget(user);
              }}
            />
            <ActionButton
              label={toggleLabel}
              danger={user.enabled}
              disabled={!canWrite || !toggleGuard.allowed}
              reason={denyReason(toggleGuard, canWrite, '需要用户管理权限')}
              onClick={() => {
                setActionFailure(null);
                if (invitePending) {
                  setResendTarget(user);
                } else {
                  setToggleTarget(user);
                }
              }}
            />
            <ActionButton
              label="重置密码"
              disabled={resetDisabled}
              reason={resetReason}
              onClick={() => {
                setActionFailure(null);
                setResetTarget(user);
              }}
            />
            <ActionButton
              label="吊销会话"
              disabled={!canWrite || !revokeGuard.allowed}
              reason={denyReason(revokeGuard, canWrite, '需要用户管理权限')}
              onClick={() => {
                setActionFailure(null);
                setRevokeTarget(user);
              }}
            />
          </Space>
        );
      },
    },
  ];

  return (
    <div className="opsmind-admin-page">
      <PageHeader
        title="用户"
        description="管理账号、角色与访问状态；权限由角色固定映射，服务端计算。"
        extra={
          <Space>
            <Button
              icon={<ReloadOutlined />}
              onClick={() => void usersQuery.refetch()}
              loading={usersQuery.isFetching}
            >
              刷新
            </Button>
            {canWrite ? (
              <Button type="primary" icon={<PlusOutlined />} onClick={() => setCreateOpen(true)}>
                创建用户
              </Button>
            ) : null}
          </Space>
        }
      />

      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="权限护栏"
        description={
          <Space direction="vertical" size={2}>
            <span>{adminGuardHint(users, currentUserId)}</span>
            <span>
              停用、改角色与重置密码会立即吊销目标账号的会话并关闭其终端/RDP；重置密码只签发一次性
              令牌，管理员无法查看或设置他人密码。
            </span>
          </Space>
        }
      />

      {actionFailure ? (
        <ApiErrorAlert failure={actionFailure} closable onClose={() => setActionFailure(null)} />
      ) : null}

      {usersQuery.isLoading ? (
        <AdminLoading />
      ) : usersQuery.isError ? (
        <QueryErrorState error={usersQuery.error} onRetry={() => void usersQuery.refetch()} />
      ) : (
        <Table<User>
          rowKey="id"
          size="middle"
          columns={columns}
          dataSource={users}
          scroll={{ x: 980 }}
          pagination={{ pageSize: 20, hideOnSinglePage: true, showSizeChanger: false }}
          locale={{
            emptyText: (
              <EmptyState description="还没有用户；创建账号后，系统会向目标邮箱发送一次性邀请链接。" />
            ),
          }}
        />
      )}

      <Flex justify="flex-end" style={{ marginTop: 12 }}>
        {usersQuery.data ? (
          <Typography.Text type="secondary">共 {usersQuery.data.length} 个用户</Typography.Text>
        ) : null}
      </Flex>

      <UserCreateModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onCreated={() => invalidate()}
      />

      <UserRoleModal
        open={roleTarget !== null}
        user={roleTarget}
        users={users}
        currentUserId={currentUserId}
        onClose={() => setRoleTarget(null)}
        onSaved={() => invalidate()}
      />

      <ConfirmActionModal
        open={toggleTarget !== null}
        title={
          toggleTarget
            ? `${toggleTarget.enabled ? '停用' : '启用'}用户 · ${toggleTarget.username}`
            : '启用/停用用户'
        }
        description={
          toggleTarget?.enabled
            ? '停用后该账号无法登录，现有会话、终端/RDP 与待处理审批会立即失效；账号数据与审计保留。'
            : '启用后该账号可重新登录并恢复其角色的权限。'
        }
        okText={toggleTarget?.enabled ? '停用' : '启用'}
        danger={Boolean(toggleTarget?.enabled)}
        loading={actionMutation.isPending}
        onConfirm={() => {
          if (toggleTarget) {
            actionMutation.mutate({ kind: 'toggle', user: toggleTarget });
          }
        }}
        onCancel={() => setToggleTarget(null)}
      />

      <ConfirmActionModal
        open={resendTarget !== null}
        title={resendTarget ? `重发邀请 · ${resendTarget.username}` : '重发邀请'}
        description={
          <Space direction="vertical" size={4}>
            <span>
              将向 {resendTarget?.email || '目标邮箱'} 重新发送一次性邀请链接（30
              分钟有效，仅可使用一次）。
            </span>
            <span>旧邀请链接会立即失效；受邀者设置密码后账号才会启用。</span>
          </Space>
        }
        okText="重发邀请"
        danger={false}
        loading={actionMutation.isPending}
        onConfirm={() => {
          if (resendTarget) {
            actionMutation.mutate({ kind: 'resend', user: resendTarget });
          }
        }}
        onCancel={() => setResendTarget(null)}
      />

      <ConfirmActionModal
        open={resetTarget !== null}
        title={resetTarget ? `重置密码 · ${resetTarget.username}` : '重置密码'}
        description={
          <Space direction="vertical" size={4}>
            <span>
              将向 {resetTarget?.email || '目标邮箱'} 发送一次性重置链接（30
              分钟有效，仅可使用一次）。
            </span>
            <span>不会显示或设置新密码；签发后该账号的现有会话会被全部吊销。</span>
          </Space>
        }
        okText="发送重置链接"
        danger={false}
        loading={actionMutation.isPending}
        onConfirm={() => {
          if (resetTarget) {
            actionMutation.mutate({ kind: 'reset', user: resetTarget });
          }
        }}
        onCancel={() => setResetTarget(null)}
      />

      <ConfirmActionModal
        open={revokeTarget !== null}
        title={revokeTarget ? `吊销会话 · ${revokeTarget.username}` : '吊销会话'}
        description="该账号的全部登录会话会被删除，进行中的终端/RDP 会断开，待处理审批会被取消。"
        okText="吊销会话"
        danger
        loading={actionMutation.isPending}
        onConfirm={() => {
          if (revokeTarget) {
            actionMutation.mutate({ kind: 'revoke', user: revokeTarget });
          }
        }}
        onCancel={() => setRevokeTarget(null)}
      />
    </div>
  );
}
