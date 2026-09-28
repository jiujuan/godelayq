/* 路由与守卫（§4.4）。
 *
 * 菜单纯粹由路由表派生：meta.minimumRole 一处声明，Sidebar 过滤、守卫拦截都读它，
 * 避免"菜单里没有但 URL 能进"或反过来。
 * 用单一档位（minimumRole）而不是角色列表：后端本来就是一条阶梯
 * （core.Role.AtLeast），列 {['admin','ops']} 只是在重复阶梯已经说过的事。
 */
import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import {
  Activity,
  ClipboardList,
  FolderOpen,
  LayoutDashboard,
  Settings,
  Wrench,
  type LucideIcon,
} from 'lucide-vue-next'
import type { RoleName } from '../api/types'
import { useAuthStore } from '../stores/auth'
import { useToastStore } from '../stores/toast'

declare module 'vue-router' {
  interface RouteMeta {
    /** 侧边栏与 Topbar 显示的标题 */
    title?: string
    /** 侧边栏图标；缺省表示不出现在菜单里（详情页） */
    icon?: LucideIcon
    /** 需要的最低角色；缺省 = viewer 即可 */
    minimumRole?: RoleName
  }
}

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('../views/LoginView.vue'),
  },
  {
    path: '/',
    name: 'dashboard',
    component: () => import('../views/DashboardView.vue'),
    meta: { title: '概览', icon: LayoutDashboard },
  },
  {
    path: '/jobs',
    name: 'jobs',
    component: () => import('../views/JobsView.vue'),
    meta: { title: '任务', icon: ClipboardList },
  },
  {
    path: '/jobs/:id',
    name: 'job-detail',
    component: () => import('../views/JobDetailView.vue'),
    // 不给 icon：菜单只收顶层入口。带 :参数 的路由进菜单会让 RouterLink 因缺参数直接抛错，
    // 详情页的归属由 Sidebar 的 isActive 高亮"任务"来表达
    meta: { title: '任务详情' },
  },
  {
    path: '/groups',
    name: 'groups',
    component: () => import('../views/GroupsView.vue'),
    meta: { title: '分组', icon: FolderOpen },
  },
  {
    path: '/monitor',
    name: 'monitor',
    component: () => import('../views/MonitorView.vue'),
    meta: { title: '实时', icon: Activity },
  },
  {
    path: '/admin',
    name: 'admin',
    component: () => import('../views/AdminView.vue'),
    // 运维页改的是整个进程的行为，只有 ops 档能进（后端 /admin/* 同档位）
    meta: { title: '运维', icon: Wrench, minimumRole: 'ops' },
  },
  {
    path: '/settings',
    name: 'settings',
    component: () => import('../views/SettingsView.vue'),
    meta: { title: '设置', icon: Settings },
  },
  { path: '/:pathMatch(.*)*', redirect: { name: 'dashboard' } },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

/** 侧边栏菜单：由路由表派生，无 icon 的项（详情页）自动排除。
    minimumRole 一并带出，好让 Sidebar 过滤掉进不去的入口，而不是点了被弹回来。 */
export function menuItems(): {
  name: string
  path: string
  title: string
  icon: LucideIcon
  minimumRole?: RoleName
}[] {
  return routes
    .filter((route) => route.meta?.icon && route.meta?.title)
    // 带路径参数的路由不能进菜单：链接需要 id，RouterLink 会在 setup 里抛错
    .filter((route) => !route.path.includes(':'))
    .map((route) => ({
      name: String(route.name ?? route.path),
      path: route.path,
      title: route.meta!.title as string,
      icon: route.meta!.icon as LucideIcon,
      minimumRole: route.meta!.minimumRole as RoleName | undefined,
    }))
}

router.beforeEach((to) => {
  const auth = useAuthStore()

  if (to.name === 'login') {
    return auth.isAuthenticated ? { name: 'dashboard' } : true
  }

  if (!auth.isAuthenticated) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }

  if (to.meta.minimumRole && !auth.roleAtLeast(to.meta.minimumRole)) {
    // 前端拦截只是体验；真正的边界在服务端 RBAC（§5.7.3）
    useToastStore().warning(`需要 ${to.meta.minimumRole} 及以上角色`)
    return { name: 'dashboard' }
  }

  return true
})
