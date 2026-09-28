/* 展示层的小工具：格式化时间与缩写 ID。
   放在这里而不是每个组件里各写一份，是因为短 ID 这件事有个不显然的前提。 */

/**
 * 任务 ID 是 UUIDv7：高 48 位是创建时的毫秒时间戳。
 * 于是一起提交的任务前几位完全相同（实测同一秒内的 6 条任务前 8 位一样），
 * 用前缀当"人眼可辨的短码"会在最需要区分它们的时候（批量操作回报）失败。
 * 尾段是随机位，所以取后 8 位。
 */
export function shortJobId(id: string): string {
  return id.length > 8 ? id.slice(-8) : id
}

function parsed(iso: string): Date | null {
  const value = new Date(iso)
  return Number.isNaN(value.getTime()) ? null : value
}

/** 列表用：日期 + 时间 */
export function formatDateTime(iso: string): string {
  const date = parsed(iso)
  return date ? date.toLocaleString() : iso
}

/** 事件流用：只看时分秒 */
export function formatClockTime(iso: string): string {
  const date = parsed(iso)
  return date ? date.toLocaleTimeString() : iso
}
