/* 极简通知队列：M3 只需要两类反馈——越权跳转与登录过期。
   组件渲染在 App 根部（components/ui/AppToast.vue）。 */
import { defineStore } from 'pinia'
import { ref } from 'vue'

export type ToastKind = 'info' | 'success' | 'warning' | 'error'

export interface Toast {
  id: number
  kind: ToastKind
  message: string
}

const DEFAULT_TIMEOUT = 4000

export const useToastStore = defineStore('toast', () => {
  const toasts = ref<Toast[]>([])
  let seq = 0

  function push(kind: ToastKind, message: string): void {
    const id = ++seq
    toasts.value = [...toasts.value, { id, kind, message }]
    setTimeout(() => dismiss(id), DEFAULT_TIMEOUT)
  }

  function dismiss(id: number): void {
    toasts.value = toasts.value.filter((toast) => toast.id !== id)
  }

  return {
    toasts,
    dismiss,
    info: (message: string) => push('info', message),
    success: (message: string) => push('success', message),
    warning: (message: string) => push('warning', message),
    error: (message: string) => push('error', message),
  }
})
