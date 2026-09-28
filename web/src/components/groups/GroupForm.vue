<script setup lang="ts">
/* 右侧编辑区。改名会连带改写挂着旧组名的任务标签（堆内条目一起改），
   所以二次确认发生在视图层的删除动作上，而不是这里——这里只管草稿。 */
import { computed, reactive, watch } from 'vue'
import UiButton from '../ui/UiButton.vue'
import UiInput from '../ui/UiInput.vue'
import type { Group } from '../../api/types'

/** 固定色板。后端只要求是 #rgb/#rrggbb，但放一个自由取色器会产出成百上千种"近似蓝" */
const PALETTE = [
  '#2563EB',
  '#4F46E5',
  '#0EA5E9',
  '#16A34A',
  '#D97706',
  '#DC2626',
  '#7C3AED',
  '#64748B',
]

const NAME_PATTERN = /^[A-Za-z0-9_-]{1,64}$/

interface Props {
  /** null = 新建；未注册的组传进来时表单退化成"只读 + 注册" */
  group: Group | null
  canWrite: boolean
  canDelete: boolean
  writeHint: string | null
  busy?: boolean
}

const props = withDefaults(defineProps<Props>(), { busy: false })

const emit = defineEmits<{
  save: [payload: { name: string; description: string; color: string }]
  register: [payload: { name: string; description: string; color: string }]
  remove: [name: string]
}>()

const draft = reactive({ name: '', description: '', color: '' })

watch(
  () => props.group,
  (group) => {
    draft.name = group?.name ?? ''
    draft.description = group?.description ?? ''
    draft.color = group?.color ?? ''
  },
  { immediate: true },
)

const isCreate = computed(() => props.group === null)
const isOrphan = computed(() => props.group !== null && !props.group.registered)

const nameError = computed(() => {
  if (draft.name === '') return '组名不能为空'
  return NAME_PATTERN.test(draft.name) ? null : '只允许 1–64 位字母、数字、下划线与短横线'
})

/** 改名后的名字与原名字不同才算改名（后端大小写不敏感，这里同口径比较） */
const renamed = computed(
  () =>
    !isCreate.value &&
    props.group !== null &&
    draft.name.toLowerCase() !== props.group.name.toLowerCase(),
)

const dirty = computed(() => {
  if (isCreate.value) return draft.name !== '' && nameError.value === null
  if (props.group === null) return false
  return (
    draft.name !== props.group.name ||
    draft.description !== (props.group.description ?? '') ||
    draft.color !== (props.group.color ?? '')
  )
})

const canSave = computed(
  () => props.canWrite && !props.busy && !isOrphan.value && dirty.value && nameError.value === null,
)
</script>

<template>
  <div class="flex flex-col gap-4">
    <UiInput
      label="组名"
      :model-value="draft.name"
      :disabled="!canWrite || isOrphan || busy"
      :error="nameError"
      hint="1–64 位字母、数字、下划线与短横线；改名会连带改写组内任务的分组标签"
      @update:model-value="draft.name = $event"
    />

    <UiInput
      label="描述"
      :model-value="draft.description"
      :disabled="!canWrite || isOrphan || busy"
      hint="留空即清空描述"
      @update:model-value="draft.description = $event"
    />

    <fieldset class="flex flex-col gap-2">
      <legend class="mb-1 text-sm font-medium text-[var(--color-text)]">颜色</legend>
      <div class="flex flex-wrap items-center gap-2">
        <button
          v-for="color in PALETTE"
          :key="color"
          type="button"
          class="h-7 w-7 rounded-full border-2 transition-transform"
          :style="{ backgroundColor: color, borderColor: draft.color === color ? 'var(--color-text)' : 'transparent' }"
          :disabled="!canWrite || isOrphan || busy"
          :aria-label="`选用颜色 ${color}`"
          :aria-pressed="draft.color === color"
          @click="draft.color = color"
        ></button>
        <UiButton
          size="sm"
          variant="ghost"
          :disabled="!canWrite || isOrphan || busy || draft.color === ''"
          @click="draft.color = ''"
        >
          无配色
        </UiButton>
      </div>
    </fieldset>

    <p
      v-if="isOrphan"
      class="rounded-[var(--radius-control)] border border-[var(--color-status-running)] bg-[#fffbeb] px-3 py-2 text-xs leading-relaxed"
    >
      <code>{{ group?.name }}</code> 目前只是任务上的标签，注册表里没有这个组，因此不能改名或删除。
      想让它成为一个真正的组（有描述与颜色），点下面的"注册这个组"。
    </p>

    <div class="flex flex-wrap items-center gap-2">
      <UiButton :disabled="!canSave" :loading="busy" :title="writeHint" @click="emit('save', { ...draft })">
        {{ isCreate ? '创建分组' : renamed ? '保存并改名' : '保存' }}
      </UiButton>

      <UiButton
        v-if="isOrphan"
        variant="outline"
        :disabled="!canWrite || busy"
        :title="writeHint"
        @click="emit('register', { name: draft.name, description: draft.description, color: draft.color })"
      >
        注册这个组
      </UiButton>

      <UiButton
        v-if="!isCreate && !isOrphan"
        variant="danger"
        :disabled="!canDelete || busy"
        @click="emit('remove', group?.name ?? '')"
      >
        删除分组
      </UiButton>
    </div>
  </div>
</template>
