<script setup lang="ts">
/* 原生 select 的薄封装：样式与 UiInput 对齐，行为交给浏览器。
   没有占位符参数——"未分组""全部状态"本身就是有意义的取值（空串是其中之一），
   做成额外的伪选项反而会把两种空值语义搅浑，所以由调用方显式给出首选项。 */
import { computed } from 'vue'

export interface SelectOption {
  value: string
  label: string
  disabled?: boolean
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
      <option v-for="option in options" :key="option.value" :value="option.value" :disabled="option.disabled">
        {{ option.label }}
      </option>
    </select>
    <p v-if="error" class="text-xs text-[var(--color-status-failed)]">{{ error }}</p>
    <p v-else-if="hint" class="text-xs text-[var(--color-text-muted)]">{{ hint }}</p>
  </div>
</template>
