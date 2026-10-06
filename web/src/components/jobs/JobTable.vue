<script setup lang="ts">
/* 任务表格：列与行操作按 §4.6 的口径来，能力的有无一律由调用方按角色传入。
   这里只负责"置灰 + 悬浮说明缺哪一档"，真正拦住越权请求的是服务端 RBAC。 */
import { computed } from 'vue'
import { FileText, Loader, Pencil, RotateCcw, SkipForward, SquarePen, Trash2 } from 'lucide-vue-next'
import JobStatusBadge from './JobStatusBadge.vue'
import type { Job, JobStatus } from '../../api/types'
import { formatDateTime, jobTypeOf, shortJobId } from '../../display'
import { formatCountdown, useNowTick } from '../../composables/useCountdown'

type ActionKey = 'open' | 'edit' | 'pause' | 'resume' | 'cancel' | 'retry' | 'force-pause'

interface RowAction {
  key: ActionKey
  label: string
  icon: typeof FileText
  /** write = operator 档；force = admin 档 */
  tier: 'none' | 'write' | 'force'
  danger?: boolean
}

// 状态决定"能做什么"，与后端 jobOpFailure 的 409 口径一致：
// running 不给 pause（要中断就走 force-pause），paused 只给恢复与取消。
// 详情对所有状态常驻：它是只读入口，viewer 就该能看，不该被状态或角色挡在外面。
const DETAIL: RowAction = { key: 'open', label: '详情', icon: FileText, tier: 'none' }

const ACTIONS: Record<JobStatus, RowAction[]> = {
  pending: [
    DETAIL,
    { key: 'edit', label: '编辑', icon: Pencil, tier: 'write' },
    { key: 'pause', label: '暂停', icon: SkipForward, tier: 'write' },
    { key: 'cancel', label: '取消', icon: Trash2, tier: 'write', danger: true },
  ],
  paused: [
    DETAIL,
    { key: 'resume', label: '恢复', icon: RotateCcw, tier: 'write' },
    { key: 'cancel', label: '取消', icon: Trash2, tier: 'write', danger: true },
  ],
  running: [DETAIL, { key: 'force-pause', label: '强制暂停', icon: SquarePen, tier: 'force' }],
  failed: [DETAIL, { key: 'retry', label: '重试', icon: RotateCcw, tier: 'write' }],
  success: [DETAIL],
  cancelled: [DETAIL],
}

interface Props {
  rows: Job[]
  selected: string[]
  canWrite: boolean
  writeHint: string | null
  canForcePause: boolean
  forcePauseHint: string | null
  /** jobId -> 正在提交的动作，用于禁用重复点击 */
  busy?: Record<string, string>
}

const props = withDefaults(defineProps<Props>(), { busy: () => ({}) })

const emit = defineEmits<{
  'toggle-one': [id: string]
  'toggle-many': [ids: string[], checked: boolean]
  open: [job: Job]
  edit: [job: Job]
  pause: [job: Job]
  resume: [job: Job]
  cancel: [job: Job]
  retry: [job: Job]
  'force-pause': [job: Job]
}>()

const now = useNowTick()

function countdown(job: Job): string | undefined {
  if (job.status !== 'pending') return undefined
  const parsed = new Date(job.trigger_at).getTime()
  if (Number.isNaN(parsed)) return undefined
  return formatCountdown(parsed - now.value)
}

function shortId(id: string): string {
  return shortJobId(id)
}

/** 已到点却没执行：只可能是调度被挂起或正在收尾，直说比显示一个负数有用 */
function countdownText(job: Job): string {
  const text = countdown(job)
  if (!text) return '—'
  return text === '0s' ? '已到点等待调度' : text
}

const allSelected = computed(
  () => props.rows.length > 0 && props.rows.every((job) => props.selected.includes(job.id)),
)

/**
 * 选中状态由父层持有，子组件只报"这条翻面"或"这一页统一成某种状态"。
 * 早先这里是子组件自己算好整个数组再 emit：两次快速点击都读到同一份旧 props，
 * 后一次覆盖前一次，实测表现为勾三行只选中一行。
 */
function toggleAll(): void {
  emit('toggle-many', props.rows.map((job) => job.id), !allSelected.value)
}

function toggleOne(id: string): void {
  emit('toggle-one', id)
}

function enabled(action: RowAction): boolean {
  if (action.tier === 'force') return props.canForcePause
  if (action.tier === 'write') return props.canWrite
  return true
}

function hintFor(action: RowAction): string | undefined {
  if (enabled(action)) return undefined
  return action.tier === 'force' ? (props.forcePauseHint ?? undefined) : (props.writeHint ?? undefined)
}

function dispatch(job: Job, action: RowAction): void {
  if (!enabled(action) || props.busy[job.id]) return
  // 逐条映射而不是把 action.key 断言成 emit 的参数类型：
  // 断言会吞掉"新加一个动作却忘了 emit"这类错误
  switch (action.key) {
    case 'open':
      emit('open', job)
      break
    case 'edit':
      emit('edit', job)
      break
    case 'pause':
      emit('pause', job)
      break
    case 'resume':
      emit('resume', job)
      break
    case 'cancel':
      emit('cancel', job)
      break
    case 'retry':
      emit('retry', job)
      break
    case 'force-pause':
      emit('force-pause', job)
      break
  }
}
</script>

<template>
  <div class="overflow-x-auto">
    <table class="w-full text-sm">
      <thead>
        <tr class="border-b border-[var(--color-border)] text-left text-xs uppercase tracking-wide text-[var(--color-text-muted)]">
          <th class="w-10 px-4 py-2.5">
            <input
              type="checkbox"
              class="accent-[var(--color-primary)]"
              :checked="allSelected"
              aria-label="全选本页"
              @change="toggleAll"
            />
          </th>
          <th class="px-2 py-2.5">ID</th>
          <th class="px-2 py-2.5">名称</th>
          <th class="px-2 py-2.5">类型</th>
          <th class="px-2 py-2.5">分组</th>
          <th class="px-2 py-2.5">状态</th>
          <th class="px-2 py-2.5">触发时间</th>
          <th class="px-2 py-2.5">重试</th>
          <th class="px-2 py-2.5">Cron</th>
          <th class="px-2 py-2.5 text-right">操作</th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="job in rows"
          :key="job.id"
          class="cursor-pointer border-b border-[var(--color-border)] transition-colors hover:bg-[var(--color-primary-soft)]"
          @click="emit('open', job)"
        >
          <td class="px-4 py-2.5" @click.stop>
            <input
              type="checkbox"
              class="accent-[var(--color-primary)]"
              :checked="selected.includes(job.id)"
              :aria-label="`选择任务 ${shortId(job.id)}`"
              @change="toggleOne(job.id)"
            />
          </td>
          <td class="px-2 py-2.5">
            <code class="text-xs text-[var(--color-text-muted)]">{{ shortId(job.id) }}</code>
          </td>
          <td class="px-2 py-2.5 font-medium">{{ job.name }}</td>
          <td class="px-2 py-2.5">
            <!-- 类型是"跑什么"那个注册键；旧写法（请求体没带 type）时名称兼作类型，jobTypeOf 跟着后端回退 -->
            <code class="text-xs">{{ jobTypeOf(job) }}</code>
          </td>
          <td class="px-2 py-2.5">{{ job.group || '—' }}</td>
          <td class="px-2 py-2.5"><JobStatusBadge :status="job.status" /></td>
          <td class="px-2 py-2.5">
            <span class="tabular-nums">{{ formatDateTime(job.trigger_at) }}</span>
            <span v-if="countdown(job)" class="ml-2 text-xs tabular-nums text-[var(--color-text-muted)]">
              {{ countdownText(job) }}
            </span>
          </td>
          <td class="px-2 py-2.5 tabular-nums">{{ job.retry_count }}/{{ job.max_retries }}</td>
          <td class="px-2 py-2.5">
            <code v-if="job.cron_expr" class="text-xs">{{ job.cron_expr }}</code>
            <span v-else class="text-[var(--color-text-muted)]">—</span>
          </td>
          <td class="px-2 py-2.5" @click.stop>
            <div class="flex items-center justify-end gap-1">
              <span
                v-for="action in ACTIONS[job.status] ?? []"
                :key="action.key"
                class="relative"
                :title="hintFor(action)"
              >
                <button
                  type="button"
                  class="flex items-center gap-1 rounded-[var(--radius-control)] px-2 py-1 text-xs transition-colors
                    enabled:hover:bg-[var(--color-surface)]
                    disabled:cursor-not-allowed disabled:text-[var(--color-text-muted)]"
                  :class="action.danger ? 'text-[var(--color-status-failed)]' : 'text-[var(--color-text)]'"
                  :disabled="!enabled(action) || Boolean(busy[job.id])"
                  @click="dispatch(job, action)"
                >
                  <Loader v-if="busy[job.id] === action.key" :size="12" class="animate-spin" aria-hidden="true" />
                  <component :is="action.icon" v-else :size="12" aria-hidden="true" />
                  {{ action.label }}
                </button>
              </span>
            </div>
          </td>
        </tr>

        <tr v-if="rows.length === 0">
          <td colspan="9" class="px-4 py-10 text-center text-sm text-[var(--color-text-muted)]">
            没有符合条件的任务
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
