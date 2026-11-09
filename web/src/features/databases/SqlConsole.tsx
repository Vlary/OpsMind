import { ClearOutlined, PlayCircleOutlined } from '@ant-design/icons';
import { useMutation } from '@tanstack/react-query';
import { Alert, Button, Drawer, Flex, Input, Tag, Typography } from 'antd';
import { useState } from 'react';

import { api } from '../../api/client';
import type { components } from '../../api/schema';
import ApiErrorAlert from '../../components/admin/ApiErrorAlert';
import {
  ApiRequestError,
  describeFailure,
  failureFromError,
  type ApiFailure,
  toApiFailure,
  writeHeaders,
} from '../../components/admin/apiError';
import { message } from '../../components/antdMessage';
import type { DatabaseEngine } from '../../components/admin/meta';

import QueryResultTable from './QueryResultTable';

type Database = components['schemas']['Database'];
type SqlQueryResult = components['schemas']['SqlQueryResult'];

interface SqlConsoleProps {
  database: Database;
  open: boolean;
  onClose: () => void;
}

export interface ConsoleSpec {
  title: string;
  initial: string;
  placeholder: string;
  allowedRule: string;
  emptyHint: string;
}

/** mongodb JSON 命令文档的顶层白名单（契约 7.6；权威校验在服务端守卫）。 */
// eslint-disable-next-line react-refresh/only-export-components -- 纯常量导出（N3）
export const MONGO_COMMAND_NAMES = [
  'find',
  'aggregate',
  'count',
  'distinct',
  'estimatedDocumentCount',
] as const;

/** 命令文档 schema 提示（契约 7.6 / 8.7，导出供单测与 UI 渲染）。 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function mongoCommandHints(): string[] {
  return [
    'find：{"find":"集合","filter":{},"sort":{},"limit":10,"skip":0,"projection":{}}',
    'aggregate：{"aggregate":"集合","pipeline":[{"$group":{"_id":"$字段","n":{"$sum":1}}}],"limit":10}',
    'count：{"count":"集合","query":{}}',
    'distinct：{"distinct":"集合","key":"字段","query":{}}',
    'estimatedDocumentCount：{"estimatedDocumentCount":"集合"}',
    '限制：maxTimeMS ≤ 60000、limit ≤ 1000；顶层只允许一个命令键。',
  ];
}

/**
 * mongodb 命令文档的本地校验（契约 7.6/G25 形状子集：JSON 对象、单个
 * 顶层命令键、无尾随内容；白名单与管道阶段的权威校验在服务端守卫，
 * 拒绝时以 SQL_NOT_READONLY 展示）。返回 null 表示通过，否则返回中文
 * 错误描述（导出供单测）。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function validateMongoDocument(text: string): string | null {
  const trimmed = text.trim();
  if (trimmed === '') {
    return '请输入 JSON 命令文档';
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch (error) {
    const detail = error instanceof Error ? error.message : 'JSON 语法错误';
    return `不是合法的 JSON 命令文档：${detail}`;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    return '命令文档必须是 JSON 对象（形如 {"find":"users","filter":{}}），不能是数组或字面量';
  }
  const keys = Object.keys(parsed);
  const commandKeys = keys.filter((key) =>
    (MONGO_COMMAND_NAMES as readonly string[]).includes(key),
  );
  if (commandKeys.length === 0) {
    return `顶层命令键必须是 ${MONGO_COMMAND_NAMES.join(' / ')} 之一`;
  }
  if (commandKeys.length > 1) {
    return `只允许一个顶层命令键（检测到 ${commandKeys.join('、')}）`;
  }
  return null;
}

/**
 * 查询抽屉按引擎适配（契约 8.7 / 7.6，导出供单测）：
 * - mongodb 输入单条 JSON 命令文档（白名单只读命令），本地做 JSON 形状校验；
 * - redis 输入单条命令文本（白名单只读命令），不是 SQL；
 * - mssql 仅单条 SELECT / WITH … SELECT（无 EXPLAIN 通道，EXEC/sp_* 拒绝）；
 * - postgres/mysql/sqlite 沿用一期 SQL 文案。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function consoleSpecFor(engine: DatabaseEngine): ConsoleSpec {
  if (engine === 'mongodb') {
    return {
      title: 'JSON 查询',
      initial: '{"find":"users","filter":{},"limit":10}',
      placeholder:
        '{"find":"users","filter":{},"limit":10} 或 {"aggregate":"orders","pipeline":[{"$group":{"_id":"$status","n":{"$sum":1}}}]}',
      allowedRule:
        'JSON 命令文档，仅支持 find / aggregate / count / distinct / estimatedDocumentCount（白名单只读）；写命令、未知命令与多文档不会执行。',
      emptyHint: '请输入要执行的 JSON 命令文档',
    };
  }
  if (engine === 'redis') {
    return {
      title: '命令查询',
      initial: 'PING',
      placeholder: 'GET user:1 或 ZRANGE key 0 -1 WITHSCORES',
      allowedRule:
        '仅支持白名单内的单条只读命令（GET / HGETALL / LRANGE / ZRANGE / INFO / SCAN 等）；写命令、未知命令、多命令与管道不会执行。',
      emptyHint: '请输入要执行的命令',
    };
  }
  if (engine === 'mssql') {
    return {
      title: 'SQL 查询',
      initial: 'SELECT 1;',
      placeholder: 'SELECT TOP (100) * FROM your_table;',
      allowedRule: '仅支持单条 SELECT / WITH … SELECT；EXEC / 存储过程、多语句与写语句不会执行。',
      emptyHint: '请输入要执行的 SQL',
    };
  }
  return {
    title: 'SQL 查询',
    initial: 'SELECT 1;',
    placeholder: 'SELECT * FROM ... LIMIT 100;',
    allowedRule:
      '仅支持单条 SELECT、WITH … SELECT、EXPLAIN（非 ANALYZE）。多语句、DDL 与写语句不会执行。',
    emptyHint: '请输入要执行的 SQL',
  };
}

// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function consoleTarget(database: Database): string {
  if (database.engine === 'sqlite') {
    return database.sqlite_path;
  }
  if (database.engine === 'redis') {
    return `${database.host}:${database.port}/db ${database.redis_db}`;
  }
  if (database.engine === 'mongodb') {
    const authSource = database.auth_db === '' ? 'admin' : database.auth_db;
    return `${database.host}:${database.port}/${database.database_name}（AuthSource ${authSource}）`;
  }
  if (database.engine === 'mssql' && database.instance_name) {
    return `${database.host}\\${database.instance_name}/${database.database_name}`;
  }
  return `${database.host}:${database.port}/${database.database_name}`;
}

/**
 * 查询失败文案按引擎适配（p2 sprint 3，导出供单测）：SQL_QUERY_FAILED 的
 * 通用映射是「SQL 执行失败」，但 redis/mongodb 抽屉的标题/初始语句/规则
 * 文案均为命令（JSON）口径（consoleSpecFor），失败标题统一为「SQL 执行
 * 失败」与之矛盾——redis 覆盖为「命令执行失败」、mongodb 覆盖为「查询
 * 执行失败」，底层原因照常拼入；其余引擎与错误码走通用 describeFailure
 * （错误码与 request_id 由 ApiErrorAlert 降级为小字）。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出
export function describeQueryFailure(failure: ApiFailure, engine: DatabaseEngine): string {
  const cliEngine = engine === 'redis' || engine === 'mongodb';
  if (cliEngine && (failure.code === 'SQL_QUERY_FAILED' || failure.code === 'SQL_TIMEOUT')) {
    // redis/mongodb 抽屉的标题/初始语句均为命令（JSON）口径，失败标题同步
    // 去 SQL 化（超时同理由后端映射 SQL_TIMEOUT，通用映射「SQL 查询超时」
    // 在 JSON 查询抽屉里口径矛盾——三期评审）；底层原因照常拼入。
    const title =
      failure.code === 'SQL_TIMEOUT'
        ? '查询超时'
        : engine === 'redis'
          ? '命令执行失败'
          : '查询执行失败';
    let detail = failure.message.trim();
    // p3 sprint 3：后端查询失败 message 固定前置「查询失败：」（databases.go
    // describeDatabaseFailure），mongodb 标题已是「查询执行失败」，保留会
    // 拼出「查询执行失败（查询失败：无法连接 …）」的双重「查询失败」
    // （实测走查 sprint 3）——拼接前剥离该前缀，对齐 describeFailure 对
    // SERVER_TEST_FAILED 通道前缀的剥离。redis 标题为命令口径、不重复，
    // 且超时 message（「查询超时」）无前缀，均不剥。
    if (
      engine === 'mongodb' &&
      failure.code === 'SQL_QUERY_FAILED' &&
      detail.startsWith('查询失败：')
    ) {
      detail = detail.slice('查询失败：'.length);
    }
    return detail !== '' ? `${title}（${detail}）` : title;
  }
  return describeFailure(failure);
}

/**
 * SQL_NOT_READONLY 通用警示文案（p3 sprint 4 导出供单测）：只读守卫不只拦
 * 写语句——MongoDB 参数超白名单上限（limit 5000 > 1000、maxTimeMS 超限等）
 * 同走 SQL_NOT_READONLY（主错误如 MONGO_PARAM_NOT_WHITELISTED），用户并
 * 无写数据意图时只提「如确需变更数据…」引导跑偏（走查 W3b），描述补一句
 * 参数类拒绝同样适用。
 */
// eslint-disable-next-line react-refresh/only-export-components -- 测试钩子导出（纯常量）
export const READONLY_REJECTION_GUIDE = {
  message: '本系统只执行只读查询，写操作不会执行',
  description:
    '如确需变更数据，请在对话中提出操作请求；风险规则评估为需审批时，将由审批流程确认后执行。参数超出白名单限制的查询同样以此拒绝。',
};

export default function SqlConsole({ database, open, onClose }: SqlConsoleProps) {
  const spec = consoleSpecFor(database.engine);
  const isMongo = database.engine === 'mongodb';
  const [sql, setSql] = useState(spec.initial);
  const [result, setResult] = useState<SqlQueryResult | null>(null);
  const [failure, setFailure] = useState<ApiFailure | null>(null);
  // mongodb 本地 JSON 校验错误（导出前的第一道拦截，不发请求）。
  const [syntaxError, setSyntaxError] = useState<string | null>(null);

  const queryMutation = useMutation({
    mutationFn: async (statement: string) => {
      const { data, error, response } = await api.POST('/api/databases/{id}/query', {
        params: { path: { id: database.id }, header: writeHeaders() },
        body: { sql: statement },
      });
      if (error) {
        throw new ApiRequestError(toApiFailure(error, response));
      }
      return data;
    },
    onSuccess: (data) => {
      setResult(data ?? null);
      setFailure(null);
    },
    onError: (error) => {
      setFailure(failureFromError(error));
      setResult(null);
    },
  });

  const run = () => {
    const statement = sql.trim();
    if (statement === '') {
      // 空语句不发起请求，但给出明确反馈（按钮禁用只挡鼠标路径，
      // Ctrl/⌘+Enter 仍会走到这里）。
      message.warning(spec.emptyHint);
      return;
    }
    if (isMongo) {
      // 命令文档本地形状校验（语法/对象/单命令键）：不合法不发请求，
      // 服务端守卫（白名单/管道阶段/上限）仍以 SQL_NOT_READONLY 展示。
      const invalid = validateMongoDocument(statement);
      if (invalid !== null) {
        setSyntaxError(invalid);
        return;
      }
    }
    setSyntaxError(null);
    if (queryMutation.isPending) {
      return;
    }
    setFailure(null);
    queryMutation.mutate(statement);
  };

  const target = consoleTarget(database);

  return (
    <Drawer
      open={open}
      onClose={onClose}
      width={920}
      title={`${spec.title} · ${database.name}`}
      extra={<Tag color="blue">只读</Tag>}
    >
      <Flex vertical gap={16}>
        <Alert
          type="info"
          showIcon
          message={
            database.engine === 'redis'
              ? '只读命令控制台（Redis）'
              : isMongo
                ? '只读 JSON 命令控制台（MongoDB）'
                : '只读 SQL 控制台'
          }
          description={
            <div>
              <div>{spec.allowedRule}</div>
              {isMongo ? (
                <div className="opsmind-admin-meta">
                  JSON 命令文档，仅 find / aggregate / count / distinct / estimatedDocumentCount。
                </div>
              ) : null}
              <div>
                写操作需审批：本控制台不会执行写操作，也不会创建审批；请在对话中提交操作，由系统按风险规则审批。
              </div>
              <div className="opsmind-admin-meta">连接：{target}</div>
            </div>
          }
        />

        {isMongo ? (
          <div className="opsmind-admin-meta">
            命令文档格式：
            {mongoCommandHints().map((hint) => (
              <div key={hint}>{hint}</div>
            ))}
          </div>
        ) : null}

        <Input.TextArea
          className="opsmind-sql-editor"
          value={sql}
          onChange={(event) => {
            setSql(event.target.value);
            if (syntaxError !== null) {
              setSyntaxError(null);
            }
          }}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
              event.preventDefault();
              run();
            }
          }}
          rows={8}
          spellCheck={false}
          placeholder={spec.placeholder}
        />

        <Flex justify="space-between" align="center" gap={12} wrap>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            Ctrl / ⌘ + Enter 执行；结果最多 1000 行 / 1MB。
          </Typography.Text>
          <Flex gap={8}>
            <Button
              icon={<ClearOutlined />}
              onClick={() => {
                setSql('');
                setResult(null);
                setFailure(null);
                setSyntaxError(null);
              }}
            >
              清空
            </Button>
            <Button
              type="primary"
              icon={<PlayCircleOutlined />}
              loading={queryMutation.isPending}
              disabled={!sql.trim()}
              onClick={run}
            >
              执行查询
            </Button>
          </Flex>
        </Flex>

        {syntaxError ? (
          <Alert type="error" showIcon message="命令文档校验未通过" description={syntaxError} />
        ) : null}

        {failure ? (
          failure.code === 'SQL_NOT_READONLY' ? (
            <ApiErrorAlert
              failure={failure}
              describe={(f) => describeQueryFailure(f, database.engine)}
            />
          ) : (
            <ApiErrorAlert
              failure={failure}
              describe={(f) => describeQueryFailure(f, database.engine)}
              onRetry={run}
            />
          )
        ) : null}

        {failure?.code === 'SQL_NOT_READONLY' ? (
          <Alert
            type="warning"
            showIcon
            message={READONLY_REJECTION_GUIDE.message}
            description={READONLY_REJECTION_GUIDE.description}
          />
        ) : null}

        {result ? (
          <div>
            <Typography.Title level={5} className="opsmind-admin-section-title">
              查询结果
            </Typography.Title>
            <QueryResultTable result={result} />
          </div>
        ) : (
          <Typography.Text type="secondary">执行查询后在此显示结果。</Typography.Text>
        )}
      </Flex>
    </Drawer>
  );
}
