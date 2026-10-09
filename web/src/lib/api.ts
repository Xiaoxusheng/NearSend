/**
 * REST 客户端。
 *
 * 约定：
 *  - 会话令牌保存在 localStorage，通过 Authorization: Bearer 头传递。
 *  - 所有错误统一抛 ApiError，携带稳定错误码；UI 依据错误码给出可执行的恢复动作，
 *    而不是显示一句「操作失败」。
 *  - 会话失效（401）会清空本地令牌并触发一次重新登记，避免页面卡在无响应状态。
 */

import type {
  AccessInfo,
  ChunkResult,
  ChunkStatus,
  ConflictPolicy,
  Diagnostics,
  Device,
  DropboxState,
  HealthInfo,
  HistoryEntry,
  PublicConfig,
  Settings,
  StorageStats,
  StorageUsage,
  Transfer,
  TransferStatus,
} from './types'

const TOKEN_KEY = 'nearsend.token'

export class ApiError extends Error {
  code: string
  status: number

  constructor(code: string, message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
  }
}

export function getToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? ''
  } catch {
    return ''
  }
}

export function setToken(token: string) {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token)
    else localStorage.removeItem(TOKEN_KEY)
  } catch {
    /* 隐私模式下 localStorage 可能不可用，此时仅影响刷新后的会话保持 */
  }
}

/** 会话失效时的回调，由 Session store 注册。 */
let onUnauthorized: (() => void) | null = null

export function setUnauthorizedHandler(fn: (() => void) | null) {
  onUnauthorized = fn
}

interface RequestOptions {
  method?: string
  body?: unknown
  /** 原始二进制请求体（分块上传使用）。 */
  raw?: BodyInit
  headers?: Record<string, string>
  signal?: AbortSignal
  /** 是否允许在 401 时触发重新登记。 */
  allowReauth?: boolean
}

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = { ...(opts.headers ?? {}) }
  const token = getToken()
  if (token) headers['Authorization'] = `Bearer ${token}`

  let body: BodyInit | undefined
  if (opts.raw !== undefined) {
    body = opts.raw
  } else if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(opts.body)
  }

  let res: Response
  try {
    res = await fetch(path, {
      method: opts.method ?? 'GET',
      headers,
      body,
      signal: opts.signal,
    })
  } catch (err) {
    if ((err as Error)?.name === 'AbortError') throw err
    throw new ApiError('internal', '无法连接到服务，请确认服务仍在运行。', 0)
  }

  if (res.status === 204) return undefined as T

  const text = await res.text()
  let payload: unknown = null
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      payload = null
    }
  }

  if (!res.ok) {
    const errObj = (payload as { error?: { code?: string; message?: string } } | null)?.error
    const code = errObj?.code ?? 'internal'
    const message = errObj?.message ?? `请求失败（HTTP ${res.status}）`
    if (res.status === 401 && opts.allowReauth !== false && onUnauthorized) {
      onUnauthorized()
    }
    throw new ApiError(code, message, res.status)
  }
  return payload as T
}

// ---------------------------------------------------------------- 会话 ---

export interface SessionResponse {
  device: Device
  token: string
  access: AccessInfo
}

export const api = {
  health: () => request<HealthInfo>('/api/health', { allowReauth: false }),

  createSession: (name?: string) =>
    request<SessionResponse>('/api/session', {
      method: 'POST',
      body: name ? { name } : {},
      allowReauth: false,
    }),

  access: () => request<AccessInfo>('/api/session/access'),

  renameSelf: (name: string) =>
    request<{ device: Device }>('/api/session', { method: 'PATCH', body: { name } }),

  claimAdmin: (adminToken: string) =>
    request<{ device: Device; message: string }>('/api/session/claim', {
      method: 'POST',
      body: { adminToken },
    }),

  config: () => request<PublicConfig>('/api/config'),

  // ---------------------------------------------------------------- 设备 ---

  devices: () =>
    request<{ devices: Device[]; meId: string; onlineCount: number; canAdmin: boolean }>(
      '/api/devices',
    ),

  renameDevice: (id: string, name: string) =>
    request<{ device: Device }>(`/api/devices/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: { name },
    }),

  trustDevice: (id: string, trusted: boolean) =>
    request<{ device: Device }>(`/api/devices/${encodeURIComponent(id)}/trust`, {
      method: trusted ? 'POST' : 'DELETE',
      body: trusted ? {} : undefined,
    }),

  removeDevice: (id: string) =>
    request<{ removed: string }>(`/api/devices/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  disconnectDevice: (id: string) =>
    request<{ disconnected: string }>(`/api/devices/${encodeURIComponent(id)}/disconnect`, {
      method: 'POST',
      body: {},
    }),

  // ---------------------------------------------------------------- 传输 ---

  createTransfer: (payload: {
    receiverIds: string[]
    files: {
      name: string
      relPath?: string
      size: number
      mime?: string
      modTime?: number
      sha256?: string
    }[]
    note?: string
    conflict?: ConflictPolicy
    dropboxToken?: string
  }) => request<{ tasks: Transfer[] }>('/api/transfers', { method: 'POST', body: payload }),

  listTransfers: (params: {
    status?: TransferStatus[]
    dir?: 'in' | 'out' | 'all'
    keyword?: string
    scope?: 'all' | 'involved'
    page?: number
    pageSize?: number
    from?: number
    to?: number
  }) => {
    const q = new URLSearchParams()
    if (params.status?.length) q.set('status', params.status.join(','))
    if (params.dir && params.dir !== 'all') q.set('dir', params.dir)
    if (params.keyword) q.set('keyword', params.keyword)
    if (params.scope) q.set('scope', params.scope)
    if (params.page) q.set('page', String(params.page))
    if (params.pageSize) q.set('pageSize', String(params.pageSize))
    if (params.from) q.set('from', String(params.from))
    if (params.to) q.set('to', String(params.to))
    return request<{ tasks: Transfer[]; total: number; page: number; pageSize: number }>(
      `/api/transfers?${q.toString()}`,
    )
  },

  getTransfer: (id: string) => request<{ task: Transfer }>(`/api/transfers/${id}`),

  accept: (id: string, conflict: ConflictPolicy) =>
    request<{ task: Transfer }>(`/api/transfers/${id}/accept`, {
      method: 'POST',
      body: { conflict },
    }),

  reject: (id: string, reason: string) =>
    request<{ task: Transfer }>(`/api/transfers/${id}/reject`, {
      method: 'POST',
      body: { reason },
    }),

  cancel: (id: string) =>
    request<{ task: Transfer }>(`/api/transfers/${id}/cancel`, { method: 'POST', body: {} }),

  retry: (id: string) =>
    request<{ task: Transfer }>(`/api/transfers/${id}/retry`, { method: 'POST', body: {} }),

  pause: (id: string) =>
    request<{ task: Transfer }>(`/api/transfers/${id}/pause`, { method: 'POST', body: {} }),

  resume: (id: string) =>
    request<{ task: Transfer }>(`/api/transfers/${id}/resume`, { method: 'POST', body: {} }),

  chunkStatus: (id: string) => request<ChunkStatus>(`/api/transfers/${id}/chunks`),

  uploadChunk: (
    taskId: string,
    fileIndex: number,
    chunkIndex: number,
    data: ArrayBuffer,
    sha256: string,
    signal?: AbortSignal,
  ) =>
    request<ChunkResult>(`/api/transfers/${taskId}/files/${fileIndex}/chunks/${chunkIndex}`, {
      method: 'POST',
      raw: data,
      headers: sha256 ? { 'X-Chunk-SHA256': sha256 } : {},
      signal,
    }),

  resetFile: (taskId: string, fileIndex: number) =>
    request<{ reset: number }>(`/api/transfers/${taskId}/files/${fileIndex}/reset`, {
      method: 'POST',
      body: {},
    }),

  createTicket: (taskId: string, fileIndex: number) =>
    request<{ ticket: string; url: string; name: string; size: number; expiresIn: number }>(
      `/api/transfers/${taskId}/files/${fileIndex}/ticket`,
      { method: 'POST', body: {} },
    ),

  reveal: (taskId: string, fileIndex: number) =>
    request<{ revealed: string }>(`/api/transfers/${taskId}/files/${fileIndex}/reveal`, {
      method: 'POST',
      body: {},
    }),

  // ---------------------------------------------------------------- 历史 ---

  history: (params: {
    keyword?: string
    status?: TransferStatus[]
    dir?: 'in' | 'out' | 'all'
    scope?: 'all' | 'involved'
    page?: number
    pageSize?: number
    from?: number
    to?: number
  }) => {
    const q = new URLSearchParams()
    if (params.keyword) q.set('keyword', params.keyword)
    if (params.status?.length) q.set('status', params.status.join(','))
    if (params.dir && params.dir !== 'all') q.set('dir', params.dir)
    if (params.scope) q.set('scope', params.scope)
    if (params.page) q.set('page', String(params.page))
    if (params.pageSize) q.set('pageSize', String(params.pageSize))
    if (params.from) q.set('from', String(params.from))
    if (params.to) q.set('to', String(params.to))
    return request<{ entries: HistoryEntry[]; total: number; page: number; pageSize: number }>(
      `/api/history?${q.toString()}`,
    )
  },

  clearHistory: () =>
    request<{ cleared: number; message: string }>('/api/history/clear', {
      method: 'POST',
      body: {},
    }),

  // ---------------------------------------------------------------- 设置 ---

  settings: () =>
    request<{ settings: Settings; canEdit: boolean; deviceId: string; deviceName: string }>(
      '/api/settings',
    ),

  /** 仅提交变更过的字段，未提交的项保持原值。 */
  patchSettings: (patch: Record<string, unknown>) =>
    request<{ settings: Settings; needsRestart: boolean; message: string }>('/api/settings', {
      method: 'PATCH',
      body: patch,
    }),

  storage: () => request<{ usage: StorageUsage; stats: StorageStats }>('/api/storage'),

  cleanupStorage: () =>
    request<{ removedDirs: number; freedBytes: number; message: string }>('/api/storage/cleanup', {
      method: 'POST',
      body: {},
    }),

  diagnostics: () => request<Diagnostics>('/api/diagnostics'),

  // ---------------------------------------------------------------- 临时接收箱 ---

  dropbox: () => request<DropboxState>('/api/dropbox'),

  enableDropbox: (ttlMinutes?: number) =>
    request<DropboxState>('/api/dropbox/enable', {
      method: 'POST',
      body: ttlMinutes ? { ttlMinutes } : {},
    }),

  disableDropbox: () =>
    request<DropboxState>('/api/dropbox/disable', { method: 'POST', body: {} }),
}
