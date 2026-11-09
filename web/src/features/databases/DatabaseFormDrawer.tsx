import { useMutation } from '@tanstack/react-query';
import { Alert, Checkbox, Form, Input, InputNumber, Modal, Select, Space, Typography } from 'antd';
import { useState } from 'react';

import { api } from '../../api/client';
import type { components } from '../../api/schema';
import ApiErrorAlert from '../../components/admin/ApiErrorAlert';
import ConfirmActionModal from '../../components/admin/ConfirmActionModal';
import {
  ApiRequestError,
  applyFieldErrors,
  failureFromError,
  type ApiFailure,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import {
  DEFAULT_PORTS,
  ENGINE_OPTIONS,
  SSL_MODE_OPTIONS,
  type DatabaseEngine,
  type SSLMode,
} from '../../components/admin/meta';
import CopyButton from '../../components/CopyButton';
import { databaseConnectionString } from './databaseView';

type Database = components['schemas']['Database'];
type DatabaseCreate = components['schemas']['DatabaseCreate'];
type DatabaseUpdate = components['schemas']['DatabaseUpdate'];

interface DatabaseFormValues {
  name: string;
  engine: DatabaseEngine;
  host?: string;
  port?: number;
  instance_name?: string;
  redis_db?: number;
  database_name?: string;
  username?: string;
  password?: string;
  auth_db?: string;
  sqlite_path?: string;
  ssl_mode?: SSLMode;
  clear_password?: boolean;
}

interface DatabaseFormModalProps {
  open: boolean;
  database: Database | null;
  onClose: () => void;
  onSaved: () => void;
}

const DATABASE_FIELDS = [
  'name',
  'engine',
  'host',
  'port',
  'instance_name',
  'redis_db',
  'database_name',
  'username',
  'password',
  'auth_db',
  'sqlite_path',
  'ssl_mode',
];

// 三期（N3）：mongodb 的引擎选项与默认端口已按主代理授权补入
// components/admin/meta.ts（ENGINE_OPTIONS / DEFAULT_PORTS，归属 FE2 的
// 纯枚举追加）；此处仅保留默认端口组合供本文件与单测使用。

function buildInitialValues(database: Database | null): Partial<DatabaseFormValues> {
  if (!database) {
    return {
      engine: 'postgres',
      port: DEFAULT_PORTS.postgres,
      ssl_mode: 'prefer',
      clear_password: false,
    };
  }
  if (database.engine === 'sqlite') {
    return {
      name: database.name,
      engine: database.engine,
      sqlite_path: database.sqlite_path,
      clear_password: false,
    };
  }
  if (database.engine === 'redis') {
    return {
      name: database.name,
      engine: database.engine,
      host: database.host,
      port: database.port,
      username: database.username,
      redis_db: database.redis_db,
      ssl_mode: database.ssl_mode,
      clear_password: false,
    };
  }
  if (database.engine === 'mongodb') {
    return {
      name: database.name,
      engine: database.engine,
      host: database.host,
      port: database.port,
      database_name: database.database_name,
      username: database.username,
      auth_db: database.auth_db,
      ssl_mode: database.ssl_mode,
      clear_password: false,
    };
  }
  return {
    name: database.name,
    engine: database.engine,
    host: database.host,
    port: database.port,
    ...(database.engine === 'mssql' ? { instance_name: database.instance_name } : {}),
    database_name: database.database_name,
    username: database.username,
    ssl_mode: database.ssl_mode,
    clear_password: false,
  };
}

// mssql 的 instance_name 与 port 互斥（契约 2.6）：instance 非空时 port 必须 0
//（经 SQL Server Browser 解析），为空时 port 1..65535（默认 1433）。
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出（纯函数）
export function resolvePort(
  engine: DatabaseEngine,
  port: number | undefined,
  instance: string,
): number {
  if (engine !== 'mssql') {
    return port ?? DEFAULT_PORTS[engine];
  }
  return instance.trim() !== '' ? 0 : (port ?? DEFAULT_PORTS.mssql);
}

// mongoAuthUsernameError 是 MongoDB username 字段校验的纯函数核心
// （组件内校验器薄封装调用，便于单测覆盖死锁回归）：
// 契约 2.6 username/password 成对；清空认证须显式勾选清除——但仅对
// 「已保存认证」的库生效：无认证库（has_password=false，创建时同空可达）
// 本来就无认证、清空复选框也不渲染，编辑它的任何字段不得被此规则拦截。
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出（纯函数，供 databaseAuth 单测）
export function mongoAuthUsernameError(
  editing: boolean,
  hasSavedAuth: boolean,
  username: string,
  password: string,
  clearing: boolean,
): string | null {
  if (username === '' && password !== '') {
    return '填写密码时需同时填写用户名（认证成对）';
  }
  // p3 sprint 3：编辑无认证库（hasSavedAuth=false）只填用户名留空密码
  // 保存，buildUpdatePayload 只发 username → 后端终态成对校验 400
  // "mongodb username and password must both be set or both empty"（英
  // 文直达用户，实测走查 sprint 3）。与创建态同文案本地拦截；编辑已
  // 保存认证的库时留空密码 = 保留已存密码，放行。
  if (username !== '' && password === '' && !(editing && hasSavedAuth)) {
    return '填写用户名时需同时填写密码；无认证部署则两者都留空';
  }
  if (editing && username === '' && !clearing && hasSavedAuth) {
    return '清空用户名即清除认证，请勾选「清除已保存的认证」';
  }
  return null;
}

// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出（纯函数，供 databaseEngineForm/databaseAuth 单测）
export function buildCreatePayload(values: DatabaseFormValues): DatabaseCreate {
  const name = values.name.trim();
  if (values.engine === 'sqlite') {
    return {
      name,
      engine: 'sqlite',
      sqlite_path: values.sqlite_path?.trim() ?? '',
    };
  }
  if (values.engine === 'redis') {
    const payload: DatabaseCreate = {
      name,
      engine: 'redis',
      host: values.host?.trim() ?? '',
      port: values.port ?? DEFAULT_PORTS.redis,
      username: values.username?.trim() ?? '',
      redis_db: values.redis_db ?? 0,
      ssl_mode: values.ssl_mode ?? 'prefer',
    };
    if (values.password) {
      payload.password = values.password;
    }
    return payload;
  }
  if (values.engine === 'mongodb') {
    // 契约 2.6：连接参数全走结构化列（不提供 URI 直连形态）；
    // username/password 成对——要么都空（无认证部署），要么都非空。
    const payload: DatabaseCreate = {
      name,
      engine: 'mongodb',
      host: values.host?.trim() ?? '',
      port: values.port ?? DEFAULT_PORTS.mongodb,
      database_name: values.database_name?.trim() ?? '',
      username: values.username?.trim() ?? '',
      auth_db: values.auth_db?.trim() ?? '',
      ssl_mode: values.ssl_mode ?? 'prefer',
    };
    if (values.password) {
      payload.password = values.password;
    }
    return payload;
  }
  const instance = values.engine === 'mssql' ? (values.instance_name?.trim() ?? '') : '';
  const payload: DatabaseCreate = {
    name,
    engine: values.engine,
    host: values.host?.trim() ?? '',
    port: resolvePort(values.engine, values.port, instance),
    database_name: values.database_name?.trim() ?? '',
    username: values.username?.trim() ?? '',
    ssl_mode: values.ssl_mode ?? 'prefer',
  };
  if (values.engine === 'mssql') {
    payload.instance_name = instance;
  }
  if (values.password) {
    payload.password = values.password;
  }
  return payload;
}

// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出（纯函数）
export function buildUpdatePayload(values: DatabaseFormValues): DatabaseUpdate {
  const payload: DatabaseUpdate = { name: values.name.trim() };
  if (values.engine === 'sqlite') {
    payload.sqlite_path = values.sqlite_path?.trim() ?? '';
  } else if (values.engine === 'redis') {
    payload.host = values.host?.trim() ?? '';
    payload.port = values.port ?? DEFAULT_PORTS.redis;
    payload.username = values.username?.trim() ?? '';
    payload.redis_db = values.redis_db ?? 0;
    payload.ssl_mode = values.ssl_mode ?? 'prefer';
  } else if (values.engine === 'mongodb') {
    payload.host = values.host?.trim() ?? '';
    payload.port = values.port ?? DEFAULT_PORTS.mongodb;
    payload.database_name = values.database_name?.trim() ?? '';
    payload.auth_db = values.auth_db?.trim() ?? '';
    payload.ssl_mode = values.ssl_mode ?? 'prefer';
    if (values.password) {
      payload.username = values.username?.trim() ?? '';
      payload.password = values.password;
    } else if (values.clear_password) {
      // 契约 2.6/schema：mongodb 清空认证须 username 与 password 同时清。
      payload.username = '';
      payload.password = '';
    } else {
      payload.username = values.username?.trim() ?? '';
    }
  } else {
    const instance = values.engine === 'mssql' ? (values.instance_name?.trim() ?? '') : '';
    payload.host = values.host?.trim() ?? '';
    payload.port = resolvePort(values.engine, values.port, instance);
    payload.database_name = values.database_name?.trim() ?? '';
    payload.username = values.username?.trim() ?? '';
    payload.ssl_mode = values.ssl_mode ?? 'prefer';
    if (values.engine === 'mssql') {
      payload.instance_name = instance;
    }
  }
  if (values.password) {
    payload.password = values.password;
  } else if (values.clear_password) {
    payload.password = '';
  }
  return payload;
}

export default function DatabaseFormModal({
  open,
  database,
  onClose,
  onSaved,
}: DatabaseFormModalProps) {
  const [form] = Form.useForm<DatabaseFormValues>();
  const [failure, setFailure] = useState<ApiFailure | null>(null);
  const [pendingValues, setPendingValues] = useState<DatabaseFormValues | null>(null);

  const engine = (Form.useWatch('engine', form) ??
    database?.engine ??
    'postgres') as DatabaseEngine;
  const isSqlite = engine === 'sqlite';
  const isRedis = engine === 'redis';
  const isMssql = engine === 'mssql';
  const isMongo = engine === 'mongodb';
  const isEditing = database !== null;
  // mssql：instance_name 非空时与 port 互斥（port 置 0 并停用输入）。
  // useWatch 必须无条件调用（rules-of-hooks）；undefined/null=字段未挂载
  // 或未交互，回退已存实例名；''=用户明确清空——不得回退，否则清空后
  // onValuesChange 已把 port 联动恢复为默认端口，而 instanceFilled 仍为
  // true 使校验要求 port===0 且输入框禁用，保存死锁（评审发现）。
  const instanceNameWatch = Form.useWatch('instance_name', form);
  const instanceFilled =
    isMssql &&
    (instanceNameWatch === undefined || instanceNameWatch === null
      ? (database?.instance_name ?? '')
      : instanceNameWatch
    ).trim() !== '';
  // mssql 密码必填（契约 4.4：password_enc NOT NULL）；编辑时已保存密码则留空不修改。
  const passwordRequired = isMssql && database === null;

  const mutation = useMutation({
    mutationFn: async (values: DatabaseFormValues) => {
      if (database) {
        const { data, error, response } = await api.PATCH('/api/databases/{id}', {
          params: { path: { id: database.id }, header: writeHeaders() },
          body: buildUpdatePayload(values),
        });
        if (error) {
          throw new ApiRequestError(toApiFailure(error, response));
        }
        return data;
      }
      const { data, error, response } = await api.POST('/api/databases', {
        body: buildCreatePayload(values),
        params: { header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: () => {
      setFailure(null);
      setPendingValues(null);
      // destroyOnHidden 只销毁 Modal 内容，useForm 的 store 跨开关存活；
      // 页面对「新增→新增」复用同一组件实例（key 同为 'new'），不重置会
      // 让上次的输入（含密码明文与引擎类型）残留到下次表单（走查 D2/D4）。
      form.resetFields();
      onSaved();
    },
    onError: (error) => {
      const nextFailure = failureFromError(error);
      applyFieldErrors(nextFailure.details, DATABASE_FIELDS, (items) =>
        form.setFields(items as unknown as Parameters<typeof form.setFields>[0]),
      );
      setFailure(nextFailure);
      setPendingValues(null);
    },
  });

  // 取消路径同样重置：resetFields 把 store 恢复到 initialValues，
  // 与 destroyOnHidden + 页面 key 复用场景（连续两次新增）共同保证
  // 每次打开都是干净表单。
  const handleCancel = () => {
    form.resetFields();
    setFailure(null);
    setPendingValues(null);
    onClose();
  };

  const submit = (values: DatabaseFormValues) => {
    setFailure(null);
    mutation.mutate(values);
  };

  return (
    <>
      <Modal
        open={open}
        title={database ? `编辑数据库 · ${database.name}` : '新增数据库'}
        width={720}
        okText="保存"
        cancelText="取消"
        confirmLoading={mutation.isPending}
        cancelButtonProps={{ disabled: mutation.isPending }}
        onOk={() => form.submit()}
        onCancel={handleCancel}
        destroyOnHidden
      >
        {failure ? <ApiErrorAlert failure={failure} /> : null}
        {/* 契约 8.7 ②（六期打磨 sprint 1）：表单只读态补复制——数据库 ID
            与连接信息组合串是派生事实，此前只在列表表格提供（实机编辑
            弹窗无复制入口）。组合串与列表共用 databaseView.databaseConnectionString，
            逐字一致。新建态无 ID/组合串，不渲染。 */}
        {database ? (
          <Space size={16} wrap style={{ marginBottom: 16 }}>
            <span className="opsmind-admin-cell-sub">
              ID <span className="opsmind-admin-mono">{database.id}</span>
              <CopyButton text={database.id} label="复制数据库 ID" />
            </span>
            <span className="opsmind-admin-cell-sub">
              <span className="opsmind-admin-mono">{databaseConnectionString(database)}</span>
              <CopyButton text={databaseConnectionString(database)} label="复制连接信息" />
            </span>
          </Space>
        ) : null}
        <Form<DatabaseFormValues>
          form={form}
          layout="vertical"
          initialValues={buildInitialValues(database)}
          requiredMark={false}
          onValuesChange={(changed: Partial<DatabaseFormValues>) => {
            if (changed.engine) {
              form.setFieldValue('port', DEFAULT_PORTS[changed.engine]);
            }
            if (changed.instance_name !== undefined) {
              // 契约 8.7：填 instance 时清零 port，清空后恢复默认直连端口。
              const filled = changed.instance_name.trim() !== '';
              const port = form.getFieldValue('port') as number | undefined;
              if (filled && port !== 0) {
                form.setFieldValue('port', 0);
              } else if (!filled && port === 0) {
                form.setFieldValue('port', DEFAULT_PORTS.mssql);
              }
            }
          }}
          onFinish={(values) => {
            if (database && values.clear_password && !values.password) {
              setPendingValues(values);
              return;
            }
            submit(values);
          }}
        >
          <div style={{ display: 'flex', gap: 12 }}>
            <Form.Item
              name="name"
              label="名称"
              style={{ flex: 1 }}
              rules={[
                { required: true, message: '请输入名称' },
                { max: 100, message: '名称不能超过 100 个字符' },
              ]}
            >
              <Input placeholder="例如 orders-prod" />
            </Form.Item>
            <Form.Item
              name="engine"
              label="类型"
              style={{ width: 200 }}
              rules={[{ required: true, message: '请选择类型' }]}
              extra={isEditing ? '创建后类型不可修改' : undefined}
            >
              <Select options={ENGINE_OPTIONS} disabled={isEditing} />
            </Form.Item>
          </div>

          {isSqlite ? (
            <Form.Item
              name="sqlite_path"
              label="SQLite 文件路径"
              rules={[
                { required: true, message: '请输入 SQLite 文件路径' },
                { max: 4096, message: '路径过长' },
                {
                  validator: async (_rule, value: string | undefined) => {
                    const path = value?.trim() ?? '';
                    if (path !== '' && !path.startsWith('/')) {
                      throw new Error('必须是服务端可读的绝对路径（以 / 开头）');
                    }
                  },
                },
              ]}
              extra="路径位于服务端，例如 /var/lib/opsmind/data.db"
            >
              <Input placeholder="/var/lib/opsmind/data.db" />
            </Form.Item>
          ) : isRedis ? (
            <>
              <div style={{ display: 'flex', gap: 12 }}>
                <Form.Item
                  name="host"
                  label="主机地址"
                  style={{ flex: 1 }}
                  rules={[
                    { required: true, message: '请输入主机地址' },
                    { max: 255, message: '地址不能超过 255 个字符' },
                  ]}
                >
                  <Input placeholder="redis.example.com" />
                </Form.Item>
                <Form.Item
                  name="port"
                  label="端口"
                  style={{ width: 140 }}
                  rules={[
                    { required: true, message: '请输入端口' },
                    { type: 'number', min: 1, max: 65535, message: '端口范围为 1-65535' },
                  ]}
                >
                  <InputNumber style={{ width: '100%' }} />
                </Form.Item>
              </div>

              <div style={{ display: 'flex', gap: 12 }}>
                <Form.Item
                  name="username"
                  label="用户名（可选）"
                  style={{ flex: 1 }}
                  rules={[{ max: 100, message: '用户名不能超过 100 个字符' }]}
                  extra="Redis 6 ACL 用户；留空使用 default。"
                >
                  <Input placeholder="default（留空）" autoComplete="off" />
                </Form.Item>
                <Form.Item
                  name="password"
                  label="密码（可选）"
                  style={{ flex: 1 }}
                  rules={[{ max: 4096, message: '密码过长' }]}
                  extra="requirepass / ACL 密码；无密码可留空。"
                >
                  <Input.Password
                    placeholder={
                      database?.has_password ? '已保存，留空则不修改' : '可选，无密码可留空'
                    }
                    autoComplete="new-password"
                  />
                </Form.Item>
              </div>

              <div style={{ display: 'flex', gap: 12 }}>
                <Form.Item
                  name="redis_db"
                  label="DB 序号"
                  style={{ width: 140 }}
                  rules={[
                    {
                      validator: async (_rule, value: number | undefined | null) => {
                        if (value === undefined || value === null) {
                          return;
                        }
                        if (!Number.isInteger(value) || value < 0 || value > 1023) {
                          throw new Error('DB 序号范围为 0-1023');
                        }
                      },
                    },
                  ]}
                  extra="逻辑库编号，默认 0"
                >
                  {/* 同 port：不设 min/max，由 Form 校验显式报错而非 blur 静默 clamp。 */}
                  <InputNumber style={{ width: '100%' }} placeholder="0" />
                </Form.Item>
                <Form.Item
                  name="ssl_mode"
                  label="SSL/TLS 模式"
                  style={{ width: 220 }}
                  rules={[{ required: true, message: '请选择 SSL/TLS 模式' }]}
                  extra="disable/prefer 不启用 TLS；require 及以上启用。"
                >
                  <Select options={SSL_MODE_OPTIONS} />
                </Form.Item>
              </div>

              {database?.has_password ? (
                <Form.Item
                  name="clear_password"
                  valuePropName="checked"
                  dependencies={['password']}
                  rules={[
                    {
                      validator: async (_rule, value: boolean | undefined) => {
                        if (value && form.getFieldValue('password')) {
                          throw new Error('已输入新密码时无需勾选清除');
                        }
                      },
                    },
                  ]}
                  extra="清除操作在保存时会弹出二次确认。"
                >
                  <Checkbox>清除已保存的密码</Checkbox>
                </Form.Item>
              ) : null}
            </>
          ) : isMongo ? (
            <>
              <Alert
                type="info"
                showIcon
                message="MongoDB：查询走 JSON 命令文档白名单，只读"
                description="控制台仅执行 find / aggregate / count / distinct / estimatedDocumentCount；连接参数走结构化字段（不提供 URI 直连形态）。"
                style={{ marginBottom: 16 }}
              />
              <div style={{ display: 'flex', gap: 12 }}>
                <Form.Item
                  name="host"
                  label="主机地址"
                  style={{ flex: 1 }}
                  rules={[
                    { required: true, message: '请输入主机地址' },
                    { max: 255, message: '地址不能超过 255 个字符' },
                  ]}
                >
                  <Input placeholder="mongo.example.com" />
                </Form.Item>
                <Form.Item
                  name="port"
                  label="端口"
                  style={{ width: 140 }}
                  rules={[
                    { required: true, message: '请输入端口' },
                    { type: 'number', min: 1, max: 65535, message: '端口范围为 1-65535' },
                  ]}
                  extra="默认 27017"
                >
                  <InputNumber style={{ width: '100%' }} />
                </Form.Item>
              </div>

              <Form.Item
                name="database_name"
                label="数据库名"
                rules={[
                  { required: true, message: '请输入数据库名' },
                  { max: 255, message: '数据库名不能超过 255 个字符' },
                ]}
                extra="目标库（查询以此为默认命名空间）。"
              >
                <Input placeholder="app" />
              </Form.Item>

              <div style={{ display: 'flex', gap: 12 }}>
                <Form.Item
                  name="username"
                  label="用户名（无认证可留空）"
                  style={{ flex: 1 }}
                  dependencies={['password', 'clear_password']}
                  rules={[
                    { max: 100, message: '用户名不能超过 100 个字符' },
                    {
                      // 契约 2.6：username/password 成对——要么都空（无认证
                      // 部署），要么都非空；清空认证须两者同时清（勾选清除）。
                      validator: async (_rule, value: string | undefined) => {
                        const username = value?.trim() ?? '';
                        const password = (
                          (form.getFieldValue('password') as string | undefined) ?? ''
                        ).trim();
                        const clearing = Boolean(form.getFieldValue('clear_password'));
                        const error = mongoAuthUsernameError(
                          database !== null,
                          database?.has_password ?? false,
                          username,
                          password,
                          clearing,
                        );
                        if (error !== null) {
                          throw new Error(error);
                        }
                      },
                    },
                  ]}
                  extra="用户名与密码成对：都留空表示无认证部署。"
                >
                  <Input placeholder="readonly（无认证可留空）" autoComplete="off" />
                </Form.Item>
                <Form.Item
                  name="password"
                  label="密码"
                  style={{ flex: 1 }}
                  rules={[{ max: 4096, message: '密码过长' }]}
                  extra="已保存后留空表示不修改。"
                >
                  <Input.Password
                    placeholder={
                      database?.has_password ? '已保存，留空则不修改' : '可选，无认证可留空'
                    }
                    autoComplete="new-password"
                  />
                </Form.Item>
              </div>

              <div style={{ display: 'flex', gap: 12 }}>
                <Form.Item
                  name="auth_db"
                  label="认证库（AuthSource）"
                  style={{ flex: 1 }}
                  rules={[{ max: 100, message: '认证库名不能超过 100 个字符' }]}
                  extra="认证所用的库；留空 = admin。"
                >
                  <Input placeholder="留空 = admin" autoComplete="off" />
                </Form.Item>
                <Form.Item
                  name="ssl_mode"
                  label="SSL/TLS 模式"
                  style={{ width: 220 }}
                  rules={[{ required: true, message: '请选择 SSL/TLS 模式' }]}
                  extra="disable/prefer 不启用 TLS；require 启用但跳过证书校验；verify-ca 及以上走系统信任链校验。"
                >
                  <Select options={SSL_MODE_OPTIONS} />
                </Form.Item>
              </div>

              {database?.has_password ? (
                <Form.Item
                  name="clear_password"
                  valuePropName="checked"
                  dependencies={['password', 'username']}
                  rules={[
                    {
                      validator: async (_rule, value: boolean | undefined) => {
                        if (
                          value &&
                          (form.getFieldValue('password') || form.getFieldValue('username'))
                        ) {
                          throw new Error('已输入新认证信息时无需勾选清除');
                        }
                      },
                    },
                  ]}
                  extra="清除 = 用户名与密码同时清空（契约：清空须两者同时清）；保存时会弹出二次确认。"
                >
                  <Checkbox>清除已保存的认证（用户名 + 密码）</Checkbox>
                </Form.Item>
              ) : null}
            </>
          ) : (
            <>
              <div style={{ display: 'flex', gap: 12 }}>
                <Form.Item
                  name="host"
                  label="主机地址"
                  style={{ flex: 1 }}
                  rules={[
                    { required: true, message: '请输入主机地址' },
                    { max: 255, message: '地址不能超过 255 个字符' },
                  ]}
                >
                  <Input placeholder="db.example.com" />
                </Form.Item>
                <Form.Item
                  name="port"
                  label="端口"
                  style={{ width: 140 }}
                  dependencies={isMssql ? ['instance_name'] : []}
                  rules={
                    isMssql && instanceFilled
                      ? [
                          {
                            validator: async (_rule, value: number | undefined) => {
                              if (value !== 0) {
                                throw new Error('已填写命名实例，端口必须为 0');
                              }
                            },
                          },
                        ]
                      : [
                          { required: true, message: '请输入端口' },
                          { type: 'number', min: 1, max: 65535, message: '端口范围为 1-65535' },
                        ]
                  }
                >
                  {/* 同 ServerFormDrawer：不设 min/max，由 Form 校验显式报错而非 blur 静默 clamp。 */}
                  <InputNumber style={{ width: '100%' }} disabled={isMssql && instanceFilled} />
                </Form.Item>
              </div>

              {isMssql ? (
                <Form.Item
                  name="instance_name"
                  label="命名实例（可选）"
                  rules={[{ max: 100, message: '实例名不能超过 100 个字符' }]}
                  extra="填写后经 SQL Server Browser（UDP 1434）解析并自动置零端口；留空则按端口直连，默认 1433。"
                >
                  <Input placeholder="例如 SQLEXPRESS（留空则端口直连）" autoComplete="off" />
                </Form.Item>
              ) : null}

              <div style={{ display: 'flex', gap: 12 }}>
                <Form.Item
                  name="database_name"
                  label="数据库名"
                  style={{ flex: 1 }}
                  rules={[
                    { required: true, message: '请输入数据库名' },
                    { max: 255, message: '数据库名不能超过 255 个字符' },
                  ]}
                >
                  <Input placeholder="app" />
                </Form.Item>
                <Form.Item
                  name="username"
                  label="用户名"
                  style={{ flex: 1 }}
                  rules={[
                    { required: true, message: '请输入用户名' },
                    { max: 100, message: '用户名不能超过 100 个字符' },
                  ]}
                >
                  <Input placeholder="readonly" autoComplete="off" />
                </Form.Item>
              </div>

              <div style={{ display: 'flex', gap: 12 }}>
                <Form.Item
                  name="password"
                  label="密码"
                  style={{ flex: 1 }}
                  rules={[
                    { required: passwordRequired, message: 'SQL Server 密码必填' },
                    { max: 4096, message: '密码过长' },
                  ]}
                  extra={
                    isMssql
                      ? 'SQL Server 密码必填；已保存后留空表示不修改。'
                      : '密码只写不读，服务端不会回传明文。'
                  }
                >
                  <Input.Password
                    placeholder={
                      database?.has_password ? '已保存，留空则不修改' : '可选，无密码可留空'
                    }
                    autoComplete="new-password"
                  />
                </Form.Item>
                <Form.Item
                  name="ssl_mode"
                  label="SSL 模式"
                  style={{ width: 200 }}
                  rules={[{ required: true, message: '请选择 SSL 模式' }]}
                  extra={isMssql ? 'require 及以上启用加密（encrypt）。' : undefined}
                >
                  <Select options={SSL_MODE_OPTIONS} />
                </Form.Item>
              </div>

              {!isMssql && database?.has_password ? (
                <Form.Item
                  name="clear_password"
                  valuePropName="checked"
                  dependencies={['password']}
                  rules={[
                    {
                      validator: async (_rule, value: boolean | undefined) => {
                        if (value && form.getFieldValue('password')) {
                          throw new Error('已输入新密码时无需勾选清除');
                        }
                      },
                    },
                  ]}
                  extra="清除操作在保存时会弹出二次确认。"
                >
                  <Checkbox>清除已保存的密码</Checkbox>
                </Form.Item>
              ) : null}
            </>
          )}

          <Alert
            type="info"
            showIcon
            message="数据库凭据仅用于建立只读连接；SQL 控制台不会执行写操作。"
            style={{ marginTop: 4 }}
          />
          <Typography.Text
            type="secondary"
            style={{ fontSize: 12, display: 'block', marginTop: 12 }}
          >
            保存后可在列表中测试连接；连接测试使用已保存的凭据执行最小探针。
          </Typography.Text>
        </Form>
      </Modal>

      <ConfirmActionModal
        open={pendingValues !== null}
        title="清除已保存的密码？"
        description="清除后该数据库连接将不再携带密码，直到重新设置。此操作会立即生效。"
        okText="清除密码"
        loading={mutation.isPending && pendingValues !== null}
        danger
        onConfirm={() => {
          if (pendingValues) {
            submit(pendingValues);
          }
        }}
        onCancel={() => setPendingValues(null)}
      />
    </>
  );
}
