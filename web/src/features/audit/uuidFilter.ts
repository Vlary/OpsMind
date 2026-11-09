// 后端对 /api/audit 的 conversation_id / server_id 参数做 uuid 校验
// （audit_handlers.go），非法值返回 400 VALIDATION_FAILED；前端提交前先做
// 同样的轻校验，避免一次注定失败的请求。
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function isValidUUID(value: string): boolean {
  return UUID_PATTERN.test(value);
}

/**
 * 提交前的 UUID 归一化：去首尾空白 + 统一小写（第 4 轮打磨）。
 *
 * 校验正则带 /i（大写 UUID 合法），但后端存的资源 ID 一律小写
 * （store 层 uuid 生成），筛选按字面匹配：粘贴全大写 UUID 会得到
 * 0 行且无任何提示。归一化到小写后与库内同值记录命中。
 */
export function normalizeUUID(value: string): string {
  return value.trim().toLowerCase();
}
