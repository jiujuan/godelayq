<script setup lang="ts">
/* payload 编辑器：等宽 textarea + 校验结果回显，不是 Monaco。
   这个字段在控制台里主要用来"看一眼、改两个键"，引一个编辑器组件
   换来的是几百 KB 首屏，不值。
   语法校验在调用方做（提交门禁与错误文案都在那儿），这里只负责显示。 */
import { computed } from 'vue'

interface Props {
  modelValue: string
  label?: string
  error?: string | null
  hint?: string
  /** 空框时的示例：textarea 的 placeholder 支持换行，正好放一段最小可用 JSON */
  placeholder?: string
  disabled?: boolean
  rows?: number
}

const props = withDefaults(defineProps<Props>(), {
  label: 'payload',
  rows: 8,
  placeholder: '',
})

const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

const invalid = computed(() => Boolean(props.error))
</script>

<template>
  <div class="flex flex-col gap-1.5">
    <label class="flex items-baseline justify-between text-sm font-medium text-[var(--color-text)]">
      {{ label }}
      <span v-if="hint" class="text-xs font-normal text-[var(--color-text-muted)]">{{ hint }}</span>
    </label>
    <textarea
      :value="modelValue"
      :rows="rows"
      :placeholder="placeholder"
      :disabled="disabled"
      spellcheck="false"
      aria-label="payload JSON"
      :aria-invalid="invalid"
      class="w-full resize-y rounded-[var(--radius-control)] border bg-[var(--color-surface)] p-3
        font-mono text-xs leading-relaxed outline-none transition-colors
        focus:border-[var(--color-primary)] focus:bg-white
        disabled:cursor-not-allowed disabled:text-[var(--color-text-muted)]"
      :class="invalid ? 'border-[var(--color-status-failed)]' : 'border-[var(--color-border)]'"
      @input="emit('update:modelValue', ($event.target as HTMLTextAreaElement).value)"
    ></textarea>
    <p v-if="error" class="text-xs text-[var(--color-status-failed)]">{{ error }}</p>
  </div>
</template>
