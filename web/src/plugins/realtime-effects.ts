/* 事件 → 缓存失效（§4.5 的第 2 件事）。
 *
 * 数据源仍是 REST：WS 事件只负责"该重取了"这个信号，不拿它当第二数据源，
 * 否则两套状态迟早对不上。debounce 500ms：到期风暴时逐条 invalidate
 * 会把请求打爆，而列表页 0.5 秒后重取一次看到的是同一份真相。
 */
import type { QueryClient } from '@tanstack/vue-query'
import { queryKeys } from '../api/keys'
import type { JobEvent } from '../api/types'
import type { useRealtimeStore } from '../stores/realtime'

const DEBOUNCE_MS = 500

export function bindRealtimeToQueries(
  realtime: ReturnType<typeof useRealtimeStore>,
  queryClient: QueryClient,
): () => void {
  const touchedJobIDs = new Set<string>()
  let timer: ReturnType<typeof setTimeout> | null = null

  function flush(): void {
    timer = null

    // 任意 job.* 事件都会牵动统计、列表与分组计数
    void queryClient.invalidateQueries({ queryKey: queryKeys.stats })
    void queryClient.invalidateQueries({ queryKey: queryKeys.jobsAll })
    void queryClient.invalidateQueries({ queryKey: queryKeys.groups })

    for (const jobID of touchedJobIDs) {
      void queryClient.invalidateQueries({ queryKey: queryKeys.job(jobID) })
      // 详情页时间线不靠重取：打开时拉一次，此后由 WS 事件 append（§4.7）
    }
    touchedJobIDs.clear()
  }

  function onEvent(event: JobEvent): void {
    // heap.updated 这类没有归属的事件不改任何任务，重取只是白费一次往返
    if (!event.job_id) return
    touchedJobIDs.add(event.job_id)
    if (timer !== null) return
    timer = setTimeout(flush, DEBOUNCE_MS)
  }

  realtime.setEventListener(onEvent)

  return () => {
    realtime.setEventListener(null)
    if (timer !== null) clearTimeout(timer)
    timer = null
    touchedJobIDs.clear()
  }
}
