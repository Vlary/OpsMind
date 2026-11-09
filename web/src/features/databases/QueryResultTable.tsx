import { Table, Tag, Typography } from 'antd';
import type { TableColumnsType } from 'antd';

import type { components } from '../../api/schema';
import { formatCell } from '../../components/admin/format';

type SqlQueryResult = components['schemas']['SqlQueryResult'];

interface QueryResultTableProps {
  result: SqlQueryResult;
}

export default function QueryResultTable({ result }: QueryResultTableProps) {
  const columns: TableColumnsType<unknown[]> = result.columns.map((column, cellIndex) => ({
    title: (
      <span>
        {column.name}
        {column.type ? (
          <Typography.Text
            type="secondary"
            style={{ marginLeft: 6, fontSize: 11, fontWeight: 400 }}
          >
            {column.type}
          </Typography.Text>
        ) : null}
      </span>
    ),
    key: `${cellIndex}-${column.name}`,
    ellipsis: true,
    render: (_value, record) => {
      const cell = record[cellIndex];
      if (cell === null || cell === undefined) {
        return <Typography.Text type="secondary">NULL</Typography.Text>;
      }
      return <span className="opsmind-admin-mono">{formatCell(cell)}</span>;
    },
  }));

  return (
    <div>
      <div className="opsmind-admin-toolbar" style={{ marginBottom: 12 }}>
        <span>返回 {result.row_count} 行</span>
        <span className="opsmind-admin-meta">耗时 {result.duration_ms} ms</span>
        {result.truncated ? <Tag color="gold">结果已截断（最多 1000 行 / 1MB）</Tag> : null}
      </div>
      <Table<unknown[]>
        rowKey={(_record, index) => String(index ?? 0)}
        columns={columns}
        dataSource={result.rows}
        size="small"
        scroll={{ x: 'max-content' }}
        locale={{ emptyText: '查询没有返回数据' }}
        pagination={{
          pageSize: 20,
          showSizeChanger: true,
          pageSizeOptions: [20, 50, 100],
          showTotal: (total) => `共 ${total} 行`,
        }}
      />
    </div>
  );
}
