/* 秒级本地倒数（§4.6 表格列 next_run_in）。
 *
 * 后端给的是 Go Duration 字符串（"5m30s"），它是响应那一刻的值，
 * 列表要停在那里不动就会显得数据已经过期。这里按 trigger_at 在本地重新走秒，
 * 到点后不显示负数——挂起调度时任务本就"该跑而没跑"，负数只会让人以为倒计时坏了。
 */
import { computed, onScopeDispose, ref, type ComputedRef } from 'vue'

/** 全表共用一个 tick：一页 50 行各自起 setInterval 是 50 个定时器 */
const now = ref(Date.now())
let interval: ReturnType<typeof setInterval> | null = null
let users = 0

function acquire(): void {
  users++
  if (interval === null) {
    interval = setInterval(() => {
      now.value = Date.now()
    }, 1000)
  }
}

function release(): void {
  users = Math.max(0, users - 1)
  if (users === 0 && interval !== null) {
    clearInterval(interval)
    interval = null
  }
}

/** 与后端 Duration 的可读程度对齐：够用就行，不做毫秒级 */
export function formatCountdown(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000))
  const hours = Math.floor(total / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  const seconds = total % 60

  if (hours > 0) return `${hours}h ${minutes}m`
  if (minutes > 0) return `${minutes}m ${seconds}s`
  return `${seconds}s`
}

/**
 * @param triggerAt ISO 时间串；非 pending 的任务传 undefined
 * @returns 每秒更新的文案，undefined 表示没有可倒的目标
 */
export function useCountdown(triggerAt?: () => string | undefined): ComputedRef<string | undefined> {
  acquire()
  onScopeDispose(release)

  return computed(() => {
    const raw = triggerAt?.()
    if (!raw) return undefined

    const parsed = new Date(raw).getTime()
    if (Number.isNaN(parsed)) return undefined

    return formatCountdown(parsed - now.value)
  })
}

/**
 * 一表多行时用的那一档：整个表格订阅一次，逐行自己算差值。
 * 给每行各起一个 useCountdown 会把"共用一个 tick"这个前提抵消掉。
 */
export function useNowTick(): ComputedRef<number> {
  acquire()
  onScopeDispose(release)

  return computed(() => now.value)
}
