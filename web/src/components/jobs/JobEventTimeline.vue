<script setup lang="ts">
/* 运行时间线：一个节点 = 一条事件。
 *
 * 事件类型与 core/event.go 的常量一一对应；新增类型时这里落到"未知"分支，
 * 不会因为不认识就把它藏起来——藏起来的那条往往正是排查要看的。 */
import { computed } from 'vue'
import {
  Ban,
  CheckCircle2,
  CirclePlay,
  Clock,
  PauseCircle,
  RotateCcw,
  TriangleAlert,
  XCircle,
  type LucideIcon,
} from 'lucide-vue-next'
import UiBadge from '../ui/UiBadge.vue'
import { eventExec, execSummary, formatDateTime } from '../../display'
import type { JobEvent } from '../../api/types'

type Tone = 'primary' | 'success' | 'warning' | 'danger' | 'neutral'

const SHAPE: Record<string, { icon: LucideIcon; tone: Tone; label: string }> = {
  'job.scheduled': { icon: Clock, tone: 'neutral', label: '已排期' },
  'job.started': { icon: CirclePlay, tone: 'primary', label: '开始执行' },
  'job.completed': { icon: CheckCircle2, tone: 'success', label: '执行成功' },
  'job.failed': { icon: XCircle, tone: 'danger', label: '执行失败' },
  'job.cancelled': { icon: Ban, tone: 'neutral', label: '已取消' },
  'job.retrying': { icon: RotateCcw, tone: 'warning', label: '等待重试' },
  'job.paused': { icon: PauseCircle, tone: 'primary', label: '已暂停' },
  'job.resumed': { icon: CirclePlay, tone: 'primary', label: '已恢复' },
}

/** 这些键有专门的说法，就不该以原始的 key=value 出现在时间线上 */
const HANDLED_KEYS = new Set([
  'attempt',
  'duration_ms',
  'is_repeat',
  'next_retry_at',
  'timeout',
  'trigger_at',
  'was_paused',
])

const props = defineProps<{ events: JobEvent[] }>()

const nodes = computed(() =>
  props.events.map((event) => {
    const shape = SHAPE[event.type] ?? { icon: TriangleAlert, tone: 'neutral' as Tone, label: event.type }
    const meta = event.metadata ?? {}
    const bits: string[] = []

    if (typeof meta.attempt === 'number') bits.push(`第 ${meta.attempt + 1} 次尝试`)
    if (typeof meta.duration_ms === 'number') bits.push(`耗时 ${meta.duration_ms}ms`)
    if (meta.timeout === true) bits.push('超时中止')
    if (meta.was_paused === true) bits.push('原本处于暂停')
    if (typeof meta.next_retry_at === 'string') bits.push(`下次重试 ${formatDateTime(meta.next_retry_at)}`)

    // 执行结论（TASK-E18 §3.4 第 1 条）：job.completed / job.failed 的 data 里带 result 时补一行短摘要。
    // 只写结论与耗时，不写输出预览：预览按字节上限也可能有几百字符，一行摘要放不下，
    // 完整内容在详情页的执行结果区块里看。
    const summary = eventExec(event)
    if (summary) bits.push(execSummary(summary))

    // 不认识的键照样露出来，只是按原样写：隐藏未知字段等于丢线索
    for (const [key, value] of Object.entries(meta)) {
      if (!HANDLED_KEYS.has(key) && value !== undefined && value !== null) bits.push(`${key}=${String(value)}`)
    }

    // 事件的 data 有两种形状：早年的取消/无 handler 那几条是纯字符串，
    // 有执行结论之后是 {error, result} 这样的对象。两种都要认，
    // 否则对象形的错误正文会在页面上直接消失。
    const detail = eventDetail(event)

    return { event, icon: shape.icon, tone: shape.tone, label: shape.label, bits, detail }
  }),
)

function eventDetail(event: JobEvent): string {
  if (typeof event.data === 'string') return event.data
  if (event.data && typeof event.data === 'object') {
    const error = (event.data as { error?: unknown }).error
    if (typeof error === 'string') return error
  }
  return ''
}
</script>

<template>
  <ol v-if="nodes.length > 0" class="flex flex-col gap-0">
    <li v-for="node in nodes" :key="`${node.event.timestamp}-${node.event.type}`" class="flex gap-3 pb-4">
      <div class="flex flex-col items-center">
        <component :is="node.icon" :size="16" class="mt-0.5" aria-hidden="true" />
        <span class="mt-1 w-px flex-1 bg-[var(--color-border)]"></span>
      </div>

      <div class="min-w-0 flex-1">
        <div class="flex flex-wrap items-center gap-2">
          <UiBadge :tone="node.tone">{{ node.event.type }}</UiBadge>
          <span class="text-sm">{{ node.label }}</span>
          <span class="ml-auto text-xs tabular-nums text-[var(--color-text-muted)]">
            {{ formatDateTime(node.event.timestamp) }}
          </span>
        </div>
        <p v-if="node.bits.length > 0" class="mt-1 break-words text-xs text-[var(--color-text-muted)]">
          {{ node.bits.join(' · ') }}
        </p>
        <p v-if="node.detail" class="mt-1 break-all text-xs text-[var(--color-status-failed)]">
          {{ node.detail }}
        </p>
      </div>
    </li>
  </ol>
</template>
