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
  /** 最近一次执行的结论摘要；非执行器任务没有这个字段 */
  exec?: ExecMeta
  /**
   * 已经启动过的执行次数（含重试），与 GET /jobs/:id/result 的 attempt 可取范围同一条。
   * 后端恒给这个键（未执行过是 0），所以这里不加 ?：
   * "缺键"与"0 次"对尝试下拉是两件事。
   */
  attempts: number
}

/** 一次执行的结论摘要（core.ExecMeta）。输出正文不在这里，走 /jobs/:id/result */
export interface ExecMeta {
  kind: 'script' | 'binary' | 'http' | string
  /** 档位名，不含 exec. 前缀 */
  profile: string
  exit_code?: number
  /** 终止进程的信号名，正常退出为空 */
  signal?: string
  http_status?: number
  duration_ms: number
  out_bytes: number
  err_bytes: number
  /** 任一流超过落盘上限，后面的内容没被采集 */
  truncated?: boolean
  /** 参数或脚本本身的问题，重试不会变好 */
  permanent?: boolean
  /** 输出尾部预览，字节上限由 executors.output.inline_preview 控制 */
  preview?: string
  /** available=文件还在 | purged=已被清理 | 缺省=还没有产物可言 */
  artifact?: 'available' | 'purged' | ''
}

/** GET /jobs/:id/result（JobResultResponse） */
export interface JobResultResponse {
  job_id: string
  attempt: number
  stream: 'out' | 'err'
  /** false 表示产物文件已经不在了，不代表"这次执行没有输出" */
  found: boolean
  /** 文件里的总量，与本次返回的字节数可以不同 */
  size_bytes: number
  returned_bytes: number
  truncated: boolean
  /** 该次尝试的结论摘要；非执行器任务为 null */
  meta: ExecMeta | null
  content: string
  /**
   * 档位声明了 secret 参数时后端给的固定一句提醒：正文由脚本或对端产生，
   * 框架层的参数掩码管不到它把值打印出来。
   * 这是提醒，不是"已脱敏"的声明——面板原样显示它，别改写成另一种说法（TASK-E16 §3.4）。
   */
  redaction_note?: string
}

/** GET /jobs/:id/result 的查询参数，省略即取后端默认值 */
export interface JobResultQuery {
  /** 省略表示最近一次已结束的尝试 */
  attempt?: number
  stream?: 'out' | 'err'
  from?: 'tail' | 'head'
  max_bytes?: number
}

/** GET /executors 里的一条档位声明（ExecutorProfileResponse） */
export interface ExecutorProfile {
  /** 注册键，形如 exec nightly_report */
  key: string
  name: string
  kind: 'script' | 'binary' | 'http' | string
  /** 这台机器现在能不能跑（程序在不在 PATH、文件在不在） */
  runtime_ok: boolean
  /** runtime_ok=false 时的原因，可直接显示 */
  reason: string
  timeout: string
  max_parallel: number
  args: ExecutorArgSpec[]
  /** 位置参数规则（payload 的 args._positional）；档位没声明就没有这个键 */
  positional?: ExecutorPositional
  /** 允许 payload 注入的环境变量键名；取值一律不外露 */
  env_allow: string[]
  /** 档位声明了至少一个 secret 参数：读取接口会掩码，结果端点会升档位 */
  has_secret_args: boolean
  /**
   * 读这个档位输出时的建议起点：http 是 head（结论在开头），进程档位是 tail（结论在末尾）。
   * 取值来自后端 executor.PreferredResultDirection，前端不再按 kind 自己判。
   */
  preferred_result_direction: 'head' | 'tail' | string
  /** 仅 http 档位有 */
  method?: string
  /** 仅 http 档位有：payload 可以覆盖的请求头名（档位自己的固定头不在这里） */
  header_allow?: string[]
  /** 仅 http 档位有：请求体形态 json | raw | none，none 时表单不该给 body 输入区 */
  body_mode?: 'json' | 'raw' | 'none' | string
  /** 仅 http 档位有，给的是模板原文（含 {占位符}） */
  url?: string
}

/** 档位的位置参数规则（ExecutorPositionalResponse） */
export interface ExecutorPositional {
  max: number
  /** 实际生效的正则：配置留空时是后端给的默认安全字符集 */
  pattern: string
}

export interface ExecutorArgSpec {
  name: string
  required: boolean
  default: string
  pattern: string
  /** 值属于凭据：不进日志、不进响应，表单要按密码框处理 */
  secret: boolean
}

export interface ExecutorListResponse {
  enabled: boolean
  profiles: ExecutorProfile[]
  /** 提交执行器任务的最低档位；执行器关闭时是 null */
  required_role: string | null
  /**
   * payload 的 timeout 能填的上限（executors.max_timeout）。
   * 只在 enabled=true 时给出，缺键表示这份部署没有可提交的档位。
   */
  max_timeout?: string
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
  /** 调度总开关是否被挂起（维护窗口）。它放在 /stats 而不是 ops 专属的
      /admin/runtime，否则"任务为什么不出"这句提示只给改得动它的人看 */
  scheduling_suspended: boolean
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
  /**
   * 执行器池的四格（TASK-E13）。后端恒给这四个键，没建池时全是 0，
   * 所以页面要按 exec_workers===0 显示"未启用执行器"而不是四个 0。
   * running 仍然只算普通池，两池之和在 /stats 的 running 里。
   */
  exec_workers: number
  exec_queue_capacity: number
  exec_queue_length: number
  exec_running: number
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
