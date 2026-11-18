import { Button, Result, Space, Typography } from 'antd';
import { useLocation, useNavigate } from 'react-router-dom';

import { defaultLandingPath, navLabelForPath } from '../lib/rbac';
import { roleLabel } from '../lib/rbacPermissions';
import { useMe } from './auth/useMe';
import '../styles/admin.css';

/**
 * 403 页（CONTRACT.md 12.4 第 1 条：无权限返回 403）。
 *
 * 直达无权限路由时由 PermissionGate 渲染本页，而不是空白或静默重定向：
 * 用户需要知道「是权限不足」而不是「页面坏了」，并有一个可操作的出口。
 * 文案里带当前角色与目标页面名，不展示原始权限串/错误码。
 */
export default function ForbiddenPage() {
  const location = useLocation();
  const navigate = useNavigate();
  const meQuery = useMe();
  const permissions = meQuery.data?.permissions ?? [];
  const landing = defaultLandingPath(permissions);
  const landingLabel = navLabelForPath(landing);
  const pageLabel = navLabelForPath(location.pathname) ?? '该页面';
  const role = roleLabel(meQuery.data?.user.role);

  return (
    <div className="opsmind-admin-page">
      <Result
        status="403"
        title="无权访问"
        subTitle={
          <Space direction="vertical" size={4}>
            <Typography.Text>
              当前账号的角色是「{role}」，没有访问「{pageLabel}」的权限。
            </Typography.Text>
            <Typography.Text type="secondary">
              如需使用该功能，请联系管理员调整角色；界面只展示当前角色可用的入口，
              越权请求会被服务端拒绝并记录审计。
            </Typography.Text>
          </Space>
        }
        extra={
          <Space>
            {landing !== location.pathname ? (
              <Button type="primary" onClick={() => navigate(landing, { replace: true })}>
                回到「{landingLabel ?? '可访问页面'}」
              </Button>
            ) : null}
            <Button onClick={() => navigate(-1)}>返回上一页</Button>
          </Space>
        }
      />
    </div>
  );
}
