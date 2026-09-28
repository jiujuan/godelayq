<script setup lang="ts">
/* 实时事件流：直接铺 realtime store 的前端缓冲（最近 200 条，DOM 只渲染 100 条）。
   这一页同时是 M3 实时链路的自检探针——连不上、票据失效、事件不进缓冲，都会立刻看得出来。
   订阅过滤（job_types/event_types 复选）随 M4 补齐。 */
import { computed } from 'vue'
import { BellOff } from 'lucide-vue-next'
import PageHeader from '../components/layout/PageHeader.vue'
import UiBadge from '../components/ui/UiBadge.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
import { useRealtimeStore } from '../stores/realtime'
import type { JobEvent } from '../api/types'

const DOM_LIMIT = 100

const realtime = useRealtimeStore()

const shown = computed(() => realtime.events.slice(0, DOM_LIMIT))

/** 事件类型 → 徽标底色：只看"是不是终态"，与后端状态色表同源 */
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

function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id
}

function timeOf(event: JobEvent): string {
  const parsed = new Date(event.timestamp)
  return Number.isNaN(parsed.getTime()) ? event.timestamp : parsed.toLocaleTimeString()
}

function summarize(event: JobEvent): string {
  const metadata = event.metadata ?? {}
  const parts = Object.entries(metadata)
    .filter(([, value]) => value !== undefined && value !== null)
    .map(([key, value]) => `${key}=${String(value)}`)
  return parts.join(' · ')
}
</script>

<template>
  <div class="p-6">
    <PageHeader title="实时事件流" subtitle="前端内存缓冲的最近事件；刷新页面即清空，从后端缓冲回填历史排在 M4">
      <template #actions>
        <UiBadge tone="neutral">{{ realtime.events.length }} 条已缓冲</UiBadge>
      </template>
    </PageHeader>

    <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
      <UiEmptyState
        v-if="shown.length === 0"
        title="还没有收到事件"
        :description="
          realtime.isConnected
            ? '连接正常。调度到有任务触发时这里才会出内容'
            : '实时通道尚未就绪，可在顶栏查看连接状态'
        "
      >
        <template #icon>
          <BellOff class="text-[var(--color-text-muted)]" :size="24" aria-hidden="true" />
        </template>
      </UiEmptyState>

      <ul v-else class="divide-y divide-[var(--color-border)]">
        <li v-for="event in shown" :key="`${event.timestamp}-${event.job_id}-${event.type}`" class="flex flex-wrap items-center gap-3 px-4 py-3">
          <UiBadge :tone="toneOf(event)">{{ event.type }}</UiBadge>
          <span class="text-sm font-medium">{{ event.job_name }}</span>
          <code class="text-xs text-[var(--color-text-muted)]">{{ shortId(event.job_id) }}</code>
          <span class="ml-auto text-xs tabular-nums text-[var(--color-text-muted)]">{{ timeOf(event) }}</span>
          <p v-if="summarize(event)" class="w-full text-xs text-[var(--color-text-muted)]">
            {{ summarize(event) }}
          </p>
        </li>
      </ul>
    </section>
  </div>
</template>
