<script setup lang="ts">
/* 概览页：统计卡（REST + 事件驱动失效）、分组健康度、最近事件三块。
   三块共用同一套 Query 键，所以在这一页看到的数字与列表页必然同源。 */
import { computed } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { AlertTriangle, CircleCheck, Clock, HardDrive, PauseCircle, Play } from 'lucide-vue-next'
import PageHeader from '../components/layout/PageHeader.vue'
import StatCard from '../components/dashboard/StatCard.vue'
import UiBadge from '../components/ui/UiBadge.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
import { listGroups } from '../api/groups'
import { queryKeys } from '../api/keys'
import { fetchStats } from '../api/stats'
import { useEventFeed } from '../composables/useEventFeed'
import { formatClockTime, shortJobId } from '../display'

const { data, isPending, error } = useQuery({
  queryKey: queryKeys.stats,
  queryFn: fetchStats,
  // 轮询兜底交给 Topbar（它每个页面都挂着，同一个 key 只需一份定时器）：
  // 这里再声明一次 refetchInterval 会让同一把钥匙被两个观察者各敲一遍，
  // 请求量凭空翻倍却拿不到更多新鲜度。实时性本来就靠事件驱动的失效。
})

const groupsQuery = useQuery({ queryKey: queryKeys.groups, queryFn: listGroups })
const groups = computed(() => groupsQuery.data.value ?? [])
const groupsPending = computed(() => groupsQuery.isPending.value)
const groupError = computed(() => groupsQuery.error.value)

// 事件流面板只要最近几条可读，完整流与筛选在实时页
const { events: feed, isPending: feedPending } = useEventFeed(20)
</script>

<template>
  <div class="p-6">
    <PageHeader title="概览" subtitle="任务数量取自存储快照，执行中与堆长度为实时值" />

    <p
      v-if="error"
      class="mb-4 flex items-center gap-2 rounded-[var(--radius-card)] border border-[var(--color-status-failed)] px-3 py-2 text-sm text-[var(--color-text)]"
    >
      <AlertTriangle :size="16" aria-hidden="true" />
      统计读取失败：{{ error.message }}
    </p>

    <div class="grid grid-cols-2 gap-4 md:grid-cols-3 xl:grid-cols-6">
      <StatCard label="待执行" :value="data?.pending ?? 0" :icon="Clock" :loading="isPending" />
      <StatCard label="执行中" :value="data?.running ?? 0" :icon="Play" :loading="isPending" />
      <StatCard
        label="已暂停"
        :value="data?.paused ?? 0"
        :icon="PauseCircle"
        accent="var(--color-status-paused)"
        :loading="isPending"
      />
      <StatCard label="已完成" :value="data?.completed ?? 0" :icon="CircleCheck" :loading="isPending" />
      <StatCard
        label="失败"
        :value="data?.failed ?? 0"
        :icon="AlertTriangle"
        accent="var(--color-status-failed)"
        :loading="isPending"
      />
      <StatCard
        label="堆中等待"
        :value="data?.heap_size ?? 0"
        :icon="HardDrive"
        :loading="isPending"
      />
    </div>

    <p v-if="data" class="mt-3 text-xs text-[var(--color-text-muted)]">
      进程已运行 {{ data.uptime }}
    </p>

    <div class="mt-8 grid gap-4 lg:grid-cols-2">
      <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
        <header class="flex items-center justify-between border-b border-[var(--color-border)] px-4 py-2.5">
          <h2 class="text-sm font-semibold">分组健康度</h2>
          <router-link
            to="/groups"
            class="text-xs text-[var(--color-primary)] hover:underline"
          >
            管理分组
          </router-link>
        </header>

        <UiEmptyState
          v-if="groupError"
          :title="`分组读取失败：${groupError.message}`"
          description="这一栏只影响概览，任务列表与分组页各自还会再取一次"
        />
        <UiEmptyState
          v-else-if="groups.length === 0 && !groupsPending"
          title="还没有分组"
          description="分组的 job_count 按实际任务标签统计，只在有任务挂着时出现"
        />

        <ul v-else class="divide-y divide-[var(--color-border)]">
          <li v-for="group in groups" :key="group.name">
            <router-link
              :to="{ name: 'jobs', query: { group: group.name } }"
              class="flex items-center gap-3 px-4 py-2.5 text-sm transition-colors hover:bg-[var(--color-surface)]"
            >
              <span
                class="h-2.5 w-2.5 shrink-0 rounded-full"
                :style="group.color ? { backgroundColor: group.color } : { border: '1px solid var(--color-border)' }"
              ></span>
              <span class="min-w-0 flex-1 truncate">{{ group.name }}</span>
              <span class="text-xs tabular-nums text-[var(--color-text-muted)]">{{ group.job_count }} 条</span>
              <UiBadge v-if="group.paused_count > 0" tone="primary">{{ group.paused_count }} 已暂停</UiBadge>
              <UiBadge v-if="!group.registered" tone="warning">未注册</UiBadge>
            </router-link>
          </li>
        </ul>
      </section>

      <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
        <header class="flex items-center justify-between border-b border-[var(--color-border)] px-4 py-2.5">
          <h2 class="text-sm font-semibold">实时事件流</h2>
          <router-link to="/monitor" class="text-xs text-[var(--color-primary)] hover:underline">
            查看全部
          </router-link>
        </header>

        <UiEmptyState
          v-if="feed.length === 0"
          :title="feedPending ? '正在读取事件缓冲' : '还没有事件'"
          description="最近几条在这里，完整流与筛选在实时页"
        />

        <ul v-else class="divide-y divide-[var(--color-border)]">
          <li
            v-for="event in feed.slice(0, 6)"
            :key="`${event.timestamp}-${event.job_id}-${event.type}`"
            class="flex items-center gap-2 px-4 py-2.5 text-sm"
          >
            <UiBadge :tone="event.type === 'job.failed' ? 'danger' : event.type === 'job.completed' ? 'success' : 'neutral'">
              {{ event.type }}
            </UiBadge>
            <span class="min-w-0 flex-1 truncate">{{ event.job_name }}</span>
            <code class="text-xs text-[var(--color-text-muted)]">{{ shortJobId(event.job_id) }}</code>
            <span class="text-xs tabular-nums text-[var(--color-text-muted)]">
              {{ formatClockTime(event.timestamp) }}
            </span>
          </li>
        </ul>
      </section>
    </div>
  </div>
</template>
