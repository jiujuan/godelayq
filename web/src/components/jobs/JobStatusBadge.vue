<script setup lang="ts">
/* 状态徽标：§4.3 状态色表的唯一实现处。
   颜色与图标都从这一张表出，页面里不许再出现第二套 status→样式的映射，
   否则同一状态在列表和详情里长得不一样，用户会以为是两种东西。 */
import { computed } from 'vue'
import {
  Ban,
  CheckCircle2,
  Clock,
  LoaderCircle,
  PauseCircle,
  XCircle,
  type LucideIcon,
} from 'lucide-vue-next'
import type { JobStatus } from '../../api/types'

const MAP: Record<JobStatus, { icon: LucideIcon; color: string; spin?: boolean }> = {
  pending: { icon: Clock, color: 'var(--color-status-pending)' },
  running: { icon: LoaderCircle, color: 'var(--color-status-running)', spin: true },
  success: { icon: CheckCircle2, color: 'var(--color-status-success)' },
  failed: { icon: XCircle, color: 'var(--color-status-failed)' },
  cancelled: { icon: Ban, color: 'var(--color-status-cancelled)' },
  paused: { icon: PauseCircle, color: 'var(--color-status-paused)' },
}

const LABEL: Record<JobStatus, string> = {
  pending: '待执行',
  running: '执行中',
  success: '已完成',
  failed: '失败',
  cancelled: '已取消',
  paused: '已暂停',
}

const props = defineProps<{ status: JobStatus }>()

const entry = computed(() => MAP[props.status] ?? MAP.cancelled)
const label = computed(() => LABEL[props.status] ?? props.status)
</script>

<template>
  <span class="inline-flex items-center gap-1.5 text-sm" :style="{ color: entry.color }">
    <component :is="entry.icon" :size="14" :class="entry.spin ? 'animate-spin' : ''" aria-hidden="true" />
    {{ label }}
    <span class="sr-only">（{{ status }}）</span>
  </span>
</template>
