<script setup lang="ts">
interface Props {
  title: string
  subtitle?: string
}

defineProps<Props>()
</script>

<template>
  <div class="mb-4 flex flex-wrap items-end justify-between gap-3">
    <div>
      <h2 class="text-lg font-semibold">{{ title }}</h2>
      <p v-if="subtitle" class="mt-1 text-sm text-[var(--color-text-muted)]">{{ subtitle }}</p>
    </div>
    <div class="flex items-center gap-2">
      <slot name="actions" />
    </div>
  </div>
</template>
