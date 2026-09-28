/* 分组注册表端点（§5.5）。未装配注册表的部署里这些端点返回 503，
   任务上的 group 标签不受影响——JobsView 的分组下拉因此不能依赖这一组端点。 */
import { request } from './client'
import type { CreateGroupRequest, Group, UpdateGroupRequest } from './types'

/** 注册条目与"只挂在任务标签上的组"合并后按名称排序返回 */
export function listGroups(): Promise<Group[]> {
  return request<Group[]>('/groups')
}

export function createGroup(body: CreateGroupRequest): Promise<Group> {
  return request<Group>('/groups', { method: 'POST', body })
}

/** 改名会连带改写挂着旧组名的任务标签（堆内条目一起改），不是只动注册表 */
export function updateGroup(name: string, body: UpdateGroupRequest): Promise<Group> {
  return request<Group>(`/groups/${encodeURIComponent(name)}`, { method: 'PUT', body })
}

/**
 * 删除组。默认 detach：组内任务只解除分组，不会被删除或取消（决策 D5）。
 * block 是显式选择的"先自己清干净"语义，组非空时后端返回 409。
 */
export function deleteGroup(name: string, strategy?: 'detach' | 'block'): Promise<void> {
  return request<void>(`/groups/${encodeURIComponent(name)}`, {
    method: 'DELETE',
    query: { strategy },
  })
}
