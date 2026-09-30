<script setup lang="ts">
/* 执行结果区块（TASK-E18 §3.2）。
 *
 * 两条形状上的约定：
 *   1. 只有 job.exec 存在才渲染——普通任务与"还没执行完"的任务都没有这个键，
 *      渲染出一张空卡片会让人以为执行器坏了；
 *   2. 输出正文是点标签页才取的：GET /jobs/:id/result 读的是磁盘文件、响应不缓存
 *      （后端带 Cache-Control: no-store），首屏拉一次等于给每个打开的详情页加一次磁盘读。 */
import { computed, ref, watch } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { ChevronDown } from 'lucide-vue-next'
import UiBadge from '../ui/UiBadge.vue'
import UiButton from '../ui/UiButton.vue'
import UiSelect from '../ui/UiSelect.vue'
import { getJobResult, listExecutors } from '../../api/executors'
import { queryKeys } from '../../api/keys'
import { ApiError } from '../../api/client'
import { execOutcome, execSummary, eventExec, formatDurationMs, sanitizeOutput } from '../../display'
import type { Job, JobEvent } from '../../api/types'

const props = defineProps<{ job: Job; events: JobEvent[] }>()

/** max_bytes 的"不要带"在前端内部的记号：后端缺省取预览上限的 4 倍，我们不复制那个数 */
const NO_BUDGET = 0

type Stream = 'out' | 'err'
type Direction = 'head' | 'tail'

/** 已展开的那一格；null 表示还没点过任何一个标签页，于是一次请求都没发 */
const opened = ref<{ stream: Stream; from: Direction; maxBytes: number } | null>(null)

const attemptCount = computed(() => Math.max(props.job.attempts, 1))
const attempt = ref(String(Math.max(props.job.attempts, 1)))

// 同一条路由记录会在 /jobs/a → /jobs/b 之间复用这个实例，换任务时必须把面板与尝试复位，
// 否则 B 任务会显示 A 任务的输出格、或停在一个 B 根本没有的尝试编号上
watch(
  () => props.job.id,
  () => {
    opened.value = null
    attempt.value = String(Math.max(props.job.attempts, 1))
  },
)

const exec = computed(() => props.job.exec ?? null)
const purged = computed(() => exec.value?.artifact === 'purged')

/**
 * 读输出的建议起点，来自 GET /executors 的 preferred_result_direction。
 *
 * 选"后端给"而不是"前端按 kind 判"（卡片 §3.2 第 2 条要求二选一）：这条规则的真身在
 * executor.PreferredResultDirection 里，http 的结论在响应体开头、进程档位的结论在末尾，
 * 前端再判一次就是一份会漂移的副本。档位已经从配置里删掉时这里退回 tail——
 * 那种情况下任务快照里的 exec 还在，读尾部至少不是更差的选择。
 */
const profilesQuery = useQuery({
  queryKey: queryKeys.executors,
  queryFn: listExecutors,
  staleTime: 5 * 60_000,
  enabled: computed(() => Boolean(exec.value)),
})
const preferredFrom = computed<Direction>(() => {
  const name = exec.value?.profile
  if (!name) return 'tail'
  const profile = profilesQuery.data.value?.profiles.find((item) => item.name === name)
  return profile?.preferred_result_direction === 'head' ? 'head' : 'tail'
})

const resultQuery = useQuery({
  // 没展开时挂在一个不会命中的键上，配合 enabled=false 保证首屏零请求
  queryKey: computed(() =>
    opened.value
      ? queryKeys.jobResult(
          props.job.id,
          Number(attempt.value),
          opened.value.stream,
          opened.value.from,
          opened.value.maxBytes,
        )
      : queryKeys.jobResult(props.job.id, NO_BUDGET, 'out', 'tail', NO_BUDGET),
  ),
  queryFn: () => {
    const current = opened.value
    if (!current) throw new Error('结果面板还没展开')
    return getJobResult(props.job.id, {
      attempt: Number(attempt.value),
      stream: current.stream,
      from: current.from,
      max_bytes: current.maxBytes === NO_BUDGET ? undefined : current.maxBytes,
    })
  },
  enabled: computed(() => opened.value !== null && !purged.value),
})

const result = computed(() => (opened.value ? (resultQuery.data.value ?? null) : null))

/** 已经读到的字节数与文件总字节数，用来把"加载更多"这件事说清楚 */
const progress = computed(() => {
  const current = result.value
  if (!current) return ''
  const read = current.returned_bytes
  const total = current.size_bytes
  if (!current.found) return '文件已不在'
  if (read >= total) return `读完整段 ${total} 字节`
  return `已读 ${read} / ${total} 字节`
})

function openTab(stream: Stream): void {
  opened.value = { stream, from: preferredFrom.value, maxBytes: NO_BUDGET }
}

function switchStream(stream: Stream): void {
  if (opened.value && opened.value.stream === stream) return
  openTab(stream)
}

function toggleDirection(): void {
  const current = opened.value
  if (!current) return
  opened.value = { ...current, from: current.from === 'tail' ? 'head' : 'tail' }
}

/**
 * 加载更多：把预算翻倍，而不是"从上一个偏移接着读"。
 *
 * 这个端点只有 from 与 max_bytes 两个旋钮，没有偏移量——它表达的是"给我这一段的首/尾
 * 多少字节"。所以翻倍之后拿回来的是包含上一次在内的更长一段，界面整体替换当前正文，
 * 不会出现同一段文字显示两遍。
 */
function loadMore(): void {
  const current = opened.value
  const loaded = result.value
  if (!current || !loaded) return

  const base = current.maxBytes === NO_BUDGET ? loaded.returned_bytes : current.maxBytes
  opened.value = { ...current, maxBytes: Math.max(base, 1) * 2 }
}

const shown = computed(() => (result.value ? sanitizeOutput(result.value.content) : ''))

/**
 * 重试链（§3.2 第 4 条）：按事件顺序列出每一次落定的结论。
 *
 * 编号用"第 N 条结论"而不是尝试号：任务快照里的 attempts 与事件里的 attempt 目前都随
 * 重试副本复位（core.CloneForRetry 不带 Attempts，见 TASK-E15 卡第 10 节末尾那条待处理），
 * 拿它当编号会把三次执行都标成"第 1 次"。重复任务（cron）也走这一列，
 * 所以标题说的是"这一世"的结论，不是重试次数。
 */
const conclusions = computed(() => {
  const rows: { index: number; summary: string; outcome: string; ok: boolean }[] = []
  for (const event of props.events) {
    if (event.type !== 'job.completed' && event.type !== 'job.failed') continue
    const meta = eventExec(event)
    if (!meta) continue
    rows.push({
      index: rows.length + 1,
      summary: execSummary(meta),
      outcome: execOutcome(meta),
      ok: event.type === 'job.completed',
    })
  }
  return rows
})

const fields = computed(() => {
  const meta = exec.value
  if (!meta) return []

  const rows: { label: string; value: string }[] = [
    { label: '档位', value: meta.profile },
    { label: '类别', value: meta.kind },
    { label: '结论', value: execOutcome(meta) },
    { label: '耗时', value: formatDurationMs(meta.duration_ms) },
    { label: 'stdout / stderr', value: `${meta.out_bytes} / ${meta.err_bytes} 字节` },
    { label: '输出是否被截断', value: meta.truncated ? '是（超出落盘上限）' : '否' },
    { label: '产物文件', value: meta.artifact === 'purged' ? '已按保留策略清理' : meta.artifact ? '在' : '还没有产物' },
  ]
  if (meta.signal) rows.push({ label: '中止信号', value: meta.signal })
  return rows
})

function describeError(err: unknown): string {
  if (err instanceof ApiError) return err.details ? `${err.message}：${err.details}` : err.message
  return '读取失败'
}
</script>

<template>
  <section
    v-if="exec"
    class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4 lg:col-span-2"
  >
    <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
      <h2 class="text-sm font-semibold">执行结果</h2>
      <UiBadge :tone="job.status === 'success' ? 'success' : job.status === 'failed' ? 'danger' : 'neutral'">
        {{ job.status }}
      </UiBadge>
    </div>

    <dl class="grid grid-cols-2 gap-x-4 gap-y-2.5 text-sm md:grid-cols-3">
      <div v-for="field in fields" :key="field.label" class="flex flex-col">
        <dt class="text-xs text-[var(--color-text-muted)]">{{ field.label }}</dt>
        <dd class="break-all">{{ field.value }}</dd>
      </div>
    </dl>

    <p
      v-if="exec.permanent"
      class="mt-3 rounded-[var(--radius-control)] bg-[var(--color-surface)] px-3 py-2 text-xs
        text-[var(--color-text-muted)]"
    >
      这次失败被判定为"重试无意义"（参数或脚本本身的问题）。再点重试只会得到同一个结果，
      请先按上面的结论核对参数或脚本。
    </p>

    <div v-if="conclusions.length > 1" class="mt-3">
      <h3 class="mb-1.5 text-xs text-[var(--color-text-muted)]">
        这一世里的执行结论（按事件顺序，共 {{ conclusions.length }} 条）
      </h3>
      <ul class="flex flex-col gap-1 text-xs">
        <li v-for="row in conclusions" :key="row.index" class="flex gap-2">
          <span class="tabular-nums text-[var(--color-text-muted)]">第 {{ row.index }} 条</span>
          <span :class="row.ok ? 'text-[var(--color-status-success)]' : 'text-[var(--color-status-failed)]'">
            {{ row.outcome }}
          </span>
          <span class="text-[var(--color-text-muted)]">{{ row.summary }}</span>
        </li>
      </ul>
    </div>

    <p v-if="purged" class="mt-3 text-sm text-[var(--color-text-muted)]">
      输出文件已按保留策略清理，摘要仍然在这里给出。要看到正文只能重跑这个任务。
    </p>

    <div v-else class="mt-4 flex flex-col gap-3">
      <div class="flex flex-wrap items-end gap-3">
        <div class="flex gap-1" role="tablist" aria-label="输出流">
          <UiButton
            size="sm"
            :variant="opened?.stream === 'out' ? 'primary' : 'outline'"
            @click="switchStream('out')"
          >
            标准输出
          </UiButton>
          <UiButton
            size="sm"
            :variant="opened?.stream === 'err' ? 'primary' : 'outline'"
            @click="switchStream('err')"
          >
            标准错误
          </UiButton>
        </div>

        <UiSelect
          v-if="attemptCount > 1"
          v-model="attempt"
          label="第几次尝试"
          :options="
            Array.from({ length: attemptCount }, (_, i) => ({
              value: String(i + 1),
              label: `第 ${i + 1} 次（共 ${attemptCount} 次）`,
            }))
          "
          class="w-48"
        />

        <template v-if="opened">
          <UiButton size="sm" variant="ghost" @click="toggleDirection">
            <ChevronDown :size="14" aria-hidden="true" />
            读{{ opened.from === 'tail' ? '尾部' : '头部' }}（点切另一端）
          </UiButton>
          <span class="text-xs tabular-nums text-[var(--color-text-muted)]">{{ progress }}</span>
        </template>
      </div>

      <p v-if="!opened" class="text-xs text-[var(--color-text-muted)]">
        正文没有随详情页一起取——点上面的标签页才读一次磁盘上的产物文件。
        起点默认按后端给这个档位的建议设置（http 读开头、进程档位读末尾）。
      </p>

      <p
        v-if="resultQuery.error.value"
        role="alert"
        class="rounded-[var(--radius-control)] border border-[var(--color-status-failed)]
          bg-[#fef2f2] px-3 py-2 text-sm text-[var(--color-status-failed)]"
      >
        读取输出失败：{{ describeError(resultQuery.error.value) }}
      </p>

      <p
        v-if="result?.redaction_note"
        class="rounded-[var(--radius-control)] bg-[var(--color-surface)] px-3 py-2 text-xs
          text-[var(--color-text-muted)]"
      >
        {{ result.redaction_note }}
      </p>

      <p v-if="result && !result.found" class="text-sm text-[var(--color-text-muted)]">
        这一次尝试的产物文件已经不在了（按保留策略清理，或所在目录被移走）。摘要与字节数照旧。
      </p>

      <pre
        v-if="result && result.found"
        class="max-h-[28rem] overflow-auto rounded-[var(--radius-control)] bg-[var(--color-surface)] p-3
          font-mono text-xs leading-relaxed"
       >{{ shown }}</pre>

      <UiButton
        v-if="result && result.found && result.returned_bytes < result.size_bytes"
        size="sm"
        variant="outline"
        class="self-start"
        :loading="resultQuery.isFetching.value"
        @click="loadMore"
      >
        加载更多
      </UiButton>
    </div>
  </section>
</template>
