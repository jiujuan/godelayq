<script setup lang="ts">
/* 实时事件流（§4.6 MonitorView，承接旧 dashboard 的能力）。
 *
 * 过滤走服务端订阅（realtime store 发 subscribe 帧）：不匹配的事件不会被推过来。
 * 视图同时再判一次谓词，因为首屏回填与已到达的缓冲不受那次订阅约束——
 * 少了这一步，同一个筛选会给出两套结果。 */
import { computed, onScopeDispose, ref, watch } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { BellOff } from 'lucide-vue-next'
import PageHeader from '../components/layout/PageHeader.vue'
import UiBadge from '../components/ui/UiBadge.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
import { fetchJobTypes } from '../api/jobs'
import { queryKeys } from '../api/keys'
import { useEventFeed } from '../composables/useEventFeed'
import { useRealtimeStore } from '../stores/realtime'
import { formatClockTime, shortJobId } from '../display'
import type { JobEvent } from '../api/types'

const DOM_LIMIT = 100

/** 与 core/event.go 的常量对应；写死而不是从事件里收集，
    否则刚重启时一个候选都列不出来 */
const EVENT_TYPES = [
  'job.scheduled',
  'job.started',
  'job.completed',
  'job.failed',
  'job.cancelled',
  'job.retrying',
  'job.paused',
  'job.resumed',
]

const realtime = useRealtimeStore()
const queryClient = useQueryClient()

const pickedEvents = ref<string[]>([])
const pickedJobTypes = ref<string[]>([])

const jobTypesQuery = useQuery({
  queryKey: queryKeys.jobTypes,
  queryFn: fetchJobTypes,
  staleTime: 5 * 60_000,
})

function predicate(event: JobEvent): boolean {
  if (pickedEvents.value.length > 0 && !pickedEvents.value.includes(event.type)) return false
  if (pickedJobTypes.value.length > 0 && !pickedJobTypes.value.includes(event.job_name)) return false
  return true
}

const { events, note, isPending } = useEventFeed(100, () => predicate)

const shown = computed(() => events.value.slice(0, DOM_LIMIT))

function toggleIn(list: string[], value: string): string[] {
  return list.includes(value) ? list.filter((item) => item !== value) : [...list, value]
}

function clearFilters(): void {
  pickedEvents.value = []
  pickedJobTypes.value = []
}

// 订阅帧只在变化时发；两个维度都清空就等于恢复全量。
// 同时把首屏那次回填作废：过滤期间服务端根本没推这些事件，
// 不重取一次首屏的话，"清除筛选"会让人以为事件凭空消失了（实测踩过）。
watch([pickedEvents, pickedJobTypes], ([eventTypes, jobTypes]) => {
  realtime.setFilter({ jobTypes, eventTypes })
  void queryClient.invalidateQueries({ queryKey: queryKeys.eventsRecent })
})

onScopeDispose(() => {
  // 离开页面就恢复全量订阅：这条连接是全局的，不能让一个页面把别人饿着
  realtime.setFilter({ jobTypes: [], eventTypes: [] })
})

function toneOf(event: JobEvent): 'success' | 'warning' | 'danger' | 'primary' | 'neutral' {
  switch (event.type) {
    case 'job.completed':
      return 'success'
    case 'job.failed':
      return 'danger'
    case 'job.started':
    case 'job.retrying':
      return 'warning'
    case 'job.paused':
    case 'job.resumed':
      return 'primary'
    default:
      return 'neutral'
  }
}

function summarize(event: JobEvent): string {
  const metadata = event.metadata ?? {}
  return Object.entries(metadata)
    .filter(([, value]) => value !== undefined && value !== null)
    .map(([key, value]) => `${key}=${String(value)}`)
    .join(' · ')
}
</script>

<template>
  <div class="p-6">
    <PageHeader
      title="实时事件流"
      :subtitle="note ? `首屏取自后端缓冲（${note}），此后由 WS 续上` : '首屏取自后端缓冲，此后由 WS 续上'"
    >
      <template #actions>
        <UiBadge tone="neutral">{{ events.length }} 条</UiBadge>
      </template>
    </PageHeader>

    <section class="mb-4 flex flex-col gap-3 rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4">
      <div class="flex flex-wrap items-center gap-2">
        <span class="text-xs font-medium uppercase tracking-wide text-[var(--color-text-muted)]">事件类型</span>
        <button
          v-for="type in EVENT_TYPES"
          :key="type"
          type="button"
          class="rounded-[var(--radius-pill)] border px-2.5 py-0.5 text-xs transition-colors"
          :class="pickedEvents.includes(type)
            ? 'border-[var(--color-primary)] bg-[var(--color-primary-soft)] text-[var(--color-primary)]'
            : 'border-[var(--color-border)] hover:border-[var(--color-primary)]'"
          :aria-pressed="pickedEvents.includes(type)"
          @click="pickedEvents = toggleIn(pickedEvents, type)"
        >
          {{ type }}
        </button>
      </div>

      <div class="flex flex-wrap items-center gap-2">
        <span class="text-xs font-medium uppercase tracking-wide text-[var(--color-text-muted)]">任务类型</span>
        <button
          v-for="type in jobTypesQuery.data.value ?? []"
          :key="type"
          type="button"
          class="rounded-[var(--radius-pill)] border px-2.5 py-0.5 text-xs transition-colors"
          :class="pickedJobTypes.includes(type)
            ? 'border-[var(--color-primary)] bg-[var(--color-primary-soft)] text-[var(--color-primary)]'
            : 'border-[var(--color-border)] hover:border-[var(--color-primary)]'"
          :aria-pressed="pickedJobTypes.includes(type)"
          @click="pickedJobTypes = toggleIn(pickedJobTypes, type)"
        >
          {{ type }}
        </button>
      </div>

      <div v-if="pickedEvents.length > 0 || pickedJobTypes.length > 0" class="self-start">
        <UiButton
          size="sm"
          variant="ghost"
          @click="clearFilters"
        >
          清除筛选（恢复全量订阅）
        </UiButton>
      </div>
    </section>

    <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
      <UiEmptyState
        v-if="shown.length === 0"
        :title="pickedEvents.length + pickedJobTypes.length > 0 ? '当前筛选没有命中' : '还没有收到事件'"
        :description="
          pickedEvents.length + pickedJobTypes.length > 0
            ? '筛选同时作用于已回填的历史与实时流'
            : isPending
              ? '正在读取后端事件缓冲'
              : '连接正常。调度到有任务触发时这里才会出内容'
        "
      >
        <template #icon>
          <BellOff class="text-[var(--color-text-muted)]" :size="24" aria-hidden="true" />
        </template>
      </UiEmptyState>

      <ul v-else class="divide-y divide-[var(--color-border)]">
        <li
          v-for="event in shown"
          :key="`${event.timestamp}-${event.job_id}-${event.type}`"
          class="flex flex-wrap items-center gap-3 px-4 py-3"
        >
          <UiBadge :tone="toneOf(event)">{{ event.type }}</UiBadge>
          <span class="text-sm font-medium">{{ event.job_name }}</span>
          <code class="text-xs text-[var(--color-text-muted)]">{{ shortJobId(event.job_id) }}</code>
          <span class="ml-auto text-xs tabular-nums text-[var(--color-text-muted)]">
            {{ formatClockTime(event.timestamp) }}
          </span>
          <p v-if="summarize(event)" class="w-full text-xs text-[var(--color-text-muted)]">
            {{ summarize(event) }}
          </p>
        </li>
      </ul>

      <p
        v-if="events.length > DOM_LIMIT"
        class="border-t border-[var(--color-border)] px-4 py-2 text-xs text-[var(--color-text-muted)]"
      >
        只渲染最近 {{ DOM_LIMIT }} 条（命中 {{ events.length }} 条）
      </p>
    </section>
  </div>
</template>
