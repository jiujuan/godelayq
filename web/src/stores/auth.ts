/* 认证态：access/refresh token、当前身份与角色。
 *
 * 凭据放内存 + sessionStorage，不写 localStorage（§4.4）：XSS 后拿到的
 * 就不是一把长期可用的钥匙；标签页关掉即失效。
 * 刷新页面时先 restore()，access token 过期不碍事——refresh 链路会静默续期。
 */
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import * as authApi from '../api/auth'
import { ApiError } from '../api/client'
import type { RoleName, TokenSession, UserInfo } from '../api/types'

const ACCESS_KEY = 'godelayq.access'
const REFRESH_KEY = 'godelayq.refresh'
const USER_KEY = 'godelayq.user'

/** 与后端 core.Role 的阶梯一致：viewer < operator < admin < ops */
const ROLE_RANK: Record<RoleName, number> = {
  viewer: 1,
  operator: 2,
  admin: 3,
  ops: 4,
}

function readSessionStorage(key: string): string | null {
  try {
    return sessionStorage.getItem(key)
  } catch {
    // 隐私模式或存储被禁用：当作没有凭据，绝不能因为取不到就抛
    return null
  }
}

function writeSessionStorage(key: string, value: string): void {
  try {
    sessionStorage.setItem(key, value)
  } catch {
    /* 同上：存不下就不存，功能降级为"标签页内有效" */
  }
}

export const useAuthStore = defineStore('auth', () => {
  const accessToken = ref<string | null>(null)
  const refreshToken = ref<string | null>(null)
  const user = ref<UserInfo | null>(null)
  /** access token 的过期时间（后端给的，不在前端解析 JWT） */
  const expiresAt = ref<string | null>(null)
  /** 后端未配任何凭据时的"直接进入"模式（§4.6 LoginView） */
  const authDisabled = ref(false)

  const isAuthenticated = computed(() => authDisabled.value || accessToken.value !== null)
  const role = computed<RoleName | null>(() => user.value?.role ?? null)

  function applySession(session: TokenSession): void {
    accessToken.value = session.access_token
    refreshToken.value = session.refresh_token
    user.value = session.user
    expiresAt.value = session.expires_at
    authDisabled.value = false
    writeSessionStorage(ACCESS_KEY, session.access_token)
    writeSessionStorage(REFRESH_KEY, session.refresh_token)
    writeSessionStorage(USER_KEY, JSON.stringify(session.user))
    writeSessionStorage('godelayq.expires_at', session.expires_at)
  }

  function clear(): void {
    accessToken.value = null
    refreshToken.value = null
    user.value = null
    expiresAt.value = null
    authDisabled.value = false
    for (const key of [ACCESS_KEY, REFRESH_KEY, USER_KEY, 'godelayq.expires_at']) {
      try {
        sessionStorage.removeItem(key)
      } catch {
        /* 忽略 */
      }
    }
  }

  /** 页面刷新后从 sessionStorage 恢复；access token 是否过期不重要 */
  function restore(): void {
    const access = readSessionStorage(ACCESS_KEY)
    const refresh = readSessionStorage(REFRESH_KEY)
    if (!access || !refresh) return

    accessToken.value = access
    refreshToken.value = refresh
    expiresAt.value = readSessionStorage('godelayq.expires_at')
    const raw = readSessionStorage(USER_KEY)
    if (raw) {
      try {
        user.value = JSON.parse(raw) as UserInfo
      } catch {
        user.value = null
      }
    }
  }

  async function login(username: string, password: string): Promise<void> {
    const session = await authApi.login(username, password)
    applySession(session)
  }

  /**
   * 并发的 401 只能触发一次刷新：refresh token 是一次一用（轮换），
   * 三个请求各自去刷就会有两个拿到已被作废的旧令牌，结果是莫名被踢出登录。
   */
  let inflight: Promise<boolean> | null = null

  async function refresh(): Promise<boolean> {
    if (authDisabled.value) return true
    if (inflight) return inflight

    inflight = (async () => {
      const current = refreshToken.value
      if (!current) return false
      try {
        const session = await authApi.refreshSession(current)
        applySession(session)
        return true
      } catch (err) {
        // 刷新令牌被吊销/过期就是真没救了；网络错误则保留凭据，让用户还在原页面
        if (err instanceof ApiError && err.status >= 400 && err.status < 500) clear()
        return false
      } finally {
        inflight = null
      }
    })()

    return inflight
  }

  /** 真注销：先让服务端吊销凭据，再清本地。服务端失败也要清，否则用户被困在登录态里 */
  async function logout(): Promise<void> {
    if (authDisabled.value) {
      clear()
      return
    }
    try {
      await authApi.logout(refreshToken.value)
    } catch {
      /* 服务端的 jti 拒绝表会兜住，本地不该因此失败 */
    }
    clear()
  }

  /**
   * 探测后端是否启用了鉴权：/health 在启用凭据时要求凭据（401），未启用时 200。
   * 这是登录页"本服务未启用鉴权，直接进入"这句提示的依据。
   */
  async function probeAuthMode(): Promise<boolean> {
    try {
      await authApi.fetchHealthAnonymous()
      authDisabled.value = true
      user.value = { name: 'anonymous', role: 'ops' }
      return true
    } catch {
      authDisabled.value = false
      return false
    }
  }

  function roleAtLeast(min: RoleName): boolean {
    if (authDisabled.value) return true
    const current = role.value
    if (!current) return false
    return ROLE_RANK[current] >= ROLE_RANK[min]
  }

  return {
    accessToken,
    refreshToken,
    user,
    expiresAt,
    authDisabled,
    isAuthenticated,
    role,
    restore,
    login,
    refresh,
    logout,
    clear,
    probeAuthMode,
    roleAtLeast,
  }
})
