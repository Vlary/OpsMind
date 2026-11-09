import { SyncOutlined } from '@ant-design/icons';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Checkbox, Form, Input, Modal, Select, Space, Typography } from 'antd';
import { message } from '../../components/antdMessage';
import { useState } from 'react';

import { api } from '../../api/client';
import { qk } from '../../api/queryKeys';
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
  KIND_DEFAULT_BASE_URL,
  PROVIDER_KIND_OPTIONS,
  type ProviderKind,
} from '../../components/admin/meta';

import ModelPicker from './ModelPicker';

type Provider = components['schemas']['Provider'];
type ProviderPreset = components['schemas']['ProviderPreset'];
type ProviderCreate = components['schemas']['ProviderCreate'];
type ProviderUpdate = components['schemas']['ProviderUpdate'];
type ProviderPresetName = components['schemas']['ProviderPresetName'];

interface ProviderFormValues {
  name: string;
  preset: ProviderPresetName;
  kind: ProviderKind;
  base_url: string;
  api_key?: string;
  default_model: string;
  clear_api_key?: boolean;
}

interface ProviderFormModalProps {
  open: boolean;
  provider: Provider | null;
  presets: ProviderPreset[];
  onClose: () => void;
  onSaved: () => void;
}

const PROVIDER_FIELDS = ['name', 'preset', 'kind', 'base_url', 'api_key', 'default_model'];

function buildInitialValues(
  provider: Provider | null,
  presets: ProviderPreset[],
): Partial<ProviderFormValues> {
  if (provider) {
    return {
      name: provider.name,
      preset: provider.preset,
      kind: provider.kind,
      base_url: provider.base_url,
      default_model: provider.default_model,
      clear_api_key: false,
    };
  }
  const first = presets[0];
  if (first) {
    return {
      name: first.name,
      preset: first.preset,
      kind: first.kind,
      base_url: first.base_url,
      default_model: first.suggested_models[0] ?? '',
      clear_api_key: false,
    };
  }
  return {
    name: '',
    preset: 'custom',
    kind: 'openai_compatible',
    base_url: '',
    default_model: '',
    clear_api_key: false,
  };
}

function normalizeBaseUrl(value: string): string {
  return value.trim().replace(/\/+$/, '');
}

function uniqueModels(values: string[]): string[] {
  return Array.from(new Set(values.filter((value) => value.trim() !== '')));
}

export default function ProviderFormModal({
  open,
  provider,
  presets,
  onClose,
  onSaved,
}: ProviderFormModalProps) {
  const queryClient = useQueryClient();
  const [form] = Form.useForm<ProviderFormValues>();
  const [failure, setFailure] = useState<ApiFailure | null>(null);
  const [pendingValues, setPendingValues] = useState<ProviderFormValues | null>(null);
  const [models, setModels] = useState<string[]>(provider?.models ?? []);

  const presetName = (Form.useWatch('preset', form) ??
    provider?.preset ??
    'custom') as ProviderPresetName;
  const kind = (Form.useWatch('kind', form) ??
    provider?.kind ??
    'openai_compatible') as ProviderKind;
  const selectedPreset = presets.find((preset) => preset.preset === presetName);
  const apiKeyRequired =
    selectedPreset?.api_key_required ?? (presetName !== 'ollama' && presetName !== 'custom');
  const isCreate = provider === null;

  const modelOptions = uniqueModels([...(selectedPreset?.suggested_models ?? []), ...models]);

  const refreshMutation = useMutation({
    mutationFn: async () => {
      if (!provider) {
        throw new ApiRequestError({
          code: 'VALIDATION_FAILED',
          message: 'provider not saved',
          requestId: '',
          details: null,
          status: null,
        });
      }
      const { data, error, response } = await api.POST('/api/providers/{id}/models/refresh', {
        params: { path: { id: provider.id }, header: writeHeaders() },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: async (data) => {
      const nextModels = data?.models ?? [];
      setModels(nextModels);
      message.success(`已拉取 ${nextModels.length} 个模型`);
      if (provider) {
        await queryClient.invalidateQueries({ queryKey: qk.providers });
        await queryClient.invalidateQueries({ queryKey: qk.provider(provider.id) });
      }
    },
    onError: (error) => {
      setFailure(failureFromError(error));
    },
  });

  const mutation = useMutation({
    mutationFn: async (values: ProviderFormValues) => {
      const apiKey = values.api_key?.trim() ?? '';
      if (provider) {
        const basePayload: Omit<ProviderUpdate, 'clear_api_key'> = {
          name: values.name.trim(),
          preset: values.preset,
          kind: values.kind,
          base_url: normalizeBaseUrl(values.base_url),
          default_model: values.default_model.trim(),
        };
        if (apiKey !== '') {
          basePayload.api_key = apiKey;
        }
        // api_key 与 clear_api_key 不能同时出现，false 也不发送。
        const payload = {
          ...basePayload,
          ...(apiKey === '' && values.clear_api_key ? { clear_api_key: true } : {}),
        } as ProviderUpdate;
        const { data, error, response } = await api.PATCH('/api/providers/{id}', {
          params: { path: { id: provider.id }, header: writeHeaders() },
          body: payload,
        });
        if (error) {
          throw new ApiRequestError(toApiFailure(error, response));
        }
        return data;
      }
      const payload: ProviderCreate = {
        name: values.name.trim(),
        preset: values.preset,
        kind: values.kind,
        base_url: normalizeBaseUrl(values.base_url),
        default_model: values.default_model.trim(),
      };
      if (apiKey !== '') {
        payload.api_key = apiKey;
      }
      if (selectedPreset && selectedPreset.suggested_models.length > 0) {
        payload.models = selectedPreset.suggested_models;
      }
      const { data, error, response } = await api.POST('/api/providers', {
        body: payload,
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
      onSaved();
    },
    onError: (error) => {
      const nextFailure = failureFromError(error);
      applyFieldErrors(nextFailure.details, PROVIDER_FIELDS, (items) =>
        form.setFields(items as unknown as Parameters<typeof form.setFields>[0]),
      );
      setFailure(nextFailure);
      setPendingValues(null);
    },
  });

  const submit = (values: ProviderFormValues) => {
    setFailure(null);
    mutation.mutate(values);
  };

  const apiKeyPlaceholder = provider
    ? provider.has_api_key
      ? `已保存（${provider.api_key_masked}），留空则不修改`
      : '未保存 API Key，可按需输入'
    : '请输入 API Key';

  return (
    <>
      <Modal
        open={open}
        title={provider ? `编辑供应商 · ${provider.name}` : '新增供应商'}
        width={720}
        okText="保存"
        cancelText="取消"
        confirmLoading={mutation.isPending}
        cancelButtonProps={{ disabled: mutation.isPending }}
        onOk={() => form.submit()}
        onCancel={onClose}
        destroyOnHidden
      >
        {failure ? <ApiErrorAlert failure={failure} /> : null}
        {presets.length === 0 ? (
          <Alert
            type="warning"
            showIcon
            message="供应商预设未加载"
            description="可以手动填写类型与 Base URL；预设仅用于自动填充。"
            style={{ marginBottom: 16 }}
          />
        ) : null}

        <Form<ProviderFormValues>
          form={form}
          layout="vertical"
          initialValues={buildInitialValues(provider, presets)}
          requiredMark={false}
          onFinish={(values) => {
            if (provider && values.clear_api_key && !values.api_key?.trim()) {
              setPendingValues(values);
              return;
            }
            submit(values);
          }}
        >
          <Form.Item
            name="preset"
            label="预设"
            rules={[{ required: true, message: '请选择预设' }]}
            extra="选择预设会自动填充名称、类型与 Base URL，仍可手动修改。"
          >
            <Select
              showSearch
              optionFilterProp="label"
              placeholder="选择供应商预设"
              options={presets.map((preset) => ({
                value: preset.preset,
                label: `${preset.name}（${preset.preset}）`,
              }))}
              onChange={(next: ProviderPresetName) => {
                const preset = presets.find((item) => item.preset === next);
                if (!preset) {
                  return;
                }
                form.setFieldsValue({
                  name: preset.name,
                  kind: preset.kind,
                  base_url: preset.base_url,
                  default_model: preset.suggested_models[0] ?? form.getFieldValue('default_model'),
                });
              }}
            />
          </Form.Item>

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
              <Input placeholder="例如 OpenAI 生产环境" />
            </Form.Item>
            <Form.Item
              name="kind"
              label="类型"
              style={{ width: 220 }}
              rules={[{ required: true, message: '请选择类型' }]}
              extra={presetName === 'custom' ? '自定义端点可选择协议类型' : '由预设决定，不可修改'}
            >
              <Select
                options={PROVIDER_KIND_OPTIONS}
                disabled={presetName !== 'custom'}
                onChange={(next: ProviderKind) => {
                  const current = form.getFieldValue('base_url') as string | undefined;
                  const defaults = Object.values(KIND_DEFAULT_BASE_URL);
                  if (!current || defaults.includes(current)) {
                    form.setFieldValue('base_url', KIND_DEFAULT_BASE_URL[next]);
                  }
                }}
              />
            </Form.Item>
          </div>

          <Form.Item
            name="base_url"
            label="Base URL"
            rules={[
              { required: true, message: '请输入 Base URL' },
              { max: 2048, message: 'Base URL 过长' },
              {
                validator: async (_rule, value: string | undefined) => {
                  const raw = value?.trim() ?? '';
                  if (raw === '') {
                    return;
                  }
                  let parsed: URL;
                  try {
                    parsed = new URL(raw);
                  } catch {
                    throw new Error('请输入合法的 http(s) 地址');
                  }
                  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
                    throw new Error('仅支持 http 或 https');
                  }
                  if (parsed.username || parsed.password) {
                    throw new Error('地址不能包含用户名或密码');
                  }
                  if (parsed.hash) {
                    throw new Error('地址不能包含片段（#）');
                  }
                  if (kind === 'anthropic' && /\/v1\/?$/.test(parsed.pathname)) {
                    throw new Error('Anthropic Base URL 只到域名，不要包含 /v1');
                  }
                },
              },
            ]}
            extra="保存时会去掉尾部斜杠。OpenAI 兼容端点通常包含 /v1。"
          >
            <Input placeholder="https://api.openai.com/v1" />
          </Form.Item>

          <Form.Item
            name="api_key"
            label="API Key"
            rules={[
              { required: isCreate && apiKeyRequired, message: '请输入 API Key' },
              { max: 8192, message: 'API Key 过长' },
            ]}
            extra="API Key 只写不读，服务端只会返回掩码。"
          >
            <Input.Password placeholder={apiKeyPlaceholder} autoComplete="new-password" />
          </Form.Item>

          {provider?.has_api_key ? (
            <Form.Item
              name="clear_api_key"
              valuePropName="checked"
              dependencies={['api_key']}
              rules={[
                {
                  validator: async (_rule, value: boolean | undefined) => {
                    if (value && form.getFieldValue('api_key')) {
                      throw new Error('已输入新 API Key 时无需勾选清除');
                    }
                  },
                },
              ]}
              extra="清除操作在保存时会弹出二次确认。"
            >
              <Checkbox>清除已保存的 API Key</Checkbox>
            </Form.Item>
          ) : null}

          <Form.Item
            name="default_model"
            label="默认模型"
            rules={[
              { required: true, message: '请选择或输入默认模型' },
              { max: 255, message: '模型名不能超过 255 个字符' },
            ]}
          >
            <ModelPicker options={modelOptions} />
          </Form.Item>

          {provider ? (
            <Space align="center" wrap style={{ marginBottom: 16 }}>
              <Button
                icon={<SyncOutlined />}
                loading={refreshMutation.isPending}
                onClick={() => refreshMutation.mutate()}
              >
                拉取模型列表
              </Button>
              <Typography.Text type="secondary">已保存 {models.length} 个模型</Typography.Text>
            </Space>
          ) : (
            <Typography.Text type="secondary" style={{ display: 'block', marginBottom: 16 }}>
              创建后可在列表中拉取模型列表。
            </Typography.Text>
          )}
        </Form>
      </Modal>

      <ConfirmActionModal
        open={pendingValues !== null}
        title="清除已保存的 API Key？"
        description="清除后该供应商的请求将不再携带认证信息，直到重新设置。此操作会立即生效。"
        okText="清除 API Key"
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
