<script setup lang="ts">
/* 右侧抽屉：新建/编辑任务表单的容器（§4.6）。
   Esc 与背板点击都能关，但正在提交时不给关——表单发出去一半却把窗口关掉，
   用户就不知道自己到底建没建成。 */
import { onScopeDispose, watch } from 'vue'
import { X } from 'lucide-vue-next'

interface Props {
  open: boolean
  title: string
  /** 提交中：禁止关闭 */
  busy?: boolean
}

const props = defineProps<Props>()
const emit = defineEmits<{ close: [] }>()

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') requestClose()
}

function requestClose(): void {
  if (props.busy) return
  emit('close')
}

document.addEventListener('keydown', onKeydown)
onScopeDispose(() => document.removeEventListener('keydown', onKeydown))

// 抽屉打开时锁住背景滚动，否则关掉后页面会停在随手滚到的位置
let scrolled = false
watch(
  () => props.open,
  (open) => {
    if (open) {
      document.body.style.overflow = 'hidden'
      scrolled = true
    } else if (scrolled) {
      document.body.style.overflow = ''
      scrolled = false
    }
  },
)
onScopeDispose(() => {
  if (scrolled) document.body.style.overflow = ''
})
</script>

<template>
  <div v-if="open" class="fixed inset-0 z-40 flex justify-end">
    <div class="absolute inset-0 bg-black/25" @click="requestClose"></div>

    <section
      role="dialog"
      aria-modal="true"
      :aria-label="title"
      class="relative flex h-full w-full max-w-xl flex-col border-l border-[var(--color-border)] bg-white shadow-lg"
    >
      <header class="flex h-[var(--height-topbar)] shrink-0 items-center justify-between border-b border-[var(--color-border)] px-5">
        <h2 class="text-base font-semibold">{{ title }}</h2>
        <button
          type="button"
          class="rounded-[var(--radius-control)] p-1.5 text-[var(--color-text-muted)] transition-colors
            hover:bg-[var(--color-surface)] hover:text-[var(--color-text)]"
          :disabled="busy"
          :aria-label="busy ? '提交中，暂不可关闭' : '关闭'"
          @click="requestClose"
        >
          <X :size="18" aria-hidden="true" />
        </button>
      </header>

      <div class="flex-1 overflow-auto px-5 py-4">
        <slot />
      </div>

      <footer v-if="$slots.footer" class="flex shrink-0 items-center justify-end gap-2 border-t border-[var(--color-border)] px-5 py-3">
        <slot name="footer" />
      </footer>
    </section>
  </div>
</template>
