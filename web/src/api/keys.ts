/* Query 键的唯一出处（§4.7）。失效管线与页面必须用同一套键，
   否则 invalidate 打不中缓存，表现为"推了事件但列表不动"。 */
import type { AuditQuery, ListJobsQuery } from './types'

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
  /** 档位列表：只有启动期配置会改它，页面自己不带轮询 */
  executors: ['executors'] as const,
  /**
   * 单条档位的定义（GET /executors/profiles/:name）。
   * 只在编辑那条档位时取，按名字分键：连续编辑两条不会共用一份草稿来源。
   */
  executorProfile: (name: string) => ['executor-profile', name] as const,
  /**
   * 写操作台账按筛选条件分页：整体失效用 auditAll。
   * 与 jobs 同一形状（filters + offset），不发明第二套分页键。
   */
  audit: (filters: AuditQuery, offset: number) => ['audit', filters, offset] as const,
  auditAll: ['audit'] as const,
  /**
   * 某次尝试的输出正文。
   * attempt / stream / from / maxBytes 都要进键：结果面板的四个控件改的就是这四元组，
   * 少一个维度就会把"读头部 4KB"的缓存当成"读尾部 4KB"的用，切标签页时显示错内容。
   */
  jobResult: (
    id: string,
    attempt: number,
    stream: 'out' | 'err',
    from: 'head' | 'tail',
    maxBytes: number,
  ) => ['job-result', id, attempt, stream, from, maxBytes] as const,
  /** 某条任务全部结果查询的前缀，供事件失效用（TASK-E18 §3.1 第 3 条） */
  jobResultAll: (id: string) => ['job-result', id] as const,
}
