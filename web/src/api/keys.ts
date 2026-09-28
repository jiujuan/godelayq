/* Query 键的唯一出处（§4.7）。失效管线与页面必须用同一套键，
   否则 invalidate 打不中缓存，表现为"推了事件但列表不动"。 */
import type { ListJobsQuery } from './types'

export const queryKeys = {
  stats: ['stats'] as const,
  /** 任务列表按筛选条件分页：整体失效用 jobsAll */
  jobs: (filters: ListJobsQuery, offset: number) => ['jobs', filters, offset] as const,
  jobsAll: ['jobs'] as const,
  job: (id: string) => ['job', id] as const,
  jobEvents: (id: string) => ['job-events', id] as const,
  eventsRecent: ['events'] as const,
  groups: ['groups'] as const,
  jobTypes: ['job-types'] as const,
  runtime: ['admin', 'runtime'] as const,
}
