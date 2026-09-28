/* 实时通道生命周期（§4.5）。
 *
 * 只做两件事：维持一条 WS 连接、把收到的事件塞进前端环形缓冲。
 * "事件 → 使哪些 Query 失效" 不在这里（plugins/realtime-effects.ts），
 * 存储层不认识 Query 客户端，否则登出/刷新链路与缓存互相缠绕，测不动。
 */
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { fetchWsTicket } from '../api/auth'
import { ApiError } from '../api/client'
import type { JobEvent } from '../api/types'

export type ConnectionState = 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'offline'

/** 前端事件缓冲条数：够 Dashboard/Monitor 用，又不至于把内存当缓存使 */
const BUFFER_LIMIT = 200

/** 服务端 60 秒空闲会断开。浏览器会自动应答协议层 ping，
    这里再发一条应用层 ping，让"连接是否还活着"在 JS 侧可观测 */
const PING_INTERVAL = 30_000

const RECONNECT_MIN = 1_000
const RECONNECT_MAX = 30_000

/** WS 服务器发的控制帧带 action（pong/subscribed/stats…），事件带 type */
function isControlFrame(payload: Record<string, unknown>): boolean {
  return typeof payload.action === 'string'
}

export const useRealtimeStore = defineStore('realtime', () => {
  const state = ref<ConnectionState>('idle')
  const events = ref<JobEvent[]>([])
  /** 每收到一个事件自增，供 effects 层 watch：数组是整体替换的引用，
      直接 watch events 会漏掉同长度替换 */
  const received = ref(0)
  const lastError = ref<string | null>(null)

  let socket: WebSocket | null = null
  let pingTimer: ReturnType<typeof setInterval> | null = null
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null
  let reconnectDelay = RECONNECT_MIN
  /** 是否"想要一条连接"：start 置真、stop 置假，重连只在这个前提下发生 */
  let wanted = false
  /** 一次只允许一个建连流程在跑（取票据是异步的，期间可能又有触发） */
  let opening = false

  const isConnected = computed(() => state.value === 'connected')

  /** 逐事件回调（plugins/realtime-effects.ts 用它做 Query 失效）。
      必须在这里给到每一个事件：缓冲是 newest-first 且定长截断的，
      外部靠 watch 数组做差分一定会漏掉 debounce 期间到达的那几条。 */
  let listener: ((event: JobEvent) => void) | null = null

  function setEventListener(next: ((event: JobEvent) => void) | null): void {
    listener = next
  }

  function clearTimers(): void {
    if (pingTimer !== null) {
      clearInterval(pingTimer)
      pingTimer = null
    }
    if (reconnectTimer !== null) {
      clearTimeout(reconnectTimer)
      reconnectTimer = null
    }
  }

  function wsUrl(ticket: string): string {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const url = new URL(`${protocol}//${window.location.host}/ws`)
    // 凭据只走一次性 ticket：JWT 出现在 URL 里会被访问日志原样记下（docs/api.md 凭据通道）
    url.searchParams.set('ticket', ticket)
    return url.toString()
  }

  function record(event: JobEvent): void {
    // 新事件放最前面：事件流 UI 要"最新在上"，固定长度的切片也最好算
    events.value = [event, ...events.value].slice(0, BUFFER_LIMIT)
    received.value++
    listener?.(event)
  }

  function scheduleReconnect(): void {
    if (!wanted || reconnectTimer !== null || socket) return
    state.value = 'reconnecting'
    const wait = reconnectDelay
    // 指数退避到 30s 封顶：服务重启时不至于把浏览器变成 DoS 源
    reconnectDelay = Math.min(reconnectDelay * 2, RECONNECT_MAX)
    reconnectTimer = setTimeout(() => {
      reconnectTimer = null
      void open()
    }, wait)
  }

  async function open(): Promise<void> {
    if (!wanted || opening || socket || reconnectTimer !== null) return
    opening = true
    state.value = 'connecting'

    let ticket: string
    try {
      ticket = (await fetchWsTicket()).ticket
    } catch (err) {
      opening = false
      lastError.value = err instanceof ApiError ? err.message : '实时票据申领失败'
      if (err instanceof ApiError && err.status === 401) {
        // 会话确实没了：停止折腾，等 wiring 层把我们停下来
        state.value = 'offline'
        return
      }
      scheduleReconnect()
      return
    }
    opening = false

    await connect(ticket)
  }

  function connect(ticket: string): Promise<void> {
    return new Promise((resolve) => {
      let settled = false
      const finish = (): void => {
        if (settled) return
        settled = true
        resolve()
      }

      state.value = 'connecting'

      try {
        socket = new WebSocket(wsUrl(ticket))
      } catch {
        lastError.value = 'WebSocket 连接失败'
        scheduleReconnect()
        finish()
        return
      }

      socket.onopen = () => {
        state.value = 'connected'
        lastError.value = null
        reconnectDelay = RECONNECT_MIN
        pingTimer = setInterval(() => {
          if (socket?.readyState === WebSocket.OPEN) {
            socket.send(JSON.stringify({ action: 'ping' }))
          }
        }, PING_INTERVAL)
        finish()
      }

      socket.onmessage = (message: MessageEvent<string>) => {
        let payload: Record<string, unknown>
        try {
          payload = JSON.parse(message.data) as Record<string, unknown>
        } catch {
          return
        }
        if (isControlFrame(payload)) return
        record(payload as unknown as JobEvent)
      }

      socket.onerror = () => {
        lastError.value = '实时连接异常'
      }

      socket.onclose = () => {
        clearTimers()
        socket = null
        if (!wanted) {
          state.value = 'idle'
          finish()
          return
        }
        scheduleReconnect()
        finish()
      }
    })
  }

  /** 登录后调用；已有连接或正在重连时不会重复起 */
  async function start(): Promise<void> {
    wanted = true
    await open()
  }

  /** 登出/会话失效：断连并清缓冲，避免下一个账号看到上一个人的事件 */
  function stop(): void {
    wanted = false
    clearTimers()
    if (socket) {
      // 先把回调摘掉：主动关闭不该被当成一次意外断线去重连
      socket.onopen = null
      socket.onmessage = null
      socket.onerror = null
      socket.onclose = null
      socket.close()
      socket = null
    }
    state.value = 'offline'
    events.value = []
    lastError.value = null
  }

  return { state, events, received, lastError, isConnected, start, stop, setEventListener }
})
