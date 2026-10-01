<script setup lang="ts">
/* 档位的定义表单（TASK-W08 §3.3）。
 *
 * 这张表单填的是"这条档位是什么"，不是"它现在能不能跑"：探测失败不阻止保存（设计文档 §5.3），
 * 保存之后的 available / reason 由左侧列表按 GET /executors 的结论回显。
 *
 * 字段的可见范围照抄 executor 的那条判据（checkFieldsMatchKind）：
 * runtime / script 只属于 script，program / fixed_args 只属于 binary，
 * cwd / args_render / env / env_allow / retry_on_exit 属于两种进程档位，
 * http 那一组只属于 http。发给后端一个"不属于当前档位的字段"必然 400，
 * 所以这里按档位类型组装载荷，而不是把草稿整份铺开。
 *
 * kind / script / program 三条改不了（决策 D7），编辑时它们锁在原地：
 * 要换内核就删了这条重建，界面不给这个口子，后端也会 400。
 */
import { computed, reactive, ref, watch } from 'vue'
import { Plus, X } from 'lucide-vue-next'
import UiButton from '../ui/UiButton.vue'
import UiInput from '../ui/UiInput.vue'
import UiSelect from '../ui/UiSelect.vue'
import type {
  ExecutorProfileArgRecord,
  ExecutorProfileRecord,
  ExecutorProfileRecordResponse,
} from '../../api/types'

type ProfileKind = 'script' | 'binary' | 'http'

interface Props {
  /** null = 新建；编辑由视图层取到定义记录后传进来 */
  record: ExecutorProfileRecordResponse | null
  /** 定义还在取：先别让人对一张空表单按保存 */
  loading?: boolean
  /** 解释器名单（GET /executors 顶层 runtime_allow）；空数组表示后端没给，退化成文本输入 */
  runtimeAllow: string[]
  /** 单次执行超时的上限（executors.max_timeout），档位声明的 timeout 也受它约束 */
  maxTimeout?: string
  canWrite: boolean
  canDelete: boolean
  writeHint: string | null
  busy?: boolean
}

const props = withDefaults(defineProps<Props>(), {
  loading: false,
  maxTimeout: '',
  busy: false,
})

const emit = defineEmits<{
  save: [payload: ExecutorProfileRecord]
  remove: [name: string]
}>()

/** 与 core.profileNamePattern、executor.profileNamePattern 同一条正则 */
const NAME_PATTERN = /^[A-Za-z0-9_-]{1,64}$/
/** executor.argNamePattern：具名参数只许小写字母、数字、下划线 */
const ARG_NAME_PATTERN = /^[a-z0-9_]+$/
/** executor.envNamePattern：环境变量名按惯例大写 */
const ENV_NAME_PATTERN = /^[A-Z0-9_]+$/
/** Go 的 duration 写法，与 JobForm 同一份规则：一串"数字+单位"，没有单位（30）不合法 */
const GO_DURATION = /^(?:\d+(?:ns|us|ms|s|m|h))+$/
const UNIT_MS: Record<string, number> = { ns: 1e-6, us: 1e-3, ms: 1, s: 1e3, m: 6e4, h: 3.6e6 }

const KIND_OPTIONS: { value: ProfileKind; label: string; note: string }[] = [
  { value: 'script', label: 'script', note: '解释器 + 一个脚本文件' },
  { value: 'binary', label: 'binary', note: 'PATH 里的程序名，或一个可执行文件' },
  { value: 'http', label: 'http', note: '一条固定下来的 HTTP 请求' },
]

interface EnvRow {
  key: string
  value: string
}
interface ArgRow {
  name: string
  required: boolean
  default: string
  pattern: string
  secret: boolean
  allow_dash: boolean
}

const draft = reactive({
  name: '',
  kind: 'script' as ProfileKind,
  runtime: '',
  script: '',
  program: '',
  fixedArgs: '',
  cwd: '',
  argsRender: '',
  envAllow: '',
  envRows: [] as EnvRow[],
  clearEnv: false,
  timeout: '',
  maxParallel: '',
  retryOnExit: '',
  // http 那一组
  method: '',
  urlTemplate: '',
  allowedHosts: '',
  headers: '',
  headerAllow: '',
  body: '',
  expectStatus: '',
  captureResponse: false,
  maxBodyBytes: '',
  maxRedirects: '',
  denyPrivate: '' as '' | 'true' | 'false',
})

const argRows = ref<ArgRow[]>([])

/** 载入时的草稿快照，编辑态用它判"有没有改过"（不比较载荷：那要先把两份都组装一遍） */
const baseline = ref('')

/** 加载进来的那份定义：判"这条是不是已存在"看它，不看 record 的两态语义 */
const isCreate = computed(() => props.record === null)
const isHttp = computed(() => draft.kind === 'http')
const isScript = computed(() => draft.kind === 'script')
const isBinary = computed(() => draft.kind === 'binary')

/** 档位已有的固定环境变量名（只有名字，取值后端不外露） */
const existingEnvKeys = computed(() => props.record?.env_keys ?? [])

watch(
  () => props.record,
  (record) => {
    resetDraft(record)
  },
  { immediate: true },
)

/** 多行文本按行拆开，空行丢掉；与"一行一个参数"的界面说法一致 */
function lines(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '')
}

/** 逗号或空格分隔的一份名单（env_allow / allowed_hosts / header_allow） */
function list(text: string): string[] {
  return text
    .split(/[,\s]+/)
    .map((item) => item.trim())
    .filter((item) => item !== '')
}

function intList(text: string): number[] {
  return list(text)
    .map((item) => Number(item))
    .filter((value) => Number.isInteger(value))
}

function headersToText(headers: Record<string, string[]> | undefined): string {
  if (!headers) return ''
  return Object.entries(headers)
    .map(([key, values]) => `${key}: ${values.join(', ')}`)
    .join('\n')
}

/** "Name: v1, v2" 一行一头；值可以重复同名，那种行会并到同一个键的列表里 */
function textToHeaders(text: string): Record<string, string[]> | undefined {
  const result: Record<string, string[]> = {}
  for (const line of lines(text)) {
    const separator = line.indexOf(':')
    if (separator <= 0) continue
    const key = line.slice(0, separator).trim()
    const values = line
      .slice(separator + 1)
      .split(',')
      .map((item) => item.trim())
      .filter((item) => item !== '')
    if (key === '' || values.length === 0) continue
    result[key] = [...(result[key] ?? []), ...values]
  }
  return Object.keys(result).length > 0 ? result : undefined
}

function emptyDraft() {
  draft.name = ''
  draft.kind = 'script'
  draft.runtime = ''
  draft.script = ''
  draft.program = ''
  draft.fixedArgs = ''
  draft.cwd = ''
  draft.argsRender = ''
  draft.envAllow = ''
  draft.envRows = []
  draft.clearEnv = false
  draft.timeout = ''
  draft.maxParallel = ''
  draft.retryOnExit = ''
  draft.method = ''
  draft.urlTemplate = ''
  draft.allowedHosts = ''
  draft.headers = ''
  draft.headerAllow = ''
  draft.body = ''
  draft.expectStatus = ''
  draft.captureResponse = false
  draft.maxBodyBytes = ''
  draft.maxRedirects = ''
  draft.denyPrivate = ''
  argRows.value = []
}

function resetDraft(record: ExecutorProfileRecordResponse | null): void {
  emptyDraft()
  if (record) {
    draft.name = record.name
    draft.kind = (record.kind as ProfileKind) || 'script'
    draft.runtime = record.runtime ?? ''
    draft.script = record.script ?? ''
    draft.program = record.program ?? ''
    draft.fixedArgs = (record.fixed_args ?? []).join('\n')
    draft.cwd = record.cwd ?? ''
    draft.argsRender = (record.args_render ?? []).join('\n')
    draft.envAllow = (record.env_allow ?? []).join(' ')
    draft.timeout = record.timeout ?? ''
    draft.maxParallel = record.max_parallel ? String(record.max_parallel) : ''
    draft.retryOnExit = (record.retry_on_exit ?? []).join(' ')
    draft.method = record.method ?? ''
    draft.urlTemplate = record.url_template ?? ''
    draft.allowedHosts = (record.allowed_hosts ?? []).join(' ')
    draft.headers = headersToText(record.headers)
    draft.headerAllow = (record.header_allow ?? []).join(' ')
    draft.body = record.body ?? ''
    draft.expectStatus = (record.expect_status ?? []).join(' ')
    draft.captureResponse = record.capture_response ?? false
    draft.maxBodyBytes = record.max_body_bytes ? String(record.max_body_bytes) : ''
    draft.maxRedirects = record.max_redirects ? String(record.max_redirects) : ''
    // Go 侧是三态（nil 等于 true），界面只在显式 false 时记住一个值
    draft.denyPrivate = record.deny_private_ranges === false ? 'false' : ''
    argRows.value = (record.args ?? []).map((arg) => ({
      name: arg.name,
      required: arg.required ?? false,
      default: arg.default ?? '',
      pattern: arg.pattern ?? '',
      secret: arg.secret ?? false,
      allow_dash: arg.allow_dash ?? false,
    }))
  }
  baseline.value = snapshot()
}

function snapshot(): string {
  return JSON.stringify({ draft, argRows: argRows.value })
}

const dirty = computed(() => !isCreate.value && snapshot() !== baseline.value)

const runtimeOptions = computed(() =>
  props.runtimeAllow.map((name) => ({ value: name, label: name })),
)
/** 名单为空（后端没给或这份部署读不到）就退化成自由文本，见卡 §3.3 的退化说明 */
const hasRuntimeList = computed(() => runtimeOptions.value.length > 0)

/* ---------------- 校验 ---------------- */

const nameError = computed(() => {
  if (draft.name === '') return '档位名不能为空'
  return NAME_PATTERN.test(draft.name) ? null : '只允许 1–64 位字母、数字、下划线与短横线'
})

/** 每种档位的那个必填项（与 checkFieldsMatchKind 末尾那三条 switch 同口径） */
const targetError = computed(() => {
  if (isScript.value) {
    if (draft.script.trim() === '') return 'script 档位必须给出脚本路径'
    if (draft.runtime.trim() === '') return 'script 档位必须选择解释器'
  }
  if (isBinary.value && draft.program.trim() === '') {
    return 'binary 档位必须给出程序名或可执行文件路径'
  }
  if (isHttp.value && draft.urlTemplate.trim() === '') {
    return 'http 档位必须给出 URL 模板'
  }
  return null
})

const timeoutError = computed(() => {
  const text = draft.timeout.trim()
  if (text === '') return null
  if (!GO_DURATION.test(text)) return '写法如 90s、2m、1h30m：单位只有 ns/us/ms/s/m/h，必须带单位'
  let total = 0
  for (const [, digits, unit] of text.matchAll(/(\d+)(ns|us|ms|s|m|h)/g)) {
    total += Number(digits) * UNIT_MS[unit]
  }
  const ceiling = durationMs(props.maxTimeout.trim())
  // 档位声明的超时超过全局上限是后端的一条拒绝（effectiveTimeout），不是夹取
  if (ceiling !== null && total > ceiling) {
    return `超过 executors.max_timeout（${props.maxTimeout}）：后端会直接拒掉这条档位`
  }
  return null
})

function durationMs(text: string): number | null {
  if (text === '' || !GO_DURATION.test(text)) return null
  let total = 0
  for (const [, digits, unit] of text.matchAll(/(\d+)(ns|us|ms|s|m|h)/g)) {
    total += Number(digits) * UNIT_MS[unit]
  }
  return total
}

const integerErrors = computed(() => {
  const checks: { field: keyof typeof draft; label: string }[] = [
    { field: 'maxParallel', label: 'max_parallel' },
    { field: 'maxBodyBytes', label: 'max_body_bytes' },
    { field: 'maxRedirects', label: 'max_redirects' },
  ]
  for (const { field, label } of checks) {
    const text = String(draft[field]).trim()
    if (text === '') continue
    const value = Number(text)
    if (!Number.isInteger(value) || value < 0) return `${label} 要填非负整数，现在是「${text}」`
  }
  for (const field of ['retryOnExit', 'expectStatus'] as const) {
    const text = String(draft[field]).trim()
    if (text === '') continue
    const bad = list(text).filter((item) => !/^-?\d+$/.test(item))
    if (bad.length > 0) {
      return `${field === 'retryOnExit' ? '退出码' : '状态码'}列表只能是整数，「${bad.join('、')}」不行`
    }
  }
  return null
})

const argErrors = computed(() => {
  const seen = new Set<string>()
  for (const row of argRows.value) {
    const name = row.name.trim()
    if (name === '') return '有一行参数没填名字'
    if (!ARG_NAME_PATTERN.test(name)) return `参数名「${name}」只能用小写字母、数字与下划线`
    if (name === '_positional') return '参数名 _positional 是位置参数的保留键，不能声明'
    if (seen.has(name)) return `参数名「${name}」声明了两次`
    seen.add(name)
  }
  return null
})

const envError = computed(() => {
  const seen = new Set<string>()
  for (const row of draft.envRows) {
    const key = row.key.trim()
    if (key === '' && row.value === '') continue
    if (key === '') return '有一行环境变量没填键名'
    if (!ENV_NAME_PATTERN.test(key)) return `环境变量名「${key}」只能大写字母、数字与下划线`
    if (key.startsWith('GODELAYQ_')) return 'GODELAYQ_ 前缀留给服务端凭据，档位不能占用'
    if (seen.has(key)) return `环境变量「${key}」填了两次`
    seen.add(key)
  }
  if (draft.envRows.some((row) => row.key.trim() !== '' && row.value === '')) {
    return '新增环境变量要给取值：留空的这一行不会被保存'
  }
  return null
})

/** args_render 里的 {占位符} 必须对应上面声明过的参数名（后端 checkArgsRender 同一条） */
const argsRenderError = computed(() => {
  if (isHttp.value) return null
  const declared = new Set(argRows.value.map((row) => row.name.trim()))
  for (const line of lines(draft.argsRender)) {
    for (const match of line.matchAll(/\{([A-Za-z0-9_-]*)\}/g)) {
      if (!declared.has(match[1])) {
        return `「${line}」引用了 ${match[1]}，但上面没有声明这个参数`
      }
    }
  }
  return null
})

const fieldErrors = computed(() => [
  nameError.value,
  targetError.value,
  timeoutError.value,
  integerErrors.value,
  argErrors.value,
  argsRenderError.value,
  envError.value,
])

const hasErrors = computed(() => fieldErrors.value.some((error) => error !== null))

const canSave = computed(
  () => props.canWrite && !props.busy && !props.loading && !hasErrors.value && (isCreate.value || dirty.value),
)

/* ---------------- 载荷组装 ---------------- */

/**
 * 固定环境变量：三种语义要分清。
 * 一个键都没填 = 整个键不发（后端把"没带 env"解释成"不改"）；
 * 勾了"清空这一组" = 发显式的空对象；
 * 填了新行 = 发这份新的集合，而它替换的是整组而不是叠加——
 * 已有的取值后端不回显，页面无从把它们合并回来，所以界面直接把这件事说在前面。
 */
function buildEnv(record: ExecutorProfileRecord): void {
  if (isHttp.value) return
  if (draft.clearEnv) {
    record.env = {}
    return
  }
  const rows = draft.envRows.filter((row) => row.key.trim() !== '')
  if (rows.length === 0) return
  const env: Record<string, string> = {}
  for (const row of rows) env[row.key.trim()] = row.value
  record.env = env
}

function buildArgs(record: ExecutorProfileRecord): void {
  if (argRows.value.length === 0) return
  record.args = argRows.value.map<ExecutorProfileArgRecord>((row) => {
    const arg: ExecutorProfileArgRecord = { name: row.name.trim() }
    if (row.required) arg.required = true
    if (row.default !== '') arg.default = row.default
    if (row.pattern.trim() !== '') arg.pattern = row.pattern.trim()
    if (row.secret) arg.secret = true
    if (row.allow_dash) arg.allow_dash = true
    return arg
  })
}

function buildNumberFields(record: ExecutorProfileRecord): void {
  const timeout = draft.timeout.trim()
  if (timeout !== '') record.timeout = timeout
  const maxParallel = Number(draft.maxParallel.trim()) || 0
  if (maxParallel !== 0) record.max_parallel = maxParallel
}

function buildProcessFields(record: ExecutorProfileRecord): void {
  const cwd = draft.cwd.trim()
  if (cwd !== '') record.cwd = cwd
  const render = lines(draft.argsRender)
  if (render.length > 0) record.args_render = render
  // 位置参数规则不在本卡的表单里（卡 §3.3 没给它控件），编辑时原样带回，
  // 免得一次改超时的保存顺手把 positional 抹掉——PUT 是整条覆盖。
  if (props.record?.positional) record.positional = props.record.positional
  buildEnv(record)
  const allow = list(draft.envAllow)
  if (allow.length > 0) record.env_allow = allow
  const retry = intList(draft.retryOnExit)
  if (retry.length > 0) record.retry_on_exit = retry
}

function buildHttpFields(record: ExecutorProfileRecord): void {
  const method = draft.method.trim()
  if (method !== '') record.method = method
  record.url_template = draft.urlTemplate.trim()
  const hosts = list(draft.allowedHosts)
  if (hosts.length > 0) record.allowed_hosts = hosts
  const headers = textToHeaders(draft.headers)
  if (headers) record.headers = headers
  const headerAllow = list(draft.headerAllow)
  if (headerAllow.length > 0) record.header_allow = headerAllow
  const body = draft.body.trim()
  if (body !== '') record.body = body
  const status = intList(draft.expectStatus)
  if (status.length > 0) record.expect_status = status
  if (draft.captureResponse) record.capture_response = true
  const maxBytes = Number(draft.maxBodyBytes.trim()) || 0
  if (maxBytes !== 0) record.max_body_bytes = maxBytes
  const redirects = Number(draft.maxRedirects.trim()) || 0
  if (redirects !== 0) record.max_redirects = redirects
  if (draft.denyPrivate !== '') record.deny_private_ranges = draft.denyPrivate === 'true'
}

function buildPayload(): ExecutorProfileRecord {
  const record: ExecutorProfileRecord = { name: draft.name.trim(), kind: draft.kind }
  if (isScript.value) {
    record.runtime = draft.runtime.trim()
    record.script = draft.script.trim()
  }
  if (isBinary.value) {
    record.program = draft.program.trim()
    const fixed = lines(draft.fixedArgs)
    if (fixed.length > 0) record.fixed_args = fixed
  }
  buildArgs(record)
  buildNumberFields(record)
  if (!isHttp.value) buildProcessFields(record)
  else buildHttpFields(record)
  return record
}

function save(): void {
  if (!canSave.value) return
  emit('save', buildPayload())
}

/* ---------------- 表内动作 ---------------- */

function addArgRow(): void {
  argRows.value = [...argRows.value, { name: '', required: false, default: '', pattern: '', secret: false, allow_dash: false }]
}

function dropArgRow(index: number): void {
  argRows.value = argRows.value.filter((_, i) => i !== index)
}

function addEnvRow(): void {
  draft.envRows = [...draft.envRows, { key: '', value: '' }]
}

function dropEnvRow(index: number): void {
  draft.envRows = draft.envRows.filter((_, i) => i !== index)
}

/** 编辑一条已存在的档位时改 kind：D7 说这三条决定"这条档位是什么"，界面不给这条路 */
const kindLocked = computed(() => !isCreate.value)
</script>

<template>
  <form class="flex flex-col gap-4" @submit.prevent="save">
    <p
      v-if="loading"
      class="rounded-[var(--radius-control)] border border-[var(--color-border)] bg-[var(--color-surface)] px-3 py-2 text-sm text-[var(--color-text-muted)]"
    >
      正在读取这条档位的定义……
    </p>

    <div class="grid gap-3 sm:grid-cols-2">
      <UiInput
        label="档位名（注册键 exec.&lt;name&gt;）"
        :model-value="draft.name"
        :disabled="!canWrite || kindLocked || busy || loading"
        :error="nameError"
        hint="1–64 位字母、数字、下划线与短横线；名字即任务类型，提交 exec.<name> 时用的就是它"
        @update:model-value="draft.name = $event"
      />
      <fieldset class="flex flex-col gap-2">
        <legend class="mb-1 text-sm font-medium text-[var(--color-text)]">类型</legend>
        <div class="flex flex-wrap gap-3">
          <label
            v-for="option in KIND_OPTIONS"
            :key="option.value"
            class="flex items-center gap-1.5 text-sm"
            :title="kindLocked ? '改类型请删除后重建' : option.note"
          >
            <input
              v-model="draft.kind"
              type="radio"
              :value="option.value"
              class="accent-[var(--color-primary)]"
              :disabled="!canWrite || kindLocked || busy || loading"
            />
            {{ option.label }}
          </label>
        </div>
        <p v-if="kindLocked" class="text-xs text-[var(--color-text-muted)]">
          类型、脚本与程序路径三条改不了：要换内核请删除这条档位再建一条新的。
        </p>
        <p v-else class="text-xs text-[var(--color-text-muted)]">
          {{ KIND_OPTIONS.find((option) => option.value === draft.kind)?.note }}
        </p>
      </fieldset>
    </div>

    <!-- script -->
    <template v-if="isScript">
      <UiSelect
        v-if="hasRuntimeList"
        label="解释器（executors.runtime_allow）"
        :model-value="draft.runtime"
        :options="[{ value: '', label: '请选择解释器' }, ...runtimeOptions]"
        :disabled="!canWrite || busy || loading"
        :error="targetError && draft.runtime.trim() === '' ? targetError : null"
        hint="名单之外的解释器后端一律拒；拼错在这一栏就选不出来"
        @update:model-value="draft.runtime = $event"
      />
      <UiInput
        v-else
        label="解释器"
        :model-value="draft.runtime"
        :disabled="!canWrite || busy || loading"
        :error="targetError && draft.runtime.trim() === '' ? targetError : null"
        placeholder="python"
        hint="后端没给解释器名单，这里按原文提交；名单之外的写法会在保存时被 400 拒绝，原文显示在表单上方"
        @update:model-value="draft.runtime = $event"
      />
      <UiInput
        label="脚本路径"
        :model-value="draft.script"
        :disabled="!canWrite || busy || loading"
        :error="targetError && draft.script.trim() === '' ? targetError : null"
        placeholder="scripts/py_hello.py 或 D:\srv\scripts\job.py"
        hint="相对 executors.workspace，也可以写绝对路径（本期允许，界面上原样显示）"
        @update:model-value="draft.script = $event"
      />
    </template>

    <!-- binary -->
    <template v-else-if="isBinary">
      <UiInput
        label="程序"
        :model-value="draft.program"
        :disabled="!canWrite || busy || loading"
        :error="targetError && draft.program.trim() === '' ? targetError : null"
        placeholder="node 或 bin\worker.exe"
        hint="命中解释器名单的写法按 PATH 里的程序名处理，否则按 executors.workspace 内的路径处理"
        @update:model-value="draft.program = $event"
      />
      <div class="flex flex-col gap-1.5">
        <label class="flex items-baseline justify-between text-sm font-medium text-[var(--color-text)]">
          固定参数（fixed_args，一行一个）
          <span class="text-xs font-normal text-[var(--color-text-muted)]">排在任务参数之前，原样进 argv</span>
        </label>
        <textarea
          v-model="draft.fixedArgs"
          rows="3"
          spellcheck="false"
          :disabled="!canWrite || busy || loading"
          class="w-full resize-y rounded-[var(--radius-control)] border border-[var(--color-border)] bg-[var(--color-surface)] p-3
            font-mono text-xs leading-relaxed outline-none transition-colors
            focus:border-[var(--color-primary)] focus:bg-white disabled:text-[var(--color-text-muted)]"
        ></textarea>
      </div>
    </template>

    <!-- http -->
    <template v-else>
      <div class="grid gap-3 sm:grid-cols-[8rem_minmax(0,1fr)]">
        <UiInput
          label="方法"
          :model-value="draft.method"
          :disabled="!canWrite || busy || loading"
          placeholder="POST"
          hint="留空按 GET"
          @update:model-value="draft.method = $event"
        />
        <UiInput
          label="URL 模板"
          :model-value="draft.urlTemplate"
          :disabled="!canWrite || busy || loading"
          :error="targetError && draft.urlTemplate.trim() === '' ? targetError : null"
          placeholder="https://api.example.com/jobs/{day}"
          hint="{参数名} 只能引用下面声明过的参数"
          @update:model-value="draft.urlTemplate = $event"
        />
      </div>

      <details class="rounded-[var(--radius-control)] border border-[var(--color-border)] p-3">
        <summary class="cursor-pointer text-sm font-medium text-[var(--color-text)]">
          HTTP 细节（可访问主机、固定请求头、期望状态码……）
        </summary>
        <div class="mt-3 flex flex-col gap-3">
          <UiInput
            label="可访问主机（allowed_hosts）"
            :model-value="draft.allowedHosts"
            :disabled="!canWrite || busy || loading"
            placeholder="api.example.com cdn.example.com"
            hint="空格或逗号分隔；留空表示不加这一层限制"
            @update:model-value="draft.allowedHosts = $event"
          />
          <div class="flex flex-col gap-1.5">
            <label class="flex items-baseline justify-between text-sm font-medium text-[var(--color-text)]">
              固定请求头（一行一头）
              <span class="text-xs font-normal text-[var(--color-text-muted)]">写法 Name: v1, v2</span>
            </label>
            <textarea
              v-model="draft.headers"
              rows="3"
              spellcheck="false"
              :disabled="!canWrite || busy || loading"
              class="w-full resize-y rounded-[var(--radius-control)] border border-[var(--color-border)] bg-[var(--color-surface)] p-3
                font-mono text-xs leading-relaxed outline-none transition-colors
                focus:border-[var(--color-primary)] focus:bg-white disabled:text-[var(--color-text-muted)]"
            ></textarea>
          </div>
          <UiInput
            label="允许 payload 注入的请求头（header_allow）"
            :model-value="draft.headerAllow"
            :disabled="!canWrite || busy || loading"
            placeholder="X-Trace-Id"
            hint="空格或逗号分隔的头名"
            @update:model-value="draft.headerAllow = $event"
          />
          <UiInput
            label="请求体模板（body）"
            :model-value="draft.body"
            :disabled="!canWrite || busy || loading"
            placeholder='{"day":"{day}"}'
            hint="一段带 {参数名} 占位符的 JSON 文本，原样存进档位"
            @update:model-value="draft.body = $event"
          />
          <UiInput
            label="期望状态码（expect_status）"
            :model-value="draft.expectStatus"
            :disabled="!canWrite || busy || loading"
            placeholder="200 201 202"
            hint="空格或逗号分隔的整数；留空表示 2xx 都算成功"
            @update:model-value="draft.expectStatus = $event"
          />
          <div class="grid gap-3 sm:grid-cols-3">
            <UiInput
              label="响应体上限（max_body_bytes）"
              :model-value="draft.maxBodyBytes"
              :disabled="!canWrite || busy || loading"
              placeholder="留空用默认"
              @update:model-value="draft.maxBodyBytes = $event"
            />
            <UiInput
              label="最多跟随几次重定向"
              :model-value="draft.maxRedirects"
              :disabled="!canWrite || busy || loading"
              placeholder="留空用默认"
              @update:model-value="draft.maxRedirects = $event"
            />
            <UiSelect
              label="禁私网地址"
              :model-value="draft.denyPrivate"
              :options="[
                { value: '', label: '默认（禁止）' },
                { value: 'true', label: '禁止' },
                { value: 'false', label: '允许' },
              ]"
              :disabled="!canWrite || busy || loading"
              hint="内网与环回地址要不要挡下来"
              @update:model-value="draft.denyPrivate = $event as '' | 'true' | 'false'"
            />
          </div>
          <label class="flex items-center gap-2 text-sm">
            <input
              v-model="draft.captureResponse"
              type="checkbox"
              class="accent-[var(--color-primary)]"
              :disabled="!canWrite || busy || loading"
            />
            把响应体作为执行结果留存（capture_response）
          </label>
        </div>
      </details>
    </template>

    <!-- 参数表：三种档位都有 args，http 那份用作 URL 查询参数 -->
    <fieldset class="flex flex-col gap-2">
      <legend class="mb-1 text-sm font-medium text-[var(--color-text)]">
        {{ isHttp ? '参数（填进 URL 与请求体的占位符）' : '具名参数（args）' }}
      </legend>

      <p v-if="argRows.length === 0" class="text-xs text-[var(--color-text-muted)]">
        这条档位不接收参数。
      </p>
      <div v-for="(row, index) in argRows" :key="index" class="flex flex-col gap-2 rounded-[var(--radius-control)]
        border border-[var(--color-border)] p-2.5">
        <div class="flex flex-wrap items-end gap-2">
          <UiInput
            :model-value="row.name"
            :disabled="!canWrite || busy || loading"
            placeholder="参数名"
            class="w-44"
            @update:model-value="row.name = $event"
          />
          <UiInput
            v-model="row.default"
            label="默认值"
            :disabled="!canWrite || busy || loading"
            class="w-44"
          />
          <UiInput
            v-model="row.pattern"
            label="格式（正则）"
            :disabled="!canWrite || busy || loading"
            class="min-w-44 flex-1"
          />
          <UiButton variant="ghost" size="sm" :disabled="!canWrite || busy" @click="dropArgRow(index)">
            <X :size="14" aria-hidden="true" />
            移除
          </UiButton>
        </div>
        <div class="flex flex-wrap gap-4 text-sm">
          <label class="flex items-center gap-1.5">
            <input v-model="row.required" type="checkbox" class="accent-[var(--color-primary)]" :disabled="!canWrite || busy" />
            必填
          </label>
          <label class="flex items-center gap-1.5">
            <input v-model="row.secret" type="checkbox" class="accent-[var(--color-primary)]" :disabled="!canWrite || busy" />
            凭据（值不回显）
          </label>
          <label v-if="!isHttp" class="flex items-center gap-1.5">
            <input v-model="row.allow_dash" type="checkbox" class="accent-[var(--color-primary)]" :disabled="!canWrite || busy" />
            值可以以短横线开头
          </label>
        </div>
      </div>
      <div class="self-start">
        <UiButton variant="ghost" size="sm" :disabled="!canWrite || busy || loading" @click="addArgRow">
          <Plus :size="14" aria-hidden="true" />
          加一个参数
        </UiButton>
      </div>
      <p v-if="argErrors" class="text-xs text-[var(--color-status-failed)]">{{ argErrors }}</p>
    </fieldset>

    <!-- 进程档位专属 -->
    <template v-if="!isHttp">
      <div class="grid gap-3 sm:grid-cols-2">
        <div class="flex flex-col gap-1.5">
          <label class="flex items-baseline justify-between text-sm font-medium text-[var(--color-text)]">
            参数渲染模板（args_render）
            <span class="text-xs font-normal text-[var(--color-text-muted)]">一行一段 argv</span>
          </label>
          <textarea
            v-model="draft.argsRender"
            rows="3"
            spellcheck="false"
            :disabled="!canWrite || busy || loading"
            placeholder="--day={day}&#10;--note={note}"
            class="w-full resize-y rounded-[var(--radius-control)] border border-[var(--color-border)] bg-[var(--color-surface)] p-3
              font-mono text-xs leading-relaxed outline-none transition-colors
              focus:border-[var(--color-primary)] focus:bg-white disabled:text-[var(--color-text-muted)]"
          ></textarea>
          <p v-if="argsRenderError" class="text-xs text-[var(--color-status-failed)]">{{ argsRenderError }}</p>
          <p v-else class="text-xs text-[var(--color-text-muted)]">
            留空即按参数名自动拼 --名=值；模板里只能引用上面声明过的参数名。
          </p>
        </div>

        <div class="flex flex-col gap-3">
          <UiInput
            label="工作目录（cwd）"
            :model-value="draft.cwd"
            :disabled="!canWrite || busy || loading"
            placeholder="留空用 executors.workspace"
            @update:model-value="draft.cwd = $event"
          />
          <!-- 位置参数规则不在本卡的表单里（卡 §3.3 没给它控件）：只读回显，保存时原样带回，
               免得一次改超时的保存顺手把 positional 抹掉——PUT 是整条覆盖。 -->
          <p v-if="props.record?.positional" class="text-xs leading-relaxed text-[var(--color-text-muted)]">
            位置参数：最多 {{ props.record.positional.max }} 个，格式
            <code class="font-mono">{{ props.record.positional.pattern || '默认字符集' }}</code>。
            这一条本页只读，保存时原样带回；要改它请编辑档位文件后重启。
          </p>
        </div>
      </div>

      <fieldset class="flex flex-col gap-2">
        <legend class="mb-1 text-sm font-medium text-[var(--color-text)]">固定注入的环境变量（env）</legend>
        <p v-if="existingEnvKeys.length > 0" class="text-xs text-[var(--color-text-muted)]">
          这条档位已固定：
          <code v-for="key in existingEnvKeys" :key="key" class="mr-1 font-mono">{{ key }}</code>
          取值后端不回显，因此这里看不到也改不了单个值。
        </p>
        <div v-for="(row, index) in draft.envRows" :key="index" class="flex items-end gap-2">
          <UiInput
            :model-value="row.key"
            :disabled="!canWrite || busy || loading"
            placeholder="键名（大写）"
            class="w-52"
            @update:model-value="row.key = $event"
          />
          <UiInput
            :model-value="row.value"
            :disabled="!canWrite || busy || loading"
            placeholder="取值"
            type="password"
            autocomplete="new-password"
            class="flex-1"
            @update:model-value="row.value = $event"
          />
          <UiButton variant="ghost" size="sm" :disabled="!canWrite || busy" @click="dropEnvRow(index)">
            <X :size="14" aria-hidden="true" />
          </UiButton>
        </div>
        <div class="flex flex-wrap items-center gap-3">
          <UiButton variant="ghost" size="sm" :disabled="!canWrite || busy || loading" @click="addEnvRow">
            <Plus :size="14" aria-hidden="true" />
            加一个变量
          </UiButton>
          <label
            v-if="existingEnvKeys.length > 0"
            class="flex items-center gap-1.5 text-xs text-[var(--color-text-muted)]"
          >
            <input v-model="draft.clearEnv" type="checkbox" class="accent-[var(--color-primary)]" :disabled="!canWrite || busy" />
            清空这一组固定环境变量
          </label>
        </div>
        <p v-if="envError" class="text-xs text-[var(--color-status-failed)]">{{ envError }}</p>
        <p v-else class="text-xs leading-relaxed text-[var(--color-text-muted)]">
          这几栏留空表示不动原有的那一组。填了新变量就是替换整组：
          已有变量的取值不回显，页面无从把它们和新填的合并成一份，所以保存前请确认这组该全部重写。
        </p>
      </fieldset>

      <UiInput
        label="可注入的环境变量名（env_allow）"
        :model-value="draft.envAllow"
        :disabled="!canWrite || busy || loading"
        placeholder="DAY NOTE_HOME"
        hint="空格或逗号分隔的大写键名；payload 只能注入列在这里的那些"
        @update:model-value="draft.envAllow = $event"
      />

      <UiInput
        label="这些退出码算可重试（retry_on_exit）"
        :model-value="draft.retryOnExit"
        :disabled="!canWrite || busy || loading"
        placeholder="1 75"
        hint="空格或逗号分隔的整数；留空表示只有非零退出中的默认那一类会重试"
        @update:model-value="draft.retryOnExit = $event"
      />
    </template>

    <div class="grid gap-3 sm:grid-cols-2">
      <UiInput
        label="单次执行超时（timeout）"
        :model-value="draft.timeout"
        :disabled="!canWrite || busy || loading"
        :error="timeoutError"
        :placeholder="`留空用 executors.default_timeout（上限 ${maxTimeout || '后端没给'}）`"
        hint="写法如 90s、2m、1h30m：必须带单位"
        @update:model-value="draft.timeout = $event"
      />
      <UiInput
        label="同时最多跑几个（max_parallel）"
        :model-value="draft.maxParallel"
        :disabled="!canWrite || busy || loading"
        placeholder="留空按 1"
        hint="跨档位共享的名额由 executors.concurrency 再压一层"
        @update:model-value="draft.maxParallel = $event"
      />
    </div>

    <p class="rounded-[var(--radius-control)] border border-[var(--color-border)] bg-[var(--color-surface)] px-3 py-2 text-xs leading-relaxed text-[var(--color-text-muted)]">
      探测失败不阻止保存：脚本还没部署、程序不在 PATH，都能先存下来。
      保存之后列表那一行会显出"不可用"和原因，那时提交这条档位的任务才会被挡住。
    </p>

    <div class="flex flex-wrap items-center gap-2">
      <UiButton type="submit" :loading="busy" :disabled="!canSave" :title="writeHint">
        {{ isCreate ? '创建档位' : '保存修改' }}
      </UiButton>
      <UiButton
        v-if="!isCreate"
        variant="danger"
        :disabled="!canDelete || busy"
        @click="emit('remove', draft.name)"
      >
        删除档位
      </UiButton>
      <span v-if="!isCreate && !dirty" class="text-xs text-[var(--color-text-muted)]">没有改动</span>
    </div>
  </form>
</template>
