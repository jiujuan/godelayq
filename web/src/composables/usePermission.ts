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
  | 'executor.profile_create'
  | 'executor.profile_update'
  | 'executor.profile_delete'
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
  // 档位的在线管理（TASK-W08 对过 api/server.go 的 profiles 分组）：四个端点一律 RequireRole(ops)，
  // 包括读定义那一个（记录里有脚本路径、固定参数与请求头）。
  // 表里不给它单列一项，是因为界面只在"要编辑"时才去读定义——那时必然已经过了上面三项之一。
  'executor.profile_create': 'ops',
  'executor.profile_update': 'ops',
  'executor.profile_delete': 'ops',
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

  /**
   * 提交执行器任务要不要这个身份（TASK-E18 §3.3 第 4 条）。
   *
   * 门槛来自 GET /executors 的 required_role，也就是配置里的 executors.required_role，
   * 不是这张能力表里的常量：它是可以按部署改的，写死在这里就会和后端配置各说各话。
   * null（执行器关闭）与不认识的档位名都按"不能提交"处理——后者宁严勿松，
   * 前端表里少一档不该变成放行理由。
   *
   * 注意：这里只决定入口露不露，服务端才是边界。按钮藏起来之后，
   * 直接调 POST /jobs 仍然会拿到 403（api/handlers_executors.go 的 gateExecutorSubmissionRole）。
   */
  function canSubmitExecutorJobs(required: string | null): boolean {
    if (!required) return false
    if (auth.authDisabled) return true
    return auth.roleAtLeast(required as RoleName)
  }

  return { can, requiredRole, blockedReason, canSubmitExecutorJobs }
}
