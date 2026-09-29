<script setup lang="ts">
/* 任务表单（新建 / 编辑同一个组件）。
 *
 * 编辑模式的字段集比新建小一圈，这不是偷懒：PUT 的契约就只能改
 * trigger_at / payload / max_retries / timeout / group（名称与 cron 是任务身份的一部分，
 * 后端不接受修改）。把新建的表单直接搬进编辑会做出一张"填了却不生效"的假表单。
 *
 * 触发方式三选一在提交时塌缩成一个：后端看到 delay / trigger_at / cron_expr 只应有一个非空。 */
import { computed, reactive, ref, watch } from 'vue'
import CronPicker from './CronPicker.vue'
import PayloadEditor from './PayloadEditor.vue'
import UiButton from '../ui/UiButton.vue'
import UiInput from '../ui/UiInput.vue'
import UiSelect from '../ui/UiSelect.vue'
import type { CreateJobRequest, Job, UpdateJobRequest } from '../../api/types'

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

const form = reactive({
  name: props.job?.name ?? '',
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

const nameMissing = computed(() => props.mode === 'create' && form.name === '')

const canSubmit = computed(
  () =>
    !props.busy &&
    !nameMissing.value &&
    !triggerError.value &&
    !payloadError.value &&
    (props.mode === 'edit' || form.triggerMode !== 'cron' || form.cronExpr.trim() !== ''),
)

const groupOptions = computed(() => [
  { value: UNGROUPED, label: '未分组' },
  ...props.groupNames.map((name) => ({ value: name, label: name })),
])

const parsedPayload = computed(() => {
  const text = form.payload.trim()
  return text === '' ? undefined : (JSON.parse(text) as unknown)
})

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

  const body: CreateJobRequest = { name: form.name, payload, group }
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

    <UiSelect
      v-if="mode === 'create'"
      label="任务名称（= 已注册的任务类型）"
      :model-value="form.name"
      :options="[{ value: '', label: '请选择任务类型' }, ...jobTypes.map((name) => ({ value: name, label: name }))]"
      :disabled="busy"
      :error="nameMissing ? '必须选择一个任务类型' : null"
      :hint="jobTypes.length === 0 ? '后端没有注册任何任务类型，先加载 handler 或检查 job 目录配置' : undefined"
      @update:model-value="form.name = $event"
    />
    <div v-else class="flex flex-col gap-1.5">
      <span class="text-sm font-medium text-[var(--color-text)]">任务名称</span>
      <p class="text-sm text-[var(--color-text-muted)]">
        <code>{{ form.name }}</code>
        <span class="ml-2 text-xs">（名称与 cron 不可修改，要换就新建一条）</span>
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

    <PayloadEditor
      :model-value="form.payload"
      :disabled="busy"
      :error="payloadError"
      hint="合法 JSON，留空表示不带 payload"
      @update:model-value="form.payload = $event"
    />

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
