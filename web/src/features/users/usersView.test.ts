import { describe, expect, it } from 'vitest';

import {
  adminGuardHint,
  canChangeRole,
  canDisableUser,
  canEnableUser,
  canResetPassword,
  canResendInvite,
  canRevokeSessions,
  canSubmitRoleChange,
  enabledAdminCount,
  isInvitePending,
  isLastEnabledAdmin,
  roleDescription,
  userStatus,
  type User,
} from './usersView';

function makeUser(overrides: Partial<User> & Pick<User, 'id' | 'username' | 'role'>): User {
  return {
    enabled: true,
    display_name: '',
    email: '',
    invite_pending: false,
    must_change_password: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

const ADMIN = makeUser({ id: 'u-admin', username: 'admin', role: 'admin' });
const ADMIN2 = makeUser({ id: 'u-admin2', username: 'admin2', role: 'admin' });
const OPERATOR = makeUser({
  id: 'u-op',
  username: 'operator',
  role: 'operator',
});
const APPROVER = makeUser({
  id: 'u-appr',
  username: 'approver',
  role: 'approver',
});
const AUDITOR = makeUser({ id: 'u-aud', username: 'auditor', role: 'auditor' });

const TWO_ADMINS = [ADMIN, ADMIN2, OPERATOR, APPROVER, AUDITOR];
const ONE_ADMIN = [ADMIN, OPERATOR, APPROVER, AUDITOR];

describe('最后管理员判定', () => {
  it('统计启用中的 admin（停用的不算）', () => {
    const disabledAdmin = makeUser({
      id: 'u-admin3',
      username: 'admin3',
      role: 'admin',
      enabled: false,
    });
    expect(enabledAdminCount([ADMIN, ADMIN2, disabledAdmin])).toBe(2);
    expect(enabledAdminCount(ONE_ADMIN)).toBe(1);
  });

  it('只有一个启用管理员时标记为最后管理员', () => {
    expect(isLastEnabledAdmin(ADMIN, ONE_ADMIN)).toBe(true);
    expect(isLastEnabledAdmin(ADMIN, TWO_ADMINS)).toBe(false);
    expect(isLastEnabledAdmin(OPERATOR, ONE_ADMIN)).toBe(false);
  });
});

describe('停用护栏（不能停用自己 / 最后管理员）', () => {
  it('两个管理员时可以停用另一个管理员', () => {
    expect(canDisableUser(ADMIN2, ADMIN.id, TWO_ADMINS)).toEqual({ allowed: true });
  });

  it('不能停用自己（即使还有别的管理员）', () => {
    expect(canDisableUser(ADMIN, ADMIN.id, TWO_ADMINS)).toEqual({
      allowed: false,
      reason: '不能停用自己',
    });
  });

  it('不能停用最后一个管理员', () => {
    expect(canDisableUser(ADMIN, ADMIN2.id, ONE_ADMIN)).toEqual({
      allowed: false,
      reason: '不能停用最后一个管理员',
    });
  });

  it('已停用账号不能重复停用；启用无护栏', () => {
    const disabled = makeUser({ id: 'u-off', username: 'off', role: 'auditor', enabled: false });
    expect(canDisableUser(disabled, ADMIN.id, TWO_ADMINS).allowed).toBe(false);
    expect(canEnableUser(disabled).allowed).toBe(true);
    expect(canEnableUser(AUDITOR).allowed).toBe(false);
  });
});

describe('改角色护栏', () => {
  it('不能改自己的角色', () => {
    expect(canChangeRole(ADMIN, ADMIN.id, TWO_ADMINS)).toEqual({
      allowed: false,
      reason: '不能修改自己的角色',
    });
  });

  it('不能降级最后一个管理员', () => {
    expect(canChangeRole(ADMIN, ADMIN2.id, ONE_ADMIN)).toEqual({
      allowed: false,
      reason: '不能降级最后一个管理员',
    });
  });

  it('普通用户角色可修改；目标角色与原角色相同不提交', () => {
    expect(canChangeRole(OPERATOR, ADMIN.id, TWO_ADMINS).allowed).toBe(true);
    expect(canSubmitRoleChange(OPERATOR, 'operator', ADMIN.id, TWO_ADMINS)).toEqual({
      allowed: false,
      reason: '角色未变化',
    });
    expect(canSubmitRoleChange(OPERATOR, 'auditor', ADMIN.id, TWO_ADMINS).allowed).toBe(true);
  });
});

describe('重置密码 / 吊销会话护栏', () => {
  it('不能重置自己的密码，也不能吊销自己的会话', () => {
    expect(canResetPassword(ADMIN, ADMIN.id).allowed).toBe(false);
    expect(canRevokeSessions(ADMIN, ADMIN.id).allowed).toBe(false);
    expect(canResetPassword(OPERATOR, ADMIN.id).allowed).toBe(true);
    expect(canRevokeSessions(AUDITOR, ADMIN.id).allowed).toBe(true);
  });
});

describe('待接受邀请（含邀请已过期）', () => {
  const PENDING = makeUser({
    id: 'u-pending',
    username: 'pending',
    role: 'auditor',
    enabled: false,
    invite_pending: true,
  });

  it('识别邀请流程中的账号', () => {
    expect(isInvitePending(PENDING)).toBe(true);
    expect(isInvitePending(AUDITOR)).toBe(false);
  });

  it('待接受邀请的账号不能启用或重置密码，只能重发邀请', () => {
    expect(canEnableUser(PENDING)).toEqual({
      allowed: false,
      reason: '账号待接受邀请（或邀请已过期），请重发邀请',
    });
    expect(canResetPassword(PENDING, ADMIN.id)).toEqual({
      allowed: false,
      reason: '账号待接受邀请（或邀请已过期），请先重发邀请',
    });
    expect(canResendInvite(PENDING).allowed).toBe(true);
  });

  it('已启用或无未使用 invite 的停用账号不提供重发邀请', () => {
    expect(canResendInvite(AUDITOR).allowed).toBe(false);
    const offNoInvite = makeUser({
      id: 'u-off-no-invite',
      username: 'off-no-invite',
      role: 'auditor',
      enabled: false,
      invite_pending: false,
    });
    expect(canResendInvite(offNoInvite).allowed).toBe(false);
    expect(canEnableUser(offNoInvite).allowed).toBe(true);
  });
});

describe('账号状态文案', () => {
  it('已启用 / 待接受邀请 / 已停用三态', () => {
    expect(userStatus(AUDITOR)).toEqual({ label: '已启用', color: 'green' });
    expect(
      userStatus(
        makeUser({ id: 'x', username: 'x', role: 'auditor', enabled: false, invite_pending: true }),
      ),
    ).toEqual({ label: '待接受邀请', color: 'gold' });
    expect(
      userStatus(makeUser({ id: 'y', username: 'y', role: 'auditor', enabled: false })),
    ).toEqual({ label: '已停用', color: 'default' });
  });
});

describe('页面护栏提示', () => {
  it('单管理员部署明确提示降级限制', () => {
    expect(adminGuardHint(ONE_ADMIN, ADMIN.id)).toContain('只有 1 个启用中的管理员');
    expect(adminGuardHint(ONE_ADMIN, ADMIN.id)).toContain('不能停用或降级最后一个管理员');
  });

  it('多管理员时提示自我操作与会话吊销影响', () => {
    const hint = adminGuardHint(TWO_ADMINS, ADMIN.id);
    expect(hint).toContain('当前有 2 个启用中的管理员');
    expect(hint).toContain('不能停用自己或修改自己的角色');
    expect(hint).toContain('吊销');
  });
});

// 改角色弹窗补职责说明（评审 medium）：四个角色的说明必须与创建弹窗同源，
// 否则管理员只看到名字、不知道选择后的能力边界。
describe('roleDescription 角色职责说明', () => {
  it('四个角色都有中文说明且与 ROLE_DESCRIPTIONS 一致', () => {
    expect(roleDescription('admin')).toContain('全部权限');
    expect(roleDescription('operator')).toContain('不能审批');
    expect(roleDescription('approver')).toContain('只审批不发起');
    expect(roleDescription('auditor')).toContain('只读治理');
  });

  it('空/未知角色返回空串（不渲染 undefined）', () => {
    expect(roleDescription(null)).toBe('');
    expect(roleDescription(undefined)).toBe('');
    expect(roleDescription('bogus')).toBe('');
  });
});
