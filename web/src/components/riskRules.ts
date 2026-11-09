// 风险规则 ID（internal/risk 命中的 rule_id）到中文说明的映射。
//
// 后端 reason 是面向审计的英文短语，直接展示在中文审批卡里读不懂；
// 审批卡按结构化 rule_id 渲染中文说明，原始 reason 折叠为「规则详情」。
// 未收录的规则 ID 返回 null，调用方回退展示原始 reason，不会丢信息。

export const RISK_RULE_LABELS: Record<string, string> = {
  ACCOUNT_CHANGE: '变更系统账号（创建/删除用户或修改密码）',
  CHMOD_SYSTEM: '修改系统关键路径的权限',
  COMMAND_INVALID: '命令无法安全解析（空命令、超长或含非法字符）',
  CONTAINER_CHANGE: '变更容器（启动、停止、删除或修改容器）',
  CRITICAL_SERVICE_STOP: '停止关键系统服务',
  CRON_CHANGE: '修改定时任务（cron）',
  CURL_PIPE_SHELL: '下载的远程内容直接交给 shell 执行',
  DD_DEVICE: '使用 dd 写入块设备或文件',
  DEFAULT_ASK: '未识别的命令，需人工确认后执行',
  DEVICE_WRITE: '直接写入设备文件',
  DYNAMIC_EVAL: '动态执行代码（eval 等运行时求值）',
  ENV_DUMP: '导出环境变量（可能包含密钥）',
  FILE_WRITE: '写入或修改文件',
  FIREWALL_FLUSH: '清空防火墙规则',
  FORK_BOMB: '检测到 fork 炸弹模式',
  HISTORY_CLEAR: '清除命令历史记录',
  KILL_INIT: '向 init 进程（PID 1）发送信号',
  LOG_TAMPER: '篡改或清空日志',
  MKFS: '格式化文件系统',
  NESTED_SSH: '通过 SSH 在远端再执行命令',
  NETWORK_CHANGE: '变更网络配置（地址、路由、防火墙等）',
  OBFUSCATION: '命令存在混淆或规避检测的迹象',
  PACKAGE_CHANGE: '安装或卸载软件包',
  PERMISSION_CHANGE: '变更文件属主或权限',
  POWER: '重启或关机',
  PROCESS_SIGNAL: '向进程发送信号',
  RM_ANY: '删除文件',
  RM_PROTECTED: '删除受保护的系统路径（递归/强制删除）',
  SAFE_FACTS: '固定的只读信息采集模板',
  SAFE_PIPE: '只读命令的常规管道',
  SAFE_READ: '只读命令',
  SAFE_SYSTEM_STATUS: '只读系统状态查询',
  SERVICE_CHANGE: '启动、停止或修改系统服务',
  SQL_DESTRUCTIVE: '破坏性 SQL（DROP、TRUNCATE、全表 DELETE 等）',
  SQL_WRITE: '写入型 SQL（INSERT、UPDATE、ALTER 等）',
  STORAGE_CHANGE: '变更存储（挂载、分区、逻辑卷等）',
};

// 三期网络设备分册（契约 7.3C）：五厂商前缀 × 同构后缀的中文说明。
// 后端 internal/risk/rules_netdev.go 按 prefix+后缀生成 rule_id
// （如 NET_CISCO_CONFIG_DUMP）；后缀语义五分册一致，按后缀统一映射。
const NETDEV_BRAND_PREFIXES = [
  'NET_CISCO_',
  'NET_HUAWEI_',
  'NET_H3C_',
  'NET_JUNOS_',
  'NET_GENERIC_',
];

const NETDEV_RULE_SUFFIX_LABELS: Record<string, string> = {
  RELOAD: '重启设备或断电',
  FACTORY_RESET: '擦除配置或恢复出厂设置',
  BOOT_CHANGE: '变更启动镜像',
  CONFIG_DUMP: '导出完整配置/日志（可能包含密钥）',
  ENABLE_MODE: '进入特权模式（二级提示符）',
  CONF_MODE: '进入配置模式',
  CONFIG_SAVE: '配置保存、导出或提交',
  CONFIG_LOAD: '注入外部配置',
  CAPTURE: '抓包可能暴露明文凭据',
  SAFE_READ: '只读查看命令',
  SAFE_PING: '网络诊断',
  SAFE_FACTS: '固定的只读信息采集模板',
  DEFAULT_ASK: '未识别的命令，需人工确认后执行',
};

/** 网络设备规则 ID：剥掉厂商前缀后按后缀映射（后缀语义五分册同构）。 */
function describeNetdevRule(ruleId: string): string | null {
  for (const prefix of NETDEV_BRAND_PREFIXES) {
    if (ruleId.startsWith(prefix)) {
      return NETDEV_RULE_SUFFIX_LABELS[ruleId.slice(prefix.length)] ?? null;
    }
  }
  return null;
}

/** 返回规则 ID 的中文说明；未收录返回 null（调用方回退展示原始 reason）。 */
export function describeRiskRule(ruleId: string | null | undefined): string | null {
  if (!ruleId) {
    return null;
  }
  if (ruleId.startsWith('NET_')) {
    return describeNetdevRule(ruleId);
  }
  return RISK_RULE_LABELS[ruleId] ?? null;
}
