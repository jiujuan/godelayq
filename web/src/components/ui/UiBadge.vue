<script setup lang="ts">
import { computed } from 'vue'

interface Props {
  tone?: 'primary' | 'success' | 'warning' | 'danger' | 'neutral'
  /** 是否画前面的小圆点（连接状态徽标用得上） */
  dot?: boolean
}

const props = withDefaults(defineProps<Props>(), { tone: 'neutral', dot: false })

const tones: Record<string, { bg: string; text: string; dot: string }> = {
  primary: { bg: 'var(--color-primary-soft)', text: 'var(--color-primary)', dot: 'var(--color-primary)' },
  success: { bg: '#f0fdf4', text: 'var(--color-status-success)', dot: 'var(--color-ws-connected)' },
  warning: { bg: '#fffbeb', text: 'var(--color-status-running)', dot: 'var(--color-ws-reconnecting)' },
  danger: { bg: '#fef2f2', text: 'var(--color-status-failed)', dot: 'var(--color-status-failed)' },
  neutral: { bg: 'var(--color-surface)', text: 'var(--color-text-muted)', dot: 'var(--color-ws-offline)' },
}

const style = computed(() => {
  const tone = tones[props.tone]
  return { backgroundColor: tone.bg, color: tone.text }
})

const dotStyle = computed(() => ({ backgroundColor: tones[props.tone].dot }))
</script>

<template>
  <span
    :style="style"
    class="inline-flex items-center gap-1.5 rounded-[var(--radius-pill)] px-2.5 py-1 text-xs font-medium"
  >
    <span v-if="dot" :style="dotStyle" class="size-1.5 rounded-full" aria-hidden="true" />
    <slot />
  </span>
</template>
