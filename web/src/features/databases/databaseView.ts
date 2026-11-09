// 数据库页与表单抽屉共用的纯展示逻辑（六期打磨 sprint 1）。
//
// 原本 databaseConnectionString 定义在 DatabasesPage.tsx（react-refresh 已用
// eslint-disable 放行导出），但表单抽屉（DatabaseFormDrawer）的只读态复制
// 需要同一组合串——两条路径必须逐字一致，否则「列表复制的连接串」与
// 「表单里复制的」不同源会误导用户。抽到此文件后页面与抽屉共用一份实现，
// node 环境可直接单测。

import type { components } from '../../api/schema';

type Database = components['schemas']['Database'];

/**
 * 连接信息组合串（契约 8.7 ②，FE-P2 六期）：`engine://host:port/db_name`；
 * sqlite 为 `sqlite://路径`；redis 为 `redis://host:port`（库名在 redis_db）；
 * mssql 指定实例时为 `mssql://host\instance/db_name`（无端口）。
 */
export function databaseConnectionString(database: Database): string {
  if (database.engine === 'sqlite') {
    return `sqlite://${database.sqlite_path}`;
  }
  if (database.engine === 'redis') {
    return `redis://${database.host}:${database.port}`;
  }
  if (database.engine === 'mssql' && database.instance_name) {
    return `mssql://${database.host}\\${database.instance_name}/${database.database_name}`;
  }
  return `${database.engine}://${database.host}:${database.port}/${database.database_name}`;
}
