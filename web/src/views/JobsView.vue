<script setup lang="ts">
/* 任务列表页（§4.6 JobsView）。
 *
 * 三件事在这一层收口：查询参数（筛选 + 分页）、动作（单条与批量）、
 * 以及"这个动作现在能不能做"（状态 × 角色）。表格与表单只收 props、只发事件。
 *
 * 批量走 batch-ops：混合结果用 207 表达，所以它不是"成功或失败"，
 * 而是"N 条成功、M 条为什么失败"——文案必须把后者说清楚，
 * 否则用户只会看到"操作完成"却发现有一半任务没动。 */
import { computed, ref, watch } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { useRoute, useRouter } from 'vue-router'
import PageHeader from '../components/layout/PageHeader.vue'
import JobFilterBar from '../components/jobs/JobFilterBar.vue'
import JobForm from '../components/jobs/JobForm.vue'
import JobTable from '../components/jobs/JobTable.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiConfirm from '../components/ui/UiConfirm.vue'
import UiDrawer from '../components/ui/UiDrawer.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
import UiSelect from '../components/ui/UiSelect.vue'
import {
  batchJobOps,
  cancelJob,
  createJob,
  fetchJobTypes,
  forcePauseJob,
  listJobs,
  pauseJob,
  retryJob,
  resumeJob,
  updateJob,
} from '../api/jobs'
import { createGroup, listGroups } from '../api/groups'
import { ApiError } from '../api/client'
import { queryKeys } from '../api/keys'
import { shortJobId } from '../display'
import { usePermission } from '../composables/usePermission'
import { useToastStore } from '../stores/toast'
import type {
  BatchJobAction,
  BatchJobOpsResponse,
  CreateJobRequest,
  Job,
  JobStatus,
  ListJobsQuery,
  UpdateJobRequest,
} from '../api/types'

const PAGE_SIZE = 50

const router = useRouter()
const route = useRoute()
const toast = useToastStore()
const queryClient = useQueryClient()
const permission = usePermission()

/**
 * 筛选条件来自 URL：分组页要能跳到"这个组的任务"，用户也要能把筛好的列表贴给别人。
 * group= 与不带 group 是两回事（未分组 vs 不过滤），所以这里必须区分空串与缺席。
 */
function initialFilters(): ListJobsQuery {
  const { status, type, name, group } = route.query
  const next: ListJobsQuery = {}
  if (typeof status === 'string' && status !== '') next.status = status as JobStatus
  if (typeof type === 'string' && type !== '') next.type = type
  if (typeof name === 'string' && name !== '') next.name = name
  if (typeof group === 'string') next.group = group
  return next
}

const filters = ref<ListJobsQuery>(initialFilters())
const offset = ref(0)

// 筛选条件写回 URL 用 replace：每次改下拉都压一条历史记录，后退键就变成筛选回放机
watch(filters, (next) => {
  // 换了筛选条件却还停在第 3 页，看到的一定是错位的数据
  offset.value = 0

  const query: Record<string, string> = {}
  if (next.status) query.status = next.status
  if (next.type) query.type = next.type
  if (next.name) query.name = next.name
  if (next.group !== undefined) query.group = next.group
  void router.replace({ name: 'jobs', query })
})

const jobsQuery = useQuery({
  queryKey: computed(() => queryKeys.jobs(filters.value, offset.value)),
  queryFn: () => listJobs({ ...filters.value, limit: PAGE_SIZE, offset: offset.value }),
})

const jobTypesQuery = useQuery({
  queryKey: queryKeys.jobTypes,
  queryFn: fetchJobTypes,
  staleTime: 5 * 60_000,
})

const groupsQuery = useQuery({ queryKey: queryKeys.groups, queryFn: listGroups })

const rows = computed(() => jobsQuery.data.value?.items ?? [])
const total = computed(() => jobsQuery.data.value?.total ?? 0)

const groupNames = computed(() => {
  const fromRegistry = (groupsQuery.data.value ?? []).map((group) => group.name)
  // 只挂在任务标签上的组也要能筛出来，否则"看着有、下拉没有"
  const fromRows = rows.value.map((job) => job.group ?? '').filter(Boolean)
  return [...new Set([...fromRegistry, ...fromRows])].sort((a, b) => a.localeCompare(b, 'zh-Hans-CN'))
})

const pageSummary = computed(() => {
  if (total.value === 0) return '没有任务'
  const from = offset.value + 1
  const to = Math.min(offset.value + PAGE_SIZE, total.value)
  return `第 ${from}–${to} 条 / 共 ${total.value} 条`
})

function goPage(next: number): void {
  // 起点是 PAGE_SIZE 的整数倍，唯一要管的是别越过最后一条
  const maxStart = Math.max(0, Math.floor((total.value - 1) / PAGE_SIZE) * PAGE_SIZE)
  const target = Math.max(0, Math.min(next, maxStart))
  if (target === offset.value) return
  offset.value = target
  // 换页后选中没意义：新一页的 ID 和上一页毫不相干
  selected.value = []
}

/** jobId -> 正在提交的动作；同一行同时只允许一个动作在飞 */
const busy = ref<Record<string, string>>({})
const selected = ref<string[]>([])

/** 选中的增删在父层落地：谁持有状态谁负责合并，
    否则一次点击里的两次上报会读到同一份旧数组，后一次覆盖前一次 */
function toggleOne(id: string): void {
  selected.value = selected.value.includes(id)
    ? selected.value.filter((selected) => selected !== id)
    : [...selected.value, id]
}

function toggleMany(ids: string[], checked: boolean): void {
  const target = new Set(ids)
  selected.value = checked
    ? [...new Set([...selected.value, ...ids])]
    : selected.value.filter((id) => !target.has(id))
}

const canWrite = computed(() => permission.can('job.create'))
const writeHint = computed(() => permission.blockedReason('job.create'))
const canForcePause = computed(() => permission.can('job.force_pause'))
const forcePauseHint = computed(() => permission.blockedReason('job.force_pause'))

function describeError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    return err.details ? `${err.message}：${err.details}` : err.message
  }
  return fallback
}

/** 列表与统计是同一份真相的两个切面，任何写操作之后都要一起重取 */
function refreshJobs(): void {
  void queryClient.invalidateQueries({ queryKey: queryKeys.jobsAll })
  void queryClient.invalidateQueries({ queryKey: queryKeys.stats })
  void queryClient.invalidateQueries({ queryKey: queryKeys.groups })
}

async function run(id: string, action: string, fn: () => Promise<unknown>): Promise<boolean> {
  busy.value = { ...busy.value, [id]: action }
  try {
    await fn()
    refreshJobs()
    return true
  } catch (err) {
    toast.error(describeError(err, `${action} 失败`))
    return false
  } finally {
    const next = { ...busy.value }
    delete next[id]
    busy.value = next
  }
}

// 抽屉：新建与编辑复用，编辑时带上原任务
const drawerOpen = ref(false)
const editing = ref<Job | null>(null)
const formBusy = ref(false)
const formError = ref<string | null>(null)

function openCreate(): void {
  editing.value = null
  formError.value = null
  drawerOpen.value = true
}

function openEdit(job: Job): void {
  editing.value = job
  formError.value = null
  drawerOpen.value = true
}

async function submitForm(body: CreateJobRequest | UpdateJobRequest): Promise<void> {
  formBusy.value = true
  formError.value = null
  try {
    if (editing.value) {
      await updateJob(editing.value.id, body as UpdateJobRequest)
      toast.success('已保存')
    } else {
      await createJob(body as CreateJobRequest)
      toast.success('任务已创建')
    }
    drawerOpen.value = false
    refreshJobs()
  } catch (err) {
    // 不关抽屉：用户填的表单比一句"再试一次"值钱
    formError.value = describeError(err, '提交失败')
  } finally {
    formBusy.value = false
  }
}

async function createGroupFromForm(name: string): Promise<void> {
  try {
    await createGroup({ name })
    await queryClient.invalidateQueries({ queryKey: queryKeys.groups })
    toast.success(`分组 ${name} 已创建`)
  } catch (err) {
    toast.error(describeError(err, '创建分组失败'))
  }
}

// 二次确认：取消（删记录）与强制暂停（中止当前执行）
type Pending = { kind: 'cancel' | 'force-pause'; job: Job } | null
const pending = ref<Pending>(null)

function askCancel(job: Job): void {
  pending.value = { kind: 'cancel', job }
}

function askForcePause(job: Job): void {
  pending.value = { kind: 'force-pause', job }
}

async function confirmPending(): Promise<void> {
  const target = pending.value
  if (!target) return
  pending.value = null

  if (target.kind === 'cancel') {
    await run(target.job.id, 'cancel', () => cancelJob(target.job.id))
    return
  }
  const ok = await run(target.job.id, 'force-pause', () => forcePauseJob(target.job.id))
  if (ok) toast.info('中止已发起，任务停在 paused 后即为生效')
}

// 批量条
const batchBusy = ref(false)
const moveTarget = ref('')

const selectionSummary = computed(() =>
  selected.value.length === 0 ? '' : `已选 ${selected.value.length} 条`,
)

function reportBatch(action: BatchJobAction, resp: BatchJobOpsResponse): void {
  const label = BATCH_LABELS[action]
  if (resp.failed === 0) {
    toast.success(`${label}：${resp.succeeded} 条成功`)
    selected.value = []
    return
  }
  // 把失败原因摊开：批量最容易出现的是"这条不在待执行队列"。
  // 保留选中，让用户能就剩下这几条再试一次而不是从头再选
  const reasons = resp.errors.map((item) => `${shortJobId(item.id)}：${item.message}`).join('；')
  toast.warning(`${label}：${resp.succeeded} 条成功、${resp.failed} 条未执行。${reasons}`)
}

const BATCH_LABELS: Record<BatchJobAction, string> = {
  cancel: '批量取消',
  pause: '批量暂停',
  'force-pause': '批量强制暂停',
  resume: '批量恢复',
  move: '批量移组',
}

async function runBatch(action: BatchJobAction, group?: string): Promise<void> {
  if (selected.value.length === 0 || batchBusy.value) return
  batchBusy.value = true
  try {
    const resp = await batchJobOps(action, selected.value, group)
    reportBatch(action, resp)
    refreshJobs()
  } catch (err) {
    toast.error(describeError(err, `${BATCH_LABELS[action]} 失败`))
  } finally {
    batchBusy.value = false
  }
}

function onRowAction(job: Job, action: 'pause' | 'resume' | 'retry'): Promise<boolean> {
  const fn = action === 'pause' ? pauseJob : action === 'resume' ? resumeJob : retryJob
  return run(job.id, action, () => fn(job.id))
}
</script>

<template>
  <div class="p-6">
    <PageHeader title="任务" subtitle="筛选、分页与批量操作都落在 GET /jobs 与 POST /jobs/batch-ops 这套已有参数上">
      <template #actions>
        <span class="text-xs tabular-nums text-[var(--color-text-muted)]">{{ pageSummary }}</span>
      </template>
    </PageHeader>

    <section class="mb-4 rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4">
      <JobFilterBar
        :filters="filters"
        :group-names="groupNames"
        :job-types="jobTypesQuery.data.value ?? []"
        :can-create="canWrite"
        :create-hint="writeHint"
        @update:filters="(next) => (filters = next)"
        @create="openCreate"
      />
    </section>

    <section
      v-if="selected.length > 0"
      class="mb-3 flex flex-wrap items-center gap-2 rounded-[var(--radius-card)]
        border border-[var(--color-primary)] bg-[var(--color-primary-soft)] px-4 py-2.5"
    >
      <span class="text-sm font-medium text-[var(--color-primary)]">{{ selectionSummary }}</span>

      <!-- 批量与行内操作同一档位：只置灰行内按钮而放过批量按钮，
           等于给低角色留了一条"点了才知道 403"的旁路 -->
      <UiButton size="sm" variant="outline" :disabled="!canWrite || batchBusy" :title="writeHint" @click="runBatch('pause')">
        批量暂停
      </UiButton>
      <UiButton size="sm" variant="outline" :disabled="!canWrite || batchBusy" :title="writeHint" @click="runBatch('resume')">
        批量恢复
      </UiButton>
      <UiButton size="sm" variant="outline" :disabled="!canWrite || batchBusy" :title="writeHint" @click="runBatch('cancel')">
        批量取消
      </UiButton>

      <div class="ml-auto flex items-end gap-2">
        <UiSelect
          :model-value="moveTarget"
          :options="[{ value: '', label: '选择目标分组…' }, { value: '__none__', label: '移出分组' }, ...groupNames.map((name) => ({ value: name, label: name }))]"
          class="w-44"
          :disabled="!canWrite"
          @update:model-value="moveTarget = $event"
        />
        <UiButton
          size="sm"
          :disabled="!canWrite || batchBusy || moveTarget === ''"
          :title="writeHint"
          @click="runBatch('move', moveTarget === '__none__' ? '' : moveTarget)"
        >
          移入
        </UiButton>
      </div>
    </section>

    <section class="overflow-hidden rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
      <p
        v-if="jobsQuery.error.value"
        role="alert"
        class="border-b border-[var(--color-border)] bg-[#fef2f2] px-4 py-2 text-sm text-[var(--color-status-failed)]"
      >
        任务列表读取失败：{{ describeError(jobsQuery.error.value, '未知错误') }}
      </p>

      <JobTable
        v-if="rows.length > 0"
        :rows="rows"
        :selected="selected"
        :can-write="canWrite"
        :write-hint="writeHint"
        :can-force-pause="canForcePause"
        :force-pause-hint="forcePauseHint"
        :busy="busy"
        @toggle-one="toggleOne"
        @toggle-many="toggleMany"
        @open="(job) => router.push({ name: 'job-detail', params: { id: job.id } })"
        @edit="openEdit"
        @pause="(job) => onRowAction(job, 'pause')"
        @resume="(job) => onRowAction(job, 'resume')"
        @retry="(job) => onRowAction(job, 'retry')"
        @cancel="askCancel"
        @force-pause="askForcePause"
      />

      <UiEmptyState
        v-else-if="!jobsQuery.isPending.value"
        :title="total === 0 ? '还没有任务' : '当前筛选没有命中'"
        :description="
          total === 0
            ? '新建一个任务，或让接入方通过 POST /api/v1/jobs 提交'
            : '换个筛选条件试试；分组与名称下拉的候选来自后端注册表与任务标签'
        "
      />

      <footer class="flex items-center justify-between border-t border-[var(--color-border)] px-4 py-2.5">
        <UiButton size="sm" variant="ghost" :disabled="offset === 0" @click="goPage(offset - PAGE_SIZE)">
          上一页
        </UiButton>
        <span class="text-xs tabular-nums text-[var(--color-text-muted)]">{{ pageSummary }}</span>
        <UiButton
          size="sm"
          variant="ghost"
          :disabled="offset + PAGE_SIZE >= total"
          @click="goPage(offset + PAGE_SIZE)"
        >
          下一页
        </UiButton>
      </footer>
    </section>

    <UiDrawer
      :open="drawerOpen"
      :title="editing ? `编辑任务 ${shortJobId(editing.id)}` : '新建任务'"
      :busy="formBusy"
      @close="drawerOpen = false"
    >
      <JobForm
        :key="editing?.id ?? 'new'"
        :mode="editing ? 'edit' : 'create'"
        :job="editing ?? undefined"
        :job-types="jobTypesQuery.data.value ?? []"
        :group-names="groupNames"
        :busy="formBusy"
        :server-error="formError"
        @submit="submitForm"
        @cancel="drawerOpen = false"
        @create-group="createGroupFromForm"
      />
    </UiDrawer>

    <UiConfirm
      :open="pending !== null"
      :title="pending?.kind === 'force-pause' ? '强制暂停这个任务？' : '取消这个任务？'"
      :confirm-label="pending?.kind === 'force-pause' ? '中止执行' : '确认取消'"
      tone="danger"
      @cancel="pending = null"
      @confirm="confirmPending"
    >
      <template v-if="pending?.kind === 'cancel'">
        取消会<em class="font-medium text-[var(--color-text)]">删除任务记录</em>（含它的历史快照），
        不是暂停。想让任务停一会儿又随时恢复，用"暂停"。
      </template>
      <template v-else>
        强制暂停会中止<em class="font-medium text-[var(--color-text)]">正在执行的这一次尝试</em>，
        不计入失败、不消耗重试次数，任务停在 paused 直到恢复。
        中止要等 Handler 自己返回——这段时间它看起来还在执行中。
      </template>
    </UiConfirm>
  </div>
</template>
