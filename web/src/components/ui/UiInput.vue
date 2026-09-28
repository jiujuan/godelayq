<script setup lang="ts">
import { computed } from 'vue'

interface Props {
  modelValue: string
  label?: string
  type?: 'text' | 'password'
  placeholder?: string
  autocomplete?: string
  error?: string | null
  disabled?: boolean
  autofocus?: boolean
}

const props = defineProps<Props>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

// label 与 input 必须成对绑定 id，否则点标签聚焦不到输入框，读屏也念不出字段名
let seq = 0
const inputId = `ui-input-${++seq}`

const invalid = computed(() => Boolean(props.error))
</script>

<template>
  <div class="flex flex-col gap-1.5">
    <label v-if="label" :for="inputId" class="text-sm font-medium text-[var(--color-text)]">
      {{ label }}
    </label>
    <input
      :id="inputId"
      :type="type ?? 'text'"
      :value="modelValue"
      :placeholder="placeholder"
      :autocomplete="autocomplete"
      :disabled="disabled"
      :autofocus="autofocus"
      :aria-invalid="invalid"
      class="h-9 rounded-[var(--radius-control)] border px-3 text-sm outline-none transition-colors
        focus:border-[var(--color-primary)]
        disabled:bg-[var(--color-surface)] disabled:text-[var(--color-text-muted)]"
      :class="invalid
        ? 'border-[var(--color-status-failed)]'
        : 'border-[var(--color-border)]'"
      @input="emit('update:modelValue', ($event.target as HTMLInputElement).value)"
    />
    <p v-if="error" class="text-xs text-[var(--color-status-failed)]">{{ error }}</p>
  </div>
</template>
