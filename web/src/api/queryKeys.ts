// CONTRACT.md 8.4：query key 唯一来源，禁止散落字符串。
export const qk = {
  auth: ['auth'] as const,
  me: () => [...qk.auth, 'me'] as const,
  conversations: ['conversations'] as const,
  conversation: (id: string) => ['conversation', id] as const,
  messages: (id: string) => ['messages', id] as const,
  toolCalls: (id: string) => ['tool-calls', id] as const,
  approvals: (conversationId: string) => ['approvals', conversationId] as const,
  // 六期打磨 sprint 3：单条审批（批量详情「编辑命令」需要原命令/风险级别/
  // server_id；按钮点击时才取，不进轮询）。
  approval: (id: string) => ['approval', id] as const,
  batchRiskPreview: (command: string) => ['batch-risk-preview', command] as const,
  pendingApprovals: (filters: Record<string, string | number | undefined>) =>
    ['approvals', filters] as const,
  servers: ['servers'] as const,
  server: (id: string) => ['server', id] as const,
  databases: ['databases'] as const,
  database: (id: string) => ['database', id] as const,
  providers: ['providers'] as const,
  provider: (id: string) => ['provider', id] as const,
  presets: ['provider-presets'] as const,
  audit: (filters: Record<string, string | number | undefined>) => ['audit', filters] as const,
  health: ['health'] as const,
  // F1 二期（CONTRACT.md 8.4）：终端/RDP 会话与录制列表 key。
  termSessions: (filters: Record<string, string | undefined>) =>
    ['term-sessions', filters] as const,
  termRecordings: ['term-recordings'] as const,
  rdpSessions: (filters: Record<string, string | undefined>) => ['rdp-sessions', filters] as const,
  rdpRecordings: ['rdp-recordings'] as const,
  // C2 四期（CONTRACT.md 8.4）：容器列表/详情/日志/镜像 key。
  containers: (serverId: string) => ['containers', serverId] as const,
  container: (serverId: string, containerId: string) =>
    ['container', serverId, containerId] as const,
  containerLogs: (serverId: string, containerId: string, tail: number) =>
    ['container-logs', serverId, containerId, tail] as const,
  serverImages: (serverId: string) => ['server-images', serverId] as const,
  // M4 五期（CONTRACT.md 8.4）：监控/告警/诊断/巡检/批量 key。
  monitorOverview: ['monitor-overview'] as const,
  collector: ['monitor-collector'] as const,
  serverMetrics: (serverId: string, minutes: number) =>
    ['server-metrics', serverId, minutes] as const,
  alertRules: ['alert-rules'] as const,
  alertRule: (id: string) => ['alert-rule', id] as const,
  alertEvents: (filters: Record<string, string | number | undefined>) =>
    ['alert-events', filters] as const,
  notificationChannels: ['notification-channels'] as const,
  diagnosisSessions: (filters: Record<string, string | undefined>) =>
    ['diagnosis-sessions', filters] as const,
  diagnosisSession: (id: string) => ['diagnosis-session', id] as const,
  inspectionJobs: ['inspection-jobs'] as const,
  inspectionJob: (id: string) => ['inspection-job', id] as const,
  inspectionReportsPrefix: ['inspection-reports'] as const,
  inspectionReports: (filters: Record<string, string | undefined>) =>
    ['inspection-reports', filters] as const,
  inspectionReport: (id: string) => ['inspection-report', id] as const,
  batchJobs: ['batch-jobs'] as const,
  batchJob: (id: string) => ['batch-job', id] as const,
  // FE-P2（六期，CONTRACT.md 8.4）：会话执行回放 key。每次打开重取
  // （staleTime 0）；流式进行中禁用回放入口（先等 done）。
  replay: (id: string) => ['replay', id] as const,
  // FE-R（八期，CONTRACT.md 8.4）：回放目录（带 q/has_actions 过滤，
  // infinite query；q/has_actions 变化直接切 key，不失效旧 key）。
  replays: (filters: Record<string, string | boolean | undefined>) => ['replays', filters] as const,
  // 六期（FE-P1，CONTRACT.md 8.4）：公开系统设置（全局审批门限）key。
  settings: ['settings'] as const,
  // 六期（FE-P1）：审批卡编辑态的改写命令实时风险预览（preview-risk）。
  // 与 batchRiskPreview 同构：相同命令 15s staleTime 去重。
  approvalRiskPreview: (command: string) => ['approval-risk-preview', command] as const,
};
