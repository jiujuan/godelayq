<script setup lang="ts">
/* 原生 select 的薄封装：样式与 UiInput 对齐，行为交给浏览器。
   没有占位符参数——"未分组""全部状态"本身就是有意义的取值（空串是其中之一），
   做成额外的伪选项反而会把两种空值语义搅浑，所以由调用方显式给出首选项。

   group 是可选的分组名（TASK-N07 的任务类型下拉要用）：原生 select 的分组只能靠 optgroup，
   用一条"—— 分组名 ——"的伪选项代替会让 :key 撞车（多个空值选项），也会让分隔行看着可选中。
   没有一项带 group 时渲染路径与之前逐字相同。 */
import { computed } from 'vue'

export interface SelectOption {
  value: string
  label: string
  disabled?: boolean
  /** 分组名；同一组按出现的先后排，不给分组就按原样平铺 */
  group?: string
}

interface Props {
  modelValue: string
  options: SelectOption[]
  label?: string
  hint?: string
  error?: string | null
  disabled?: boolean
}

const props = defineProps<Props>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

let seq = 0
const selectId = `ui-select-${++seq}`

const invalid = computed(() => Boolean(props.error))

/** 按 group 分桶，桶的顺序跟着第一次出现走；没有分组的项留在最前面平铺给出。 */
const grouped = computed(() => props.options.some((option) => option.group !== undefined))
const plainOptions = computed(() => props.options.filter((option) => option.group === undefined))
const optionGroups = computed(() => {
  const order: string[] = []
  const buckets = new Map<string, SelectOption[]>()
  for (const option of props.options) {
    const name = option.group ?? ''
    if (!name) continue
    if (!buckets.has(name)) {
      buckets.set(name, [])
      order.push(name)
    }
    buckets.get(name)!.push(option)
  }
  return order.map((name) => ({ label: name, options: buckets.get(name)! }))
})
</script>

<template>
  <div class="flex flex-col gap-1.5">
    <label v-if="label" :for="selectId" class="text-sm font-medium text-[var(--color-text)]">
      {{ label }}
    </label>
    <select
      :id="selectId"
      :value="modelValue"
      :disabled="disabled"
      :aria-invalid="invalid"
      class="h-9 rounded-[var(--radius-control)] border bg-white px-2.5 text-sm outline-none transition-colors
        focus:border-[var(--color-primary)]
        disabled:bg-[var(--color-surface)] disabled:text-[var(--color-text-muted)]"
      :class="invalid ? 'border-[var(--color-status-failed)]' : 'border-[var(--color-border)]'"
      @change="emit('update:modelValue', ($event.target as HTMLSelectElement).value)"
    >
      <template v-if="grouped">
        <option
          v-for="option in plainOptions"
          :key="`plain-${option.value}`"
          :value="option.value"
          :disabled="option.disabled"
        >
          {{ option.label }}
        </option>
        <optgroup v-for="bucket in optionGroups" :key="bucket.label" :label="bucket.label">
          <option
            v-for="option in bucket.options"
            :key="option.value"
            :value="option.value"
            :disabled="option.disabled"
          >
            {{ option.label }}
          </option>
        </optgroup>
      </template>
      <option v-else v-for="option in options" :key="option.value" :value="option.value" :disabled="option.disabled">
        {{ option.label }}
      </option>
    </select>
    <p v-if="error" class="text-xs text-[var(--color-status-failed)]">{{ error }}</p>
    <p v-else-if="hint" class="text-xs text-[var(--color-text-muted)]">{{ hint }}</p>
  </div>
</template>
