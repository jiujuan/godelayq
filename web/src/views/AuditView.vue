<script setup lang="ts">
/* 写操作台账页（§5.7.6 + TASK-S08，仅 ops）。
 *
 * 职责是"查一条具体记录"：按身份、动作、结论、时间窗筛，翻页，点开一行看全部字段。
 *
 * 刷新策略与 MonitorView 相反，这一条是有意为之，别照抄那一页：
 * 台账用于事后追溯，不是实时观测，所以进入页面拉一次、不轮询、也不接 WS 失效
 * （realtime-effects 里没有 audit 规则）。要新数据点"重新查询"，
 * 它只失效本页的查询键，不去动 stats 与任务列表。
 *
 * 筛选条件不写回 URL：台账行本身含账号名与拒绝原因，查询串会原样留在浏览器历史
 * 与反向代理日志里，而这一页没有"把筛好的台账贴给别人"的既有需求。 */
import { computed, ref } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { RefreshCw, ScrollText } from 'lucide-vue-next'
import PageHeader from '../components/layout/PageHeader.vue'
import UiBadge from '../components/ui/UiBadge.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiDrawer from '../components/ui/UiDrawer.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
import UiInput from '../components/ui/UiInput.vue'
import UiSelect from '../components/ui/UiSelect.vue'
import { fetchAudit } from '../api/admin'
import { ApiError } from '../api/client'
import { queryKeys } from '../api/keys'
import { formatDateTime, formatDurationMs } from '../display'
import type { AuditAction, AuditEntry, AuditQuery, AuditVerdict } from '../api/types'
import { AUDIT_ACTIONS, AUDIT_VERDICTS } from '../api/types'

/** 与任务列表页同一档：一页 50 行，也是后端 limit 的默认值 */
const PAGE_SIZE = 50

type BadgeTone = 'primary' | 'success' | 'warning' | 'danger' | 'neutral'

/** 结论着色集中在这一张表里，模板不写三元表达式（§3.5） */
const VERDICT_TONES: Partial<Record<AuditVerdict, BadgeTone>> = {
  ok: 'neutral',
  denied: 'warning',
  error: 'danger',
  bad_request: 'warning',
  conflict: 'warning',
  throttled: 'warning',
  not_found: 'primary',
  partial: 'primary',
  other: 'primary',
}

function verdictTone(verdict: AuditVerdict): BadgeTone {
  return VERDICT_TONES[verdict] ?? 'neutral'
}

/**
 * 时间窗按"切换它的那一刻"折算成一个绝对的 since：
 *   - 它必须进查询键，否则换窗不会重取（实测过一次：三个窗口的计数一模一样）；
 *   - 它不能每次发请求都重算，否则翻到第二页时窗口已经往前挪，
 *     第 2 页的过滤条件与第 1 页就不是同一件事了。
 * 所以"最近 1 小时"指的是点下这个选项之后的一小时，要更窄的窗口重点一次。
 */
const WINDOWS = {
  '1h': { label: '最近 1 小时', ms: 60 * 60_000 },
  '24h': { label: '最近 24 小时', ms: 24 * 60 * 60_000 },
  '7d': { label: '最近 7 天', ms: 7 * 24 * 60 * 60_000 },
  all: { label: '全部', ms: 0 },
} as const

type WindowKey = keyof typeof WINDOWS

const actor = ref('')
const action = ref('')
const verdict = ref('')
const window = ref<string>('24h')
const offset = ref(0)
const detail = ref<AuditEntry | null>(null)
const queryClient = useQueryClient()

/** 参与缓存键的筛选：offset 单独一格，与 queryKeys.jobs 的成对写法一致。
    下拉的值来自 AUDIT_ACTIONS / AUDIT_VERDICTS 这两份常量，所以这里的断言只是将
    "控件的 string"收回到联合类型，不是在猜后端的取值。 */
const since = computed<string | undefined>(() => {
  const span = WINDOWS[window.value as WindowKey].ms
  return span === 0 ? undefined : new Date(Date.now() - span).toISOString()
})

const filters = computed<AuditQuery>(() => ({
  actor: actor.value === '' ? undefined : actor.value,
  action: action.value === '' ? undefined : (action.value as AuditAction),
  verdict: verdict.value === '' ? undefined : (verdict.value as AuditVerdict),
  since: since.value,
}))

const hasFilter = computed(
  () => actor.value !== '' || action.value !== '' || verdict.value !== '' || window.value !== 'all',
)

const auditQuery = useQuery({
  queryKey: computed(() => queryKeys.audit(filters.value, offset.value)),
  queryFn: () => fetchAudit({ ...filters.value, limit: PAGE_SIZE, offset: offset.value }),
})

const rows = computed(() => auditQuery.data.value?.items ?? [])
const total = computed(() => auditQuery.data.value?.total ?? 0)

/**
 * 503 是"这次部署没在记"，不是故障：与未启用鉴权同类，所以给说明态而不是错误横幅。
 * 后端在观测层关闭时用这一档挡住查询端点（api/handlers_admin.go 的 requireAudit）。
 */
const notEnabled = computed(() => {
  const err = auditQuery.error.value
  return err instanceof ApiError && err.status === 503
})

const loadError = computed(() => {
  const err = auditQuery.error.value
  if (!err || notEnabled.value) return null
  if (err instanceof ApiError) return err.details ? `${err.message}：${err.details}` : err.message
  return '未知错误'
})

const pageSummary = computed(() => {
  if (notEnabled.value) return '未启用'
  if (total.value === 0) return '没有记录'
  const from = offset.value + 1
  const to = Math.min(offset.value + PAGE_SIZE, total.value)
  return `第 ${from}–${to} 条 / 共 ${total.value} 条`
})

/** 换筛选条件却停在第 3 页，看到的一定是错位的数据——与任务列表页同一处理 */
function resetPage(): void {
  offset.value = 0
}

function goPage(next: number): void {
  const maxStart = Math.max(0, Math.floor((total.value - 1) / PAGE_SIZE) * PAGE_SIZE)
  const target = Math.max(0, Math.min(next, maxStart))
  if (target === offset.value) return
  offset.value = target
}

function refresh(): void {
  // 只失效本页的查询键：台账不进 WS 失效管线，也不该顺带把 stats 与任务列表重取一次
  void queryClient.invalidateQueries({ queryKey: queryKeys.auditAll })
}

const actionOptions = [
  { value: '', label: '全部动作' },
  ...AUDIT_ACTIONS.map((item) => ({ value: item, label: item })),
]

const verdictOptions = [
  { value: '', label: '全部结论' },
  ...AUDIT_VERDICTS.map((item) => ({ value: item, label: item })),
]

const windowOptions = (Object.keys(WINDOWS) as WindowKey[]).map((key) => ({
  value: key,
  label: WINDOWS[key].label,
}))

/** 耗时取自后端的微秒整数；快请求给 0 是时钟粒度，不是查询失败 */
function latencyText(entry: AuditEntry): string {
  return formatDurationMs(Math.round(entry.latency_us / 1000))
}

function openDetail(entry: AuditEntry): void {
  detail.value = entry
}

/** 抽屉里的字段清单：值空一律显式给 '—'，免得"没有这一列"与"这列是空的"看着一样 */
const detailRows = computed(() => {
  const entry = detail.value
  if (!entry) return []
  return [
    { label: '时间（本地）', value: formatDateTime(entry.time) },
    { label: '身份', value: entry.actor || '—' },
    { label: '身份类型', value: entry.actor_kind || '—' },
    { label: '档位', value: entry.role || '—' },
    { label: '动作', value: entry.action },
    { label: '方法', value: entry.method },
    { label: '路由模板', value: entry.route || '—' },
    { label: '状态码', value: String(entry.status) },
    { label: '结论', value: entry.verdict },
    { label: '耗时', value: `${latencyText(entry)}（后端给的是微秒整数）` },
    { label: '执行器结论', value: entry.exec_verdict ?? '—' },
    { label: '执行器原因码', value: entry.exec_reason_code ?? '—' },
    { label: '档位键', value: entry.handler_key ?? '—' },
    { label: '档位名', value: entry.profile ?? '—' },
    { label: '任务 ID', value: entry.job_id ?? '—' },
    { label: '客户端 IP', value: entry.remote_ip ?? '—' },
    { label: 'User-Agent', value: entry.user_agent ?? '—' },
  ]
})
</script>

<template>
  <div class="p-6">
    <PageHeader title="审计" subtitle="写操作台账（POST/PUT/DELETE 每请求一行）；读操作与握手不进这张表">
      <template #actions>
        <span class="text-xs tabular-nums text-[var(--color-text-muted)]">{{ pageSummary }}</span>
        <UiButton size="sm" variant="outline" :loading="auditQuery.isFetching.value" @click="refresh">
          <RefreshCw :size="14" aria-hidden="true" />
          重新查询
        </UiButton>
      </template>
    </PageHeader>

    <section class="mb-4 rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4">
      <div class="grid gap-3 md:grid-cols-2 lg:grid-cols-4">
        <UiInput
          v-model="actor"
          label="身份"
          placeholder="账号名，精确匹配"
          hint="匿名与静态 token 请求这一列是空串，筛不出来，看抽屉里的身份类型"
          @update:model-value="resetPage"
        />
        <UiSelect
          v-model="action"
          label="动作"
          :options="actionOptions"
          hint="封闭词表，与后端 api/audit.go 的映射表对照维护"
          @update:model-value="resetPage"
        />
        <UiSelect
          v-model="verdict"
          label="结论"
          :options="verdictOptions"
          hint="由状态码派生；批量端点恒为 partial"
          @update:model-value="resetPage"
        />
        <UiSelect v-model="window" label="时间窗" :options="windowOptions" @update:model-value="resetPage" />
      </div>
    </section>

    <section class="overflow-hidden rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
      <p
        v-if="loadError"
        role="alert"
        class="border-b border-[var(--color-border)] bg-[#fef2f2] px-4 py-2 text-sm text-[var(--color-status-failed)]"
      >
        台账读取失败：{{ loadError }}
      </p>

      <div v-else-if="rows.length > 0" class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-[var(--color-border)] text-left text-xs uppercase tracking-wide text-[var(--color-text-muted)]">
              <th class="px-4 py-2.5">时间</th>
              <th class="px-2 py-2.5">身份</th>
              <th class="px-2 py-2.5">动作</th>
              <th class="px-2 py-2.5">方法</th>
              <th class="px-2 py-2.5">结论</th>
              <th class="px-2 py-2.5 text-right">耗时</th>
              <th class="px-2 py-2.5">执行器结论</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="(entry, index) in rows"
              :key="`${offset}:${index}`"
              class="cursor-pointer border-b border-[var(--color-border)] transition-colors hover:bg-[var(--color-primary-soft)]"
              @click="openDetail(entry)"
            >
              <td class="whitespace-nowrap px-4 py-2.5 tabular-nums">{{ formatDateTime(entry.time) }}</td>
              <td class="px-2 py-2.5">
                <span class="font-medium">{{ entry.actor || '—' }}</span>
                <!-- role 为空有两种来源：登录请求（身份还没建立）与未启用鉴权的部署。
                     这里不写"匿名"：登录行显示成匿名会把"谁在试着登录"这件事说反，
                     那一列的真相在抽屉的"身份类型"里 -->
                <UiBadge class="ml-2" :tone="entry.role === 'ops' ? 'danger' : 'neutral'">
                  {{ entry.role || '—' }}
                </UiBadge>
              </td>
              <td class="px-2 py-2.5"><code class="text-xs">{{ entry.action }}</code></td>
              <td class="px-2 py-2.5 text-[var(--color-text-muted)]">{{ entry.method }}</td>
              <td class="px-2 py-2.5">
                <UiBadge :tone="verdictTone(entry.verdict)">
                  {{ entry.verdict }}
                  <span class="tabular-nums opacity-70">{{ entry.status }}</span>
                </UiBadge>
              </td>
              <td class="px-2 py-2.5 text-right tabular-nums text-[var(--color-text-muted)]">
                {{ latencyText(entry) }}
              </td>
              <td class="px-2 py-2.5">
                <code v-if="entry.exec_verdict" class="text-xs">{{ entry.exec_verdict }}</code>
                <span v-else class="text-[var(--color-text-muted)]">—</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <UiEmptyState
        v-else-if="notEnabled"
        title="这次部署没有启用写操作台账"
        description="台账落在观测库的 write_audit 表里，要 observability.enabled 与 observability.audit.enabled 同时为真并重启进程。关着的时候每个写请求仍然会落一行结构化日志（msg=&quot;write operation audited&quot;），只是不能按条件查、也不能翻页。这不是故障。"
      >
        <template #icon>
          <ScrollText :size="28" class="text-[var(--color-text-muted)]" aria-hidden="true" />
        </template>
      </UiEmptyState>

      <UiEmptyState
        v-else-if="!auditQuery.isPending.value && !loadError"
        :title="hasFilter ? '当前筛选没有命中' : '台账里还没有记录'"
        :description="
          hasFilter
            ? '放宽条件试试：身份是精确匹配、时间窗默认只取最近 24 小时，这两项最容易筛空'
            : '台账从启用观测层的这个版本开始记：在此之前发生的写操作没有行，读操作与 WebSocket 握手任何时候都不进这张表'
        "
      >
        <template #icon>
          <ScrollText :size="28" class="text-[var(--color-text-muted)]" aria-hidden="true" />
        </template>
      </UiEmptyState>

      <footer class="flex items-center justify-between border-t border-[var(--color-border)] px-4 py-2.5">
        <UiButton size="sm" variant="ghost" :disabled="offset === 0" @click="goPage(offset - PAGE_SIZE)">
          上一页
        </UiButton>
        <span class="text-xs tabular-nums text-[var(--color-text-muted)]">{{ pageSummary }}</span>
        <UiButton
          size="sm"
          variant="ghost"
          :disabled="notEnabled || offset + PAGE_SIZE >= total"
          @click="goPage(offset + PAGE_SIZE)"
        >
          下一页
        </UiButton>
      </footer>
    </section>

    <UiDrawer :open="detail !== null" title="台账行详情" @close="detail = null">
      <dl class="grid grid-cols-1 gap-x-6 gap-y-3 text-sm">
        <div v-for="item in detailRows" :key="item.label" class="flex flex-col">
          <dt class="text-xs text-[var(--color-text-muted)]">{{ item.label }}</dt>
          <dd class="break-all"><code class="text-xs">{{ item.value }}</code></dd>
        </div>
      </dl>
      <p class="mt-4 text-xs leading-relaxed text-[var(--color-text-muted)]">
        这一页能看到的就是表里的全部列。参数取值、请求体与校验错误原文都不进表（设计文档 D7），
        所以在这里也搜不到；那些内容在访问日志与响应体里。
      </p>
    </UiDrawer>
  </div>
</template>
