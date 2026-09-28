/* 能力表：把"这个按钮要不要置灰"从散落各处的角色字符串比较，
   收敛成一张与后端路由一一对应的表。
   注意：这里只决定 UI 体验，服务端 RBAC 才是边界（§5.7.3）；
   新增能力时必须同时确认后端有对应的 RequireRole，否则前端只是自我安慰。 */
import { useAuthStore } from '../stores/auth'
import type { RoleName } from '../api/types'

export type Capability =
  | 'jobs.read'
  | 'events.read'
  | 'job.create'
  | 'job.update'
  | 'job.cancel'
  | 'job.retry'
  | 'job.pause'
  | 'job.resume'
  | 'job.batch_ops'
  | 'job.force_pause'
  | 'group.read'
  | 'group.write'
  | 'group.delete'
  | 'admin.access'

/** 与 api/server.go 的 setupRoutes 对齐：改后端档位必须改这张表 */
const REQUIRED_ROLE: Record<Capability, RoleName> = {
  'jobs.read': 'viewer',
  'events.read': 'viewer',
  'job.create': 'operator',
  'job.update': 'operator',
  'job.cancel': 'operator',
  'job.retry': 'operator',
  'job.pause': 'operator',
  'job.resume': 'operator',
  'job.batch_ops': 'operator',
  'job.force_pause': 'admin',
  'group.read': 'viewer',
  'group.write': 'operator',
  'group.delete': 'admin',
  'admin.access': 'ops',
}

export function usePermission() {
  const auth = useAuthStore()

  function requiredRole(capability: Capability): RoleName {
    return REQUIRED_ROLE[capability]
  }

  function can(capability: Capability): boolean {
    return auth.roleAtLeast(requiredRole(capability))
  }

  /** 置灰按钮的提示文案：直接说出缺哪一档，用户才知道找谁申请 */
  function blockedReason(capability: Capability): string | null {
    if (can(capability)) return null
    return `需要 ${requiredRole(capability)} 及以上角色`
  }

  return { can, requiredRole, blockedReason }
}
