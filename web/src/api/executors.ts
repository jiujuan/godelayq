/* 执行器档位与执行结果（TASK-E07）。
 * 本卡只提供读取封装与类型：结果面板与档位下拉的页面渲染在 TASK-E18，
 * 表单按 args 生成与 secret 参数的收严在 TASK-E16。 */
import { request } from './client'
import type { ExecutorListResponse, JobResultQuery, JobResultResponse } from './types'

/**
 * 档位列表。执行器关闭时后端回 { enabled: false, web_enabled: false, profiles: [] }，不是错误；
 * 每一行带着来源与可编辑标记（TASK-W07），页面上要不要给编辑入口只看 editable 这一个布尔。
 */
export function listExecutors(): Promise<ExecutorListResponse> {
  return request<ExecutorListResponse>('/executors')
}

/**
 * 某次尝试的输出正文。
 * 每次调用都要重新取：这个端点不缓存（后端带 Cache-Control: no-store），
 * 结果面板按需展开时才调，别放进轮询里。
 */
export function getJobResult(id: string, query: JobResultQuery = {}): Promise<JobResultResponse> {
  // 逐字段列出：后端只认这四个参数，类型里长出别的东西时要在这里露头
  return request<JobResultResponse>(`/jobs/${id}/result`, {
    query: {
      attempt: query.attempt,
      stream: query.stream,
      from: query.from,
      max_bytes: query.max_bytes,
    },
  })
}
