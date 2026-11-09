import { useMutation } from '@tanstack/react-query';
import {
  Alert,
  Checkbox,
  Form,
  Input,
  InputNumber,
  Modal,
  Radio,
  Select,
  Space,
  Switch,
  Typography,
} from 'antd';
import { useState } from 'react';

import { api } from '../../api/client';
import type { components } from '../../api/schema';
import ApiErrorAlert from '../../components/admin/ApiErrorAlert';
import {
  ApiRequestError,
  applyFieldErrors,
  failureFromError,
  type ApiFailure,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import {
  authMethodOptionsForOS,
  AUTH_METHOD_LABELS,
  isWinRMAuthMethod,
  normalizeAuthMethodForOS,
  OS_OPTIONS,
  type AuthMethod,
  type TargetOS,
} from '../../components/admin/meta';
import CopyButton from '../../components/CopyButton';

type Server = components['schemas']['Server'];
type ServerCreate = components['schemas']['ServerCreate'];
type ServerUpdate = components['schemas']['ServerUpdate'];
type DeviceKind = components['schemas']['DeviceKind'];
type NetdevBrand = components['schemas']['NetdevBrand'];
type AccessProto = components['schemas']['AccessProto'];

export interface ServerFormValues {
  name: string;
  device_kind: DeviceKind;
  os: TargetOS;
  netdev_brand?: NetdevBrand;
  access_proto?: AccessProto;
  host: string;
  port: number;
  winrm_port?: number;
  username: string;
  auth_method: AuthMethod;
  password?: string;
  private_key?: string;
  passphrase?: string;
  enable_secret?: string;
  clear_enable_secret?: boolean;
  tags?: string[];
  auto_exec_enabled: boolean;
}

// ---- 三期（N3）：网络设备元数据（契约 2.6/2.8，导出供列表页与终端页复用）----

// eslint-disable-next-line react-refresh/only-export-components -- 纯常量导出（N3）
export const DEVICE_KIND_OPTIONS: { value: DeviceKind; label: string }[] = [
  { value: 'server', label: '服务器' },
  { value: 'network_device', label: '网络设备' },
];

/** 品牌五选（契约 2.6：cisco|huawei|h3c|junos|generic，缺省 cisco）。 */
// eslint-disable-next-line react-refresh/only-export-components -- 纯常量导出（N3）
export const NETDEV_BRAND_OPTIONS: { value: NetdevBrand; label: string }[] = [
  { value: 'cisco', label: 'Cisco' },
  { value: 'huawei', label: '华为' },
  { value: 'h3c', label: 'H3C' },
  { value: 'junos', label: 'Juniper' },
  { value: 'generic', label: '通用（Generic）' },
];

// eslint-disable-next-line react-refresh/only-export-components -- 纯常量导出（N3）
export const NETDEV_BRAND_LABELS: Record<NetdevBrand, string> = {
  cisco: 'Cisco',
  huawei: '华为',
  h3c: 'H3C',
  junos: 'Juniper',
  generic: '通用',
};

// eslint-disable-next-line react-refresh/only-export-components -- 纯函数导出（N3）
export function netdevBrandLabel(brand: NetdevBrand | ''): string {
  return brand === '' ? '通用' : (NETDEV_BRAND_LABELS[brand] ?? brand);
}

/** 接入协议（契约 2.6：ssh|telnet，缺省 ssh；telnet 明文需显式确认）。 */
// eslint-disable-next-line react-refresh/only-export-components -- 纯常量导出（N3）
export const ACCESS_PROTO_OPTIONS: { value: AccessProto; label: string }[] = [
  { value: 'ssh', label: 'SSH' },
  { value: 'telnet', label: 'Telnet（明文）' },
];

/** 端口缺省联动（契约 2.6：port 缺省 22，telnet 时建议 23）。 */
// eslint-disable-next-line react-refresh/only-export-components -- 纯常量导出（N3）
export const ACCESS_PROTO_DEFAULT_PORTS: Record<AccessProto, number> = {
  ssh: 22,
  telnet: 23,
};

// eslint-disable-next-line react-refresh/only-export-components -- 纯函数导出（N3）
export function isNetworkDevice(server: Server): boolean {
  return server.device_kind === 'network_device';
}

// p3 sprint 3：编辑态「修改地址或端口会清除已信任的主机密钥」提示只对
// 已保存通道有主机密钥语义的资产渲染（对齐 ServersPage serverChannelOf
// 的 hasHostKey）：Telnet 接入的网络设备是登录探针、WinRM 认证
//（NTLM/Basic）无 TOFU，均无主机密钥语义——此前一律渲染，与同一弹窗
// 底部「Telnet 通道为登录探针，无主机密钥语义」文案自相矛盾（实测
// 走查 sprint 3）。后端 store 对 host/port 变化清空 host_key 的逻辑不变
//（对无语义通道本就恒空，幂等）。
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出（纯函数，供 serverFormOs 单测）
export function editingHostKeyNoticeVisible(server: Server): boolean {
  if (isNetworkDevice(server)) {
    return server.access_proto !== 'telnet';
  }
  return !(server.os === 'windows' && isWinRMAuthMethod(server.auth_method));
}

interface ServerFormModalProps {
  open: boolean;
  server: Server | null;
  onClose: () => void;
  onSaved: () => void;
}

const DEFAULT_WINRM_PORT = 5985;

const SERVER_FIELDS = [
  'name',
  'device_kind',
  'os',
  'netdev_brand',
  'access_proto',
  'host',
  'port',
  'winrm_port',
  'username',
  'auth_method',
  'password',
  'private_key',
  'passphrase',
  'enable_secret',
  'tags',
  'auto_exec_enabled',
];

function buildInitialValues(server: Server | null): Partial<ServerFormValues> {
  if (!server) {
    return {
      device_kind: 'server',
      os: 'linux',
      access_proto: 'ssh',
      netdev_brand: 'cisco',
      port: 22,
      winrm_port: DEFAULT_WINRM_PORT,
      auth_method: 'password',
      tags: [],
      auto_exec_enabled: false,
    };
  }
  return {
    name: server.name,
    device_kind: server.device_kind,
    os: server.os,
    netdev_brand: server.device_kind === 'network_device' ? server.netdev_brand : 'cisco',
    access_proto: server.device_kind === 'network_device' ? server.access_proto : 'ssh',
    host: server.host,
    port: server.port,
    winrm_port: server.winrm_port || DEFAULT_WINRM_PORT,
    username: server.username,
    auth_method: server.auth_method,
    // enable_secret 只写不读（契约 8.7）：编辑时留空表示不修改，不回填。
    clear_enable_secret: false,
    tags: server.tags,
    auto_exec_enabled: server.auto_exec_enabled,
  };
}

/** 表单态的三期字段归一结果（导出供单测）：网络设备固定 os=network、auth_method=password。 */
// eslint-disable-next-line react-refresh/only-export-components -- 纯函数导出（N3）
export function resolveNetdevFields(values: ServerFormValues): {
  os: TargetOS;
  netdev_brand: NetdevBrand | '';
  access_proto: AccessProto;
  auth_method: AuthMethod;
} {
  if (values.device_kind === 'network_device') {
    // 契约 2.6：network_device 时 os 必须为 network、auth_method 只允许
    // password（SSH 密钥认证失败关闭）；表单层直接归一，不依赖后端 400。
    return {
      os: 'network',
      netdev_brand: values.netdev_brand ?? 'cisco',
      access_proto: values.access_proto ?? 'ssh',
      auth_method: 'password',
    };
  }
  return {
    os: values.os,
    netdev_brand: '',
    access_proto: 'ssh',
    auth_method: values.auth_method,
  };
}

// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出（纯函数，供 os/winrm 联动单测）
export function buildCreatePayload(values: ServerFormValues): ServerCreate {
  const netdev = resolveNetdevFields(values);
  const payload: ServerCreate = {
    name: values.name.trim(),
    device_kind: values.device_kind,
    os: netdev.os,
    netdev_brand: netdev.netdev_brand,
    access_proto: netdev.access_proto,
    host: values.host.trim(),
    port: values.port,
    winrm_port: values.winrm_port ?? DEFAULT_WINRM_PORT,
    username: values.username.trim(),
    auth_method: netdev.auth_method,
    tags: values.tags ?? [],
    auto_exec_enabled: values.auto_exec_enabled,
    // 生成类型把带 default 的字段标为必填；普通服务器在下方显式剔除。
    confirm_telnet_cleartext: netdev.access_proto === 'telnet',
  };
  if (netdev.auth_method === 'private_key') {
    payload.private_key = values.private_key ?? '';
    if (values.passphrase) {
      payload.passphrase = values.passphrase;
    }
  } else {
    // password / password_ntlm / password_basic 共用 password 字段（schema：三种认证必填 password）。
    payload.password = values.password ?? '';
  }
  if (values.device_kind === 'network_device') {
    // 契约 2.6：confirm_telnet_cleartext 是网络设备专属字段——telnet 必须
    // 显式 true（缺失/false 服务端 400）；普通服务器（device_kind=server）
    // 携带该字段会被服务端以「only allowed for network devices」拒绝
    // （三期拼装接口不一致，e2e 实测普通服务器创建被 422 拒绝）。
    payload.confirm_telnet_cleartext = netdev.access_proto === 'telnet';
    if (values.enable_secret) {
      payload.enable_secret = values.enable_secret;
    }
  } else {
    // 普通服务器不携带（exactOptionalPropertyTypes 下用 delete 剔除，
    // JSON.stringify 不会序列化已删除的键）。
    delete (payload as Partial<ServerCreate>).confirm_telnet_cleartext;
  }
  return payload;
}

// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出（纯函数，供 os/winrm 联动单测）
export function buildUpdatePayload(server: Server, values: ServerFormValues): ServerUpdate {
  const netdev = resolveNetdevFields(values);
  const payload: ServerUpdate = {
    name: values.name.trim(),
    host: values.host.trim(),
    port: values.port,
    username: values.username.trim(),
    tags: values.tags ?? [],
    auto_exec_enabled: values.auto_exec_enabled,
    // 生成类型把带 default 的字段标为必填；普通服务器在下方显式剔除。
    confirm_telnet_cleartext: netdev.access_proto === 'telnet',
  };
  const kindChanged = values.device_kind !== server.device_kind;
  if (kindChanged || netdev.os !== server.os) {
    payload.os = netdev.os;
  }
  if (kindChanged) {
    payload.device_kind = values.device_kind;
  }
  if (kindChanged || netdev.netdev_brand !== server.netdev_brand) {
    payload.netdev_brand = netdev.netdev_brand;
  }
  if (kindChanged || netdev.access_proto !== server.access_proto) {
    payload.access_proto = netdev.access_proto;
  }
  const winrmPort = values.winrm_port ?? DEFAULT_WINRM_PORT;
  if (winrmPort !== server.winrm_port) {
    payload.winrm_port = winrmPort;
  }
  const methodChanged = kindChanged || netdev.auth_method !== server.auth_method;
  // credentialEntered 只统计与目标认证方式匹配的字段：编辑私钥服务器时
  // 临时切到密码认证输入过密码、再切回私钥后，残留的 password 不属于
  // private_key 的凭据——若计入会携带 auth_method=private_key 却不带
  // private_key，服务端按 allOf 条件 required 返回 400，且密码框已卸载
  // 无法清除残留值自救（评审发现的保存死锁）。
  const credentialEntered =
    netdev.auth_method === 'private_key'
      ? Boolean(values.private_key || values.passphrase)
      : Boolean(values.password);
  if (methodChanged || credentialEntered) {
    payload.auth_method = netdev.auth_method;
  }
  if (netdev.auth_method === 'private_key') {
    if (values.private_key) {
      payload.private_key = values.private_key;
    }
    if (values.passphrase) {
      payload.passphrase = values.passphrase;
    }
  } else if (values.password) {
    payload.password = values.password;
  }
  // enable_secret 与 password 同语义（契约 2.6）：提供即替换，空串表示清空。
  if (values.device_kind === 'network_device') {
    // confirm_telnet_cleartext 是网络设备专属字段（契约 2.6）：access_proto
    // 生效为 telnet 时必须显式 true；普通服务器不携带（携带会被服务端
    // 以「only allowed for network devices」拒绝）。
    payload.confirm_telnet_cleartext = netdev.access_proto === 'telnet';
    if (values.enable_secret) {
      payload.enable_secret = values.enable_secret;
    } else if (values.clear_enable_secret && server.has_enable_secret) {
      payload.enable_secret = '';
    }
  } else {
    // 普通服务器不携带（契约 2.6 网络设备专属）：delete 后序列化不含该键。
    delete (payload as Partial<ServerUpdate>).confirm_telnet_cleartext;
  }
  return payload;
}

export default function ServerFormModal({ open, server, onClose, onSaved }: ServerFormModalProps) {
  const [form] = Form.useForm<ServerFormValues>();
  const [failure, setFailure] = useState<ApiFailure | null>(null);

  const os = (Form.useWatch('os', form) ?? server?.os ?? 'linux') as TargetOS;
  const deviceKind = (Form.useWatch('device_kind', form) ??
    server?.device_kind ??
    'server') as DeviceKind;
  const accessProto = (Form.useWatch('access_proto', form) ??
    (server?.device_kind === 'network_device' ? server.access_proto : 'ssh') ??
    'ssh') as AccessProto;
  const isNetdev = deviceKind === 'network_device';
  const isTelnet = isNetdev && accessProto === 'telnet';
  // useWatch 必须无条件调用（rules-of-hooks）；网络设备认证固定 password
  // （契约 2.6），按归一后的认证方式驱动 UI。
  const authMethodWatch = (Form.useWatch('auth_method', form) ??
    server?.auth_method ??
    'password') as AuthMethod;
  const authMethod: AuthMethod = isNetdev ? 'password' : authMethodWatch;
  const usesWinRM = !isNetdev && isWinRMAuthMethod(authMethod);
  const requiresCredential = server === null || authMethod !== server.auth_method;

  const mutation = useMutation({
    mutationFn: async (values: ServerFormValues) => {
      if (server) {
        const { data, error, response } = await api.PATCH('/api/servers/{id}', {
          params: { path: { id: server.id }, header: writeHeaders() },
          body: buildUpdatePayload(server, values),
        });
        if (error) {
          throw new ApiRequestError(toApiFailure(error, response));
        }
        return data;
      }
      const { data, error, response } = await api.POST('/api/servers', {
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
      // destroyOnHidden 只销毁 Modal 内容，useForm 的 store 跨开关存活；
      // 页面对「新增→新增」复用同一组件实例（key 同为 'new'），不重置会
      // 让上次的输入（含密码明文）残留到下次表单（走查 D4）。
      form.resetFields();
      onSaved();
    },
    onError: (error) => {
      const nextFailure = failureFromError(error);
      applyFieldErrors(nextFailure.details, SERVER_FIELDS, (items) =>
        form.setFields(items as unknown as Parameters<typeof form.setFields>[0]),
      );
      setFailure(nextFailure);
    },
  });

  // 取消路径同样重置：resetFields 把 store 恢复到 initialValues，与
  // destroyOnHidden + 页面 key 复用场景（连续两次新增）共同保证每次
  // 打开都是干净表单。
  const handleCancel = () => {
    form.resetFields();
    setFailure(null);
    onClose();
  };

  const passwordPlaceholder = server
    ? server.has_password
      ? '已保存，留空则不修改'
      : '未保存密码，请输入'
    : '请输入密码';
  const privateKeyPlaceholder = server
    ? server.has_private_key
      ? '已保存，留空则不修改'
      : '未保存私钥，请粘贴私钥'
    : '粘贴 OpenSSH 私钥';

  // Telnet 明文风险的显著二次确认（契约 2.6/8.7）：表单内常驻警示横幅 +
  // 保存按钮文案变化之外，提交前再弹一次不可跳过的确认对话框。
  const handleSubmit = () => {
    if (isTelnet) {
      Modal.confirm({
        title: '确认使用 Telnet 明文传输？',
        content:
          'Telnet 为明文传输，登录密码与 enable/super 特权密码在网络中可被截获。请确认该设备处于可接受明文风险的网络环境。',
        okText: '确认明文风险并保存',
        okButtonProps: { danger: true },
        cancelText: '返回检查',
        onOk: () => form.submit(),
      });
      return;
    }
    form.submit();
  };

  return (
    <Modal
      open={open}
      title={
        server
          ? isNetworkDevice(server)
            ? `编辑网络设备 · ${server.name}`
            : `编辑服务器 · ${server.name}`
          : isNetdev
            ? '新增网络设备'
            : '新增服务器'
      }
      width={680}
      okText={isTelnet ? '确认明文风险并保存' : '保存'}
      cancelText="取消"
      confirmLoading={mutation.isPending}
      cancelButtonProps={{ disabled: mutation.isPending }}
      onOk={handleSubmit}
      onCancel={handleCancel}
      destroyOnHidden
    >
      {failure ? <ApiErrorAlert failure={failure} /> : null}
      {/* 契约 8.7 ①（六期打磨 sprint 1）：详情/编辑态补「表单只读态」复制——
          服务器 ID 与 host:port 是不可编辑的派生事实，此前只在列表表格提供
          复制（实机编辑弹窗 .opsmind-copy-button 计数 = 0）。新建态无 ID
          可复制（host:port 尚在输入中），不渲染。 */}
      {server ? (
        <Space size={16} wrap style={{ marginBottom: 16 }}>
          <span className="opsmind-admin-cell-sub">
            ID <span className="opsmind-admin-mono">{server.id}</span>
            <CopyButton text={server.id} label="复制服务器 ID" />
          </span>
          <span className="opsmind-admin-cell-sub">
            <span className="opsmind-admin-mono">
              {server.host}:{server.port}
            </span>
            <CopyButton text={`${server.host}:${server.port}`} label="复制地址" />
          </span>
        </Space>
      ) : null}
      {server && editingHostKeyNoticeVisible(server) ? (
        <Alert
          type="info"
          showIcon
          message="修改地址或端口会清除已信任的主机密钥，下次连接需重新确认指纹。"
          style={{ marginBottom: 16 }}
        />
      ) : null}
      <Form<ServerFormValues>
        form={form}
        layout="vertical"
        initialValues={buildInitialValues(server)}
        onValuesChange={(changed: Partial<ServerFormValues>) => {
          if (changed.device_kind) {
            if (changed.device_kind === 'network_device') {
              // 契约 2.6：network_device 固定 os=network、auth_method=password；
              // 品牌/协议取缺省（cisco/ssh），端口重置为协议默认端口。
              // enable_secret 用空串清除残留（exactOptionalPropertyTypes 下
              // 不能显式传 undefined），空串与未输入等价。
              form.setFieldsValue({
                os: 'network',
                auth_method: 'password',
                netdev_brand: 'cisco',
                access_proto: 'ssh',
                port: ACCESS_PROTO_DEFAULT_PORTS.ssh,
                enable_secret: '',
                clear_enable_secret: false,
              });
            } else {
              form.setFieldsValue({
                os: 'linux',
                netdev_brand: 'cisco',
                access_proto: 'ssh',
                enable_secret: '',
                clear_enable_secret: false,
              });
              // 切回服务器形态：认证方式按 linux 归一（契约 2.6），端口
              // 从 telnet 默认 23 恢复 22（其余自定义端口保留）。
              const current = form.getFieldValue('auth_method') as AuthMethod;
              const normalized = normalizeAuthMethodForOS('linux', current);
              if (normalized !== current) {
                form.setFieldValue('auth_method', normalized);
              }
              if (form.getFieldValue('port') === ACCESS_PROTO_DEFAULT_PORTS.telnet) {
                form.setFieldValue('port', ACCESS_PROTO_DEFAULT_PORTS.ssh);
              }
            }
            return;
          }
          if (changed.access_proto) {
            // 端口默认 22/23 联动（契约 2.6：port 缺省 22，telnet 建议 23）：
            // 仅当端口仍是对侧协议的默认值时替换，用户自定义端口不动。
            const port = form.getFieldValue('port') as number | undefined;
            const from =
              ACCESS_PROTO_DEFAULT_PORTS[changed.access_proto === 'ssh' ? 'telnet' : 'ssh'];
            const to = ACCESS_PROTO_DEFAULT_PORTS[changed.access_proto];
            if (port === from) {
              form.setFieldValue('port', to);
            }
            return;
          }
          if (changed.os) {
            // 切换 os 后现存认证方式可能不合法（契约 2.6 后端 400），前端先行归一。
            const current = form.getFieldValue('auth_method') as AuthMethod;
            const normalized = normalizeAuthMethodForOS(changed.os, current);
            if (normalized !== current) {
              form.setFieldValue('auth_method', normalized);
            }
          }
          if (
            changed.auth_method &&
            isWinRMAuthMethod(changed.auth_method) &&
            !form.getFieldValue('winrm_port')
          ) {
            form.setFieldValue('winrm_port', DEFAULT_WINRM_PORT);
          }
        }}
        onFinish={(values) => {
          setFailure(null);
          mutation.mutate(values);
        }}
        requiredMark={false}
      >
        <Form.Item
          name="name"
          label="名称"
          rules={[
            { required: true, message: '请输入名称' },
            { max: 100, message: '名称不能超过 100 个字符' },
          ]}
        >
          <Input placeholder="例如 web-prod-01" />
        </Form.Item>

        <Form.Item
          name="device_kind"
          label="设备类型"
          rules={[{ required: true, message: '请选择设备类型' }]}
          extra={
            isNetdev
              ? '交换机 / 路由器 / 防火墙等 CLI 行协议设备，命令走品牌分册风险规则。'
              : 'Linux / Windows 主机，通过 SSH（或 WinRM）执行命令。'
          }
        >
          <Radio.Group optionType="button" buttonStyle="solid" options={DEVICE_KIND_OPTIONS} />
        </Form.Item>

        {isNetdev ? (
          <>
            <div style={{ display: 'flex', gap: 12 }}>
              <Form.Item
                name="netdev_brand"
                label="设备品牌"
                style={{ flex: 1 }}
                rules={[{ required: true, message: '请选择设备品牌' }]}
                extra="决定 netcli 交互 profile 与风险规则分册。"
              >
                <Select options={NETDEV_BRAND_OPTIONS} placeholder="选择品牌" />
              </Form.Item>
              <Form.Item
                name="access_proto"
                label="接入协议"
                style={{ width: 190 }}
                rules={[{ required: true, message: '请选择接入协议' }]}
              >
                <Radio.Group
                  optionType="button"
                  buttonStyle="solid"
                  options={ACCESS_PROTO_OPTIONS}
                />
              </Form.Item>
            </div>
            <Form.Item name="os" hidden initialValue="network">
              <Input />
            </Form.Item>
          </>
        ) : (
          <Form.Item
            name="os"
            label="操作系统"
            rules={[{ required: true, message: '请选择操作系统' }]}
            extra={
              os === 'windows'
                ? 'Windows 可走内置 OpenSSH（SSH 通道）或 WinRM（NTLM/Basic 密码认证）。'
                : 'Linux 通过 SSH 执行命令。'
            }
          >
            <Radio.Group optionType="button" buttonStyle="solid" options={OS_OPTIONS} />
          </Form.Item>
        )}

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
            <Input placeholder="192.0.2.10 或 host.example.com" />
          </Form.Item>
          {usesWinRM ? (
            <Form.Item
              name="winrm_port"
              label="WinRM 端口"
              style={{ width: 140 }}
              rules={[
                { required: true, message: '请输入 WinRM 端口' },
                {
                  type: 'number',
                  min: 1,
                  max: 65535,
                  message: '端口范围为 1-65535',
                },
              ]}
              extra="5985=HTTP，5986=HTTPS"
            >
              {/* 同 port：不设 min/max，由 Form 校验显式报错而非 blur 静默 clamp。 */}
              <InputNumber style={{ width: '100%' }} />
            </Form.Item>
          ) : (
            <Form.Item
              name="port"
              label="端口"
              style={{ width: 140 }}
              rules={[
                { required: true, message: '请输入端口' },
                {
                  type: 'number',
                  min: 1,
                  max: 65535,
                  message: '端口范围为 1-65535',
                },
              ]}
            >
              {/* 不设 InputNumber 的 min/max：antd 的 blur clamp 会把 0/越界值
                  静默改成 1/65535 后直接落库（走查 C1 实测输 0 建档成 :1），
                  让 Form 校验规则显式报「端口范围为 1-65535」拦截提交。 */}
              <InputNumber style={{ width: '100%' }} />
            </Form.Item>
          )}
        </div>

        <Form.Item
          name="username"
          label="登录用户"
          rules={[
            { required: true, message: '请输入登录用户' },
            { max: 100, message: '用户名不能超过 100 个字符' },
          ]}
        >
          <Input placeholder="root" autoComplete="off" />
        </Form.Item>

        {isNetdev ? (
          <Form.Item
            name="auth_method"
            label="认证方式"
            rules={[{ required: true, message: '请选择认证方式' }]}
            extra="网络设备仅支持密码登录（SSH 密钥认证三期不支持，失败关闭）；特权密码（enable/super）在下方单独配置。"
          >
            <Radio.Group
              optionType="button"
              buttonStyle="solid"
              disabled
              options={[{ value: 'password', label: AUTH_METHOD_LABELS.password }]}
            />
          </Form.Item>
        ) : (
          <Form.Item
            name="auth_method"
            label="认证方式"
            rules={[{ required: true, message: '请选择认证方式' }]}
          >
            <Radio.Group
              optionType="button"
              buttonStyle="solid"
              options={authMethodOptionsForOS(os)}
            />
          </Form.Item>
        )}

        {isTelnet ? (
          <Alert
            type="warning"
            showIcon
            message="Telnet 为明文传输，登录与特权密码可被截获"
            description="保存时需确认明文风险（confirm_telnet_cleartext）；建议改用 SSH。"
            style={{ marginBottom: 16 }}
          />
        ) : null}

        {os === 'windows' && !usesWinRM ? (
          <Alert
            type="info"
            showIcon
            message="密码/私钥认证走 Windows 内置 OpenSSH Server（Server 2019 / Win10 1809+ 起内置），需要在目标机启用。"
            style={{ marginBottom: 16 }}
          />
        ) : null}
        {authMethod === 'password_basic' ? (
          <Alert
            type="warning"
            showIcon
            message="Basic 为明文认证，建议仅在内网或 HTTPS(5986) 使用。"
            style={{ marginBottom: 16 }}
          />
        ) : null}

        {authMethod === 'private_key' ? (
          <>
            <Form.Item
              name="private_key"
              label="私钥"
              rules={[
                { required: requiresCredential, message: '请粘贴私钥' },
                { max: 65536, message: '私钥过长' },
              ]}
              extra="私钥只写不读，服务端不会回传明文。"
            >
              <Input.TextArea
                rows={6}
                placeholder={privateKeyPlaceholder}
                spellCheck={false}
                style={{ fontFamily: "'JetBrains Mono', 'Fira Code', monospace", fontSize: 12 }}
              />
            </Form.Item>
            <Form.Item
              name="passphrase"
              label="私钥口令"
              dependencies={['private_key']}
              rules={[
                { max: 4096, message: '口令过长' },
                {
                  validator: async (_rule, value: string | undefined) => {
                    if (value && !form.getFieldValue('private_key')) {
                      throw new Error('修改口令时需同时粘贴私钥');
                    }
                  },
                },
              ]}
            >
              <Input.Password
                placeholder={
                  server?.has_private_key ? '已保存，留空则不修改' : '可选，用于加密私钥'
                }
                autoComplete="new-password"
              />
            </Form.Item>
          </>
        ) : (
          <Form.Item
            name="password"
            label={usesWinRM ? `密码（${AUTH_METHOD_LABELS[authMethod]}）` : '密码'}
            rules={[
              { required: requiresCredential, message: '请输入密码' },
              { max: 4096, message: '密码过长' },
            ]}
            extra="密码只写不读，服务端不会回传明文。"
          >
            <Input.Password placeholder={passwordPlaceholder} autoComplete="new-password" />
          </Form.Item>
        )}

        {isNetdev ? (
          <Form.Item
            name="enable_secret"
            label="特权密码（enable / super）"
            dependencies={['clear_enable_secret']}
            rules={[
              { max: 4096, message: '特权密码过长' },
              {
                validator: async (_rule, value: string | undefined) => {
                  if (value && form.getFieldValue('clear_enable_secret')) {
                    throw new Error('已勾选清除时无需输入新特权密码');
                  }
                },
              },
            ]}
            extra={
              server?.has_enable_secret
                ? '已保存特权密码，留空表示不修改；设备需要特权模式时自动应答。'
                : '可选：Cisco enable / 华为、H3C super 的特权密码，独立于登录密码。'
            }
          >
            <Input.Password
              placeholder={
                server?.has_enable_secret ? '已保存，留空则不修改' : '可选，留空表示不配置'
              }
              autoComplete="new-password"
            />
          </Form.Item>
        ) : null}
        {isNetdev && server?.has_enable_secret ? (
          <Form.Item
            name="clear_enable_secret"
            valuePropName="checked"
            dependencies={['enable_secret']}
            extra="清除后设备需要特权模式的命令将直接返回设备侧报错，不再自动应答。"
          >
            <Checkbox>清除已保存的特权密码</Checkbox>
          </Form.Item>
        ) : null}

        <Form.Item
          name="tags"
          label="标签"
          rules={[
            {
              validator: async (_rule, value: string[] | undefined) => {
                const tags = value ?? [];
                if (tags.length > 16) {
                  throw new Error('标签最多 16 个');
                }
                if (tags.some((tag) => tag.length === 0 || tag.length > 32)) {
                  throw new Error('每个标签长度需为 1-32 个字符');
                }
              },
            },
          ]}
        >
          <Select
            mode="tags"
            maxCount={16}
            placeholder="输入后回车添加，例如 prod、web"
            tokenSeparators={[',', ' ']}
            options={[]}
          />
        </Form.Item>

        <Form.Item
          name="auto_exec_enabled"
          label="自动执行"
          valuePropName="checked"
          extra="开启后模型可直接执行被判定为安全的命令；风险命令仍需逐条审批。"
        >
          <Switch />
        </Form.Item>

        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {isNetdev
            ? isTelnet
              ? '新建后可在列表中测试连接；Telnet 通道为登录探针，无主机密钥语义。'
              : '新建后可在列表中测试连接；SSH 通道做握手并执行品牌探针命令（只读）。'
            : '新建后可在列表中测试连接；SSH 通道只做握手、不执行命令，WinRM 通道仅执行只读探针。'}
        </Typography.Text>
      </Form>
    </Modal>
  );
}
