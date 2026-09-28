/* 认证端点封装（POST /auth/login|refresh|logout、GET /auth/me、POST /auth/ws-ticket）。
   login/refresh 必须 skipAuth：它们发生在"没有可用 access token"的时候，
   而且它们的 401 绝不能触发刷新链路。 */
import { request } from './client'
import type { TokenSession, UserInfo, WhoAmI, WsTicket } from './types'

export function login(username: string, password: string): Promise<TokenSession> {
  return request<TokenSession>('/auth/login', {
    method: 'POST',
    body: { username, password },
    skipAuth: true,
  })
}

export function refreshSession(refreshToken: string): Promise<TokenSession> {
  return request<TokenSession>('/auth/refresh', {
    method: 'POST',
    body: { refresh_token: refreshToken },
    skipAuth: true,
  })
}

/**
 * 登出。后端的 logout 需要凭据（吊销当前 access token 的 jti），
 * 所以它是"带凭据的正常请求"而不是 skipAuth：否则服务端的真注销做不到。
 */
export function logout(refreshToken: string | null): Promise<UserInfo | undefined> {
  return request<UserInfo | undefined>('/auth/logout', {
    method: 'POST',
    body: { refresh_token: refreshToken ?? '' },
  })
}

export function fetchMe(): Promise<WhoAmI> {
  return request<WhoAmI>('/auth/me')
}

/** 一次性实时通道票据（§5.7.5）：JWT 不进 URL 与访问日志 */
export function fetchWsTicket(): Promise<WsTicket> {
  return request<WsTicket>('/auth/ws-ticket', { method: 'POST' })
}

/**
 * 匿名探测 /health：启用凭据后它也要鉴据（401），未启用时 200。
 * 登录页据此决定要不要给出"直接进入"。skipAuth 是这条判断的全部意义——
 * 带着一个过期 token 去探会得到 401，从而把"没启用鉴权"误判成"已登录但坏了"。
 */
export function fetchHealthAnonymous(): Promise<{ status: string; time: string }> {
  return request<{ status: string; time: string }>('/health', { skipAuth: true })
}
