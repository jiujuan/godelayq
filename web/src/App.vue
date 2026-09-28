<script setup lang="ts">
/* 应用外壳：登录页独占整屏，其余页面走两栏骨架（左导航 + 右内容，§4.3）。 */
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import AppSidebar from './components/layout/AppSidebar.vue'
import AppTopbar from './components/layout/AppTopbar.vue'
import AppToast from './components/ui/AppToast.vue'

const route = useRoute()
// name 为 undefined 表示初始导航还没落地：此刻不能当成"已登录的页面"，
// 否则整壳（含顶栏的 stats 轮询）会在冷启动的一拍里挂上去，
// 拿着空凭据打一次 401，再把守卫算好的 ?redirect= 覆盖掉
const hasChrome = computed(() => route.name !== undefined && route.name !== 'login')
</script>

<template>
  <div v-if="hasChrome" class="flex h-full overflow-hidden">
    <AppSidebar />
    <div class="flex min-w-0 flex-1 flex-col">
      <AppTopbar />
      <main class="flex-1 overflow-auto bg-[var(--color-surface)]">
        <RouterView />
      </main>
    </div>
  </div>
  <RouterView v-else />

  <AppToast />
</template>
