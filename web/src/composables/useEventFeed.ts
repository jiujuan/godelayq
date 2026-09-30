/* 全局事件流：后端缓冲回填 + 前端实时缓冲的归并（§4.5、§4.7 的 events 键）。
 *
 * 刷新页面不该让刚发生的事凭空消失：GET /events 补一次历史，此后由 WS append。
 * 首屏那批读的是持久化事件库还是内存缓冲由响应的 note 说明（装没装配事件库决定），
 * 但两路的内容同源——都是总线发布过的那一份，所以归并只需要去重，不存在两份真相。
 * 去重键用的是事件自带的 timestamp：只有"同一条事件既被 WS 推过又出现在首屏"时才用上，
 * 而库里读回来的时间戳精确到微秒（写入时截断），与 WS 那份不严格相等，
 * 这种重叠是刷新与推送抢同一瞬间的产物，不是常态。 */
import { useQuery } from '@tanstack/vue-query'
import { computed, type ComputedRef } from 'vue'
import { fetchRecentEvents } from '../api/events'
import { queryKeys } from '../api/keys'
import { useRealtimeStore } from '../stores/realtime'
import type { JobEvent } from '../api/types'

function eventKey(event: JobEvent): string {
  return `${event.job_id}|${event.timestamp}|${event.type}`
}

/**
 * @param backfillLimit 首屏从后端取多少条
 * @param matches 视图级谓词：服务端订阅过滤只作用于此后推来的事件，
 *                已经躺在缓冲与首屏里的事件仍要在展示时判一次，否则同一筛选两套结果
 */
export function useEventFeed(
  backfillLimit = 100,
  matches?: () => (event: JobEvent) => boolean,
): { events: ComputedRef<JobEvent[]>; note: ComputedRef<string>; isPending: ComputedRef<boolean> } {
  const realtime = useRealtimeStore()

  const backfill = useQuery({
    queryKey: queryKeys.eventsRecent,
    queryFn: () => fetchRecentEvents(backfillLimit),
    // 历史部分只补一次：之后的更新来自 WS，轮询一份只会变旧的第二数据源
    staleTime: Number.POSITIVE_INFINITY,
  })

  const events = computed(() => {
    const history = backfill.data.value?.items ?? []
    // 缓冲是 newest-first，首屏是 ascending；先拼起来再统一按时间倒序（最新在上）
    const merged: JobEvent[] = []
    const seen = new Set<string>()

    for (const event of [...history, ...realtime.events]) {
      const key = eventKey(event)
      if (seen.has(key)) continue
      seen.add(key)
      merged.push(event)
    }

    const predicate = matches?.()
    const shown = predicate ? merged.filter(predicate) : merged

    return shown.sort((a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime())
  })

  return {
    events,
    note: computed(() => backfill.data.value?.note ?? ''),
    isPending: computed(() => backfill.isPending.value),
  }
}
