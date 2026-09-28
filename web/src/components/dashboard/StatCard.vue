<script setup lang="ts">
import type { LucideIcon } from 'lucide-vue-next'

interface Props {
  label: string
  value: number | string
  icon?: LucideIcon
  /** 强调色：给 paused/failed 这类需要一眼看见的计数用 */
  accent?: string
  loading?: boolean
}

const props = defineProps<Props>()
</script>

<template>
  <div class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4">
    <div class="flex items-center justify-between">
      <p class="text-sm text-[var(--color-text-muted)]">{{ props.label }}</p>
      <component
        :is="props.icon"
        v-if="props.icon"
        class="text-[var(--color-text-muted)]"
        :size="16"
        aria-hidden="true"
      />
    </div>
    <p
      class="mt-2 text-2xl font-semibold tabular-nums"
      :style="props.accent ? { color: props.accent } : undefined"
    >
      <span v-if="props.loading" class="inline-block h-7 w-12 animate-pulse rounded bg-[var(--color-surface)]" />
      <template v-else>{{ props.value }}</template>
    </p>
  </div>
</template>
