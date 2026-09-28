/* TanStack Query 客户端的默认策略。
 *
 * 4xx 不重试：403/404/409 是"这件事现在就是不行"，重试只是把同一句拒绝再问一遍。
 * 网络错误与 5xx 重试两次（含 401→刷新→重放那条链路之外的抖动）。
 */
import { QueryClient } from '@tanstack/vue-query'
import { ApiError } from '../api/client'

function shouldRetry(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError) {
    // 401 交给 client 的刷新链路：它能救回来就不会走到这里，
    // 走到这里说明确实没救，重试没有意义。
    if (error.status >= 400 && error.status < 500) return false
  }
  return failureCount < 2
}

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: shouldRetry,
        staleTime: 5_000,
        refetchOnWindowFocus: false,
      },
      mutations: { retry: false },
    },
  })
}
