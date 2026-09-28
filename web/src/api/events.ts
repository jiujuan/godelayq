/* 事件端点（§5.6）：都是内存缓冲，重启即清空——响应里的 note 字段就是这句话，
   UI 直接引用它，别在组件里另写一套说法。 */
import { request } from './client'
import type { EventsResponse } from './types'

/** 某个任务的最近事件，时间升序，详情页时间线的首屏 */
export function fetchJobEvents(id: string, limit?: number): Promise<EventsResponse> {
  return request<EventsResponse>(`/jobs/${id}/events`, { query: { limit } })
}

/** 全局最近事件，Monitor 刷新后回灌用 */
export function fetchRecentEvents(limit?: number): Promise<EventsResponse> {
  return request<EventsResponse>('/events', { query: { limit } })
}
