<script setup lang="ts">
/* 任务详情：基本信息 + payload 只读 + 运行时间线 + 与列表同一套操作规则。
 *
 * 时间线首屏来自 GET /jobs/:id/events，此后由 WS 事件续上。首屏那批读的是持久化事件库
 * 还是进程内存缓冲，由响应的 note 字段说明，页面只引用它——所以"能看到多久的历史"
 * 随这次部署的观测层配置而变，别在这里写死成某一种。 */
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { ArrowLeft } from 'lucide-vue-next'
import JobEventTimeline from '../components/jobs/JobEventTimeline.vue'
import JobExecResult from '../components/jobs/JobExecResult.vue'
import JobForm from '../components/jobs/JobForm.vue'
import JobStatusBadge from '../components/jobs/JobStatusBadge.vue'
import PageHeader from '../components/layout/PageHeader.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiConfirm from '../components/ui/UiConfirm.vue'
import UiDrawer from '../components/ui/UiDrawer.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
import { fetchJob, fetchJobTypes, pauseJob, retryJob, resumeJob, cancelJob, forcePauseJob, updateJob } from '../api/jobs'
import { listExecutors } from '../api/executors'
import { listGroups } from '../api/groups'
import { ApiError } from '../api/client'
import { queryKeys } from '../api/keys'
import { useJobEvents } from '../composables/useJobEvents'
import { usePermission } from '../composables/usePermission'
import { useToastStore } from '../stores/toast'
import { formatDateTime, jobTypeOf, shortJobId } from '../display'
import type { Job, UpdateJobRequest } from '../api/types'

const route = useRoute()
const router = useRouter()
const toast = useToastStore()
const queryClient = useQueryClient()
const permission = usePermission()

/** 同一条路由记录会在 /jobs/a → /jobs/b 之间复用这个实例，所以 id 必须是响应式的 */
const jobId = computed(() => String(route.params.id ?? ''))
const jobQuery = useQuery({
  queryKey: computed(() => queryKeys.job(jobId.value)),
  queryFn: () => fetchJob(jobId.value),
})
const job = computed(() => jobQuery.data.value ?? null)

const { timeline, note, isLoading: timelineLoading } = useJobEvents(jobId)

const groupsQuery = useQuery({ queryKey: queryKeys.groups, queryFn: listGroups })
const jobTypesQuery = useQuery({
  queryKey: queryKeys.jobTypes,
  queryFn: fetchJobTypes,
  staleTime: 5 * 60_000,
})

const busy = ref('')
const drawerOpen = ref(false)
const formBusy = ref(false)
const formError = ref<string | null>(null)
const confirmKind = ref<'cancel' | 'force-pause' | null>(null)

const canWrite = computed(() => permission.can('job.create'))
const writeHint = computed(() => permission.blockedReason('job.create'))
const canForcePause = computed(() => permission.can('job.force_pause'))

/** 操作区与 JobTable 的 ACTIONS 表同一口径：状态决定能做什么 */
const available = computed(() => {
  const status = job.value?.status
  return {
    edit: status === 'pending',
    pause: status === 'pending',
    resume: status === 'paused',
    cancel: status === 'pending' || status === 'paused',
    retry: status === 'failed',
    forcePause: status === 'running',
  }
})

function describeError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.details ? `${err.message}：${err.details}` : err.message
  return fallback
}

function refresh(): void {
  void queryClient.invalidateQueries({ queryKey: queryKeys.job(jobId.value) })
  void queryClient.invalidateQueries({ queryKey: queryKeys.jobsAll })
  void queryClient.invalidateQueries({ queryKey: queryKeys.stats })
}

async function act(kind: string, fn: () => Promise<unknown>, done?: string): Promise<void> {
  busy.value = kind
  try {
    await fn()
    refresh()
    if (done) toast.success(done)
  } catch (err) {
    toast.error(describeError(err, `${kind} 失败`))
  } finally {
    busy.value = ''
  }
}

async function confirmAction(): Promise<void> {
  const kind = confirmKind.value
  confirmKind.value = null
  if (!kind) return

  if (kind === 'cancel') {
    await act('cancel', () => cancelJob(jobId.value), '任务已取消（记录随之删除）')
    await router.push({ name: 'jobs' })
    return
  }
  await act('force-pause', () => forcePauseJob(jobId.value))
  toast.info('中止已发起，任务停在 paused 后即为生效')
}

async function submitEdit(body: UpdateJobRequest): Promise<void> {
  formBusy.value = true
  formError.value = null
  try {
    await updateJob(jobId.value, body)
    drawerOpen.value = false
    refresh()
    toast.success('已保存')
  } catch (err) {
    formError.value = describeError(err, '提交失败')
  } finally {
    formBusy.value = false
  }
}

/** payload 用格式化后的 JSON 展示；不是对象也照原样给出来 */
const payloadText = computed(() => {
  const payload = job.value?.payload
  if (payload === undefined || payload === null) return ''
  return typeof payload === 'string' ? payload : JSON.stringify(payload, null, 2)
})

/**
 * 这条任务跑的是不是某个档位：判据是"类型在不在 GET /executors 的登记表里"，
 * 不再看名称前缀——名称与类型解耦之后（TASK-N02），前缀判据既读不到标签里的东西，
 * 也不该由前端复制一份键名规则。
 */
const executorsQuery = useQuery({
  queryKey: queryKeys.executors,
  queryFn: listExecutors,
  staleTime: 5 * 60_000,
})

/** 类型那个注册键（旧写法时后端与这里都回退到名称） */
const jobType = computed(() => (job.value ? jobTypeOf(job.value) : ''))

const jobProfile = computed(
  () =>
    executorsQuery.data.value?.profiles.find((item) => item.key === jobType.value && !item.degraded) ??
    null,
)

const isExecutorJob = computed(() => jobProfile.value !== null)

/** 自由执行任务的位置：值在 payload 顶层，键名由档位的 location 给 */
const jobLocation = computed(() => {
  const spec = jobProfile.value?.location
  const payload = job.value?.payload
  if (!spec || !payload || typeof payload !== 'object' || Array.isArray(payload)) return null
  const value = (payload as Record<string, unknown>)[spec.key]
  return typeof value === 'string' && value !== '' ? value : null
})

const fields = computed(() => {
  const current: Job | null = job.value
  if (!current) return []
  return [
    { label: '任务 ID', value: current.id, mono: true },
    { label: '名称', value: current.name },
    {
      label: '任务类型',
      value: current.type?.trim() ? current.type : `${jobType.value}（旧写法：名称就是类型）`,
      mono: true,
    },
    ...(jobLocation.value
      ? [{ label: jobProfile.value?.location?.label ?? '执行位置', value: jobLocation.value, mono: true }]
      : []),
    { label: '分组', value: current.group || '未分组' },
    { label: '触发时间', value: formatDateTime(current.trigger_at) },
    { label: '创建时间', value: formatDateTime(current.created_at) },
    { label: '最后更新', value: formatDateTime(current.updated_at) },
    { label: '重试', value: `${current.retry_count}/${current.max_retries}` },
    { label: '单次超时', value: current.timeout || '不限制' },
    { label: 'Cron', value: current.cron_expr || '—' },
    { label: '重复执行', value: current.is_repeat ? '是' : '否' },
  ]
})
</script>

<template>
  <div class="p-6">
    <PageHeader
      :title="job ? `任务 ${job.name}` : '任务详情'"
      :subtitle="job ? `ID ${shortJobId(job.id)}（${job.id}）` : '按 ID 读取单个任务快照与它的运行时间线'"
    >
      <template #actions>
        <UiButton variant="ghost" size="sm" @click="router.push({ name: 'jobs' })">
          <ArrowLeft :size="14" aria-hidden="true" />
          返回列表
        </UiButton>
      </template>
    </PageHeader>

    <p
      v-if="jobQuery.error.value"
      role="alert"
      class="mb-4 rounded-[var(--radius-control)] border border-[var(--color-status-failed)]
        bg-[#fef2f2] px-3 py-2 text-sm text-[var(--color-status-failed)]"
    >
      读取失败：{{ describeError(jobQuery.error.value, '未知错误') }}
    </p>

    <UiEmptyState
      v-if="!job && !jobQuery.isPending.value"
      title="找不到这个任务"
      description="任务被取消（记录已删）或 ID 不属于当前进程"
    />

    <div v-if="job" class="grid gap-4 lg:grid-cols-2">
      <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4">
        <div class="mb-4 flex items-center justify-between">
          <h2 class="text-sm font-semibold">基本信息</h2>
          <JobStatusBadge :status="job.status" />
        </div>

        <dl class="grid grid-cols-2 gap-x-4 gap-y-2.5 text-sm">
          <div v-for="field in fields" :key="field.label" class="flex flex-col">
            <dt class="text-xs text-[var(--color-text-muted)]">{{ field.label }}</dt>
            <dd :class="field.mono ? 'break-all font-mono text-xs' : ''">{{ field.value }}</dd>
          </div>
        </dl>

        <div class="mt-4 flex flex-wrap gap-2">
          <UiButton
            v-if="available.edit"
            size="sm"
            variant="outline"
            :disabled="!canWrite || busy !== ''"
            :title="writeHint"
            @click="drawerOpen = true"
          >
            编辑
          </UiButton>
          <UiButton
            v-if="available.pause"
            size="sm"
            variant="outline"
            :disabled="!canWrite || busy !== ''"
            :title="writeHint"
            @click="act('pause', () => pauseJob(jobId), '已暂停')"
          >
            {{ busy === 'pause' ? '处理中…' : '暂停' }}
          </UiButton>
          <UiButton
            v-if="available.resume"
            size="sm"
            variant="outline"
            :disabled="!canWrite || busy !== ''"
            :title="writeHint"
            @click="act('resume', () => resumeJob(jobId), '已恢复并重新排期')"
          >
            {{ busy === 'resume' ? '处理中…' : '恢复' }}
          </UiButton>
          <UiButton
            v-if="available.retry"
            size="sm"
            variant="outline"
            :disabled="!canWrite || busy !== ''"
            :title="writeHint"
            @click="act('retry', () => retryJob(jobId), '已重新排期（1 秒后执行）')"
          >
            重试
          </UiButton>
          <UiButton
            v-if="available.forcePause"
            size="sm"
            variant="danger"
            :disabled="!canForcePause || busy !== ''"
            :title="canForcePause ? '中止当前执行并停在 paused' : permission.blockedReason('job.force_pause')"
            @click="confirmKind = 'force-pause'"
          >
            强制暂停
          </UiButton>
          <UiButton
            v-if="available.cancel"
            size="sm"
            variant="ghost"
            :disabled="!canWrite || busy !== ''"
            :title="writeHint"
            @click="confirmKind = 'cancel'"
          >
            取消任务
          </UiButton>
        </div>
      </section>

      <section class="flex flex-col rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4">
        <h2 class="mb-3 text-sm font-semibold">payload</h2>
        <pre
          v-if="payloadText"
          class="max-h-64 overflow-auto rounded-[var(--radius-control)] bg-[var(--color-surface)] p-3 font-mono text-xs leading-relaxed"
          >{{ payloadText }}</pre>
        <p v-else class="text-sm text-[var(--color-text-muted)]">这个任务不带 payload。</p>

        <!-- 掩码只发生在读取响应这一层（TASK-E16）：看的人要知道 *** 不是任务本身的样子 -->
        <p
          v-if="isExecutorJob"
          class="mt-2 text-xs text-[var(--color-text-muted)]"
        >
          档位声明为 secret 的参数在这里显示成 <code>***</code>，输出预览里被替换掉的也是同一批值；
          任务快照与产物文件里存的仍是提交时的原值。
        </p>
      </section>

      <JobExecResult :job="job" :events="timeline" />

      <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4 lg:col-span-2">
        <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
          <h2 class="text-sm font-semibold">运行时间线</h2>
          <span class="text-xs text-[var(--color-text-muted)]">{{ note }}</span>
        </div>

        <UiEmptyState
          v-if="timeline.length === 0"
          :title="timelineLoading ? '正在读取事件缓冲' : '这个任务还没有事件'"
          :description="
            timelineLoading
              ? '首屏取自 GET /jobs/:id/events'
              : '缓冲只覆盖当前进程的这一生；任务在此之前的运行不在这里'
          "
        />

        <JobEventTimeline v-else :events="timeline" />
      </section>
    </div>

    <UiDrawer :open="drawerOpen" :title="`编辑任务 ${shortJobId(jobId)}`" :busy="formBusy" @close="drawerOpen = false">
      <JobForm
        v-if="job"
        mode="edit"
        :job="job"
        :job-types="jobTypesQuery.data.value ?? []"
        :group-names="(groupsQuery.data.value ?? []).map((group) => group.name)"
        :busy="formBusy"
        :server-error="formError"
        @submit="submitEdit"
        @cancel="drawerOpen = false"
      />
    </UiDrawer>

    <UiConfirm
      :open="confirmKind !== null"
      :title="confirmKind === 'force-pause' ? '强制暂停这个任务？' : '取消这个任务？'"
      :confirm-label="confirmKind === 'force-pause' ? '中止执行' : '确认取消'"
      tone="danger"
      @cancel="confirmKind = null"
      @confirm="confirmAction"
    >
      <template v-if="confirmKind === 'cancel'">
        取消会<em class="font-medium text-[var(--color-text)]">删除任务记录</em>（含它的历史快照），
        页面随后回到列表。想让任务停一会儿又随时恢复，用"暂停"。
      </template>
      <template v-else>
        强制暂停会中止<em class="font-medium text-[var(--color-text)]">正在执行的这一次尝试</em>，
        不计入失败、不消耗重试次数，任务停在 paused 直到恢复。
        中止要等 Handler 自己返回——这段时间它看起来还在执行中。
      </template>
    </UiConfirm>
  </div>
</template>
