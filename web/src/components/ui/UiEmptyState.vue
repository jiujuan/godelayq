<script setup lang="ts">
interface Props {
  title: string
  description?: string
}

defineProps<Props>()
</script>

<template>
  <div class="flex flex-col items-center justify-center gap-2 py-16 text-center">
    <slot name="icon" />
    <p class="text-sm font-medium text-[var(--color-text)]">{{ title }}</p>
    <p v-if="description" class="max-w-md text-sm text-[var(--color-text-muted)]">{{ description }}</p>
    <div class="pt-2">
      <slot />
    </div>
  </div>
</template>
