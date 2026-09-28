<script setup lang="ts">
/* 筛选条：只落后端已有的三个参数（status / group / name）。
   没有关键字框——列表端点不支持，硬加一个"只过滤当前这一页"的输入框
   会让翻页和总数自相矛盾，比没有更糟（设计文档 §4.6 已按此修订）。 */
import { Plus, RotateCcw } from 'lucide-vue-next'
import UiButton from '../ui/UiButton.vue'
import UiSelect from '../ui/UiSelect.vue'
import type { JobStatus, ListJobsQuery } from '../../api/types'

/** select 里不能直接写 undefined（它就是"不筛选"），所以给两种空值各一个哨兵 */
const ALL = '__all__'
const UNGROUPED = '__none__'

const STATUS_OPTIONS: { value: string; label: string }[] = [
  { value: ALL, label: '全部状态' },
  { value: 'pending', label: '待执行' },
  { value: 'running', label: '执行中' },
  { value: 'paused', label: '已暂停' },
  { value: 'success', label: '已完成' },
  { value: 'failed', label: '失败' },
  { value: 'cancelled', label: '已取消' },
]

interface Props {
  filters: ListJobsQuery
  /** 分组下拉的候选：注册表 + 只挂在任务标签上的组名 */
  groupNames: string[]
  /** 名称下拉的候选：GET /job-types */
  jobTypes: string[]
  canCreate: boolean
  createHint: string | null
}

const props = defineProps<Props>()
const emit = defineEmits<{ 'update:filters': [next: ListJobsQuery], create: [] }>()

function statusValue(): string {
  return props.filters.status ?? ALL
}

function groupValue(): string {
  if (props.filters.group === undefined) return ALL
  if (props.filters.group === '') return UNGROUPED
  return props.filters.group
}

function nameValue(): string {
  return props.filters.name ?? ALL
}

function patch(next: Partial<ListJobsQuery>): void {
  emit('update:filters', { ...props.filters, ...next })
}
</script>

<template>
  <div class="flex flex-wrap items-end gap-3">
    <UiSelect
      class="w-40"
      label="状态"
      :model-value="statusValue()"
      :options="STATUS_OPTIONS"
      @update:model-value="(value) => patch({ status: value === ALL ? undefined : (value as JobStatus) })"
    />

    <UiSelect
      class="w-44"
      label="分组"
      :model-value="groupValue()"
      :options="[
        { value: ALL, label: '全部分组' },
        { value: UNGROUPED, label: '未分组' },
        ...groupNames.map((name) => ({ value: name, label: name })),
      ]"
      @update:model-value="
        (value) => patch({ group: value === ALL ? undefined : value === UNGROUPED ? '' : value })
      "
    />

    <UiSelect
      class="w-44"
      label="名称"
      :model-value="nameValue()"
      :options="[{ value: ALL, label: '全部名称' }, ...jobTypes.map((name) => ({ value: name, label: name }))]"
      @update:model-value="(value) => patch({ name: value === ALL ? undefined : value })"
    />

    <UiButton variant="ghost" size="sm" @click="emit('update:filters', {})">
      <RotateCcw :size="14" aria-hidden="true" />
      重置
    </UiButton>

    <div class="ml-auto">
      <span :title="createHint ?? undefined">
        <UiButton :disabled="!canCreate" @click="emit('create')">
          <Plus :size="14" aria-hidden="true" />
          新建任务
        </UiButton>
      </span>
    </div>
  </div>
</template>
