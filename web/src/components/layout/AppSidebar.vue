<script setup lang="ts">
/* 左栏导航：240px，可折叠到 64px 只留图标（§4.3）。
   菜单项来自路由表并按时角色过滤——隐藏只是体验，守卫与服务端 RBAC 才是边界。 */
import { computed, ref } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { PanelLeftClose, PanelLeftOpen, Timer } from 'lucide-vue-next'
import { menuItems } from '../../router'
import { useAuthStore } from '../../stores/auth'

const COLLAPSE_KEY = 'godelayq.sidebar_collapsed'
// 折叠偏好放 sessionStorage：换标签页不该重置成一个看不见菜单的窄栏
const collapsed = ref(sessionStorage.getItem(COLLAPSE_KEY) === '1')

const route = useRoute()
const auth = useAuthStore()

const items = computed(() =>
  menuItems().filter((item) => !item.minimumRole || auth.roleAtLeast(item.minimumRole)),
)

/** 详情页要高亮"任务"：用路由名而不是路径前缀匹配，避免 /jobs/:id 里的 id 撞名 */
function isActive(name: string): boolean {
  if (route.name === name) return true
  if (name === 'jobs' && route.name === 'job-detail') return true
  return false
}

function toggle(): void {
  collapsed.value = !collapsed.value
  sessionStorage.setItem(COLLAPSE_KEY, collapsed.value ? '1' : '0')
}

const linkBase =
  'flex items-center gap-3 rounded-[var(--radius-control)] px-3 py-2 text-sm transition-colors'
</script>

<template>
  <aside
    :class="collapsed ? 'w-[var(--width-sidebar-collapsed)]' : 'w-[var(--width-sidebar)]'"
    class="flex h-full shrink-0 flex-col border-r border-[var(--color-border)] bg-white transition-[width]"
    aria-label="主导航"
  >
    <div class="flex h-[var(--height-topbar)] items-center gap-2 px-4">
      <Timer class="text-[var(--color-primary)]" :size="20" aria-hidden="true" />
      <span v-if="!collapsed" class="truncate text-sm font-semibold">godelayq 控制台</span>
    </div>

    <nav class="flex flex-1 flex-col gap-1 px-2 py-2">
      <RouterLink
        v-for="item in items"
        :key="item.name"
        :to="{ name: item.name }"
        :class="[
          linkBase,
          isActive(item.name)
            ? 'bg-[var(--color-primary-soft)] font-medium text-[var(--color-primary)]'
            : 'text-[var(--color-text-muted)] hover:bg-[var(--color-surface)] hover:text-[var(--color-text)]',
          collapsed ? 'justify-center px-0' : '',
        ]"
        :title="collapsed ? item.title : undefined"
      >
        <component :is="item.icon" :size="18" aria-hidden="true" />
        <span v-if="!collapsed">{{ item.title }}</span>
      </RouterLink>
    </nav>

    <div class="border-t border-[var(--color-border)] p-2">
      <button
        type="button"
        :class="[linkBase, collapsed ? 'justify-center px-0' : '', 'text-[var(--color-text-muted)] hover:bg-[var(--color-surface)]']"
        :aria-label="collapsed ? '展开侧栏' : '折叠侧栏'"
        @click="toggle"
      >
        <component :is="collapsed ? PanelLeftOpen : PanelLeftClose" :size="18" aria-hidden="true" />
        <span v-if="!collapsed">折叠</span>
      </button>
    </div>
  </aside>
</template>
