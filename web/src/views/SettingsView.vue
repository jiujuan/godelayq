<script setup lang="ts">
/* 设置页：当前身份与凭据边界。
   这里刻意只读——账号增删改是配置文件的事，运行时改账号不在本期范围（§1 非目标）。 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import PageHeader from '../components/layout/PageHeader.vue'
import UiBadge from '../components/ui/UiBadge.vue'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()

const now = ref(Date.now())
const timer = setInterval(() => {
  now.value = Date.now()
}, 10_000)
onMounted(() => (now.value = Date.now()))
onBeforeUnmount(() => clearInterval(timer))

/** 剩余有效期：只用来提示"该续了"，不做本地鉴权判断（服务端才是边界） */
const remaining = computed(() => {
  if (auth.authDisabled) return '未启用鉴权'
  if (!auth.expiresAt) return '—'
  const ms = new Date(auth.expiresAt).getTime() - now.value
  if (Number.isNaN(ms)) return '—'
  if (ms <= 0) return '已过期（下次请求自动续期）'
  const minutes = Math.floor(ms / 60_000)
  return minutes >= 60 ? `${Math.floor(minutes / 60)} 小时 ${minutes % 60} 分` : `${minutes} 分`
})

const rows = computed(() => [
  { label: '账号', value: auth.user?.name ?? '未登录' },
  { label: '角色', value: auth.user?.role ?? '—' },
  { label: '访问令牌剩余', value: remaining.value },
  { label: '凭据存放', value: '内存 + sessionStorage（关闭标签页即失效）' },
  { label: '实时通道', value: '一次一用的 ticket，凭据不出现在 URL 与访问日志' },
])
</script>

<template>
  <div class="p-6">
    <PageHeader title="设置" subtitle="当前身份与凭据边界">
      <template #actions>
        <UiBadge v-if="auth.authDisabled" tone="warning">本服务未启用鉴权</UiBadge>
        <UiBadge v-else tone="primary">{{ auth.user?.role }}</UiBadge>
      </template>
    </PageHeader>

    <section class="rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white">
      <dl class="divide-y divide-[var(--color-border)]">
        <div v-for="row in rows" :key="row.label" class="flex gap-4 px-4 py-3 text-sm">
          <dt class="w-40 shrink-0 text-[var(--color-text-muted)]">{{ row.label }}</dt>
          <dd>{{ row.value }}</dd>
        </div>
      </dl>
    </section>

    <p class="mt-4 max-w-2xl text-xs leading-relaxed text-[var(--color-text-muted)]">
      账号与角色由服务端配置声明（<code>server.auth.users</code>），改动需重启进程；前端隐藏按钮只是体验，
      越权请求仍会被服务端以 403 拒绝。跨域与来源白名单见 <code>docs/api.md</code> 的鉴权与跨域章节。
    </p>
  </div>
</template>
