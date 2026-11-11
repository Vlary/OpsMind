// FE-R（八期）：独立回放页 `/replays` 与 `/replays/:conversationId`
// （契约 2.12 约定 1/5，8.1 路由表，8.7「回放页」）。
//
// 页面 IA：页头（三层一致的标题「回放执行」）+ 左栏会话目录 + 右栏播放器。
// 选中会话来自 URL 参数——刷新/分享链接可复原，不放进任何 store（8.5）。
// 三处入口（导航 / 对话页「回放执行」按钮 / 审计行「回放」）统一 SPA 跳转
// 到本页，Modal 形态已下线。

import { useCallback } from 'react';
import { useNavigate, useParams } from 'react-router-dom';

import PageHeader from '../../components/PageHeader';
import ReplayCatalog from './ReplayCatalog';
import ReplayPlayer from './ReplayPlayer';
import './replay.css';

export default function ReplaysPage() {
  const { conversationId } = useParams<{ conversationId: string }>();
  const navigate = useNavigate();
  const activeId = conversationId && conversationId.length > 0 ? conversationId : null;

  // 点击目录行：push 到该会话（浏览器后退必须能回上一个会话，契约 8.7 ⑧）。
  const handleSelect = useCallback(
    (id: string) => {
      navigate(`/replays/${encodeURIComponent(id)}`);
    },
    [navigate],
  );

  return (
    <div className="opsmind-replay-page">
      <PageHeader
        title="回放执行"
        description="按时间轴回放 AI 会话的消息、工具调用、审批与执行；左侧选择会话，右侧播放。"
      />

      <div className="opsmind-replay-layout">
        <ReplayCatalog activeId={activeId} onSelect={handleSelect} />
        {/* key 保证切换会话时播放器状态（播放位置/速度/筛选/搜索）复位，
            而不是沿用上一个会话的进度。 */}
        <ReplayPlayer key={activeId ?? 'none'} conversationId={activeId ?? undefined} />
      </div>
    </div>
  );
}
