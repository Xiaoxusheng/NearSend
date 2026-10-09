/**
 * 会话 store：负责「打开页面就知道服务是否可用」这件事。
 *
 * 启动顺序：
 *   1. GET /api/health —— 判断服务是否可用；不可用直接给出明确原因。
 *   2. 复用 localStorage 中的令牌；无效则重新登记一个会话。
 *   3. 拉取公开配置、设备列表、任务列表。
 *   4. 建立 WebSocket；断线自动重连，重连成功后重新同步真实状态。
 *
 * 令牌来自服务端随机生成，前端无法伪造；权限由服务端判定。
 */

import { create } from 'zustand'

import { ApiError, api, getToken, setToken, setUnauthorizedHandler } from '../lib/api'
import type { AccessInfo, HealthInfo } from '../lib/types'
import { WsClient, type WsEnvelope, type WsStatus } from '../lib/ws'
import { useConfigStore } from './config'
import { useDevices } from './devices'
import { setDeviceName, setMeId, setPrivileged } from './identity'
import { explainErrorCode, toast } from './notify'
import { useTransfers } from './transfers'

export type BootStatus = 'booting' | 'ready' | 'blocked'

interface SessionState {
  status: BootStatus
  /** 服务不可用或鉴权失败时的原因，直接展示给用户。 */
  blockedReason: string
  deviceId: string
  deviceName: string
  access: AccessInfo | null
  health: HealthInfo | null
  wsStatus: WsStatus
  wsError: string
  lastResyncAt: number

  boot: () => Promise<void>
  retryBoot: () => Promise<void>
  reloadAccess: () => Promise<void>
  refreshHealth: () => Promise<void>
  rename: (name: string) => Promise<boolean>
  claimAdmin: (token: string) => Promise<boolean>
  reconnectWs: () => void
  shutdown: () => void
}

let ws: WsClient | null = null
let booting = false

/** 页面可见性恢复时补齐状态：浏览器在后台可能抑制了 WebSocket 与定时器。 */
function registerVisibilityResync() {
  if (typeof document === 'undefined') return
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState !== 'visible') return
    const state = useSession.getState()
    if (state.status !== 'ready') return
    // 回到前台时：确认连接与数据都是真实的，而不是内存中的旧值。
    if (ws && ws.getStatus() !== 'online') {
      ws.reconnectNow()
    }
    void useTransfers.getState().load()
    void useDevices.getState().load()
  })
}

let visibilityRegistered = false

export const useSession = create<SessionState>((set, get) => ({
  status: 'booting',
  blockedReason: '',
  deviceId: '',
  deviceName: '',
  access: null,
  health: null,
  wsStatus: 'idle',
  wsError: '',
  lastResyncAt: 0,

  boot: async () => {
    if (booting) return
    booting = true
    set({ status: 'booting', blockedReason: '' })
    try {
      // 1) 服务可用性
      let health: HealthInfo
      try {
        health = await api.health()
      } catch {
        set({
          status: 'blocked',
          blockedReason:
            '无法连接到服务端。请确认服务程序仍在运行，并且当前设备与它处于同一局域网。',
        })
        return
      }
      set({ health })

      // 2) 会话
      if (!getToken()) {
        const created = await api.createSession()
        setToken(created.token)
        setMeId(created.device.id)
        setDeviceName(created.device.name)
        setPrivileged(created.device.trusted || created.device.self)
        set({ deviceId: created.device.id, deviceName: created.device.name, access: created.access })
      } else {
        try {
          const access = await api.access()
          set({ access })
        } catch (err) {
          // 令牌失效：清掉并重新登记，而不是让页面一直报 401。
          if (err instanceof ApiError && (err.status === 401 || err.code === 'session_expired')) {
            setToken('')
            const created = await api.createSession()
            setToken(created.token)
            setMeId(created.device.id)
            setDeviceName(created.device.name)
            setPrivileged(created.device.trusted || created.device.self)
            set({
              deviceId: created.device.id,
              deviceName: created.device.name,
              access: created.access,
            })
          } else {
            throw err
          }
        }
      }

      // 会话失效回调：清空令牌并提示刷新，避免静默失败。
      setUnauthorizedHandler(() => {
        setToken('')
        set({
          status: 'blocked',
          blockedReason: '会话已失效。请刷新页面重新连接。',
        })
      })

      // 3) 初始数据
      await Promise.all([useConfigStore.getState().loadConfig(), useDevices.getState().load()])
      const cfg = useConfigStore.getState().config
      if (cfg) {
        setMeId(cfg.selfDeviceId)
        setDeviceName(cfg.selfDeviceName)
        setPrivileged(cfg.privileged)
        set({ deviceId: cfg.selfDeviceId, deviceName: cfg.selfDeviceName })
      }
      await useTransfers.getState().load()

      // 4) 实时通道
      startWs(set, get)

      set({ status: 'ready' })
      if (!visibilityRegistered) {
        visibilityRegistered = true
        registerVisibilityResync()
      }
    } catch (err) {
      const message =
        err instanceof ApiError
          ? explainErrorCode(err.code, err.message)
          : err instanceof Error
            ? err.message
            : '初始化失败'
      set({ status: 'blocked', blockedReason: message })
    } finally {
      booting = false
    }
  },

  retryBoot: async () => {
    ws?.close()
    await get().boot()
  },

  reloadAccess: async () => {
    try {
      const access = await api.access()
      set({ access })
    } catch {
      /* 地址信息刷新失败不阻塞主流程 */
    }
  },

  refreshHealth: async () => {
    try {
      const health = await api.health()
      set({ health })
    } catch {
      set({ health: null })
    }
  },

  rename: async (name) => {
    try {
      const res = await api.renameSelf(name)
      setDeviceName(res.device.name)
      set({ deviceName: res.device.name })
      // 重命名会影响其它设备看到的名字，刷新列表以拿到权威数据。
      await useDevices.getState().load()
      toast.success('设备名称已更新')
      return true
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '重命名失败'
      toast.error(message)
      return false
    }
  },

  claimAdmin: async (adminToken) => {
    try {
      const res = await api.claimAdmin(adminToken)
      setPrivileged(true)
      await useConfigStore.getState().loadConfig()
      await useDevices.getState().load()
      toast.success(res.message || '已取得管理权限')
      return true
    } catch (err) {
      const message =
        err instanceof ApiError
          ? err.code === 'forbidden'
            ? '管理令牌不正确，请检查后重试。令牌可在服务控制台或数据目录的 admin-token.txt 中找到。'
            : explainErrorCode(err.code, err.message)
          : '操作失败'
      toast.error(message)
      return false
    }
  },

  reconnectWs: () => {
    ws?.reconnectNow()
  },

  shutdown: () => {
    ws?.close()
    ws = null
  },
}))

/** 建立 WebSocket 并把事件分发给各 store。 */
function startWs(
  set: (partial: Partial<SessionState> | ((s: SessionState) => Partial<SessionState>)) => void,
  get: () => SessionState,
) {
  ws?.close()
  ws = new WsClient({
    onEvent: (env: WsEnvelope) => {
      useDevices.getState().applyEvent(env)
      useTransfers.getState().applyEvent(env)
      if (env.type === 'settings.changed') {
        void useConfigStore.getState().loadConfig()
      }
    },
    onStatus: (status, detail) => {
      set({ wsStatus: status, wsError: detail ?? ws?.getLastError() ?? '' })
    },
    onResync: () => {
      // 重连成功：不能相信内存里的旧数据，全部重新拉取。
      set({ lastResyncAt: Date.now() })
      void (async () => {
        await Promise.all([
          useDevices.getState().load(),
          useTransfers.getState().load(),
          useConfigStore.getState().loadConfig(),
          get().reloadAccess(),
          get().refreshHealth(),
        ])
        toast.success('已重新连接到服务，状态已同步')
      })()
    },
  })
  ws.connect()
}
