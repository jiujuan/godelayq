<script setup lang="ts">
/* 档位管理（TASK-W08）：左列表右编辑，布局照抄分组页。
 *
 * 列表的数据源仍是 GET /executors（TASK-W07 扩过的那一份）：每一行带着来源、可编辑标记、
 * 降级状态与探测结论，页面只读结论不再自己判"这条能不能改"。
 * 定义（脚本路径、参数表、http 那一组）另有一个按名字取的门（ops 档），
 * 只有 editable 的行才会去开它——配置侧的档位没有存储记录，那里返回的是 409。
 *
 * 删除比编辑更要紧：它会把这一类型的待执行任务全部钉住（决策 D6），
 * 而正在执行的那条不会被中止，之后恢复它又会因为找不到处理函数而失败。
 * 这三件事都写进确认弹窗，而不是留给用户事后发现。 */
import { computed, ref, watch } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { Plus } from 'lucide-vue-next'
import ProfileForm from '../components/executors/ProfileForm.vue'
import PageHeader from '../components/layout/PageHeader.vue'
import UiBadge from '../components/ui/UiBadge.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiConfirm from '../components/ui/UiConfirm.vue'
import { getExecutorProfileRecord, createExecutorProfile, updateExecutorProfile, deleteExecutorProfile } from '../api/executor-profiles'
import { listExecutors } from '../api/executors'
import { ApiError } from '../api/client'
import { queryKeys } from '../api/keys'
import { usePermission } from '../composables/usePermission'
import { useToastStore } from '../stores/toast'
import type { ExecutorProfile, ExecutorProfileRecord } from '../api/types'

type DeleteStrategy = 'pause' | 'block'
type Mode = 'create' | 'edit' | 'readonly'

const toast = useToastStore()
const queryClient = useQueryClient()
const permission = usePermission()

const executorsQuery = useQuery({
  queryKey: queryKeys.executors,
  queryFn: listExecutors,
  // 页面自己会刷新这份列表，5 分钟内不重取；写操作之后由 refresh() 显式失效
  staleTime: 5 * 60_000,
})

const rows = computed(() => executorsQuery.data.value?.profiles ?? [])
const webEnabled = computed(() => executorsQuery.data.value?.web_enabled ?? false)
const runtimeAllow = computed(() => executorsQuery.data.value?.runtime_allow ?? [])
const maxTimeout = computed(() => executorsQuery.data.value?.max_timeout ?? '')

/** 选中的那一行；null 表示右边是"新建"这张空表单 */
const selected = ref<ExecutorProfile | null>(null)
const mode = computed<Mode>(() => {
  if (selected.value === null) return 'create'
  return selected.value.editable ? 'edit' : 'readonly'
})

const busy = ref(false)
const formError = ref<string | null>(null)
const pendingDelete = ref<ExecutorProfile | null>(null)
const deleteStrategy = ref<DeleteStrategy>('pause')

const canCreate = computed(() => permission.can('executor.profile_create'))
const canUpdate = computed(() => permission.can('executor.profile_update'))
const canDelete = computed(() => permission.can('executor.profile_delete'))
const writeHint = computed(() => permission.blockedReason('executor.profile_create'))

/**
 * 编辑对象的定义。只在 edit 模式下取，键跟着选中的那一行走。
 *
 * 低档位不参与这一趟读取：读定义那个端点与写它是同一条 ops 门槛（api/server.go 的 profiles 分组），
 * 按钮都点不动的账号没必要收一个 403。
 */
const definition = useQuery({
  queryKey: computed(() =>
    selected.value === null ? [...queryKeys.executors, 'no-profile'] : queryKeys.executorProfile(selected.value.name),
  ),
  queryFn: () => getExecutorProfileRecord(selected.value?.name ?? ''),
  enabled: computed(() => mode.value === 'edit' && selected.value !== null && canUpdate.value),
})

const record = computed(() => definition.data.value ?? null)
const loadingRecord = computed(() => mode.value === 'edit' && definition.isFetching.value)

watch(selected, () => {
  formError.value = null
})

function describeError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.details ? `${err.message}：${err.details}` : err.message
  return fallback
}

/** 档位变了，能执行什么就变了：任务类型的下拉、列表里那些 exec.* 的行、统计里的暂停数都跟着走 */
function refresh(): void {
  void queryClient.invalidateQueries({ queryKey: queryKeys.executors })
  void queryClient.invalidateQueries({ queryKey: queryKeys.jobTypes })
  void queryClient.invalidateQueries({ queryKey: queryKeys.jobsAll })
  void queryClient.invalidateQueries({ queryKey: queryKeys.stats })
}

function startCreate(): void {
  selected.value = null
  formError.value = null
}

function select(row: ExecutorProfile): void {
  selected.value = row
}

async function save(payload: ExecutorProfileRecord): Promise<void> {
  const target = selected.value
  busy.value = true
  formError.value = null
  try {
    const saved =
      target === null || mode.value !== 'edit'
        ? await createExecutorProfile(payload)
        : await updateExecutorProfile(target.name, payload)
    toast.success(
      saved.runtime_ok
        ? `档位 ${saved.key} 已保存，现在可用`
        : `档位 ${saved.key} 已保存，但这台机器现在跑不了：${saved.reason}`,
    )
    // 写端点的响应就是列表那一行的形状（同一个 toExecutorProfile），直接拿来当选中项：
    // 表单随即变成编辑态，name/kind 也照规则锁起来。
    selected.value = saved
    refresh()
  } catch (err) {
    // 失败时表单保持打开：整份草稿都是填好的内容，关回去等于让人重填一遍
    formError.value = describeError(err, '保存失败')
  } finally {
    busy.value = false
  }
}

async function confirmDelete(): Promise<void> {
  const target = pendingDelete.value
  pendingDelete.value = null
  if (!target) return

  busy.value = true
  formError.value = null
  try {
    const result = await deleteExecutorProfile(target.name, deleteStrategy.value)
    toast.success(
      `档位 ${result.key} 已删除：钉住 ${result.paused_jobs} 条待执行，` +
        `${result.running_jobs} 条正在执行的一条没动`,
    )
    if (selected.value?.name === target.name) startCreate()
    refresh()
  } catch (err) {
    formError.value = describeError(err, '删除失败')
  } finally {
    busy.value = false
  }
}

function askDelete(row: ExecutorProfile): void {
  deleteStrategy.value = 'pause'
  pendingDelete.value = row
}

function rowKey(row: ExecutorProfile): string {
  // 同一个 key 在降级时会出现两行，:key 与选中判定都要把它们分开（与 SettingsView 同一写法）
  return row.key + (row.degraded ? '#degraded' : '')
}

function isSelected(row: ExecutorProfile): boolean {
  return selected.value !== null && rowKey(selected.value) === rowKey(row)
}
</script>

<template>
  <div class="p-6">
    <PageHeader title="档位" subtitle="这台机器能执行什么：配置里的 executors.commands 与档位文件里的在线档位">
      <template #actions>
        <UiBadge v-if="!webEnabled" tone="neutral">在线管理未开启</UiBadge>
        <UiButton
          v-else
          size="sm"
          variant="ghost"
          :disabled="!canCreate"
          :title="writeHint"
          @click="startCreate"
        >
          <Plus :size="14" aria-hidden="true" />
          新建档位
        </UiButton>
      </template>
    </PageHeader>

    <p
      v-if="executorsQuery.error.value"
      role="alert"
      class="mb-4 rounded-[var(--radius-control)] border border-[var(--color-status-failed)]
        bg-[#fef2f2] px-3 py-2 text-sm text-[var(--color-status-failed)]"
    >
      档位列表读取失败：{{ describeError(executorsQuery.error.value, '未知错误') }}
    </p>

    <p
      v-else-if="!webEnabled"
      class="mb-4 rounded-[var(--radius-control)] border border-[var(--color-status-running)] bg-[#fffbeb]
        px-3 py-2 text-xs leading-relaxed"
    >
      <code>executors.web_enabled</code> 是关闭的：这一页只能看，四个管理端点都会返回 503。
      要在线增删改档位，请在 <code>configs/config.yaml</code> 里把它打开并重启进程；
      配置里的 <code>executors.commands</code> 无论如何都只能改文件后重启。
    </p>

    <div class="grid gap-4 lg:grid-cols-[minmax(0,1.15fr)_minmax(0,1fr)]">
      <!-- 列表 -->
      <section class="overflow-hidden rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
        <div class="flex items-center justify-between border-b border-[var(--color-border)] px-4 py-2.5">
          <h2 class="text-sm font-semibold">全部档位</h2>
          <span class="text-xs tabular-nums text-[var(--color-text-muted)]">{{ rows.length }}</span>
        </div>

        <ul class="flex flex-col divide-y divide-[var(--color-border)]">
          <li v-for="row in rows" :key="rowKey(row)" class="flex flex-col gap-2 px-4 py-3">
            <button
              type="button"
              class="flex w-full flex-col gap-1.5 border-l-2 pl-2 text-left transition-colors"
              :class="[
                isSelected(row)
                  ? 'border-l-[var(--color-primary)] bg-[var(--color-primary-soft)]'
                  : 'border-l-transparent hover:bg-[var(--color-surface)]',
              ]"
              @click="select(row)"
            >
              <div class="flex flex-wrap items-center gap-2">
                <code class="font-mono text-xs">{{ row.key }}</code>
                <UiBadge :tone="row.runtime_ok ? 'success' : 'danger'">
                  {{ row.runtime_ok ? '可用' : '不可用' }}
                </UiBadge>
                <UiBadge :tone="row.source === 'store' ? 'primary' : 'neutral'">
                  {{ row.source === 'store' ? '在线档位' : '来自配置' }}
                </UiBadge>
                <UiBadge v-if="row.degraded" tone="warning">与配置同名，未生效</UiBadge>
                <span class="ml-auto text-xs tabular-nums text-[var(--color-text-muted)]">
                  {{ row.timeout }} · 并发 {{ row.max_parallel }}
                </span>
              </div>

              <div class="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-[var(--color-text-muted)]">
                <span>{{ row.kind }}</span>
                <code v-if="row.path_display" class="font-mono">{{ row.path_display }}</code>
                <span v-else-if="row.url" class="font-mono">{{ row.url }}</span>
              </div>

              <p v-if="!row.runtime_ok" class="text-xs text-[var(--color-status-failed)]">
                原因：{{ row.reason }}
              </p>
            </button>

            <div v-if="row.editable" class="flex gap-2 pl-2">
              <UiButton size="sm" variant="outline" :disabled="busy" @click="select(row)">编辑</UiButton>
              <UiButton size="sm" variant="danger" :disabled="!canDelete || busy" @click="askDelete(row)">
                删除
              </UiButton>
            </div>
          </li>

          <li v-if="rows.length === 0" class="px-4 py-6 text-sm text-[var(--color-text-muted)]">
            还没有档位：配置的 <code>executors.commands</code> 与档位文件都是空的。
          </li>
        </ul>
      </section>

      <!-- 编辑区 -->
      <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4">
        <h2 class="mb-1 text-sm font-semibold">
          {{ mode === 'create' ? '新建档位' : selected?.key }}
        </h2>
        <p v-if="mode === 'readonly'" class="mb-3 text-xs leading-relaxed text-[var(--color-text-muted)]">
          <template v-if="selected?.degraded">
            这一条与 <code>executors.commands</code> 里的同名档位撞上了，配置那一份生效、这一份没有进入登记表，
            所以页面上不给编辑：改一条不会被读到的记录只会让人以为改动已经生效。
            请删掉配置里的同名档位，或者给在线档位换一个名字。
          </template>
          <template v-else>
            这条档位来自配置的 <code>executors.commands</code>，页面上只读：要改它请编辑配置文件并重启进程。
            想在线管理它，可以把那一条从配置里删掉，再用同一名字在这一页新建一条。
          </template>
        </p>

        <p
          v-if="formError"
          role="alert"
          class="mb-4 rounded-[var(--radius-control)] border border-[var(--color-status-failed)]
            bg-[#fef2f2] px-3 py-2 text-sm text-[var(--color-status-failed)]"
        >
          {{ formError }}
        </p>

        <p
          v-else-if="definition.error.value"
          role="alert"
          class="mb-4 rounded-[var(--radius-control)] border border-[var(--color-status-failed)]
            bg-[#fef2f2] px-3 py-2 text-sm text-[var(--color-status-failed)]"
        >
          档位定义读取失败：{{ describeError(definition.error.value, '未知错误') }}
        </p>

        <ProfileForm
          v-if="mode !== 'readonly'"
          :key="mode === 'edit' ? selected?.name ?? 'edit' : 'create'"
          :record="record"
          :loading="loadingRecord"
          :runtime-allow="runtimeAllow"
          :max-timeout="maxTimeout"
          :can-write="mode === 'create' ? canCreate : canUpdate"
          :can-delete="canDelete"
          :write-hint="writeHint"
          :busy="busy"
          @save="save"
          @remove="pendingDelete = selected"
        />
      </section>
    </div>

    <UiConfirm
      :open="pendingDelete !== null"
      :title="`删除档位 ${pendingDelete?.key ?? ''}？`"
      :confirm-label="deleteStrategy === 'pause' ? '删除并钉住待执行任务' : '只在没有任务时删除'"
      tone="danger"
      :busy="busy"
      @cancel="pendingDelete = null"
      @confirm="confirmDelete"
    >
      <div class="flex flex-col gap-3">
        <p>删除之后会发生三件事，请按顺序看：</p>
        <ol class="list-decimal space-y-1 pl-5">
          <li>
            这个类型的<em class="font-medium text-[var(--color-text)]">全部待执行任务会被钉住</em>（转成暂停状态），
            它们不会被删除，也不会被取消。
          </li>
          <li>
            <em class="font-medium text-[var(--color-text)]">正在执行的那一条不会被中止</em>：
            中止属于强制暂停（admin 档）的动作，删除档位不做这件事。
          </li>
          <li>
            之后想恢复那些被钉住的任务，会因为找不到这个处理函数而失败——
            要恢复执行，得用同一个名字再建一条档位。
          </li>
        </ol>
        <fieldset class="flex flex-col gap-1.5 border-t border-[var(--color-border)] pt-3">
          <label class="flex items-start gap-2 text-sm">
            <input v-model="deleteStrategy" type="radio" value="pause" class="mt-0.5 accent-[var(--color-status-failed)]" />
            <span>
              照上面三条执行（默认）
              <span class="block text-xs text-[var(--color-text-muted)]">
                对应 <code>?jobs=pause</code>
              </span>
            </span>
          </label>
          <label class="flex items-start gap-2 text-sm">
            <input v-model="deleteStrategy" type="radio" value="block" class="mt-0.5 accent-[var(--color-status-failed)]" />
            <span>
              先看有没有任务：这个类型还有任务就直接报错、什么都不动
              <span class="block text-xs text-[var(--color-text-muted)]">
                对应 <code>?jobs=block</code>，后端回 409 并给出条数
              </span>
            </span>
          </label>
        </fieldset>
      </div>
    </UiConfirm>
  </div>
</template>
