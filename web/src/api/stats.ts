/* 统计与只读元数据端点。列表/分组等写端点封装随 M4 的页面一起加。 */
import { request } from './client'
import type { Stats } from './types'

export function fetchStats(): Promise<Stats> {
  return request<Stats>('/stats')
}

export function fetchJobTypes(): Promise<string[]> {
  return request<{ types: string[] }>('/job-types').then((response) => response.types)
}
