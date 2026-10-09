/**
 * 配置 store：公开配置（影响客户端行为）+ 完整设置（特权设备可编辑）。
 */

import { create } from 'zustand'

import { ApiError, api } from '../lib/api'
import type { PublicConfig, Settings } from '../lib/types'
import { setConfigSnapshotReader } from './transfers'
import { isPrivileged, setMeId, setPrivileged } from './identity'
import { explainErrorCode, toast } from './notify'

interface ConfigState {
  config: PublicConfig | null
  settings: Settings | null
  canEdit: boolean
  loadingConfig: boolean
  loadingSettings: boolean
  saving: boolean
  error: string | null

  loadConfig: () => Promise<void>
  loadSettings: () => Promise<void>
  patchSettings: (patch: Record<string, unknown>) => Promise<boolean>
}

export const useConfigStore = create<ConfigState>((set, get) => ({
  config: null,
  settings: null,
  canEdit: false,
  loadingConfig: false,
  loadingSettings: false,
  saving: false,
  error: null,

  loadConfig: async () => {
    set({ loadingConfig: true })
    try {
      const config = await api.config()
      set({ config, error: null })
      // 身份与权限一律以服务端返回为准：前端不自行推断特权。
      if (config.selfDeviceId) setMeId(config.selfDeviceId)
      setPrivileged(config.privileged)
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '无法获取配置'
      set({ error: message })
    } finally {
      set({ loadingConfig: false })
    }
  },

  loadSettings: async () => {
    set({ loadingSettings: true })
    try {
      const res = await api.settings()
      set({ settings: res.settings, canEdit: res.canEdit, error: null })
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '无法获取设置'
      set({ error: message })
    } finally {
      set({ loadingSettings: false })
    }
  },

  patchSettings: async (patch) => {
    set({ saving: true })
    try {
      const res = await api.patchSettings(patch)
      set({ settings: res.settings })
      // 权限相关设置变化后刷新公开配置，保证其它页面读到的是最新值。
      await get().loadConfig()
      if (res.needsRestart) {
        toast.warning(res.message)
      } else {
        toast.success(res.message)
      }
      return true
    } catch (err) {
      const message =
        err instanceof ApiError ? explainErrorCode(err.code, err.message) : '保存设置失败'
      toast.error(message)
      return false
    } finally {
      set({ saving: false })
    }
  },
}))

// 让 transfers store 能够读取「可信设备自动接收」等策略，而不产生循环依赖。
setConfigSnapshotReader(() => {
  const cfg = useConfigStore.getState().config
  return cfg ? { allowTrustedAutoAccept: cfg.allowTrustedAutoAccept } : null
})

export function currentPrivileged(): boolean {
  return isPrivileged()
}
