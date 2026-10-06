/* 任务相关端点（§5.3/§5.4）。
 * 动作类端点的返回值不统一是后端的既有事实：cancel 是 204 无体，
 * pause/resume/force-pause/retry 回任务快照，batch-ops 回 207 混合结果。
 * 前端照着写，不做"统一成都返回 Job"的美化——那要动已交付的契约。 */
import { request } from './client'
import type {
  BatchCreateJobsResponse,
  BatchJobAction,
  BatchJobOpsResponse,
  CreateJobRequest,
  Job,
  ListJobsQuery,
  ListJobsResponse,
  UpdateJobRequest,
} from './types'

export function listJobs(query: ListJobsQuery = {}): Promise<ListJobsResponse> {
  // 逐字段列出而不是整体展开：后端只认这几个参数，
  // 类型里长出别的东西时要在这里露头，而不是静默变成一个被忽略的查询串
  return request<ListJobsResponse>('/jobs', {
    query: {
      status: query.status,
      type: query.type,
      name: query.name,
      group: query.group,
      limit: query.limit,
      offset: query.offset,
    },
  })
}

export function fetchJob(id: string): Promise<Job> {
  return request<Job>(`/jobs/${id}`)
}

export function fetchJobTypes(): Promise<string[]> {
  return request<{ types: string[] }>('/job-types').then((resp) => resp.types)
}

export function createJob(body: CreateJobRequest): Promise<Job> {
  return request<Job>('/jobs', { method: 'POST', body })
}

export function batchCreateJobs(body: CreateJobRequest[]): Promise<BatchCreateJobsResponse> {
  return request<BatchCreateJobsResponse>('/jobs/batch', { method: 'POST', body: body })
}

export function updateJob(id: string, body: UpdateJobRequest): Promise<Job> {
  return request<Job>(`/jobs/${id}`, { method: 'PUT', body })
}

/** 取消即删除记录：与"暂停保留快照"是两种后果，所以它是 DELETE 而不是 pause */
export function cancelJob(id: string): Promise<void> {
  return request<void>(`/jobs/${id}/cancel`, { method: 'POST' })
}

/** 只接受 failed 状态的任务，其它状态 404（后端按"失败任务找不到"处理） */
export function retryJob(id: string): Promise<Job> {
  return request<Job>(`/jobs/${id}/retry`, { method: 'POST' })
}

export function pauseJob(id: string): Promise<Job> {
  return request<Job>(`/jobs/${id}/pause`, { method: 'POST' })
}

export function resumeJob(id: string): Promise<Job> {
  return request<Job>(`/jobs/${id}/resume`, { method: 'POST' })
}

/** admin 档：中止当前 attempt 并停在 paused，200 只代表"中止已发起" */
export function forcePauseJob(id: string): Promise<Job> {
  return request<Job>(`/jobs/${id}/force-pause`, { method: 'POST' })
}

/** 整批一个动作，逐条独立执行，混合结果用 207 表达（≤100 条） */
export function batchJobOps(
  action: BatchJobAction,
  ids: string[],
  group?: string,
): Promise<BatchJobOpsResponse> {
  return request<BatchJobOpsResponse>('/jobs/batch-ops', {
    method: 'POST',
    body: group === undefined ? { action, ids } : { action, ids, group },
  })
}
