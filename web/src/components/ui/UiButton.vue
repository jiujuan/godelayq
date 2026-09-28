<script setup lang="ts">
/* 统一按钮：变体只有四种，够用就好，避免每页各写一套 hover/focus 样式 */
import { computed } from 'vue'
import { LoaderCircle } from 'lucide-vue-next'

interface Props {
  variant?: 'primary' | 'outline' | 'ghost' | 'danger'
  size?: 'sm' | 'md'
  loading?: boolean
  disabled?: boolean
  type?: 'button' | 'submit'
}

const props = withDefaults(defineProps<Props>(), {
  variant: 'primary',
  size: 'md',
  loading: false,
  disabled: false,
  type: 'button',
})

const sizes: Record<string, string> = {
  sm: 'h-8 px-3 text-sm',
  md: 'h-9 px-4 text-sm',
}

const variants: Record<string, string> = {
  primary: 'bg-[var(--color-primary)] text-white hover:bg-[var(--color-primary-hover)]',
  outline:
    'border border-[var(--color-border)] bg-white text-[var(--color-text)] hover:bg-[var(--color-surface)]',
  ghost: 'text-[var(--color-text-muted)] hover:bg-[var(--color-surface)] hover:text-[var(--color-text)]',
  danger: 'bg-[var(--color-status-failed)] text-white hover:opacity-90',
}

const classes = computed(
  () =>
    'inline-flex items-center justify-center gap-2 rounded-[var(--radius-control)] font-medium transition-colors ' +
    'disabled:cursor-not-allowed disabled:opacity-50 ' +
    `${sizes[props.size]} ${variants[props.variant]}`,
)
</script>

<template>
  <button :type="type" :class="classes" :disabled="disabled || loading">
    <LoaderCircle v-if="loading" class="animate-spin" :size="16" aria-hidden="true" />
    <slot />
  </button>
</template>
