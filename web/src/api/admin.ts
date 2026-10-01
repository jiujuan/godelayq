/* 运维端点（§5.4）：ops 档专属，低档位调用会得到 403。
   前端的入口隐藏只是体验，真正拦下请求的是服务端 RBAC。 */
import { request } from './client'
import type { AuditListResponse, AuditQuery, RuntimeInfo } from './types'

export function fetchRuntime(): Promise<RuntimeInfo> {
  return request<RuntimeInfo>('/admin/runtime')
}

/**
 * 写操作台账（TASK-S06 的 GET /admin/audit）。
 *
 * 未启用观测层时后端回 503，那是部署选择而不是故障，调用方按"没在记"呈现。
 * 逐字段列出查询参数而不是整体展开：与 listJobs 同一条理由——
 * 类型里长出后端不认的键时要在这里露头，而不是静默变成一个被忽略的查询串。
 */
export function fetchAudit(query: AuditQuery = {}): Promise<AuditListResponse> {
  return request<AuditListResponse>('/admin/audit', {
    query: {
      actor: query.actor,
      action: query.action,
      verdict: query.verdict,
      since: query.since,
      until: query.until,
      limit: query.limit,
      offset: query.offset,
    },
  })
}

/** 挂起调度循环（维护窗口）：执行中的任务照常跑完，堆与存储都不动 */
export function suspendScheduler(): Promise<{ suspended: boolean }> {
  return request<{ suspended: boolean }>('/admin/scheduler/suspend', { method: 'POST' })
}

export function unsuspendScheduler(): Promise<{ suspended: boolean }> {
  return request<{ suspended: boolean }>('/admin/scheduler/unsuspend', { method: 'POST' })
}

/** 清空的是事件内存缓冲：未装配持久化事件库时详情页时间线从当前时刻重新开始；
 * 装配了库时时间线读库，清空只影响这份缓冲，任务历史与库里的事件都不动 */
export function clearEventHistory(): Promise<{ cleared: number }> {
  return request<{ cleared: number }>('/admin/events', { method: 'DELETE' })
}
