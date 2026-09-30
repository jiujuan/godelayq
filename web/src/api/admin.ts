/* 运维端点（§5.4）：ops 档专属，低档位调用会得到 403。
   前端的入口隐藏只是体验，真正拦下请求的是服务端 RBAC。 */
import { request } from './client'
import type { RuntimeInfo } from './types'

export function fetchRuntime(): Promise<RuntimeInfo> {
  return request<RuntimeInfo>('/admin/runtime')
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
