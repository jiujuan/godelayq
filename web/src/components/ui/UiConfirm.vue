<script setup lang="ts">
/* 二次确认对话框：删分组、强制暂停、挂起调度这三类"后果不是一行数据"的动作。
   说明文案由调用方通过默认插槽给出——后果要说多细取决于动作本身，
   在这里预置一段通用话术只会把最该说清的部分糊成一句"确定吗"。 */
import { onScopeDispose } from 'vue'
import { AlertTriangle } from 'lucide-vue-next'
import UiButton from './UiButton.vue'

interface Props {
  open: boolean
  title: string
  confirmLabel?: string
  cancelLabel?: string
  /** danger 用于不可逆或牵动一批数据的动作 */
  tone?: 'primary' | 'danger'
  busy?: boolean
}

const props = defineProps<Props>()
const emit = defineEmits<{ confirm: []; cancel: [] }>()

function onKeydown(event: KeyboardEvent): void {
  if (!props.open) return
  if (event.key === 'Escape' && !props.busy) emit('cancel')
}

document.addEventListener('keydown', onKeydown)
onScopeDispose(() => document.removeEventListener('keydown', onKeydown))
</script>

<template>
  <div v-if="open" class="fixed inset-0 z-50 flex items-center justify-center p-6">
    <div class="absolute inset-0 bg-black/30" @click="busy ? undefined : emit('cancel')"></div>

    <section
      role="dialog"
      aria-modal="true"
      :aria-label="title"
      class="relative w-full max-w-md rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-5 shadow-lg"
    >
      <div class="mb-3 flex items-start gap-2.5">
        <AlertTriangle
          class="mt-0.5 shrink-0"
          :size="18"
          :class="tone === 'danger' ? 'text-[var(--color-status-failed)]' : 'text-[var(--color-primary)]'"
          aria-hidden="true"
        />
        <h2 class="text-base font-semibold">{{ title }}</h2>
      </div>

      <div class="mb-5 text-sm leading-relaxed text-[var(--color-text-muted)]">
        <slot />
      </div>

      <div class="flex justify-end gap-2">
        <UiButton variant="ghost" :disabled="busy" @click="emit('cancel')">
          {{ cancelLabel ?? '取消' }}
        </UiButton>
        <UiButton :variant="tone === 'danger' ? 'danger' : 'primary'" :loading="busy" @click="emit('confirm')">
          {{ confirmLabel ?? '确定' }}
        </UiButton>
      </div>
    </section>
  </div>
</template>
