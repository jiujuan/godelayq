/* 与后端 api/dto.go、core 结构逐字段对齐的 TS 类型。
   手维护：字段名改动要同时改这里，评审时对照 api/dto.go 一遍。
   前端不解析 JWT，角色与有效期一律取后端响应里给出的值。 */

export type JobStatus = 'pending' | 'running' | 'success' | 'failed' | 'cancelled' | 'paused'

export type RoleName = 'viewer' | 'operator' | 'admin' | 'ops'

/** 任务视图（JobResponse） */
export interface Job {
  id: string
  name: string
  status: JobStatus
  group?: string
  trigger_at: string
  payload?: unknown
  retry_count: number
  max_retries: number
  /** 单次执行超时，缺省表示不限制 */
  timeout?: string
  is_repeat: boolean
  cron_expr?: string
  created_at: string
  updated_at: string
  /** 后端算好的剩余时长；仅 pending 有值 */
  next_run_in?: string
}

export interface ListJobsResponse {
  total: number
  items: Job[]
}

/** GET /stats（StatsResponse） */
export interface Stats {
  pending: number
  running: number
  /** 暂停中的任务：既不在 pending 也不在 heap_size 里 */
  paused: number
  completed: number
  failed: number
  heap_size: number
  uptime: string
}

export interface ErrorResponse {
  code: number
  message: string
  details?: string
}

/** POST /jobs 请求体（CreateJobRequest） */
export interface CreateJobRequest {
  name: string
  delay?: string
  trigger_at?: string
  cron_expr?: string
  payload?: unknown
  is_repeat?: boolean
  group?: string
  timeout?: string
  max_retries?: number
  retry_delay?: string
}

/** PUT /jobs/:id 请求体（UpdateJobRequest）：字段省略即不改 */
export interface UpdateJobRequest {
  trigger_at?: string
  payload?: unknown
  max_retries?: number
  timeout?: string
  /** 传空串是明确的"取消分组"，与省略不同 */
  group?: string
}

export interface BatchItemError {
  index: number
  code: number
  message: string
  details?: string
}

/** POST /jobs/batch 的混合结果（207） */
export interface BatchCreateJobsResponse {
  succeeded: number
  failed: number
  items: Job[]
  errors: BatchItemError[]
}

export type BatchJobAction = 'cancel' | 'pause' | 'force-pause' | 'resume' | 'move'

export interface BatchJobOpsRequest {
  action: BatchJobAction
  ids: string[]
  /** action=move 必填，空串表示取消分组 */
  group?: string
}

export interface BatchOpsItemError {
  id: string
  code: number
  message: string
}

export interface BatchJobOpsResponse {
  action: BatchJobAction
  succeeded: number
  failed: number
  items?: Job[]
  errors: BatchOpsItemError[]
}

/** 分组（GroupResponse：注册表条目 + 实时计数） */
export interface Group {
  name: string
  description?: string
  color?: string
  created_at: string
  updated_at: string
  job_count: number
  paused_count: number
  /** false 表示组名只出现在任务标签上，注册表里没有对应条目 */
  registered: boolean
}

export interface CreateGroupRequest {
  name: string
  description?: string
  color?: string
}

export interface UpdateGroupRequest {
  name?: string
  description?: string
  color?: string
}

/** 事件（core.Event）。status 是数字枚举，与 JobStatus 的取值顺序一致 */
export interface JobEvent {
  type: string
  job_id: string
  job_name: string
  status: number
  timestamp: string
  data?: unknown
  metadata?: Record<string, unknown>
}

export interface EventsResponse {
  job_id?: string
  count: number
  items: JobEvent[]
  note: string
}

export interface UserInfo {
  name: string
  role: RoleName
}

export interface TokenSession {
  access_token: string
  refresh_token: string
  token_type: string
  expires_at: string
  user: UserInfo
}

export interface WhoAmI extends UserInfo {
  expires_at: string
}

export interface WsTicket {
  ticket: string
  expires_in_seconds: number
}

export interface SchedulerRuntime {
  started: boolean
  workers: number
  queue_capacity: number
  queue_length: number
  running: number
  heap_size: number
  suspended: boolean
  force_pause_pending: number
}

export interface EventHistoryStats {
  jobs: number
  events: number
  global_capacity: number
  per_job_capacity: number
  job_capacity: number
}

/** GET /admin/runtime（RuntimeResponse） */
export interface RuntimeInfo {
  uptime: string
  started_at: string
  scheduler: SchedulerRuntime
  event_history: EventHistoryStats
  scheduling_suspended_note?: string
}

export interface ListJobsQuery {
  status?: JobStatus
  name?: string
  /** 传空串=只看未分组；不传=不过滤（与后端 GetQuery 的区分一致） */
  group?: string
  limit?: number
  offset?: number
}
