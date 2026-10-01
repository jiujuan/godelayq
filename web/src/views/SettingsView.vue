<script setup lang="ts">
/* 设置页：当前身份与凭据边界 + 这台服务器声明了哪些执行器档位。
   账号增删改是配置文件的事；档位这一节曾经也只能读，TASK-W06/W08 之后在线档位归
   档位管理页（ProfilesView）管，这里留一份只读概览与一个入口，不做第二套表单。 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { useQuery } from '@tanstack/vue-query'
import { ExternalLink } from 'lucide-vue-next'
import PageHeader from '../components/layout/PageHeader.vue'
import UiBadge from '../components/ui/UiBadge.vue'
import { listExecutors } from '../api/executors'
import { queryKeys } from '../api/keys'
import { useAuthStore } from '../stores/auth'
import { usePermission } from '../composables/usePermission'

const auth = useAuthStore()
const permission = usePermission()

/** 档位管理页的入口只对管得动它的人露出来（与服务端四个管理端点同一条 ops 门槛） */
const canManageProfiles = computed(() => permission.can('executor.profile_create'))

/**
 * 档位一览留在本页（TASK-E18 §3.5 第 3 条）：这一页本来就是"看这台机器配置了什么"的地方，
 * 而"改"的部分自 TASK-W08 起搬到了独立视图——表单有十几组字段和一张参数表，塞进来会把这里变成第二个编辑页。
 */
const executorsQuery = useQuery({
  queryKey: queryKeys.executors,
  queryFn: listExecutors,
  staleTime: 5 * 60_000,
})

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

/** 执行器一节的状态：关闭 / 开了但没档位 / 有档位，三种情况给三种说法而不是空表 */
const executorState = computed(() => {
  const list = executorsQuery.data.value
  if (!list) return 'loading'
  if (!list.enabled) return 'disabled'
  return list.profiles.length === 0 ? 'empty' : 'ready'
})

/**
 * 档位一览的显示行。
 *
 * 不显示 env 的固定取值：那是配置里的凭据，后端的 GET /executors 本来也不给（env_allow 只有键名），
 * 这一页同样只列键名。参数列给的是"名字 + 必填 + 是否凭据 + 格式"，
 * 格式原文可能很长，所以只在这一节的说明里出现一次，不做逐行截断。
 */
const profiles = computed(() => executorsQuery.data.value?.profiles ?? [])
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

    <!-- 执行器档位：这一节只读，增删改在线档位去档位管理页（TASK-W08） -->
    <section class="mt-6 rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-4">
      <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
        <h2 class="text-sm font-semibold">执行器档位</h2>
        <div class="flex flex-wrap items-center gap-2">
          <UiBadge :tone="executorState === 'ready' ? 'success' : executorState === 'disabled' ? 'neutral' : 'warning'">
            {{ executorState === 'ready' ? `${profiles.length} 个档位` : executorState === 'disabled' ? '未启用' : '开了，但没有档位' }}
          </UiBadge>
          <RouterLink
            v-if="canManageProfiles"
            :to="{ name: 'profiles' }"
            class="flex items-center gap-1 text-xs text-[var(--color-primary)] hover:underline"
          >
            <ExternalLink :size="12" aria-hidden="true" />
            去档位管理
          </RouterLink>
        </div>
      </div>

      <p class="mb-3 text-xs leading-relaxed text-[var(--color-text-muted)]">
        档位可以来自两个地方：配置的 <code>executors.commands</code>，以及档位文件
        <code>executors.profiles_path</code>。<em class="font-medium text-[var(--color-text)]">在
        档位管理页上改的那些立即生效，并且活过重启</em>；<em class="font-medium text-[var(--color-text)]">配置里的那一部分仍然要重启进程
        才改得动</em>（<code>executors.web_enabled</code> 打开时由 ops 身份在线增删改档位文件）。
        这一节只是把"这台机器现在能执行什么"说清楚。<em class="font-medium text-[var(--color-text)]">"现在能不能跑"是探测结论</em>，
        看的是程序在不在 PATH、脚本文件在不在，与配置是否合法是两件事。
      </p>

      <p v-if="executorState === 'disabled'" class="text-sm text-[var(--color-text-muted)]">
        <code>executors.enabled</code> 是关闭的：没有任何档位可提交，任务列表里的 exec.* 类型也不会被接受。
      </p>
      <p v-else-if="executorState === 'empty'" class="text-sm text-[var(--color-text-muted)]">
        执行器已启用，但两份来源里都还没有档位（配置的 <code>executors.commands</code> 与档位文件
        <code>executors.profiles_path</code>）——先声明至少一个档位才能提交执行器任务。
      </p>

      <ul v-else-if="executorState === 'ready'" class="flex flex-col divide-y divide-[var(--color-border)]">
        <li v-for="profile in profiles" :key="profile.key + (profile.degraded ? '#degraded' : '')"
            class="flex flex-col gap-2 py-3 text-sm">
          <div class="flex flex-wrap items-center gap-2">
            <code class="font-mono text-xs">{{ profile.key }}</code>
            <UiBadge :tone="profile.runtime_ok ? 'success' : 'danger'">
              {{ profile.runtime_ok ? '可用' : '不可用' }}
            </UiBadge>
            <UiBadge :tone="profile.source === 'store' ? 'primary' : 'neutral'">
              {{ profile.source === 'store' ? '在线档位' : '来自配置' }}
            </UiBadge>
            <UiBadge v-if="profile.degraded" tone="warning">与配置同名，未生效</UiBadge>
            <span class="text-xs text-[var(--color-text-muted)]">{{ profile.kind }}</span>
            <span class="ml-auto text-xs tabular-nums text-[var(--color-text-muted)]">
              单次执行 {{ profile.timeout }} · 同时最多 {{ profile.max_parallel }} 个
            </span>
          </div>

          <p v-if="!profile.runtime_ok" class="text-xs text-[var(--color-status-failed)]">
            原因：{{ profile.reason }}
          </p>

          <p v-if="profile.url" class="text-xs text-[var(--color-text-muted)]">
            地址模板 <code class="font-mono">{{ profile.url }}</code>
            <template v-if="profile.method"> · 方法 {{ profile.method }}</template>
            <template v-if="profile.body_mode && profile.body_mode !== 'none'"> · 请求体 {{ profile.body_mode }}</template>
          </p>

          <p class="text-xs leading-relaxed text-[var(--color-text-muted)]">
            <template v-if="profile.args.length === 0">没有具名参数。</template>
            <template v-else>
              参数：
              <span v-for="(arg, index) in profile.args" :key="arg.name">
                <code class="font-mono">{{ arg.name }}</code>
                <template v-if="arg.required">（必填）</template>
                <template v-if="arg.secret">（凭据，不回显）</template>
                <template v-if="index < profile.args.length - 1">、</template>
              </span>
            </template>
            <template v-if="profile.positional">
              · 位置参数最多 {{ profile.positional.max }} 个
            </template>
            <template v-if="profile.env_allow.length > 0">
              · 可注入的环境变量：{{ profile.env_allow.join('、') }}（取值由配置决定，这里不显示）
            </template>
          </p>
        </li>
      </ul>

      <p v-if="executorState === 'ready'" class="mt-3 text-xs leading-relaxed text-[var(--color-text-muted)]">
        提交这些任务需要的档位：
        <code>{{ executorsQuery.data.value?.required_role ?? '—' }}</code>
        （配置项 <code>executors.required_role</code>）。
        单次执行超时的上限是 <code>{{ executorsQuery.data.value?.max_timeout ?? '—' }}</code>
        （配置项 <code>executors.max_timeout</code>）。
      </p>
    </section>
  </div>
</template>
