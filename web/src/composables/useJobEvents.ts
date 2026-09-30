/* 详情页运行时间线（§4.6 JobDetailView）。
 *
 * 首屏取 GET /jobs/:id/events（时间升序；读的是持久化事件库还是内存缓冲由响应的 note 说明），
 * 此后由 WS 事件续上。
 * 实时部分读的是 realtime store 的 200 条全局环形缓冲，而不是给它再开一个监听位：
 * 监听位只有一个且已被"事件 → Query 失效"管线占用，多处注册会互相覆盖，
 * 而缓冲里本来就带着这条时间线需要的全部字段。
 * 代价是高频任务的旧事件可能被缓冲挤出——首屏那批已经在手上，不会因此丢历史。
 */
import { useQuery } from '@tanstack/vue-query'
import { computed, toValue, type MaybeRefOrGetter } from 'vue'
import { fetchJobEvents } from '../api/events'
import { queryKeys } from '../api/keys'
import { useRealtimeStore } from '../stores/realtime'
import type { JobEvent } from '../api/types'

/** 同一条事件既可能来自首屏、也可能来自缓冲：按这三个字段判定重复 */
function eventKey(event: JobEvent): string {
  return `${event.job_id}|${event.timestamp}|${event.type}`
}

function timeOf(event: JobEvent): number {
  const parsed = new Date(event.timestamp).getTime()
  return Number.isNaN(parsed) ? 0 : parsed
}

/**
 * 参数用 MaybeRefOrGetter：/jobs/:id → /jobs/:id 是同一条路由记录，
 * 组件实例会被复用，捕获一次 id 就会永远停在第一条任务上（实测踩过）。
 */
export function useJobEvents(jobId: MaybeRefOrGetter<string>) {
  const realtime = useRealtimeStore()
  const currentId = () => toValue(jobId)

  const query = useQuery({
    queryKey: computed(() => queryKeys.jobEvents(currentId())),
    queryFn: () => fetchJobEvents(currentId()),
    // 打开时拉一次，之后靠 WS append：轮询一份只会 append 的时间线是白费往返
    staleTime: Number.POSITIVE_INFINITY,
  })

  const timeline = computed<JobEvent[]>(() => {
    const id = currentId()
    const history = query.data.value?.items ?? []

    // 缓冲是 newest-first 的全局流，先按 job_id 收窄再归并
    const live = realtime.events.filter((event) => event.job_id === id)

    const seen = new Set<string>()
    const merged: JobEvent[] = []
    for (const event of [...history, ...live]) {
      const key = eventKey(event)
      if (seen.has(key)) continue
      seen.add(key)
      merged.push(event)
    }

    return merged.sort((a, b) => timeOf(a) - timeOf(b))
  })

  return {
    timeline,
    // note 由后端给出，说明这批事件读的是哪一份数据；UI 引用它而不是复述某一句文本
    note: computed(() => query.data.value?.note ?? ''),
    isLoading: computed(() => query.isPending.value),
    error: computed(() => query.error.value ?? null),
    /** 首屏那批的条数，用来区分"这个任务确实没事件"与"还没取到" */
    historyCount: computed(() => query.data.value?.count ?? 0),
  }
}
