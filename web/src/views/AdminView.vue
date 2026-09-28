<script setup lang="ts">
/* 运维页（§4.6 AdminView，仅 ops）。
 *
 * 这里的三个动作改的都是整个进程的行为，不是某条任务：
 * 挂起调度、恢复调度、清空事件缓冲。所以每个动作都带二次确认，
 * 并且把"重启即自动解除"这类边界直接写在按钮旁边。 */
import { computed, ref } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { Pause, Play, Trash2 } from 'lucide-vue-next'
import PageHeader from '../components/layout/PageHeader.vue'
import UiBadge from '../components/ui/UiBadge.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiConfirm from '../components/ui/UiConfirm.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
import { clearEventHistory, fetchRuntime, suspendScheduler, unsuspendScheduler } from '../api/admin'
import { ApiError } from '../api/client'
import { queryKeys } from '../api/keys'
import { useToastStore } from '../stores/toast'
import { formatDateTime } from '../display'

const toast = useToastStore()
const queryClient = useQueryClient()

const runtimeQuery = useQuery({
  queryKey: queryKeys.runtime,
  queryFn: fetchRuntime,
  // 诊断值本来就是"此刻的占用"，5s 一次够看趋势，又不至于变成自压测
  refetchInterval: 5_000,
})

const runtime = computed(() => runtimeQuery.data.value ?? null)
const scheduler = computed(() => runtime.value?.scheduler ?? null)

const busy = ref('')
const confirmKind = ref<'suspend' | 'unsuspend' | 'clear' | null>(null)

function describeError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.details ? `${err.message}：${err.details}` : err.message
  return fallback
}

async function run(kind: string, fn: () => Promise<unknown>, done: string): Promise<void> {
  busy.value = kind
  try {
    await fn()
    toast.success(done)
    await queryClient.invalidateQueries({ queryKey: queryKeys.runtime })
    // 挂起与否会改变 /stats 的 scheduling_suspended，横幅读的是它
    await queryClient.invalidateQueries({ queryKey: queryKeys.stats })
  } catch (err) {
    toast.error(describeError(err, '操作失败'))
  } finally {
    busy.value = ''
  }
}

async function confirmAction(): Promise<void> {
  const kind = confirmKind.value
  confirmKind.value = null
  if (!kind) return

  if (kind === 'suspend') {
    await run('suspend', () => suspendScheduler(), '调度已挂起：到点任务不再弹出')
    return
  }
  if (kind === 'unsuspend') {
    await run('unsuspend', () => unsuspendScheduler(), '调度已恢复：到点任务按原时间补跑')
    return
  }
  const resp = await clearEventHistory().catch((err) => {
    toast.error(describeError(err, '清空失败'))
    return null
  })
  if (resp) {
    toast.success(`已清空事件缓冲（${resp.cleared} 条）`)
    await queryClient.invalidateQueries({ queryKey: queryKeys.runtime })
  }
}

const occupancy = computed(() => {
  const current = scheduler.value
  if (!current) return []
  return [
    { label: '并发 worker', value: current.workers },
    { label: '执行队列', value: `${current.queue_length}/${current.queue_capacity}` },
    { label: '执行中', value: current.running },
    { label: '堆中等待', value: current.heap_size },
    { label: '待认领的强制暂停', value: current.force_pause_pending },
    { label: '调度循环', value: current.started ? '运行中' : '未启动' },
  ]
})

const buffer = computed(() => {
  const history = runtime.value?.event_history
  if (!history) return []
  return [
    { label: '缓冲事件总数', value: history.events },
    { label: '有记录的任务数', value: history.jobs },
    { label: '全局窗口', value: history.global_capacity },
    { label: '单任务窗口', value: history.per_job_capacity },
  ]
})
</script>

<template>
  <div class="p-6">
    <PageHeader title="运维" subtitle="进程级开关与只读诊断；改不动 worker 数与队列容量（那些在配置文件里）">
      <template #actions>
        <UiBadge :tone="runtime?.scheduler.suspended ? 'warning' : 'success'">
          {{ runtime?.scheduler.suspended ? '调度已挂起' : '调度正常' }}
        </UiBadge>
      </template>
    </PageHeader>

    <p
      v-if="runtimeQuery.error.value"
      role="alert"
      class="mb-4 rounded-[var(--radius-control)] border border-[var(--color-status-failed)]
        bg-[#fef2f2] px-3 py-2 text-sm text-[var(--color-status-failed)]"
    >
      诊断读取失败：{{ describeError(runtimeQuery.error.value, '未知错误') }}
    </p>

    <section class="mb-4 rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4">
      <h2 class="mb-2 text-sm font-semibold">调度总开关</h2>
      <p class="mb-4 text-sm leading-relaxed text-[var(--color-text-muted)]">
        挂起只停"把到点任务取出来执行"这一步：正在跑的任务照常跑完，堆与存储都不动，
        挂起期间提交的新任务照常进堆。标志位是进程内的，<em class="font-medium text-[var(--color-text)]">重启自动解除</em>。
      </p>

      <div class="flex flex-wrap gap-2">
        <UiButton
          v-if="!runtime?.scheduler.suspended"
          variant="danger"
          :disabled="busy !== ''"
          :loading="busy === 'suspend'"
          @click="confirmKind = 'suspend'"
        >
          <Pause :size="14" aria-hidden="true" />
          挂起调度
        </UiButton>
        <UiButton
          v-else
          :disabled="busy !== ''"
          :loading="busy === 'unsuspend'"
          @click="confirmKind = 'unsuspend'"
        >
          <Play :size="14" aria-hidden="true" />
          恢复调度
        </UiButton>

        <UiButton variant="outline" :disabled="busy !== ''" @click="confirmKind = 'clear'">
          <Trash2 :size="14" aria-hidden="true" />
          清空事件缓冲
        </UiButton>
      </div>
    </section>

    <div class="grid gap-4 lg:grid-cols-2">
      <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4">
        <h2 class="mb-3 text-sm font-semibold">运行时占用</h2>

        <UiEmptyState
          v-if="!runtime && !runtimeQuery.isPending.value"
          title="没有取到诊断数据"
          description="这一页只给 ops 角色，低档位会被服务端拦下"
        />

        <dl v-else class="grid grid-cols-2 gap-x-4 gap-y-2.5 text-sm">
          <div v-for="item in occupancy" :key="item.label" class="flex flex-col">
            <dt class="text-xs text-[var(--color-text-muted)]">{{ item.label }}</dt>
            <dd class="tabular-nums">{{ item.value }}</dd>
          </div>
          <div class="flex flex-col">
            <dt class="text-xs text-[var(--color-text-muted)]">进程启动于</dt>
            <dd>{{ runtime ? formatDateTime(runtime.started_at) : '—' }}</dd>
          </div>
          <div class="flex flex-col">
            <dt class="text-xs text-[var(--color-text-muted)]">已运行</dt>
            <dd class="tabular-nums">{{ runtime?.uptime ?? '—' }}</dd>
          </div>
        </dl>

        <p
          v-if="runtime?.scheduling_suspended_note"
          class="mt-3 rounded-[var(--radius-control)] border border-[var(--color-status-running)] bg-[#fffbeb] px-3 py-2 text-xs"
        >
          {{ runtime.scheduling_suspended_note }}
        </p>
      </section>

      <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4">
        <h2 class="mb-3 text-sm font-semibold">事件缓冲</h2>
        <dl class="grid grid-cols-2 gap-x-4 gap-y-2.5 text-sm">
          <div v-for="item in buffer" :key="item.label" class="flex flex-col">
            <dt class="text-xs text-[var(--color-text-muted)]">{{ item.label }}</dt>
            <dd class="tabular-nums">{{ item.value }}</dd>
          </div>
        </dl>
        <p class="mt-3 text-xs leading-relaxed text-[var(--color-text-muted)]">
          缓冲是内存里的，进程重启即清空；它是详情页时间线的数据源，不是审计日志。
          清空后各详情页从当前时刻重新开始，任务本身的历史记录不受影响。
        </p>
      </section>
    </div>

    <UiConfirm
      :open="confirmKind !== null"
      :title="
        confirmKind === 'suspend'
          ? '挂起整个进程的调度？'
          : confirmKind === 'unsuspend'
            ? '恢复调度？'
            : '清空事件缓冲？'
      "
      :confirm-label="confirmKind === 'clear' ? '清空' : '确定'"
      :tone="confirmKind === 'unsuspend' ? 'primary' : 'danger'"
      :busy="busy !== ''"
      @cancel="confirmKind = null"
      @confirm="confirmAction"
    >
      <template v-if="confirmKind === 'suspend'">
        挂起后<em class="font-medium text-[var(--color-text)]">所有到点任务都不再弹出</em>，
        影响的是整个进程，不是某一条任务；控制台顶栏会一直显示"调度已挂起"提醒所有人。
      </template>
      <template v-else-if="confirmKind === 'unsuspend'">
        恢复后到点的任务按原时间补跑，已过期的一次性任务会立即执行一次。
      </template>
      <template v-else>
        当前缓冲
        <em class="font-medium text-[var(--color-text)]">{{ runtime?.event_history.events ?? 0 }}</em>
        条事件会被丢弃，任务详情页的时间线从此刻重新开始记录。
      </template>
    </UiConfirm>
  </div>
</template>
