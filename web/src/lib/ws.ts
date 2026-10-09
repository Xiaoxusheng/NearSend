/**
 * WebSocket 客户端。
 *
 * 职责：
 *  - 首帧发送 client.hello 携带会话令牌（令牌不进 URL，避免进入访问日志）。
 *  - 断线自动重连（指数退避 + 抖动），重连成功后触发 onResync，
 *    由上层重新拉取真实任务/设备状态——绝不依赖内存中的旧数据。
 *  - 前端与后端的事件名严格一致，集中在下面的事件常量里。
 */

import { getToken } from './api'
import type { FileProgress, TransferStatus } from './types'

/** 与 internal/protocol/protocol.go 保持一致的事件名。 */
export const WS_EVENTS = {
  hello: 'server.hello',
  error: 'server.error',
  ping: 'server.ping',
  pong: 'server.pong',
  deviceOnline: 'device.online',
  deviceOffline: 'device.offline',
  deviceUpdated: 'device.updated',
  transferRequested: 'transfer.requested',
  transferAccepted: 'transfer.accepted',
  transferRejected: 'transfer.rejected',
  transferQueued: 'transfer.queued',
  transferProgress: 'transfer.progress',
  transferFileCompleted: 'transfer.file.completed',
  transferCompleted: 'transfer.completed',
  transferFailed: 'transfer.failed',
  transferCancelled: 'transfer.cancelled',
  transferPaused: 'transfer.paused',
  transferResumed: 'transfer.resumed',
  transferRetrying: 'transfer.retrying',
  dropboxChanged: 'dropbox.changed',
  settingsChanged: 'settings.changed',
} as const

export interface WsEnvelope {
  type: string
  ts: number
  taskId?: string
  deviceId?: string
  data?: unknown
}

export interface ProgressPayload {
  taskId: string
  status: TransferStatus
  doneBytes: number
  totalSize: number
  totalFiles: number
  files?: FileProgress[]
}

export type WsStatus = 'idle' | 'connecting' | 'online' | 'offline'

interface WsOptions {
  onEvent: (env: WsEnvelope) => void
  onStatus: (status: WsStatus, detail?: string) => void
  /** 重连成功后调用，用于重新同步真实状态。 */
  onResync: () => void
}

export class WsClient {
  private socket: WebSocket | null = null
  private status: WsStatus = 'idle'
  private attempt = 0
  private closedByUser = false
  private reconnectTimer: number | null = null
  private pingTimer: number | null = null
  private connectTimer: number | null = null
  private lastError = ''

  constructor(private opts: WsOptions) {}

  getStatus(): WsStatus {
    return this.status
  }

  getLastError(): string {
    return this.lastError
  }

  connect() {
    this.closedByUser = false
    if (
      this.socket &&
      (this.socket.readyState === WebSocket.OPEN || this.socket.readyState === WebSocket.CONNECTING)
    ) {
      return
    }
    this.setStatus('connecting')

    const url = new URL('/ws', window.location.href)
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'

    let socket: WebSocket
    try {
      socket = new WebSocket(url.toString())
    } catch (err) {
      this.lastError = err instanceof Error ? err.message : '无法建立 WebSocket'
      this.scheduleReconnect()
      return
    }
    this.socket = socket

    // 握手超时保护：连上了但服务端不应答时也要能重试。
    this.connectTimer = window.setTimeout(() => {
      if (socket.readyState === WebSocket.CONNECTING) {
        this.lastError = '连接超时，请确认服务仍在运行'
        try {
          socket.close()
        } catch {
          /* 忽略 */
        }
      }
    }, 8000)

    socket.onopen = () => {
      // 首帧必须携带令牌。
      const token = getToken()
      if (!token) {
        this.lastError = '缺少会话令牌，请刷新页面'
        this.setStatus('offline')
        socket.close()
        return
      }
      socket.send(JSON.stringify({ type: 'client.hello', data: { token } }))
    }

    socket.onmessage = (event) => {
      let env: WsEnvelope
      try {
        env = JSON.parse(event.data as string) as WsEnvelope
      } catch {
        return
      }
      if (env.type === 'server.hello') {
        if (this.connectTimer !== null) {
          window.clearTimeout(this.connectTimer)
          this.connectTimer = null
        }
        const wasOffline = this.attempt > 0
        this.attempt = 0
        this.lastError = ''
        this.setStatus('online')
        this.startPing()
        // 只有「曾经断开后重连」才需要重新同步；首次连接由页面自身加载流程负责。
        if (wasOffline) this.opts.onResync()
        return
      }
      if (env.type === 'server.error') {
        const detail = (env.data as { message?: string } | undefined)?.message ?? '鉴权失败'
        this.lastError = detail
        this.setStatus('offline', detail)
        // 鉴权失败重连也不会成功，等用户刷新页面。
        this.closedByUser = true
        try {
          socket.close()
        } catch {
          /* 忽略 */
        }
        return
      }
      if (env.type === 'server.ping') {
        this.send('client.pong')
        return
      }
      this.opts.onEvent(env)
    }

    socket.onerror = () => {
      this.lastError = 'WebSocket 连接异常'
    }

    socket.onclose = (ev) => {
      if (this.connectTimer !== null) {
        window.clearTimeout(this.connectTimer)
        this.connectTimer = null
      }
      this.stopPing()
      this.socket = null
      if (this.closedByUser) {
        this.setStatus('idle')
        return
      }
      if (!this.lastError) {
        this.lastError =
          ev.code === 1006 ? '连接被中断（服务可能已停止或网络已切换）' : `连接已关闭（${ev.code}）`
      }
      this.setStatus('offline')
      this.scheduleReconnect()
    }
  }

  /** 主动断开（页面卸载时调用），不再自动重连。 */
  close() {
    this.closedByUser = true
    this.stopPing()
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    try {
      this.socket?.close()
    } catch {
      /* 忽略 */
    }
    this.socket = null
    this.setStatus('idle')
  }

  /** 手动重连（诊断页的「重新连接」按钮）。 */
  reconnectNow() {
    this.attempt = 0
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    this.lastError = ''
    this.connect()
  }

  send(type: string, data?: unknown) {
    if (this.socket?.readyState !== WebSocket.OPEN) return false
    try {
      this.socket.send(JSON.stringify({ type, data, ts: Date.now() }))
      return true
    } catch {
      return false
    }
  }

  private setStatus(status: WsStatus, detail?: string) {
    if (this.status === status && !detail) return
    this.status = status
    this.opts.onStatus(status, detail)
  }

  private scheduleReconnect() {
    if (this.closedByUser || this.reconnectTimer !== null) return
    this.attempt += 1
    // 指数退避，上限 10 秒，并加入抖动避免多标签页同时重连。
    const base = Math.min(10000, 500 * 2 ** Math.min(this.attempt, 4))
    const jitter = Math.random() * 400
    this.reconnectTimer = window.setTimeout(() => {
      this.reconnectTimer = null
      this.connect()
    }, base + jitter)
  }

  private startPing() {
    this.stopPing()
    this.pingTimer = window.setInterval(() => {
      this.send('client.ping')
    }, 20000)
  }

  private stopPing() {
    if (this.pingTimer !== null) {
      window.clearInterval(this.pingTimer)
      this.pingTimer = null
    }
  }
}
