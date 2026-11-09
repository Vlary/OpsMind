// M4 五期：告警页（CONTRACT.md 8.1 /alerts、8.7——Tab：规则/事件/通知渠道，
// 另含 AI 诊断会话入口）。页面只做 Tab 编排与抽屉状态；各 Tab 数据自取。

import { Tabs } from 'antd';
import { useState } from 'react';
import { useSearchParams } from 'react-router-dom';

import type { components } from '../../api/schema';
import PageHeader from '../../components/PageHeader';
import '../../styles/admin.css';

import ChannelFormDrawer from './ChannelFormDrawer';
import ChannelsTab from './ChannelsTab';
import DiagnosisDrawer from './DiagnosisDrawer';
import DiagnosisTab from './DiagnosisTab';
import EventsTable from './EventsTable';
import RuleFormDrawer from './RuleFormDrawer';
import RulesTab from './RulesTab';
import { useNotificationChannels } from './useNotificationChannels';

type AlertRule = components['schemas']['AlertRule'];
type NotificationChannel = components['schemas']['NotificationChannel'];

export default function AlertsPage() {
  // 深链（契约 8.7 五期工具卡）：/alerts?diagnosis=<id> 直接打开诊断会话
  // 抽屉（run_diagnosis 结果卡片的跳转目标）。
  const [searchParams, setSearchParams] = useSearchParams();
  const linkedDiagnosis = searchParams.get('diagnosis');
  const [activeTab, setActiveTab] = useState(linkedDiagnosis !== null ? 'diagnosis' : 'rules');
  // 规则抽屉：rule=null 表示新建。
  const [ruleDrawer, setRuleDrawer] = useState<{ open: boolean; rule: AlertRule | null }>({
    open: false,
    rule: null,
  });
  const [channelDrawer, setChannelDrawer] = useState<{
    open: boolean;
    channel: NotificationChannel | null;
  }>({ open: false, channel: null });
  const [diagnosisId, setDiagnosisId] = useState<string | null>(linkedDiagnosis);

  // 渠道列表供规则表单的绑定下拉（挂在页面级，规则 Tab 打开时也可见）。
  const channelsQuery = useNotificationChannels();
  const channels = channelsQuery.data ?? [];

  return (
    <div className="opsmind-admin-page">
      <PageHeader title="告警" description="阈值规则、触发事件、通知渠道与 AI 自诊断。" />
      <Tabs
        activeKey={activeTab}
        onChange={setActiveTab}
        items={[
          {
            key: 'rules',
            label: '规则',
            children: (
              <RulesTab
                onCreate={() => {
                  setRuleDrawer({ open: true, rule: null });
                }}
                onEdit={(rule) => {
                  setRuleDrawer({ open: true, rule });
                }}
              />
            ),
          },
          {
            key: 'events',
            label: '事件',
            children: (
              <EventsTable
                onDiagnose={() => {
                  // 事件行「AI 诊断」入口：跳到诊断 Tab 由用户选择目标服务器
                  // 发起（事件维度自动诊断由后端 monitor 评估器触发，页面
                  // 不重复建会话）；此处切换 Tab 展示诊断列表。
                  setActiveTab('diagnosis');
                }}
              />
            ),
          },
          {
            key: 'channels',
            label: '通知渠道',
            children: (
              <ChannelsTab
                onCreate={() => {
                  setChannelDrawer({ open: true, channel: null });
                }}
                onEdit={(channel) => {
                  setChannelDrawer({ open: true, channel });
                }}
              />
            ),
          },
          {
            key: 'diagnosis',
            label: 'AI 诊断',
            children: <DiagnosisTab onOpen={setDiagnosisId} />,
          },
        ]}
      />

      <RuleFormDrawer
        key={ruleDrawer.rule?.id ?? 'new'}
        open={ruleDrawer.open}
        rule={ruleDrawer.rule}
        channels={channels}
        onClose={() => {
          setRuleDrawer({ open: false, rule: null });
        }}
      />
      <ChannelFormDrawer
        key={channelDrawer.channel?.id ?? 'new'}
        open={channelDrawer.open}
        channel={channelDrawer.channel}
        onClose={() => {
          setChannelDrawer({ open: false, channel: null });
        }}
      />
      <DiagnosisDrawer
        open={diagnosisId !== null}
        sessionId={diagnosisId}
        onClose={() => {
          setDiagnosisId(null);
          if (linkedDiagnosis !== null) {
            const next = new URLSearchParams(searchParams);
            next.delete('diagnosis');
            setSearchParams(next, { replace: true });
          }
        }}
      />
    </div>
  );
}
