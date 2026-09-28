<script setup lang="ts">
/* 分组管理（§4.6 GroupsView）：左列表右编辑。
 *
 * 删除是唯一会牵动一批任务的写操作，所以它带二次确认并写清后果（决策 D5）：
 * 组内任务只解除分组、不被关闭或删除。档位也因此比创建/改名更高（admin 起）。
 *
 * "只挂在任务标签上的组"（registered=false）是注册表外的一等公民：
 * 它不能改名也不能删（后端 404），能做的只有注册成真正的组。 */
import { computed, ref } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { useRouter } from 'vue-router'
import { ExternalLink, Plus } from 'lucide-vue-next'
import GroupForm from '../components/groups/GroupForm.vue'
import GroupList from '../components/groups/GroupList.vue'
import PageHeader from '../components/layout/PageHeader.vue'
import UiButton from '../components/ui/UiButton.vue'
import UiConfirm from '../components/ui/UiConfirm.vue'
import UiEmptyState from '../components/ui/UiEmptyState.vue'
import { createGroup, deleteGroup, listGroups, updateGroup } from '../api/groups'
import { ApiError } from '../api/client'
import { queryKeys } from '../api/keys'
import { usePermission } from '../composables/usePermission'
import { useToastStore } from '../stores/toast'
import type { Group } from '../api/types'

interface Draft {
  name: string
  description: string
  color: string
}

const router = useRouter()
const toast = useToastStore()
const queryClient = useQueryClient()
const permission = usePermission()

const groupsQuery = useQuery({ queryKey: queryKeys.groups, queryFn: listGroups })
const groups = computed(() => groupsQuery.data.value ?? [])

/** null 表示"新建"这张空表单 */
const selectedName = ref<string | null>(null)
const busy = ref(false)
const formError = ref<string | null>(null)
const pendingDelete = ref<Group | null>(null)

const selected = computed(() => {
  if (selectedName.value === null) return null
  const lower = selectedName.value.toLowerCase()
  return groups.value.find((group) => group.name.toLowerCase() === lower) ?? null
})

const canWrite = computed(() => permission.can('group.write'))
const canDelete = computed(() => permission.can('group.delete'))
const writeHint = computed(() => permission.blockedReason('group.write'))

function describeError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.details ? `${err.message}：${err.details}` : err.message
  return fallback
}

function refresh(): void {
  void queryClient.invalidateQueries({ queryKey: queryKeys.groups })
  // 组名是任务上的标签，改名/删除都会牵动列表与统计
  void queryClient.invalidateQueries({ queryKey: queryKeys.jobsAll })
  void queryClient.invalidateQueries({ queryKey: queryKeys.stats })
}

function startCreate(): void {
  selectedName.value = null
  formError.value = null
}

function select(name: string): void {
  selectedName.value = name
  formError.value = null
}

async function save(draft: Draft): Promise<void> {
  busy.value = true
  formError.value = null
  try {
    if (selected.value === null) {
      const created = await createGroup(draft)
      selectedName.value = created.name
      toast.success(`分组 ${created.name} 已创建`)
    } else {
      const updated = await updateGroup(selected.value.name, draft)
      selectedName.value = updated.name
      toast.success(selected.value.name === updated.name ? '已保存' : `已改名为 ${updated.name}`)
    }
    refresh()
  } catch (err) {
    formError.value = describeError(err, '保存失败')
  } finally {
    busy.value = false
  }
}

async function register(draft: Draft): Promise<void> {
  busy.value = true
  formError.value = null
  try {
    await createGroup(draft)
    toast.success(`分组 ${draft.name} 已注册`)
    refresh()
  } catch (err) {
    formError.value = describeError(err, '注册失败')
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
    await deleteGroup(target.name)
    toast.success(
      target.job_count > 0
        ? `分组 ${target.name} 已删除，${target.job_count} 条任务改为未分组（任务本身未受影响）`
        : `分组 ${target.name} 已删除`,
    )
    if (selectedName.value?.toLowerCase() === target.name.toLowerCase()) startCreate()
    refresh()
  } catch (err) {
    formError.value = describeError(err, '删除失败')
  } finally {
    busy.value = false
  }
}

function openGroupJobs(name: string): void {
  void router.push({ name: 'jobs', query: { group: name } })
}
</script>

<template>
  <div class="p-6">
    <PageHeader title="分组" subtitle="注册表里存名称/描述/颜色；任务数按实际任务标签实时统计">
      <template #actions>
        <UiButton size="sm" variant="ghost" :disabled="!canWrite" :title="writeHint" @click="startCreate">
          <Plus :size="14" aria-hidden="true" />
          新建分组
        </UiButton>
      </template>
    </PageHeader>

    <p
      v-if="groupsQuery.error.value"
      role="alert"
      class="mb-4 rounded-[var(--radius-control)] border border-[var(--color-status-failed)]
        bg-[#fef2f2] px-3 py-2 text-sm text-[var(--color-status-failed)]"
    >
      <!-- 未装配注册表的部署这里会是 503：任务的 group 标签不受影响，只有分组管理不可用 -->
      分组注册表读取失败：{{ describeError(groupsQuery.error.value, '未知错误') }}
    </p>

    <div class="grid gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)]">
      <section class="overflow-hidden rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
        <div class="flex items-center justify-between border-b border-[var(--color-border)] px-4 py-2.5">
          <h2 class="text-sm font-semibold">全部组</h2>
          <span class="text-xs tabular-nums text-[var(--color-text-muted)]">{{ groups.length }}</span>
        </div>

        <GroupList :groups="groups" :selected="selectedName ?? ''" @select="select" />

        <button
          type="button"
          class="flex w-full items-center gap-2 border-t border-[var(--color-border)] px-4 py-2.5 text-left text-sm
            text-[var(--color-text-muted)] transition-colors hover:bg-[var(--color-surface)]"
          @click="openGroupJobs('')"
        >
          <ExternalLink :size="14" aria-hidden="true" />
          未分组任务
        </button>
      </section>

      <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4">
        <div v-if="selected" class="mb-4 flex flex-wrap items-center gap-2">
          <h2 class="text-sm font-semibold">{{ selected.name }}</h2>
          <span class="text-xs text-[var(--color-text-muted)]">
            {{ selected.job_count }} 条任务<template v-if="selected.paused_count > 0">（{{ selected.paused_count }} 条已暂停）</template>
          </span>
          <UiButton size="sm" variant="ghost" class="ml-auto" @click="openGroupJobs(selected.name)">
            <ExternalLink :size="14" aria-hidden="true" />
            查看组内任务
          </UiButton>
        </div>
        <h2 v-else class="mb-4 text-sm font-semibold">{{ selectedName === null ? '新建分组' : '分组详情' }}</h2>

        <p
          v-if="formError"
          role="alert"
          class="mb-4 rounded-[var(--radius-control)] border border-[var(--color-status-failed)]
            bg-[#fef2f2] px-3 py-2 text-sm text-[var(--color-status-failed)]"
        >
          {{ formError }}
        </p>

        <GroupForm
          :key="selected?.name ?? 'create'"
          :group="selected"
          :can-write="canWrite"
          :can-delete="canDelete"
          :write-hint="writeHint"
          :busy="busy"
          @save="save"
          @register="register"
          @remove="pendingDelete = selected"
        />

        <UiEmptyState
          v-if="!canWrite && selectedName === null"
          title="当前角色不能管理分组"
          :description="writeHint ?? '需要 operator 及以上角色'"
        />
      </section>
    </div>

    <UiConfirm
      :open="pendingDelete !== null"
      :title="`删除分组 ${pendingDelete?.name ?? ''}？`"
      confirm-label="删除并解除分组"
      tone="danger"
      :busy="busy"
      @cancel="pendingDelete = null"
      @confirm="confirmDelete"
    >
      <template v-if="pendingDelete && pendingDelete.job_count > 0">
        组里还有
        <em class="font-medium text-[var(--color-text)]">{{ pendingDelete.job_count }} 条任务</em>。
        删除只会让它们<em class="font-medium text-[var(--color-text)]">变成未分组</em>：
        任务不会被取消、不会改变状态、也不会被删除，执行计划完全不变。
      </template>
      <template v-else> 这个组当前没有任务，删除只移除它的名称、描述与颜色。 </template>
    </UiConfirm>
  </div>
</template>
