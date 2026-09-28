<script setup lang="ts">
/* 概览页：先把"统计卡"这一条链路打通（登录 → REST 查询 → WS 事件触发失效 → 数字变动），
   它是 M3 骨架能否端到端验证的探针。分组健康度与事件流两个面板随 M4 落地。 */
import { useQuery } from '@tanstack/vue-query'
import { AlertTriangle, CircleCheck, Clock, HardDrive, PauseCircle, Play } from 'lucide-vue-next'
import PageHeader from '../components/layout/PageHeader.vue'
import StatCard from '../components/dashboard/StatCard.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
import { queryKeys } from '../api/keys'
import { fetchStats } from '../api/stats'

const { data, isPending, error } = useQuery({
  queryKey: queryKeys.stats,
  queryFn: fetchStats,
  // 5s 轮询是 WS 断线期间的兜底（§4.6），实时性主要靠事件驱动的失效
  refetchInterval: 5_000,
})
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
        <UiEmptyState title="分组健康度" description="每组 pending/failed 计数，随 M4 的分组视图一起落地" />
      </section>
      <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
        <UiEmptyState title="实时事件流" description="前端缓冲的最近事件，随 M4 的 Monitor 面板落地" />
      </section>
    </div>
  </div>
</template>
