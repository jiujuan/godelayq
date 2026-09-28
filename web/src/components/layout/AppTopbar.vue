<script setup lang="ts">
/* 顶栏：页面标题 + 实时连接状态 + 当前身份与退出。
   连接徽标是给运维/用户看的唯一"数据是不是新的"提示，断线时必须显眼，
   否则用户会盯着一份静止的列表以为调度停了。 */
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { LogOut, UserRound } from 'lucide-vue-next'
import UiBadge from '../ui/UiBadge.vue'
import { useAuthStore } from '../../stores/auth'
import { useRealtimeStore, type ConnectionState } from '../../stores/realtime'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const realtime = useRealtimeStore()

const title = computed(() => route.meta.title ?? '控制台')

const connectionLabel: Record<ConnectionState, string> = {
  idle: '未连接',
  connecting: '连接中',
  connected: '实时连接',
  reconnecting: '重连中',
  offline: '已断开',
}

const tone = computed(() => {
  switch (realtime.state) {
    case 'connected':
      return 'success' as const
    case 'reconnecting':
    case 'connecting':
      return 'warning' as const
    default:
      return 'neutral' as const
  }
})

const title2 = computed(() => realtime.lastError ?? connectionLabel[realtime.state])

/** 退出后必须离开当前页：停留在原地会让用户以为"退出了但还在看数据"
    （§4.6.3：清本地态 + 断 WS + 跳 /login）。未启用鉴权时同样跳，
    登录页探测后给出"直接进入"，行为可预期。 */
async function signOut(): Promise<void> {
  await auth.logout()
  await router.replace({ name: 'login' })
}
</script>

<template>
  <header
    class="flex h-[var(--height-topbar)] shrink-0 items-center justify-between border-b border-[var(--color-border)] bg-white px-6"
  >
    <h1 class="text-base font-semibold">{{ title }}</h1>

    <div class="flex items-center gap-3">
      <UiBadge :tone="tone" dot :title="title2">
        {{ connectionLabel[realtime.state] }}
      </UiBadge>

      <div v-if="auth.user" class="flex items-center gap-2 text-sm text-[var(--color-text-muted)]">
        <UserRound :size="16" aria-hidden="true" />
        <span>{{ auth.user.name }}</span>
        <UiBadge tone="primary">{{ auth.user.role }}</UiBadge>
      </div>

      <button
        type="button"
        class="flex items-center gap-1.5 rounded-[var(--radius-control)] px-2 py-1 text-sm
          text-[var(--color-text-muted)] transition-colors hover:bg-[var(--color-surface)] hover:text-[var(--color-text)]"
        @click="signOut"
      >
        <LogOut :size="16" aria-hidden="true" />
        退出
      </button>
    </div>
  </header>
</template>
