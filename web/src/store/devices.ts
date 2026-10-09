/**
 * 设备 store。
 *
 * 在线状态完全由服务端的 WebSocket 连接决定，前端不做任何推测：
 * 未连接到本服务的设备不会出现在「当前在线」中。
 */

import { create } from 'zustand'

import { ApiError, api } from '../lib/api'
import type { Device } from '../lib/types'
import { WS_EVENTS, type WsEnvelope } from '../lib/ws'
import { explainErrorCode, toast } from './notify'

interface DevicesState {
  devices: Device[]
  meId: string
  onlineCount: number
  canAdmin: boolean
  loading: boolean
  loadedOnce: boolean
  error: string | null
  load: () => Promise<void>
  applyEvent: (env: WsEnvelope) => void
  setTrust: (id: string, trusted: boolean) => Promise<void>
  rename: (id: string, name: string) => Promise<void>
  remove: (id: string) => Promise<void>
  disconnect: (id: string) => Promise<void>
  /** 当前在线且不是自己的设备，按最近活跃排序。 */
  onlineOthers: () => Device[]
  byId: (id: string) => Device | undefined
}

function sortDevices(list: Device[]): Device[] {
  return [...list].sort((a, b) => {
    // 在线优先，其次受信任，最后按最近活跃
    if (a.online !== b.online) return a.online ? -1 : 1
    if (a.trusted !== b.trusted) return a.trusted ? -1 : 1
    return b.lastSeen - a.lastSeen
  })
}

export const useDevices = create<DevicesState>((set, get) => ({
  devices: [],
  meId: '',
  onlineCount: 0,
  canAdmin: false,
  loading: false,
  loadedOnce: false,
  error: null,

  load: async () => {
    set({ loading: true })
    try {
      const res = await api.devices()
      set({
        devices: sortDevices(res.devices),
        meId: res.meId,
        onlineCount: res.onlineCount,
        canAdmin: res.canAdmin,
        error: null,
        loadedOnce: true,
      })
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '无法获取设备列表'
      set({ error: message, loadedOnce: true })
    } finally {
      set({ loading: false })
    }
  },

  applyEvent: (env) => {
    const state = get()
    switch (env.type) {
      case WS_EVENTS.deviceOnline:
      case WS_EVENTS.deviceUpdated: {
        const dev = env.data as Device | undefined
        if (!dev) return
        const next = { ...dev, online: env.type === WS_EVENTS.deviceOnline ? true : dev.online }
        const exists = state.devices.some((d) => d.id === next.id)
        const devices = exists
          ? state.devices.map((d) => (d.id === next.id ? { ...d, ...next } : d))
          : [...state.devices, next]
        set({ devices: sortDevices(devices), onlineCount: devices.filter((d) => d.online).length })
        return
      }
      case WS_EVENTS.deviceOffline: {
        const dev = env.data as Device | undefined
        if (!dev) return
        const devices = state.devices.map((d) => (d.id === dev.id ? { ...d, ...dev, online: false } : d))
        set({ devices: sortDevices(devices), onlineCount: devices.filter((d) => d.online).length })
        return
      }
      default:
    }
  },

  setTrust: async (id, trusted) => {
    try {
      await api.trustDevice(id, trusted)
      toast.success(trusted ? '已设为可信设备' : '已撤销信任，该设备的连接已断开')
      await get().load()
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '操作失败'
      toast.error(message)
    }
  },

  rename: async (id, name) => {
    try {
      await api.renameDevice(id, name)
      toast.success('设备名称已更新')
      await get().load()
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '重命名失败'
      toast.error(message)
      throw err
    }
  },

  remove: async (id) => {
    try {
      await api.removeDevice(id)
      toast.success('设备已移除，其会话令牌同时失效')
      await get().load()
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '移除失败'
      toast.error(message)
    }
  },

  disconnect: async (id) => {
    try {
      await api.disconnectDevice(id)
      toast.success('已断开该设备的实时连接')
      await get().load()
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '断开失败'
      toast.error(message)
    }
  },

  onlineOthers: () => {
    const { devices, meId } = get()
    return devices.filter((d) => d.online && d.id !== meId)
  },

  byId: (id) => get().devices.find((d) => d.id === id),
}))
