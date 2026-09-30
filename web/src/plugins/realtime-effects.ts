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
  const finishedWithResult = new Set<string>()
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
    // 执行结论落定的那条任务，输出正文也可能刚被写进产物文件：让结果面板下次点击重取。
    // 只在这里失效、不去预取——/result 是 no-store 的按次读取，事件到页面开之间那段时间
    // 没有任何人会看它（TASK-E18 §3.1 第 3 条）。
    for (const jobID of finishedWithResult) {
      void queryClient.invalidateQueries({ queryKey: queryKeys.jobResultAll(jobID) })
    }
    touchedJobIDs.clear()
    finishedWithResult.clear()
  }

  function onEvent(event: JobEvent): void {
    // heap.updated 这类没有归属的事件不改任何任务，重取只是白费一次往返
    if (!event.job_id) return
    touchedJobIDs.add(event.job_id)
    if (carriesResult(event)) finishedWithResult.add(event.job_id)
    if (timer !== null) return
    timer = setTimeout(flush, DEBOUNCE_MS)
  }

  realtime.setEventListener(onEvent)

  return () => {
    realtime.setEventListener(null)
    if (timer !== null) clearTimeout(timer)
    timer = null
    touchedJobIDs.clear()
    finishedWithResult.clear()
  }
}

/**
 * 事件里有没有执行结论。
 *
 * 只有 job.completed / job.failed 会带 `data.result`（core 的 eventData 在有结论时才写这个键），
 * 按类型 + 按键存在一起判：类型会漏掉以后新增的带结论事件，只判键会把无关事件也算进来。
 */
function carriesResult(event: JobEvent): boolean {
  if (event.type !== 'job.completed' && event.type !== 'job.failed') return false
  const data = event.data as { result?: unknown } | null | undefined
  return !!data && data.result !== undefined && data.result !== null
}
