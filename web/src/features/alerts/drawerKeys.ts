// 告警页抽屉的 React key（纯函数，node 环境直测）。
//
// 走查第 2 轮 low：规则抽屉与通知渠道抽屉此前都用 `id ?? 'new'`，两个
// 新建态同时挂载时 key 都是 'new'，React 报重复 key。key 必须带类型前缀。

export function formDrawerKey(kind: 'rule' | 'channel', id?: string | null): string {
  return `${kind}-${id ?? 'new'}`;
}
