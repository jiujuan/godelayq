/* fetch 封装：注入 Bearer、401 静默刷新一次并重放、错误规范化。
 *
 * 这一层刻意不认识 Pinia：认证态在 stores/auth.ts，由它启动时把自己注册进来
 * （registerAuthBridge）。否则 client ↔ store 互相 import 成环，
 * 打包后的初始化顺序会变成"看运气"。
 */
import type { ErrorResponse } from './types'

export const API_BASE = '/api/v1'

/** 后端错误响应（ErrorResponse）的规范化形态 */
export class ApiError extends Error {
  readonly status: number
  readonly code: number
  readonly details?: string

  constructor(status: number, payload?: Partial<ErrorResponse>) {
    const message = payload?.message ?? `request failed with status ${status}`
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = payload?.code ?? status
    this.details = payload?.details
  }
}

export interface AuthBridge {
  /** 当前 access token；未登录返回 null */
  accessToken(): string | null
  /** 用 refresh token 换新 access token，成功返回 true */
  refresh(): Promise<boolean>
  /** 刷新也救不回来：清凭据并跳登录 */
  sessionLost(): void
}

let bridge: AuthBridge | null = null

export function registerAuthBridge(next: AuthBridge): void {
  bridge = next
}

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'DELETE'
  /** 请求体：对象会 JSON 序列化 */
  body?: unknown
  /** 查询参数：值为 undefined 的键整个省略，空串保留（group= 与省略是两回事） */
  query?: Record<string, string | number | undefined>
  signal?: AbortSignal
  /**
   * 不带凭据：登录与刷新自身要用"没有可用 access token"的身份发请求，
   * 且它们的 401 绝不能触发刷新链路（否则一次失败会递归重试成环）。
   */
  skipAuth?: boolean
}

function buildUrl(path: string, query?: Record<string, string | number | undefined>): string {
  const url = `${API_BASE}${path}`
  if (!query) return url

  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined) continue
    params.set(key, String(value))
  }
  const encoded = params.toString()
  return encoded ? `${url}?${encoded}` : url
}

async function parseError(response: Response): Promise<ApiError> {
  // 错误体解析失败也要给出可读错误：网关或反代可能返回 HTML
  let payload: Partial<ErrorResponse> | undefined
  try {
    payload = (await response.json()) as Partial<ErrorResponse>
  } catch {
    payload = undefined
  }
  return new ApiError(response.status, payload)
}

async function send(path: string, options: RequestOptions): Promise<Response> {
  const headers: Record<string, string> = {}
  if (options.body !== undefined) headers['Content-Type'] = 'application/json'

  if (!options.skipAuth) {
    const token = bridge?.accessToken()
    if (token) headers['Authorization'] = `Bearer ${token}`
  }

  return fetch(buildUrl(path, options.query), {
    method: options.method ?? 'GET',
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
    signal: options.signal,
  })
}

/**
 * 发一个请求并按 JSON 解析响应。
 * 401 且不是登录/刷新自身时：刷新一次并重放；再失败就交给 sessionLost() 收尾。
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  let response = await send(path, options)

  if (response.status === 401 && !options.skipAuth && bridge) {
    const refreshed = await bridge.refresh()
    if (!refreshed) {
      bridge.sessionLost()
      throw await parseError(response)
    }
    response = await send(path, options)
  }

  if (!response.ok) throw await parseError(response)

  if (response.status === 204) return undefined as T

  const text = await response.text()
  if (text === '') return undefined as T
  return JSON.parse(text) as T
}

/** 只关心成功与否、不取响应体的写法（204 与空体都算成功） */
export async function requestNoContent(path: string, options: RequestOptions = {}): Promise<void> {
  await request<void>(path, options)
}
