/* 与后端 api/dto.go、core 结构逐字段对齐的 TS 类型。
   手维护：字段名改动要同时改这里，评审时对照 api/dto.go 一遍。
   前端不解析 JWT，角色与有效期一律取后端响应里给出的值。 */

export type JobStatus = 'pending' | 'running' | 'success' | 'failed' | 'cancelled' | 'paused'

export type RoleName = 'viewer' | 'operator' | 'admin' | 'ops'

/** 任务视图（JobResponse） */
export interface Job {
  id: string
  name: string
  /**
   * 任务类型：决定这条任务跑什么（代码里注册的 Handler 名，或执行器档位的注册键）。
   * 空表示旧写法——请求体没带 type，名称兼作类型，此时按 name 查档位（见 api/dto.go 同一条口径）。
   */
  type?: string
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
  /**
   * 这条档位的来源：config 来自配置的 executors.commands（在页面上只读），
   * store 来自档位文件 executors.profiles_path（TASK-W06 那三个写端点管它）。
   */
  source: 'config' | 'store' | string
  /**
   * 能不能在页面上改它：web_enabled 且来源是 store 且它没被降级，三件事由后端一次算好。
   * 它只决定按钮显不显示——写请求的边界仍是服务端的 ops 档判定，隐藏按钮不是安全边界。
   */
  editable: boolean
  /**
   * 为真表示这条档位与 executors.commands 里的同名档位撞上了：看得见但没生效。
   * 同一个 key 因此可能在列表里出现两行（生效那条 + 降级这条），必须靠这个字段区分，
   * 渲染时的 :key 也要带上它，不然 Vue 会报重复键。
   */
  degraded: boolean
  /**
   * 这个档位指向的本机文件写法：workspace 内给相对写法，之外给绝对路径（D5）。
   * http 档位与"program 写成 PATH 程序名"的 binary 档位没有这个键。
   */
  path_display?: string
  /**
   * 这条是不是自由执行档位（executors.adhoc 打开时那四条内置档位）。
   * 普通档位是 false 而这个键照样给出：缺键分不出"普通档位"与"后端还不认识自由执行档位"。
   */
  adhoc?: boolean
  /**
   * 只有 adhoc 档位给：执行位置由任务的 payload 给出，这一项说的是那个输入框。
   * 界面以"有没有 location"作为唯一判据，不再自己认 exec.php 这类键名（TASK-N06/N07）。
   */
  location?: ExecutorProfileLocation
}

/** 自由执行档位的位置输入说明（api.ProfileLocation） */
export interface ExecutorProfileLocation {
  /** payload 顶层的键名：script | url */
  key: string
  /** 位置的形态：path | url，决定输入框的提示与前端那一层形态检查 */
  kind: 'path' | 'url' | string
  /** 输入框标题，含扩展名要求（后端已经拼好，前端只呈现） */
  label: string
  /** 内置四条恒为 true：没有位置就不知道跑什么 */
  required: boolean
  /** 这份部署对取值范围的说法（限定目录、主机名单、地址守卫） */
  hint: string
}

/** 档位的位置参数规则（ExecutorPositionalResponse） */
export interface ExecutorPositional {
  max: number
  /** 实际生效的正则：配置留空时是后端给的默认安全字符集 */
  pattern: string
}

/** 档位文件里的一条参数声明（core.ExecutorArgRecord） */
export interface ExecutorProfileArgRecord {
  name: string
  required?: boolean
  default?: string
  pattern?: string
  secret?: boolean
  allow_dash?: boolean
}

/** 档位文件里的位置参数声明（core.ExecutorPositionalRecord） */
export interface ExecutorProfilePositionalRecord {
  max: number
  pattern?: string
}

/**
 * 档位文件里的那条记录本身（core.ExecutorProfileRecord），也就是档位的**定义**。
 *
 * 与 ExecutorProfile 的分工：那一份说的是"这条档位现在的处境"（来源、能不能改、
 * 这台机器跑不跑得动），这一份说的是"它是什么"（用什么程序、跑哪个文件、允许哪些参数）。
 * 定义只在 GET /executors/profiles/:name 上给（TASK-W08 补的口，ops 档）。
 *
 * 除 name/kind 之外全部可选：后端每个字段都是 omitempty，没填的字段整个键不要出现，
 * 空串与"没写"在两处判据里不是一回事（例如 positional 与 http 的 body_mode）。
 */
export interface ExecutorProfileRecord {
  name: string
  kind: 'script' | 'binary' | 'http' | string
  runtime?: string
  script?: string
  program?: string
  fixed_args?: string[]
  args?: ExecutorProfileArgRecord[]
  args_render?: string[]
  positional?: ExecutorProfilePositionalRecord
  cwd?: string
  /**
   * 固定注入的环境变量。读端点**不回显取值**（只给 env_keys），
   * 写端点把"没带这个键"解释成"不改"，因此编辑一条档位时不要伪造一个空对象发回去。
   */
  env?: Record<string, string>
  env_allow?: string[]
  /** time.ParseDuration 的写法，如 90s / 5m / 1h30m */
  timeout?: string
  max_parallel?: number
  retry_on_exit?: number[]
  method?: string
  url_template?: string
  allowed_hosts?: string[]
  headers?: Record<string, string[]>
  header_allow?: string[]
  body?: string
  expect_status?: number[]
  capture_response?: boolean
  max_body_bytes?: number
  max_redirects?: number
  deny_private_ranges?: boolean
  /** 服务端事实，客户端填了也会被覆盖；表单不发这两个键 */
  created_at?: string
  updated_at?: string
}

/** GET /executors/profiles/:name（ExecutorProfileRecordResponse） */
export interface ExecutorProfileRecordResponse extends ExecutorProfileRecord {
  /** 这条档位固定的环境变量名（按字典序）。取值不外露，因此也不参与编辑回填。 */
  env_keys: string[]
}

/** DELETE /executors/profiles/:name 的响应（条数口径见 api 文档那一节） */
export interface ExecutorProfileDeleteResult {
  key: string
  name: string
  /** 本次新钉住的**待执行**任务数：不含正在执行的那条，也不删之前就已经暂停的 */
  paused_jobs: number
  /** 此刻仍在执行、因此一条都没被动的任务数 */
  running_jobs: number
  /** 删除之前就已经在 paused 上的条数 */
  already_paused_jobs: number
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
  /**
   * 档位的在线管理开没开（executors.web_enabled）。关闭时这个键也在并回 false：
   * 它是页面判断"能不能改档位"的唯一判据，缺键就等于让人猜后端认不认识这套管理。
   */
  web_enabled: boolean
  /**
   * 解释器名单（executors.runtime_allow，归一化补齐默认值之后的那一份），
   * 给档位表单的"解释器"下拉用。它与上面两项不同，执行器关闭时也给出——
   * 那两项说的是"现在能不能提交执行任务"，这一项是一份配置事实。
   */
  runtime_allow: string[]
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
  /** 给人看的标签：汉字、英文字母与数字，最长 64 个字符（判据在 core.ValidateJobName） */
  name: string
  /**
   * 跑什么：代码里注册的 Handler 名或执行器档位的注册键。
   * 省略是旧写法——那时 name 兼作类型，且 name 不套标签规则（设计文档 §D4 的兼容）。
   */
  type?: string
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
  /** 按类型筛（后端比的是 Job.HandlerKey()：带 type 用 type，旧写法回退名称） */
  type?: string
  /** 按名称标签筛：后端是精确匹配，不是子串 */
  name?: string
  /** 传空串=只看未分组；不传=不过滤（与后端 GetQuery 的区分一致） */
  group?: string
  limit?: number
  offset?: number
}

/**
 * 写操作台账的一行（GET /admin/audit 的 AuditItem）。字段与列名逐一对应，
 * 前端不重新解释后端语义：verdict 由状态码派生、route 存的是路由模板。
 */
export interface AuditEntry {
  time: string
  /** 账号名；machine 与匿名请求这里是空串，区分看 actor_kind */
  actor: string
  actor_kind: string
  /** 档位名，含 machine；未启用鉴权时是空串 */
  role: string
  action: string
  method: string
  /** gin 的路由模板（/api/v1/jobs/:id），不是原始 URL */
  route: string
  status: number
  /** 微秒整数；快请求可能是 0（时钟粒度），是量级参考不是精确计时 */
  latency_us: number
  verdict: AuditVerdict
  exec_verdict?: AuditExecVerdict
  /** 结论码；role_denied 那行存的是要求达到的档位名 */
  exec_reason_code?: string
  handler_key?: string
  profile?: string
  job_id?: string
  remote_ip?: string
  user_agent?: string
}

/** 后端由状态码折出的封闭集（api/audit.go 的 auditVerdict*），九个取值一个不少 */
export type AuditVerdict =
  | 'ok'
  | 'bad_request'
  | 'denied'
  | 'not_found'
  | 'conflict'
  | 'partial'
  | 'throttled'
  | 'error'
  | 'other'

export type AuditExecVerdict =
  | 'accepted'
  | 'role_denied'
  | 'profile_unavailable'
  | 'payload_rejected'
  | 'timeout_rejected'

/**
 * action 的全部合法取值 = 22 个动作 + 两个兜底（unmatched / other）。
 *
 * 与 api/audit.go 的 auditActions 映射表对照维护：那张表没有透出到任何端点，
 * 而下拉需要完整候选项，所以这是全项目唯一一份前端复制后端枚举的地方。
 * 两边不同步的后果是"下拉里少一个选项"或"选了之后恒 400"，
 * 改后端映射表时要同时改这里（后端侧的校验列表是 AuditActions()）。
 */
export const AUDIT_ACTIONS = [
  'auth.login',
  'auth.refresh',
  'auth.logout',
  'auth.ws_ticket',
  'job.create',
  'job.update',
  'job.cancel',
  'job.retry',
  'job.pause',
  'job.resume',
  'job.force_pause',
  'job.batch_create',
  'job.batch_op',
  'group.create',
  'group.update',
  'group.delete',
  'executor.profile_create',
  'executor.profile_update',
  'executor.profile_delete',
  'admin.scheduler_suspend',
  'admin.scheduler_unsuspend',
  'admin.events_clear',
  'unmatched',
  'other',
] as const

export type AuditAction = (typeof AUDIT_ACTIONS)[number]

/** AUDIT_VERDICTS 与后端 api.AuditVerdicts 同一份取值，顺序也照抄（校验用的就是它） */
export const AUDIT_VERDICTS: AuditVerdict[] = [
  'ok',
  'bad_request',
  'denied',
  'not_found',
  'conflict',
  'partial',
  'throttled',
  'error',
  'other',
]

/** GET /admin/audit 的查询参数，省略即取后端默认值（limit 50、offset 0、不过滤） */
export interface AuditQuery {
  actor?: string
  action?: AuditAction | ''
  verdict?: AuditVerdict | ''
  /** RFC3339，两端都含 */
  since?: string
  until?: string
  limit?: number
  offset?: number
}

/** GET /admin/audit（AuditResponse）：total 是匹配条件的总行数，count 是本次行数 */
export interface AuditListResponse {
  count: number
  total: number
  limit: number
  offset: number
  items: AuditEntry[]
}

