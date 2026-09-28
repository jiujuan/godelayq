/* 组合根：store、router、query client 在这里相遇。
 *
 * client 的刷新链路、事件→失效管线都在这装配，而不是让 api/client.ts 去 import store：
 * 那样 client ↔ store 成环，模块初始化顺序变成打包顺序的函数。
 */
import { watch } from 'vue'
import { createApp } from 'vue'
import { createPinia } from 'pinia'
import { VueQueryPlugin } from '@tanstack/vue-query'

import App from './App.vue'
import { router } from './router'
import { createQueryClient } from './plugins/query'
import { bindRealtimeToQueries } from './plugins/realtime-effects'
import { registerAuthBridge } from './api/client'
import { useAuthStore } from './stores/auth'
import { useRealtimeStore } from './stores/realtime'
import { useToastStore } from './stores/toast'
import './styles/index.css'

const app = createApp(App)
const pinia = createPinia()

app.use(pinia)
app.use(router)

const queryClient = createQueryClient()
app.use(VueQueryPlugin, { queryClient })

const auth = useAuthStore(pinia)
const realtime = useRealtimeStore(pinia)
const toasts = useToastStore(pinia)

auth.restore()

registerAuthBridge({
  accessToken: () => auth.accessToken,
  refresh: () => auth.refresh(),
  // 刷新链路彻底失败：本地凭据、实时连接、路由三处都要收口，缺一不可
  sessionLost: () => {
    auth.clear()
    realtime.stop()
    if (router.currentRoute.value.name !== 'login') {
      toasts.warning('登录已过期，请重新登录')
      void router.replace({ name: 'login', query: { redirect: router.currentRoute.value.fullPath } })
    }
  },
})

bindRealtimeToQueries(realtime, queryClient)

// 登录态驱动实时通道：登录即连、登出或会话失效即断
watch(
  () => auth.isAuthenticated,
  (authenticated) => {
    if (authenticated) void realtime.start()
    else realtime.stop()
  },
  { immediate: true },
)

// 换账号后 token 会刷新：连接本身不用重建（ticket 只在握握手时用一次）
app.mount('#app')
