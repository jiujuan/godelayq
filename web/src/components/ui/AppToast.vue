<script setup lang="ts">
import { AlertTriangle, CheckCircle2, Info, X, XCircle } from 'lucide-vue-next'
import { useToastStore, type ToastKind } from '../../stores/toast'

const toasts = useToastStore()

const icons: Record<ToastKind, typeof Info> = {
  info: Info,
  success: CheckCircle2,
  warning: AlertTriangle,
  error: XCircle,
}

const tones: Record<ToastKind, string> = {
  info: 'border-[var(--color-border)] text-[var(--color-text)]',
  success: 'border-[var(--color-status-success)] text-[var(--color-text)]',
  warning: 'border-[var(--color-status-running)] text-[var(--color-text)]',
  error: 'border-[var(--color-status-failed)] text-[var(--color-text)]',
}
</script>

<template>
  <div class="pointer-events-none fixed bottom-6 right-6 z-50 flex w-80 flex-col gap-2">
    <div
      v-for="toast in toasts.toasts"
      :key="toast.id"
      :class="tones[toast.kind]"
      class="pointer-events-auto flex items-start gap-2 rounded-[var(--radius-card)] border-l-4
        bg-white px-3 py-2 text-sm shadow-lg"
      role="status"
    >
      <component :is="icons[toast.kind]" class="mt-0.5 shrink-0" :size="16" aria-hidden="true" />
      <p class="flex-1">{{ toast.message }}</p>
      <button
        type="button"
        class="text-[var(--color-text-muted)] hover:text-[var(--color-text)]"
        aria-label="关闭提示"
        @click="toasts.dismiss(toast.id)"
      >
        <X :size="14" />
      </button>
    </div>
  </div>
</template>
