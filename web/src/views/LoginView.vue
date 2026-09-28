<script setup lang="ts">
/* 登录页。错误文案按后端状态码分派：
   401 一律"账号或密码错误"（后端对"用户不存在"与"密码错"返回同一码同一文案，
   前端不要试图区分，否则等于帮忙枚举账号）；429 是限流；
   400 "console accounts are not configured" 说明这台服务压根没账号体系。 */
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { AlertTriangle, ShieldOff, Timer } from 'lucide-vue-next'
import { ApiError } from '../api/client'
import { useAuthStore } from '../stores/auth'
import UiButton from '../components/ui/UiButton.vue'
import UiInput from '../components/ui/UiInput.vue'

const auth = useAuthStore()
const router = useRouter()
const route = useRoute()

const username = ref('')
const password = ref('')
const submitting = ref(false)
const errorMessage = ref<string | null>(null)

const target = computed(() => {
  const redirect = route.query.redirect
  return typeof redirect === 'string' && redirect.startsWith('/') ? redirect : '/'
})

async function submit(): Promise<void> {
  if (!username.value || !password.value) {
    errorMessage.value = '请输入账号与密码'
    return
  }

  submitting.value = true
  errorMessage.value = null
  try {
    await auth.login(username.value, password.value)
    await router.replace(target.value)
  } catch (err) {
    errorMessage.value = describe(err)
  } finally {
    submitting.value = false
  }
}

function describe(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 401) return '账号或密码错误'
    if (err.status === 429) return '尝试过于频繁，请稍后再试'
    if (err.status === 400 && err.message.includes('not configured')) {
      return '本服务未配置控制台账号，请改用直接入口或联系部署方'
    }
    return err.message
  }
  // fetch 抛 TypeError 意味着请求压根没发出去（后端没起 / 跨域被拒 / 断网）
  return '服务不可达，请确认后端地址与网络'
}

async function enterWithoutAuth(): Promise<void> {
  await router.replace(target.value)
}

onMounted(async () => {
  // 探测放在登录页而不是全局启动路径：已登录用户不该为这个请求多等一趟
  await auth.probeAuthMode()
})
</script>

<template>
  <div class="flex min-h-full items-center justify-center bg-[var(--color-surface)] p-6">
    <div class="w-full max-w-sm rounded-[var(--radius-card)] border border-[var(--color-border)] bg-white p-6 shadow-sm">
      <div class="mb-6 flex items-center gap-2">
        <Timer class="text-[var(--color-primary)]" :size="22" aria-hidden="true" />
        <h1 class="text-base font-semibold">godelayq 控制台</h1>
      </div>

      <form class="flex flex-col gap-4" @submit.prevent="submit">
        <UiInput
          v-model="username"
          label="账号"
          autocomplete="username"
          :autofocus="true"
          :disabled="submitting"
          @update:model-value="errorMessage = null"
        />
        <UiInput
          v-model="password"
          label="密码"
          type="password"
          autocomplete="current-password"
          :disabled="submitting"
          @update:model-value="errorMessage = null"
        />

        <p
          v-if="errorMessage"
          class="flex items-start gap-2 text-sm text-[var(--color-status-failed)]"
          role="alert"
        >
          <AlertTriangle class="mt-0.5 shrink-0" :size="14" aria-hidden="true" />
          {{ errorMessage }}
        </p>

        <UiButton type="submit" :loading="submitting" class="w-full">登录控制台</UiButton>
      </form>

      <div
        v-if="auth.authDisabled"
        class="mt-6 flex flex-col gap-3 border-t border-[var(--color-border)] pt-4"
      >
        <p class="flex items-center gap-2 text-sm text-[var(--color-text-muted)]">
          <ShieldOff :size="16" aria-hidden="true" />
          本服务未启用鉴权，可直接进入
        </p>
        <UiButton variant="outline" @click="enterWithoutAuth">直接进入</UiButton>
      </div>
    </div>
  </div>
</template>
