<script setup lang="ts">
/* 任务表单（新建 / 编辑同一个组件）。
 *
 * 编辑模式的字段集比新建小一圈，这不是偷懒：PUT 的契约就只能改
 * trigger_at / payload / max_retries / timeout / group（名称与 cron 是任务身份的一部分，
 * 后端不接受修改）。把新建的表单直接搬进编辑会做出一张"填了却不生效"的假表单。
 *
 * 触发方式三选一在提交时塌缩成一个：后端看到 delay / trigger_at / cron_expr 只应有一个非空。
 *
 * 名称与类型是两件事（TASK-N02）：名称只是给人看的标签，类型才是"跑什么"——
 * 代码里注册的 Handler 名、`executors.commands` 声明的档位，或 `executors.adhoc` 那四条自由执行档位。
 * 选中档位类型时（TASK-E18 §3.3）payload 不再是手写 JSON，而是按 GET /executors 给出的
 * 档位声明生成的参数表单：必填、默认值、格式、允许的环境变量与请求头、能不能带请求体，
 * 全部来自那个响应——前端不复制第二份规则，副本迟早和配置漂移。
 * 自由执行档位再多一项：位置输入框（跑哪个文件、打到哪条地址），它的键名、标题与范围说明
 * 也全部来自那份响应里的 location（TASK-N06/N07），拒绝权在服务端。
 * 只有新建走这些表单：编辑模式下执行器任务的参数值在读取响应里是掩码后的（TASK-E16），
 * 回填进来的是一串 ***，把它当原值提交等于把凭据写成字面量，所以编辑照旧给 JSON 编辑器。 */
import { computed, reactive, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { useQuery } from '@tanstack/vue-query'
import { ExternalLink, Plus, X } from 'lucide-vue-next'
import CronPicker from './CronPicker.vue'
import PayloadEditor from './PayloadEditor.vue'
import UiButton from '../ui/UiButton.vue'
import UiInput from '../ui/UiInput.vue'
import UiSelect from '../ui/UiSelect.vue'
import { listExecutors } from '../../api/executors'
import { queryKeys } from '../../api/keys'
import { usePermission } from '../../composables/usePermission'
import { jobTypeOf } from '../../display'
import type {
  CreateJobRequest,
  ExecutorArgSpec,
  ExecutorProfile,
  ExecutorProfileLocation,
  Job,
  UpdateJobRequest,
} from '../../api/types'

type TriggerMode = 'delay' | 'at' | 'cron'
type Mode = 'create' | 'edit'

interface Props {
  mode: Mode
  /** 编辑模式下的原任务 */
  job?: Job
  jobTypes: string[]
  groupNames: string[]
  busy?: boolean
  /** 后端返回的错误文案，摆在表单顶部 */
  serverError?: string | null
}

const props = withDefaults(defineProps<Props>(), { busy: false, serverError: null })

const emit = defineEmits<{
  submit: [body: CreateJobRequest | UpdateJobRequest]
  cancel: []
  'create-group': [name: string]
}>()

const UNGROUPED = ''

/** 任务名称的字符集与长度：与 core.ValidateJobName（core/job_name.go）同一条规则。
 *  服务端仍然会判一遍并把 400 原文回显到表单顶部，这里只求少跑一次明显错误的提交。
 *  写成 \p{Script=Han} 而不是 \p{Han}：两者同源数据，后者 TypeScript 的正则检查器不认。 */
const JOB_NAME_RULE = /^[\p{Script=Han}A-Za-z0-9]{1,64}$/u

/**
 * 编辑模式下那条自由执行任务的位置：值在 payload 顶层（`script` 或 `url`）。
 * 只做展示用（只读），不回填成可编辑输入框——编辑模式给的是 JSON 编辑器，
 * 那一侧的既有理由见文件头；改位置由人在 JSON 里改。
 */
function locationFromPayload(payload: unknown): string {
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) return ''
  const record = payload as Record<string, unknown>
  for (const key of ['script', 'url']) {
    const value = record[key]
    if (typeof value === 'string' && value !== '') return value
  }
  return ''
}

const form = reactive({
  name: props.job?.name ?? '',
  jobType: props.job ? jobTypeOf(props.job) : '',
  location: locationFromPayload(props.job?.payload),
  triggerMode: (props.job ? 'at' : 'delay') as TriggerMode,
  delay: '10m',
  triggerAt: toLocalInput(props.job?.trigger_at),
  cronExpr: props.job?.cron_expr ?? '',
  payload: props.job?.payload === undefined ? '' : JSON.stringify(props.job.payload, null, 2),
  group: props.job?.group ?? UNGROUPED,
  timeout: props.job?.timeout ?? '',
  maxRetries: props.job?.max_retries ?? 0,
  retryDelay: '30s',
  isRepeat: props.job?.is_repeat ?? false,
})

const quickDelays = ['1m', '10m', '1h', '24h']

/* ---------------- 执行器档位模式（TASK-E18 §3.3） ---------------- */

/**
 * 档位表在表单内部取一次，不给两个宿主页面各传一层 props：
 * 新建抽屉与详情编辑抽屉用的是同一份配置事实，5 分钟内不重取（与任务类型下拉同一口径）。
 */
const executorsQuery = useQuery({
  queryKey: queryKeys.executors,
  queryFn: listExecutors,
  staleTime: 5 * 60_000,
})
const permission = usePermission()

/** 执行器关闭时按"没有任何档位"处理：那时连"提交档位任务"这件事的前提都不存在 */
const profiles = computed<ExecutorProfile[]>(() => {
  const list = executorsQuery.data.value
  return list?.enabled ? list.profiles : []
})

/** 全局超时上限（executors.max_timeout），后端不给就是空串 */
const maxTimeout = computed(() => executorsQuery.data.value?.max_timeout ?? '')

/** 提交执行器任务需要的档位；null 表示执行器关闭，没有这件事 */
const executorRequiredRole = computed(() => executorsQuery.data.value?.required_role ?? null)

/**
 * 当前身份能不能提交档位任务。
 *
 * 只决定这些名字在不在这个下拉里：入口隐藏只是体验，服务端 403 才是边界
 * （口径见 docs/design/web-console-design.md §5.7，判定实现是 api 的 gateExecutorSubmissionRole）。
 */
const canSubmitExecutor = computed(() => permission.canSubmitExecutorJobs(executorRequiredRole.value))

/**
 * 选中类型对应的档位声明；普通任务与普通模式下都是 null。
 *
 * 判"是不是档位"用的是 GET /executors 里有没有这个注册键，不再看名称前缀：
 * 名称与类型解耦之后（TASK-N02），标签里查不到任何档位，前缀判据会跟着一起失效。
 * 模式限制成 create 的理由写在文件头：编辑模式里响应给的参数值是掩码后的，
 * 回填等于把 *** 当原值提交。
 */
const profile = computed<ExecutorProfile | null>(() => {
  if (props.mode !== 'create') return null
  const key = form.jobType.trim()
  if (!key) return null
  return profiles.value.find((item) => item.key === key) ?? null
})

/**
 * 这条选中档位的位置输入说明（TASK-N06 的 location）。
 * 有它就意味着"跑哪个文件、打到哪里由这条任务给出"，表单换成一个位置输入框。
 */
const locationSpec = computed<ExecutorProfileLocation | null>(() => profile.value?.location ?? null)

/**
 * 位置输入框的举例。后端没把扩展名清单单独给前端（只在 location.label 里带了一句），
 * 所以这里举两种后缀，避免对 exec.php 说"就像 report.sh 那样填"这种错位示例；
 * 真正允许的扩展名写在标签括号里，范围说明在下方 hint。
 */
const locationPlaceholder = computed(() =>
  locationSpec.value?.kind === 'url'
    ? 'https://hook.example.com/orders/7'
    : 'D:\\work\\report.php 或 /srv/app/run.sh',
)

/**
 * 编辑模式那条任务的档位（只为只读展示位置与类型而查，不给参数表单）。
 * 后端禁改类型（api/handlers.go 的 UpdateJob），所以这一份只用来把 payload 的键名说清楚。
 */
const editProfile = computed<ExecutorProfile | null>(() => {
  if (props.mode !== 'edit') return null
  const key = form.jobType.trim()
  if (!key) return null
  return profiles.value.find((item) => item.key === key) ?? null
})

/** 选中档位时的"跑的是哪个文件 / 哪条地址"：后端给的写法原样展示，前端不拼路径 */
const profileTarget = computed(() => {
  const spec = profile.value ?? editProfile.value
  if (!spec) return null
  if (spec.location) return null
  return spec.path_display || spec.url || null
})

type EntryRow = { key: string; value: string }

const argValues = reactive<Record<string, string>>({})
const positionalValues = ref<string[]>([''])
const envRows = ref<EntryRow[]>([])
const headerRows = ref<EntryRow[]>([])
const bodyText = ref('')
/**
 * payload 里的 timeout 键。
 *
 * 执行器任务的生效超时来自这里（提交期由 Registry.EffectiveTimeout 合成），
 * 请求体顶层那个 timeout 字段对档位任务只是"不得超过档位声明值"的一道检查，
 * 所以表单把它挪到参数区里，免得填在一个不生效的地方。
 */
const execTimeout = ref('')

watch(
  profile,
  (next) => {
    // 换档位（包括切回普通任务）就把上一档的输入清空：留着会跟着提交进 payload
    for (const key of Object.keys(argValues)) delete argValues[key]
    positionalValues.value = ['']
    envRows.value = []
    headerRows.value = []
    bodyText.value = ''
    execTimeout.value = ''
    form.location = ''
    if (!next) return
    for (const spec of next.args) {
      // secret 参数连档位声明的默认值都不预填：那也是一个凭据，不该出现在输入框和 DOM 里
      if (spec.default && !spec.secret) argValues[spec.name] = spec.default
    }
  },
)

/**
 * 用档位给的 regex 判一次取值。
 *
 * 正则是 Go 的 RE2 写法，字符类、锚点、量词这些在 JS 里同义；遇到 JS 编译不了的写法就放行，
 * 让服务端按同一份声明去拒（api 的提交期校验才是边界）。前端这里只求少跑一次明显错误的提交。
 */
function patternMatches(pattern: string, value: string): boolean {
  if (!pattern) return true
  try {
    return new RegExp(`^(?:${pattern})$`).test(value)
  } catch {
    return true
  }
}

function argError(spec: ExecutorArgSpec): string | null {
  const value = (argValues[spec.name] ?? '').trim()
  if (value === '') return spec.required ? '这个参数是必填的' : null
  if (!patternMatches(spec.pattern, value)) return `取值不符合档位声明的格式：${spec.pattern}`
  return null
}

/** 与后端错误串里用的段名对齐（http 档位的同一批声明写在 params 下，其余在 args 下） */
const valueSection = computed(() => (profile.value?.kind === 'http' ? 'params' : 'args'))

const argErrors = computed(() => {
  const spec = profile.value
  if (!spec) return {}
  const errors: Record<string, string | null> = {}
  for (const arg of spec.args) errors[arg.name] = argError(arg)
  return errors
})

const positionalError = computed(() => {
  const spec = profile.value?.positional
  if (!spec) return null
  const filled = positionalValues.value.map((value) => value.trim()).filter((value) => value !== '')
  if (filled.length > spec.max) return `最多 ${spec.max} 个位置参数，现在填了 ${filled.length} 个`
  for (const value of filled) {
    if (!patternMatches(spec.pattern, value)) return `位置参数「${value}」不符合档位声明的格式：${spec.pattern}`
  }
  return null
})

function rowsError(rows: EntryRow[], what: string): string | null {
  const seen = new Set<string>()
  for (const row of rows) {
    const key = row.key.trim()
    if (!key && row.value === '') continue
    if (!key) return `有一${what}没选键名`
    if (seen.has(key)) return `${what}「${key}」填了两次`
    seen.add(key)
  }
  return null
}

const envError = computed(() => rowsError(envRows.value, '个环境变量'))
const headerError = computed(() => rowsError(headerRows.value, '个请求头'))

const bodyError = computed(() => {
  const mode = profile.value?.body_mode
  if (!mode || mode === 'none') return null
  const text = bodyText.value.trim()
  if (text === '') return null
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch (err) {
    return `请求体不是合法 JSON：${(err as Error).message}`
  }
  if (mode === 'json' && (typeof parsed !== 'object' || parsed === null)) {
    return '这个档位要求的请求体是 JSON 对象或数组'
  }
  return null
})

/** Go 的 duration 写法：一串"数字+单位"，单位只有这六种，没单位（30）不合法 */
const GO_DURATION = /^(?:\d+(?:ns|us|ms|s|m|h))+$/
const UNIT_MS: Record<string, number> = { ns: 1e-6, us: 1e-3, ms: 1, s: 1e3, m: 6e4, h: 3.6e6 }

function durationToMs(text: string): number | null {
  if (!GO_DURATION.test(text)) return null
  let total = 0
  for (const [, digits, unit] of text.matchAll(/(\d+)(ns|us|ms|s|m|h)/g)) {
    total += Number(digits) * UNIT_MS[unit]
  }
  return total
}

/**
 * 超时输入框的区间说明与拦下条件。
 *
 * 上限是 executors.max_timeout（响应里的 max_timeout）：超过它的请求值在执行侧会被夹到上限，
 * 也就是说"填 1 小时、实际 30 分钟被断"，这种静默落差比报个错更难排查，所以在前端就拦下。
 */
const timeoutCeilingMs = computed(() => durationToMs(maxTimeout.value.trim()))

const execTimeoutError = computed(() => {
  const text = execTimeout.value.trim()
  if (text === '') return null
  const ms = durationToMs(text)
  if (ms === null) return '写法如 90s、2m、1h30m：必须带单位（ns、us、ms、s、m、h）'
  const ceiling = timeoutCeilingMs.value
  if (ceiling !== null && ms > ceiling) {
    return `超过 executors.max_timeout（${maxTimeout.value}）：执行时会被夹到上限，这个值不会生效`
  }
  return null
})

/** 档位在这台机器现在的结论：不可用时不让人提交（服务端同样会拒，见 E16 §3.2） */
const profileUnavailable = computed(() => {
  const spec = profile.value
  return spec && !spec.runtime_ok ? spec.reason || '这台机器现在跑不了这个档位' : null
})

/**
 * 位置输入框的检查：必填 + 一条形态提示。
 *
 * 只做形态检查，范围与合法性一概由服务端判（路径落在哪些目录、地址打到哪些主机、
 * 文件在不在，前端都不知道也不该猜）；检查出来的问题写成一句提示，
 * 而拒绝权在 POST /jobs 那一侧，400 的原文会回到表单顶部。
 */
const locationError = computed(() => {
  const spec = locationSpec.value
  if (!spec) return null
  const value = form.location.trim()
  if (value === '') return spec.required ? `请填写${spec.label}` : null
  if (/[\u0000-\u001f\u007f]/.test(value)) {
    return '取值里不能有控制字符（路径里的空格是允许的）'
  }
  if (spec.kind === 'url' && !/^https?:\/\/\S+$/i.test(value)) {
    return '写法是完整地址，以 http:// 或 https:// 开头'
  }
  return null
})

/** 后端关于位置的拒绝同样落到这个输入框下（原文仍在表单顶部，那里才是排查依据） */
const locationDisplayError = computed(() => {
  const spec = locationSpec.value
  if (!spec) return null
  return locationError.value ?? serverErrorFor(`payload key "${spec.key}"`)
})

const executorErrors = computed(() => [
  profileUnavailable.value,
  locationError.value,
  ...Object.values(argErrors.value),
  positionalError.value,
  envError.value,
  headerError.value,
  bodyError.value,
  execTimeoutError.value,
])

/**
 * 后端 400 的错误按参数名落到对应输入项下面（§3.3 第 3 条）。
 *
 * 只补一句"这条被拒了"而不复述全文：details 的原文已经摆在表单顶部，
 * 抄一份到每个参数后面会把能填的格子刷成一片红。
 */
function serverErrorFor(needle: string): string | null {
  const text = props.serverError
  if (!text || !needle) return null
  return text.includes(needle) ? '后端拒了这条取值，原文见表单顶部的错误说明' : null
}

function argDisplayError(name: string): string | null {
  const local = argErrors.value[name] ?? null
  if (local) return local
  return serverErrorFor(`${valueSection.value}.${name}:`)
}

/** 参数输入框下的说明：格式与"这是凭据"两件事，都取自档位声明 */
function argHint(spec: ExecutorArgSpec): string | undefined {
  const parts: string[] = []
  if (spec.pattern) parts.push(`格式：${spec.pattern}`)
  if (spec.secret) parts.push('这是凭据：不回显，读取接口里也会掩码')
  else if (spec.default) parts.push(`已按档位默认值预填：${spec.default}`)
  return parts.length ? parts.join(' · ') : undefined
}

function dropPositional(index: number): void {
  positionalValues.value = positionalValues.value.filter((_, i) => i !== index)
  if (positionalValues.value.length === 0) positionalValues.value = ['']
}

/**
 * 编辑模式下的执行器任务：payload 里可能含有后端掩码后的值。
 *
 * 这里仍然给 JSON 编辑器而不是参数表单，是因为读取响应给的 secret 参数是 ***（TASK-E16），
 * 回填进输入框再保存等于把"星号"当成凭据原文提交。给一张空白参数表会让人以为参数丢了，
 * 也不对；所以留 JSON 原文 + 一句明确说明，改哪个键由人决定。
 *
 * 判据是"这条任务的类型在不在登记表里"（档位对象给的就是这件事），不是名称前缀。
 */
const isExecutorJobEdit = computed(() => props.mode === 'edit' && editProfile.value !== null)

/** 档位任务的 payload 组装；调用方保证此刻没有校验错误 */
function buildExecutorPayload(): Record<string, unknown> {
  const spec = profile.value
  if (!spec) return {}

  // 自由执行档位只带位置与可选超时：它没有参数声明，args/env/params/headers/body 一律不收
  // （收了的拒绝文本由服务端给，见 executor/adhoc.go 的 takeAdhocLocation）
  if (spec.location) {
    const payload: Record<string, unknown> = { [spec.location.key]: form.location.trim() }
    const timeout = execTimeout.value.trim()
    if (timeout !== '') payload.timeout = timeout
    return payload
  }

  const payload: Record<string, unknown> = {}
  const values: Record<string, unknown> = {}
  for (const arg of spec.args) {
    const value = (argValues[arg.name] ?? '').trim()
    if (value !== '') values[arg.name] = value
  }

  if (spec.kind === 'http') {
    if (Object.keys(values).length > 0) payload.params = values

    const headers: Record<string, string> = {}
    for (const row of headerRows.value) {
      const key = row.key.trim()
      if (key && row.value !== '') headers[key] = row.value
    }
    if (Object.keys(headers).length > 0) payload.headers = headers

    const body = bodyText.value.trim()
    if (body !== '') payload.body = JSON.parse(body)
  } else {
    const positional = positionalValues.value.map((value) => value.trim()).filter((value) => value !== '')
    if (positional.length > 0) values._positional = positional
    if (Object.keys(values).length > 0) payload.args = values

    const env: Record<string, string> = {}
    for (const row of envRows.value) {
      const key = row.key.trim()
      if (key && row.value !== '') env[key] = row.value
    }
    if (Object.keys(env).length > 0) payload.env = env
  }

  const timeout = execTimeout.value.trim()
  if (timeout !== '') payload.timeout = timeout
  return payload
}

/** 空框时贴在 payload 输入区里的最小示例。textarea 的 placeholder 支持换行，
    用户照着改两个键就能提交，比在框外写一段说明更省事。 */
const payloadPlaceholder = `{
  "order_id": "ORD-2024-001",
  "amount": "199.99",
  "notify": { "email": "ops@example.com" }
}`
const newGroupName = ref('')
const showingGroupInput = ref(false)

/** datetime-local 需要本地时区的 "YYYY-MM-DDTHH:mm"，不是 ISO 串 */
function toLocalInput(iso?: string): string {
  if (!iso) return ''
  const parsed = new Date(iso)
  if (Number.isNaN(parsed.getTime())) return ''
  const pad = (value: number) => String(value).padStart(2, '0')
  return `${parsed.getFullYear()}-${pad(parsed.getMonth() + 1)}-${pad(parsed.getDate())}` +
    `T${pad(parsed.getHours())}:${pad(parsed.getMinutes())}`
}

// cron 之外没有"重复执行"这件事：切走时把它压回 false，别让一个看不见的勾选跟着提交
watch(
  () => form.triggerMode,
  (mode) => {
    if (mode !== 'cron') form.isRepeat = false
  },
)

const payloadError = computed(() => {
  // 档位模式下 payload 文本框不参与提交（值由参数表单组装），那里的旧文本不该拦住提交按钮
  if (profile.value) return null
  const text = form.payload.trim()
  if (text === '') return null
  try {
    JSON.parse(text)
    return null
  } catch (err) {
    return `JSON 不合法：${(err as Error).message}`
  }
})

const triggerError = computed(() => {
  if (form.triggerMode === 'delay' && !form.delay.trim()) return '请填写相对延迟'
  if (form.triggerMode === 'at' && !form.triggerAt) return '请选择绝对时间'
  return null
})

/**
 * 名称与类型两项的缺判据。
 *
 * 名称是标签（可以与类型同名，也可以完全不相关），类型才是"跑什么"。
 * 两者在请求体里是两个字段（TASK-N02），这里也各判各的。
 */
const nameMissing = computed(() => props.mode === 'create' && form.name.trim() === '')
const typeMissing = computed(() => props.mode === 'create' && form.jobType.trim() === '')

const nameError = computed(() => {
  if (props.mode !== 'create') return null
  if (nameMissing.value) return '请填写任务名称'
  if (!JOB_NAME_RULE.test(form.name)) return '只能是汉字、英文字母与数字，最长 64 个字符（不能有空格、标点或符号）'
  return null
})

const canSubmit = computed(
  () =>
    !props.busy &&
    !nameError.value &&
    !typeMissing.value &&
    !triggerError.value &&
    !payloadError.value &&
    executorErrors.value.every((error) => error === null) &&
    (props.mode === 'edit' || form.triggerMode !== 'cron' || form.cronExpr.trim() !== ''),
)

/**
 * 任务类型下拉，三组（TASK-N07 §3.2）。
 *
 * 分组判据全部来自 GET /executors 的那份档位表：带 location 的是自由执行档位，
 * 在表里但没有 location 的是声明式档位，不在表里的就是代码里注册的普通任务类型。
 * 前端不再按 exec. 前缀判——名称与类型解耦之后那条前缀判据既不该出现在标签上，
 * 也不该出现在这里。
 *
 * 两条既有口径照旧：
 *   - 当前身份不够 required_role：档位两组整体不出现（入口体验，服务端 403 才是边界）；
 *   - 档位在这台机器不可用：留着可见但选不动，标签直接写原因——
 *     让人看见"配置里有但跑不了"比藏起来更有用，藏起来会让人以为配置没生效。
 */
const typeOptions = computed(() => {
  type Option = { value: string; label: string; disabled?: boolean; group?: string }
  const plain: Option[] = []
  const declared: Option[] = []
  const free: Option[] = []

  for (const name of props.jobTypes) {
    const spec = profiles.value.find((item) => item.key === name)
    if (!spec) {
      plain.push({ value: name, label: name, group: '普通任务' })
      continue
    }
    if (!canSubmitExecutor.value) continue
    const option: Option = {
      value: name,
      label: spec.runtime_ok ? name : `${name}（不可用：${spec.reason}）`,
      disabled: !spec.runtime_ok,
    }
    if (spec.location) free.push(option)
    else declared.push(option)
  }

  // 组名是界面语言，与后端的 kind/source 这些取值无关；空数组的组不给组名，
  // 于是 UiSelect 那边也不会渲染一个空分组。首选项"请选择任务类型"留在分组外平铺：
  // 它不是一个可提交的类型，放进 optgroup 会让人以为它也属于某一组。
  const groups: Option[] = [{ value: '', label: '请选择任务类型' }, ...plain]
  if (declared.length > 0) groups.push(...declared.map((item) => ({ ...item, group: '执行器档位' })))
  if (free.length > 0) groups.push(...free.map((item) => ({ ...item, group: '自由执行（位置由任务给出）' })))
  return groups
})

const hiddenExecutorHint = computed(() => {
  if (props.mode !== 'create') return null
  if (!executorRequiredRole.value || canSubmitExecutor.value) return null
  return `执行器档位与自由执行类型需要 ${executorRequiredRole.value} 及以上才能提交，当前身份的下拉里没有它们。`
})

const groupOptions = computed(() => [
  { value: UNGROUPED, label: '未分组' },
  ...props.groupNames.map((name) => ({ value: name, label: name })),
])

const parsedPayload = computed(() => {
  if (profile.value) return buildExecutorPayload()
  const text = form.payload.trim()
  return text === '' ? undefined : (JSON.parse(text) as unknown)
})

function addEnvRow(): void {
  envRows.value = [...envRows.value, { key: '', value: '' }]
}

function addHeaderRow(): void {
  headerRows.value = [...headerRows.value, { key: '', value: '' }]
}

function dropRow(rows: 'env' | 'header', index: number): void {
  if (rows === 'env') envRows.value = envRows.value.filter((_, i) => i !== index)
  else headerRows.value = headerRows.value.filter((_, i) => i !== index)
}

// UiSelect 没有占位符参数（它自己的注释说明了为什么），所以"还没选键名"要显式给一个空值选项
const envKeyOptions = computed(() => [
  { value: '', label: '请选择变量名' },
  ...(profile.value?.env_allow ?? []).map((name) => ({ value: name, label: name })),
])
const headerKeyOptions = computed(() => [
  { value: '', label: '请选择头名' },
  ...(profile.value?.header_allow ?? []).map((name) => ({ value: name, label: name })),
])

function submit(): void {
  if (!canSubmit.value) return
  const payload = parsedPayload.value
  const group = form.group || undefined

  if (props.mode === 'edit') {
    const body: UpdateJobRequest = {}
    if (form.triggerAt) {
      const parsed = new Date(form.triggerAt)
      if (!Number.isNaN(parsed.getTime())) body.trigger_at = parsed.toISOString()
    }
    if (payload !== undefined) body.payload = payload
    body.max_retries = form.maxRetries
    body.timeout = form.timeout.trim()
    // 指针语义：空串是明确的"取消分组"，必须显式带上而不是省略
    body.group = form.group
    emit('submit', body)
    return
  }

  const body: CreateJobRequest = {
    name: form.name.trim(),
    type: form.jobType.trim(),
    payload,
    group,
  }
  if (form.triggerMode === 'delay') body.delay = form.delay.trim()
  if (form.triggerMode === 'at') {
    const parsed = new Date(form.triggerAt)
    body.trigger_at = parsed.toISOString()
  }
  if (form.triggerMode === 'cron') {
    body.cron_expr = form.cronExpr.trim()
    body.is_repeat = form.isRepeat
  }
  const timeout = form.timeout.trim()
  if (timeout) body.timeout = timeout
  if (form.maxRetries > 0) {
    body.max_retries = form.maxRetries
    const retryDelay = form.retryDelay.trim()
    if (retryDelay) body.retry_delay = retryDelay
  }

  emit('submit', body)
}

function createGroup(): void {
  const name = newGroupName.value.trim()
  if (!name) return
  emit('create-group', name)
  form.group = name
  newGroupName.value = ''
  showingGroupInput.value = false
}
</script>

<template>
  <form class="flex flex-col gap-5" @submit.prevent="submit">
    <p
      v-if="serverError"
      role="alert"
      class="rounded-[var(--radius-control)] border border-[var(--color-status-failed)]
        bg-[#fef2f2] px-3 py-2 text-sm text-[var(--color-status-failed)]"
    >
      {{ serverError }}
    </p>

    <template v-if="mode === 'create'">
      <UiInput
        label="任务名称"
        :model-value="form.name"
        :disabled="busy"
        placeholder="每晚对账"
        :error="nameError"
        hint="名称只是给人看的标签，跑什么由下面的任务类型决定；只能用汉字、英文字母与数字，最长 64 个字符"
        @update:model-value="form.name = $event"
      />

      <UiSelect
        label="任务类型"
        :model-value="form.jobType"
        :options="typeOptions"
        :disabled="busy"
        :error="typeMissing ? '请选择任务类型' : null"
        :hint="
          hiddenExecutorHint ??
          (jobTypes.length === 0
            ? '后端没有注册任何任务类型，先加载 handler 或检查 job 目录配置'
            : undefined)
        "
        @update:model-value="form.jobType = $event"
      />

      <!-- 自由执行类型：跑哪个文件、打到哪条地址由这条任务给（TASK-N07 §3.1） -->
      <UiInput
        v-if="locationSpec"
        :label="`${locationSpec.label}（必填）`"
        :model-value="form.location"
        :disabled="busy"
        :placeholder="locationPlaceholder"
        :error="locationDisplayError"
        :hint="locationSpec.hint"
        @update:model-value="form.location = $event"
      />

      <!-- 声明式档位的位置写在档位定义里：这里只读展示后端给的写法 -->
      <div v-else-if="profileTarget" class="flex flex-col gap-1.5">
        <span class="text-sm font-medium text-[var(--color-text)]">这个档位跑的是</span>
        <p class="break-all text-sm text-[var(--color-text-muted)]"><code>{{ profileTarget }}</code></p>
        <p class="text-xs text-[var(--color-text-muted)]">位置来自档位定义，任务只给参数。</p>
      </div>
    </template>

    <div v-else class="flex flex-col gap-1.5">
      <span class="text-sm font-medium text-[var(--color-text)]">任务名称与类型</span>
      <p class="break-all text-sm text-[var(--color-text-muted)]">
        <code>{{ form.name }}</code>
        <!-- 旧写法任务的类型就是名称，重复打印一遍只会让人以为是两个不同的值 -->
        <template v-if="job?.type">
          <span class="mx-2">·</span>
          <code>{{ form.jobType }}</code>
        </template>
        <span v-if="!job?.type" class="ml-2 text-xs">（旧写法：名称就是类型）</span>
        <span class="ml-2 text-xs">（名称与类型不可修改，要换就新建一条）</span>
      </p>
      <p v-if="editProfile?.location" class="break-all text-sm text-[var(--color-text-muted)]">
        <span class="text-xs">{{ editProfile.location.label }}：</span>
        <code>{{ form.location || '这份 payload 里没有位置' }}</code>
      </p>
    </div>

    <fieldset class="flex flex-col gap-3">
      <legend class="mb-1 text-sm font-medium text-[var(--color-text)]">触发方式</legend>

      <div class="flex flex-wrap gap-4">
        <label v-if="mode === 'create'" class="flex items-center gap-1.5 text-sm">
          <input v-model="form.triggerMode" type="radio" value="delay" class="accent-[var(--color-primary)]" />
          相对延迟
        </label>
        <label class="flex items-center gap-1.5 text-sm">
          <input v-model="form.triggerMode" type="radio" value="at" class="accent-[var(--color-primary)]" />
          绝对时间
        </label>
        <label v-if="mode === 'create'" class="flex items-center gap-1.5 text-sm">
          <input v-model="form.triggerMode" type="radio" value="cron" class="accent-[var(--color-primary)]" />
          Cron 周期
        </label>
      </div>

      <div v-if="form.triggerMode === 'delay'" class="flex flex-col gap-2">
        <UiInput
          label="延迟时长"
          :model-value="form.delay"
          :disabled="busy"
          placeholder="10m / 1h30s"
          :error="triggerError"
          hint="Go duration 写法，如 30s、10m、1h30s"
          @update:model-value="form.delay = $event"
        />
        <div class="flex gap-1.5">
          <button
            v-for="value in quickDelays"
            :key="value"
            type="button"
            class="rounded-[var(--radius-pill)] border border-[var(--color-border)] px-2.5 py-0.5 text-xs
              transition-colors hover:border-[var(--color-primary)] hover:text-[var(--color-primary)]"
            :class="form.delay === value ? 'border-[var(--color-primary)] text-[var(--color-primary)]' : ''"
            @click="form.delay = value"
          >
            {{ value }}
          </button>
        </div>
      </div>

      <UiInput
        v-else-if="form.triggerMode === 'at'"
        label="触发时间"
        :model-value="form.triggerAt"
        :disabled="busy"
        :error="triggerError"
        @update:model-value="form.triggerAt = $event"
      />

      <CronPicker
        v-else
        :model-value="form.cronExpr"
        :disabled="busy"
        @update:model-value="form.cronExpr = $event"
      />

      <label v-if="mode === 'create' && form.triggerMode === 'cron'" class="flex items-center gap-2 text-sm">
        <input v-model="form.isRepeat" type="checkbox" class="accent-[var(--color-primary)]" />
        重复执行（勾选后按 cron 周期反复排期；不勾选则只触发一次）
      </label>
    </fieldset>

    <div class="flex items-center justify-between">
      <span class="text-sm font-medium text-[var(--color-text)]">
        {{ locationSpec ? '这条任务的位置与超时' : profile ? '档位参数' : 'payload 写什么' }}
      </span>
      <RouterLink
        :to="{ name: 'job-template' }"
        target="_blank"
        class="flex items-center gap-1 text-xs text-[var(--color-primary)] hover:underline"
      >
        <ExternalLink :size="12" aria-hidden="true" />
        打开任务模板与示例（新标签页）
      </RouterLink>
    </div>

    <!--
      选中档位类型时，payload 的结构由档位声明决定，所以这里按 GET /executors 的规格生成输入项：
      必填标记、默认值预填、格式提示都来自那份响应，前端不复制规则（TASK-E18 §3.3 第 1 条）。
      声明式档位组装出的顶层键固定是那几种（args / env / params / headers / body / timeout），
      自由执行档位只带位置与 timeout（位置键名由响应的 location.key 给，TASK-N06/N07），
      写错的键会在提交期被服务端整条拒掉，因此这里不给自由 JSON 入口。
    -->
    <template v-if="profile">
      <p v-if="profileUnavailable" role="alert" class="rounded-[var(--radius-control)] border
        border-[var(--color-status-failed)] bg-[#fef2f2] px-3 py-2 text-sm text-[var(--color-status-failed)]">
        这台机器现在跑不了这个档位：{{ profileUnavailable }}。提交同样会被服务端拒，请先补上脚本或程序。
      </p>

      <p v-if="locationSpec" class="text-xs text-[var(--color-text-muted)]">
        这条类型只收位置（payload 顶层的 <code>{{ locationSpec.key }}</code>）与单次超时：
        参数、环境变量与请求头都不接受，填了会在提交期被整条拒掉。
      </p>

      <template v-if="!locationSpec">
        <p v-if="profile.has_secret_args" class="text-xs text-[var(--color-text-muted)]">
          这个档位声明了 secret 参数：这类值在任务详情、列表与输出预览里都不会回显，
          留空就表示这次不提交它（档位自己声明的默认值仍由后端使用）。
        </p>

        <div class="flex flex-col gap-3">
          <UiInput
            v-for="arg in profile.args"
            :key="arg.name"
            :label="`${arg.name}${arg.required ? '（必填）' : ''} · ${valueSection === 'params' ? 'URL 参数' : '命令行参数'}`"
            :model-value="argValues[arg.name] ?? ''"
            :type="arg.secret ? 'password' : 'text'"
            :autocomplete="arg.secret ? 'new-password' : 'off'"
            :disabled="busy"
            :error="argDisplayError(arg.name)"
            :hint="argHint(arg)"
            @update:model-value="argValues[arg.name] = $event"
          />
          <p v-if="profile.args.length === 0" class="text-xs text-[var(--color-text-muted)]">
            这个档位没有具名参数。
          </p>
        </div>

        <!-- 位置参数只属于进程档位：http 的 payload 没有 args 这一层，后端会直接拒 -->
        <fieldset v-if="profile.positional && profile.kind !== 'http'" class="flex flex-col gap-2">
          <legend class="mb-1 text-sm font-medium text-[var(--color-text)]">
            位置参数（最多 {{ profile.positional.max }} 个）
          </legend>
          <div v-for="(value, index) in positionalValues" :key="index" class="flex items-end gap-2">
            <UiInput
              :model-value="value"
              :disabled="busy"
              :placeholder="`第 ${index + 1} 个`"
              @update:model-value="positionalValues[index] = $event"
            />
            <UiButton variant="ghost" size="sm" :disabled="busy" @click="dropPositional(index)">
              <X :size="14" aria-hidden="true" />
              移除
            </UiButton>
          </div>
          <div class="self-start">
            <UiButton
              variant="ghost"
              size="sm"
              :disabled="busy || positionalValues.length >= profile.positional.max"
              @click="positionalValues = [...positionalValues, '']"
            >
              <Plus :size="14" aria-hidden="true" />
              加一个位置参数
            </UiButton>
          </div>
          <p v-if="positionalError" class="text-xs text-[var(--color-status-failed)]">{{ positionalError }}</p>
          <p v-else class="text-xs text-[var(--color-text-muted)]">
            按顺序拼进命令行；格式：{{ profile.positional.pattern }}
          </p>
        </fieldset>

        <fieldset v-if="profile.kind === 'http'" class="flex flex-col gap-2">
          <legend class="mb-1 text-sm font-medium text-[var(--color-text)]">
            请求头（只列档位允许的：{{ profile.header_allow?.length ? profile.header_allow.join('、') : '无' }}）
          </legend>
          <div v-for="(row, index) in headerRows" :key="`h${index}`" class="flex items-end gap-2">
            <UiSelect
              :model-value="row.key"
              :options="headerKeyOptions"
              :disabled="busy"
              class="w-56"
              @update:model-value="row.key = $event"
            />
            <UiInput
              :model-value="row.value"
              :disabled="busy"
              placeholder="取值"
              @update:model-value="row.value = $event"
            />
            <UiButton variant="ghost" size="sm" :disabled="busy" @click="dropRow('header', index)">
              <X :size="14" aria-hidden="true" />
            </UiButton>
          </div>
          <div class="self-start">
            <UiButton variant="ghost" size="sm" :disabled="busy || !headerKeyOptions.length" @click="addHeaderRow">
              <Plus :size="14" aria-hidden="true" />
              加一个请求头
            </UiButton>
          </div>
          <p v-if="headerError" class="text-xs text-[var(--color-status-failed)]">{{ headerError }}</p>
        </fieldset>

        <PayloadEditor
          v-if="profile.kind === 'http' && profile.body_mode && profile.body_mode !== 'none'"
          v-model="bodyText"
          label="请求体"
          :disabled="busy"
          :error="bodyError"
          :hint="
            profile.body_mode === 'json'
              ? '这个档位要求 JSON 对象或数组，Content-Type 会由执行器按 json 模式补上'
              : '任意合法 JSON 值，按原样字节发送'
          "
        />

        <fieldset v-if="profile.kind !== 'http'" class="flex flex-col gap-2">
          <legend class="mb-1 text-sm font-medium text-[var(--color-text)]">
            环境变量（只列档位允许的：{{ profile.env_allow.length ? profile.env_allow.join('、') : '无' }}）
          </legend>
          <div v-for="(row, index) in envRows" :key="`e${index}`" class="flex items-end gap-2">
            <UiSelect
              :model-value="row.key"
              :options="envKeyOptions"
              :disabled="busy"
              class="w-56"
              @update:model-value="row.key = $event"
            />
            <UiInput
              :model-value="row.value"
              :disabled="busy"
              placeholder="取值"
              @update:model-value="row.value = $event"
            />
            <UiButton variant="ghost" size="sm" :disabled="busy" @click="dropRow('env', index)">
              <X :size="14" aria-hidden="true" />
            </UiButton>
          </div>
          <div class="self-start">
            <UiButton variant="ghost" size="sm" :disabled="busy || !envKeyOptions.length" @click="addEnvRow">
              <Plus :size="14" aria-hidden="true" />
              加一个环境变量
            </UiButton>
          </div>
          <p v-if="envError" class="text-xs text-[var(--color-status-failed)]">{{ envError }}</p>
        </fieldset>

      </template>

      <UiInput
        label="单次执行超时（写进 payload）"
        :model-value="execTimeout"
        :disabled="busy"
        :placeholder="`留空按档位默认 ${profile.timeout}`"
        :error="execTimeoutError"
        :hint="`生效区间：档位默认 ${profile.timeout}，全局上限 ${maxTimeout || '（后端没给）'}；超过上限的值会被夹到上限，所以这里直接拦住`"
        @update:model-value="execTimeout = $event"
      />
    </template>

    <template v-else>
      <PayloadEditor
        :model-value="form.payload"
        :disabled="busy"
        :error="payloadError"
        :placeholder="payloadPlaceholder"
        hint="合法 JSON，字段由该任务类型的 Handler 定义；留空表示不带 payload"
        @update:model-value="form.payload = $event"
      />

      <p v-if="isExecutorJobEdit" class="text-xs text-[var(--color-text-muted)]">
        这是执行器档位任务：上面里的 <code>***</code> 是后端掩码后的样子（档位声明为 secret 的那些参数），
        不是任务原本的值。要改参数就把它改成真实取值再保存；只改时间或分组可以原样留着，
        但保存后那份掩码会成为新的 payload。
      </p>
    </template>

    <div class="flex flex-col gap-2">
      <UiSelect
        label="分组"
        :model-value="form.group"
        :options="groupOptions"
        :disabled="busy"
        hint="分组只是标签，不要求先在注册表里存在"
        @update:model-value="form.group = $event"
      />
      <div v-if="!showingGroupInput" class="self-start">
        <UiButton variant="ghost" size="sm" :disabled="busy" @click="showingGroupInput = true">
          ＋ 新建分组
        </UiButton>
      </div>
      <div v-else class="flex items-end gap-2">
        <UiInput
          label="新分组名"
          :model-value="newGroupName"
          :disabled="busy"
          placeholder="nightly"
          @update:model-value="newGroupName = $event"
        />
        <UiButton size="sm" :disabled="!newGroupName.trim() || busy" @click="createGroup">创建并选中</UiButton>
        <UiButton variant="ghost" size="sm" @click="showingGroupInput = false">收起</UiButton>
      </div>
    </div>

    <div class="grid grid-cols-2 gap-3">
      <UiInput
        label="单次执行超时"
        :model-value="form.timeout"
        :disabled="busy"
        placeholder="留空不限制"
        hint="如 90s、2m、1h30m：单位只有 ns/us/ms/s/m/h，必须带单位（30 无效，也没有 d）；只有检查 ctx 的 Handler 才可能被超时中止"
        @update:model-value="form.timeout = $event"
      />
      <UiInput
        label="最大重试次数"
        type="text"
        :model-value="String(form.maxRetries)"
        :disabled="busy"
        hint="0 表示不重试"
        @update:model-value="form.maxRetries = Number($event.replace(/\D/g, '')) || 0"
      />
      <UiInput
        v-if="mode === 'create' && form.maxRetries > 0"
        label="重试间隔"
        :model-value="form.retryDelay"
        :disabled="busy"
        hint="指数退避的基准间隔"
        @update:model-value="form.retryDelay = $event"
      />
    </div>

    <div class="flex justify-end gap-2 pt-1">
      <UiButton variant="ghost" :disabled="busy" @click="emit('cancel')">取消</UiButton>
      <UiButton type="submit" :loading="busy" :disabled="!canSubmit">
        {{ mode === 'create' ? '创建任务' : '保存修改' }}
      </UiButton>
    </div>
  </form>
</template>
