import { Tag } from 'antd';

const levelMap: Record<string, { color: string; label: string }> = {
  safe: { color: 'green', label: '安全' },
  needs_approval: { color: 'gold', label: '需审批' },
  forbidden: { color: 'red', label: '禁止' },
};

export default function RiskBadge({ level }: { level: string }) {
  const hit = levelMap[level] ?? { color: 'default', label: level };
  return <Tag color={hit.color}>{hit.label}</Tag>;
}
