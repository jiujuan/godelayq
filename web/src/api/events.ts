/* 事件端点（§5.6）：读的是持久化事件库还是进程内存缓冲，取决于这次部署装没装配事件库，
   后端把结论写在响应的 note 字段里。UI 直接引用 note，别在组件里复述某一句具体文本——
   那句话已经在两个取值之间分岔过一回，写死在组件里就等着过期。 */
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
