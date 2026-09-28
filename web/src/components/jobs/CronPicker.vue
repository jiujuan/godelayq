<script setup lang="ts">
/* Cron 输入：只做"形状"检查（5 段或 6 段），语义交给后端。
   core/cron.go 认 5/6 段（6 段以秒开头），真正的解析与下一个触发时刻计算都在服务端，
   在这里复刻一套解析器只会在两者不一致时给用户一个假的对勾。 */
import { computed } from 'vue'

interface Props {
  modelValue: string
  disabled?: boolean
  /** 后端返回过的错误（400 details），有值时直接摆在字段下面 */
  serverError?: string | null
}

const props = defineProps<Props>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

const segments = computed(() => props.modelValue.trim().split(/\s+/).filter(Boolean))

const localError = computed(() => {
  if (props.modelValue.trim() === '') return null
  const count = segments.value.length
  if (count === 5 || count === 6) return null
  return `应为 5 或 6 段，当前 ${count} 段`
})

const error = computed(() => props.serverError ?? localError.value)
</script>

<template>
  <div class="flex flex-col gap-1.5">
    <label class="text-sm font-medium text-[var(--color-text)]" for="cron-expr">Cron 表达式</label>
    <input
      id="cron-expr"
      type="text"
      :value="modelValue"
      :disabled="disabled"
      placeholder="0 */5 * * * *"
      spellcheck="false"
      aria-describedby="cron-help"
      :aria-invalid="Boolean(error)"
      class="h-9 rounded-[var(--radius-control)] border bg-white px-3 font-mono text-sm outline-none transition-colors
        focus:border-[var(--color-primary)]
        disabled:bg-[var(--color-surface)] disabled:text-[var(--color-text-muted)]"
      :class="error ? 'border-[var(--color-status-failed)]' : 'border-[var(--color-border)]'"
      @input="emit('update:modelValue', ($event.target as HTMLInputElement).value)"
    />
    <p id="cron-help" class="text-xs text-[var(--color-text-muted)]">
      6 段以秒开头（秒 分 时 日 月 周），也接受 5 段的常见写法。示例
      <code>0 */5 * * * *</code> 表示每 5 分钟整点触发。
    </p>
    <p v-if="error" class="text-xs text-[var(--color-status-failed)]">{{ error }}</p>
  </div>
</template>
