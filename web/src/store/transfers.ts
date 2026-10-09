/**
 * 传输任务 store。
 *
 * 职责：
 *  - 保存全部任务，按 WebSocket 事件增量更新（不整表刷新，避免进度跳动时重渲染）。
 *  - 接收方确认 / 拒绝，发送方启动分块上传，并处理暂停、继续、取消、重试。
 *  - 维护「本会话内的乐观进度」：哈希与上传是本地驱动的，先给出即时反馈，
 *    服务端的权威进度到达后覆盖；两者取较大值，避免进度回退造成闪烁。
 *
 * 明确不做的事：
 *  - 不伪造设备、不伪造进度；没有源文件时不做「假装还在传」的展示。
 */

import { create } from 'zustand'

import { ApiError, api } from '../lib/api'
import type { ConflictPolicy, Transfer, TransferFile } from '../lib/types'
import { TaskUploader, type SourceFile } from '../lib/uploader'
import { WS_EVENTS, type ProgressPayload, type WsEnvelope } from '../lib/ws'
import { useDevices } from './devices'
import { getMeId } from './identity'
import { explainErrorCode, toast } from './notify'

const TERMINAL: string[] = ['completed', 'failed', 'cancelled', 'rejected']

export function isTerminal(status: string): boolean {
  return TERMINAL.includes(status)
}

/** 活跃任务：需要用户关注或正在占用资源的任务。 */
export function isActiveTask(status: string): boolean {
  return status === 'awaiting' || status === 'queued' || status === 'uploading' || status === 'paused'
}

/**
 * 前端内部的统一事件类型：既包含服务端 WebSocket 事件，
 * 也包含「主动拉取的快照」，这样状态写入只有一个入口，不会出现两套逻辑不一致。
 */
export type ClientEvent =
  | WsEnvelope
  | { type: 'task.snapshot'; ts: number; taskId: string; data: Transfer }

type SetFn = (
  partial: Partial<TransfersState> | ((s: TransfersState) => Partial<TransfersState>),
) => void
type GetFn = () => TransfersState

interface TransfersState {
  tasks: Record<string, Transfer>
  order: string[]
  loading: boolean
  loadedOnce: boolean
  error: string | null
  lastSyncAt: number

  /** 需要我确认的任务（接收方视角）。 */
  dismissedIncoming: string[]
  /** 本会话登记的源文件，按任务 ID 索引。页面刷新后为空，这是浏览器的硬限制。 */
  sources: Record<string, SourceFile[]>
  runners: Record<string, TaskUploader>
  localDone: Record<string, number>
  /** 上传异常提示，展示在任务行内。 */
  uploadNotes: Record<string, string>

  load: () => Promise<void>
  refreshTask: (id: string) => Promise<void>
  applyEvent: (env: ClientEvent) => void

  accept: (id: string, conflict: ConflictPolicy) => Promise<void>
  reject: (id: string, reason: string) => Promise<void>
  cancel: (id: string) => Promise<void>
  retry: (id: string) => Promise<void>
  pause: (id: string) => Promise<void>
  resume: (id: string) => Promise<void>

  registerSources: (taskId: string, files: SourceFile[]) => void
  dropSources: (taskId: string) => void
  ensureUploading: (taskId: string) => void
  stopUpload: (taskId: string, mode: 'pause' | 'abort') => void

  dismissIncoming: (id: string) => void
  pendingIncoming: () => Transfer[]
  sortedTasks: () => Transfer[]
  doneBytesOf: (task: Transfer) => number
  uploadNote: (id: string) => string | undefined
}

function mergeFiles(existing: TransferFile[] | undefined, incoming: TransferFile[] | undefined) {
  if (!incoming || incoming.length === 0) return existing
  return incoming
}

export const useTransfers = create<TransfersState>((set, get) => ({
  tasks: {},
  order: [],
  loading: false,
  loadedOnce: false,
  error: null,
  lastSyncAt: 0,

  dismissedIncoming: [],
  sources: {},
  runners: {},
  localDone: {},
  uploadNotes: {},

  load: async () => {
    const meId = getMeId()
    if (!meId) return
    set({ loading: true })
    try {
      // 任务页需要看到全部与我相关的任务，包括终态，因此不带 status 过滤。
      const res = await api.listTransfers({ page: 1, pageSize: 200 })
      const tasks: Record<string, Transfer> = { ...get().tasks }
      for (const t of res.tasks) {
        const prev = tasks[t.id]
        tasks[t.id] = prev ? { ...prev, ...t, files: mergeFiles(prev.files, t.files) } : t
      }
      set({
        tasks,
        order: sortOrder(tasks),
        error: null,
        loadedOnce: true,
        lastSyncAt: Date.now(),
      })
      // 页面刷新或重连后，可能仍有权限内未完成的上传；
      // 只有本地还持有源文件时才能继续，否则由 UI 明确提示重新选择文件。
      for (const t of Object.values(tasks)) {
        if (t.senderId === meId && t.status === 'uploading' && get().sources[t.id]?.length) {
          get().ensureUploading(t.id)
        }
      }
    } catch (err) {
      const message =
        err instanceof ApiError ? explainErrorCode(err.code, err.message) : '无法获取任务列表'
      set({ error: message, loadedOnce: true })
    } finally {
      set({ loading: false })
    }
  },

  refreshTask: async (id) => {
    try {
      const res = await api.getTransfer(id)
      get().applyEvent({ type: 'task.snapshot', ts: Date.now(), taskId: id, data: res.task })
    } catch {
      /* 单个任务刷新失败不影响其余任务 */
    }
  },

  applyEvent: (env) => {
    const meId = getMeId()
    const state = get()

    // 任务快照（主动刷新时复用同一条通道，避免两套写入口）。
    if (env.type === 'task.snapshot') {
      const t = env.data as Transfer
      upsertSnapshot(set, get, t)
      return
    }

    const taskId = env.taskId
    if (!taskId) return

    switch (env.type) {
      case WS_EVENTS.transferRequested:
      case WS_EVENTS.transferAccepted:
      case WS_EVENTS.transferRejected:
      case WS_EVENTS.transferQueued:
      case WS_EVENTS.transferPaused:
      case WS_EVENTS.transferResumed:
      case WS_EVENTS.transferCancelled:
      case WS_EVENTS.transferFailed:
      case WS_EVENTS.transferRetrying:
      case WS_EVENTS.transferCompleted: {
        const payload = env.data as { task?: Transfer } | undefined
        if (payload?.task) {
          upsertSnapshot(set, get, payload.task)
        } else {
          // 事件没带完整任务时，只更新状态字段并异步补齐详情。
          const existing = state.tasks[taskId]
          if (existing) {
            const status = statusFromEvent(env.type)
            if (status) {
              patch(set, get, taskId, { status })
            }
          }
          void get().refreshTask(taskId)
        }

        // 通知：接收方收到新的传输请求
        if (env.type === WS_EVENTS.transferRequested) {
          const t = get().tasks[taskId]
          if (t && t.receiverId === meId) {
            const autoAccept = shouldAutoAccept(t)
            if (autoAccept) {
              void get().accept(taskId, t.conflict)
            } else {
              toast.info(`${t.senderName} 想向你发送 ${t.totalFiles} 个文件（${prettyBytes(t.totalSize)}）`)
            }
          }
        }

        // 发送方：服务端分配了槽位，开始/继续上传。
        //
        // 这里刻意不校验本地状态是否为 uploading：该事件本身携带 no-task 负载，
        // 本地状态往往还停留在 queued（权威状态要等一次 refreshTask）。
        // 上传器会自己向服务端确认分块状态，因此「先启动、后校验」是安全的，
        // 反过来（要求本地状态先正确）会导致上传永远不启动。
        if (env.type === WS_EVENTS.transferQueued || env.type === WS_EVENTS.transferResumed) {
          const t = get().tasks[taskId]
          const detail = env.data as { waiting?: boolean; granted?: boolean } | undefined
          const granted = detail?.granted === true || detail?.waiting === false
          if (t && t.senderId === meId && granted) {
            get().ensureUploading(taskId)
          }
        }

        if (env.type === WS_EVENTS.transferPaused) {
          get().stopUpload(taskId, 'pause')
        }
        if (env.type === WS_EVENTS.transferCompleted) {
          set((s) => ({
            localDone: { ...s.localDone, [taskId]: 0 },
            uploadNotes: omit(s.uploadNotes, taskId),
          }))
        }
        if (env.type === WS_EVENTS.transferFailed) {
          const err = env.data as { message?: string; code?: string; resumable?: boolean } | undefined
          get().stopUpload(taskId, 'abort')
          set((s) => ({
            uploadNotes: {
              ...s.uploadNotes,
              [taskId]: explainErrorCode(err?.code ?? 'internal', err?.message ?? '传输失败'),
            },
          }))
          toast.error(`${explainErrorCode(err?.code ?? 'internal', err?.message ?? '传输失败')}`)
        }
        if (env.type === WS_EVENTS.transferRejected) {
          get().stopUpload(taskId, 'abort')
          const reason = (env.data as { reason?: string } | undefined)?.reason
          toast.warning(reason ? `对方拒绝了传输：${reason}` : '对方拒绝了本次传输')
        }
        if (env.type === WS_EVENTS.transferCancelled) {
          get().stopUpload(taskId, 'abort')
          set((s) => ({ uploadNotes: omit(s.uploadNotes, taskId) }))
        }
        return
      }

      case WS_EVENTS.transferProgress: {
        const p = env.data as ProgressPayload | undefined
        if (!p) return
        const existing = state.tasks[taskId]
        if (!existing) {
          void get().refreshTask(taskId)
          return
        }
        patch(set, get, taskId, {
          status: p.status,
          doneSize: p.doneBytes,
          totalSize: p.totalSize,
          totalFiles: p.totalFiles,
          ...(p.files
            ? {
                files: existing.files
                  ? existing.files.map((f) => {
                      const upd = p.files?.find((x) => x.index === f.index)
                      return upd
                        ? {
                            ...f,
                            doneBytes: upd.doneBytes,
                            status: upd.status,
                            receivedChunks: upd.receivedChunks,
                            chunkCount: upd.chunkCount,
                            error: upd.error ?? f.error,
                          }
                        : f
                    })
                  : existing.files,
              }
            : {}),
        })
        // 服务端权威进度到达：清理本会话的乐观值，避免长期偏高。
        set((s) => {
          const local = s.localDone[taskId] ?? 0
          if (local <= p.doneBytes) {
            return { localDone: { ...s.localDone, [taskId]: 0 } }
          }
          return s
        })
        return
      }

      case WS_EVENTS.transferFileCompleted: {
        const d = env.data as { fileIndex?: number; finalName?: string } | undefined
        if (d?.fileIndex === undefined) return
        const existing = get().tasks[taskId]
        if (!existing?.files) return
        patch(set, get, taskId, {
          files: existing.files.map((f) =>
            f.index === d.fileIndex
              ? { ...f, status: 'ready', finalName: d.finalName ?? f.finalName, doneBytes: f.size }
              : f,
          ),
        })
        return
      }

      default:
    }
  },

  accept: async (id, conflict) => {
    try {
      const res = await api.accept(id, conflict)
      upsertSnapshot(set, get, res.task)
      set((s) => ({ dismissedIncoming: [...new Set([...s.dismissedIncoming, id])] }))
      toast.success('已接受传输请求')
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '接受失败'
      toast.error(message)
    }
  },

  reject: async (id, reason) => {
    try {
      const res = await api.reject(id, reason)
      upsertSnapshot(set, get, res.task)
      set((s) => ({ dismissedIncoming: [...new Set([...s.dismissedIncoming, id])] }))
      toast.info('已拒绝本次传输')
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '拒绝失败'
      toast.error(message)
    }
  },

  cancel: async (id) => {
    get().stopUpload(id, 'abort')
    try {
      const res = await api.cancel(id)
      upsertSnapshot(set, get, res.task)
      set((s) => ({ uploadNotes: omit(s.uploadNotes, id) }))
      toast.info('任务已取消')
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '取消失败'
      toast.error(message)
    }
  },

  retry: async (id) => {
    try {
      const res = await api.retry(id)
      upsertSnapshot(set, get, res.task)
      set((s) => ({ uploadNotes: omit(s.uploadNotes, id) }))
      toast.info('已重新排队')
    } catch (err) {
      const code = err instanceof ApiError ? err.code : 'internal'
      const message =
        err instanceof ApiError ? explainErrorCode(err.code, err.message) : '重试失败'
      // 不支持续传时给出明确原因，而不是含糊失败。
      if (code === 'not_resumable') {
        toast.error(`无法续传：${message} 请重新选择文件发起传输。`)
      } else {
        toast.error(message)
      }
    }
  },

  pause: async (id) => {
    get().stopUpload(id, 'pause')
    try {
      const res = await api.pause(id)
      upsertSnapshot(set, get, res.task)
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '暂停失败'
      toast.error(message)
    }
  },

  resume: async (id) => {
    try {
      const res = await api.resume(id)
      upsertSnapshot(set, get, res.task)
      if (res.task.status === 'uploading' || res.task.status === 'queued') {
        get().ensureUploading(id)
      }
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '继续失败'
      toast.error(message)
    }
  },

  registerSources: (taskId, files) => {
    set((s) => ({ sources: { ...s.sources, [taskId]: files } }))
  },

  dropSources: (taskId) => {
    set((s) => ({ sources: omit(s.sources, taskId) }))
  },

  ensureUploading: (taskId) => {
    const state = get()
    const task = state.tasks[taskId]
    if (!task) return
    if (task.senderId !== getMeId()) return

    const runner = state.runners[taskId]
    if (runner?.isRunning()) {
      // 已在运行：只需确保它没有被暂停标记拦住。
      runner.authorize()
      runner.resume()
      return
    }

    const sources = state.sources[taskId]
    if (!sources || sources.length === 0) {
      set((s) => ({
        uploadNotes: {
          ...s.uploadNotes,
          [taskId]: '源文件不在当前页面会话中（页面可能已刷新），无法继续上传。请重新选择文件后重新发送。',
        },
      }))
      return
    }

    const chunkSize = task.chunkSize || 8 * 1024 * 1024
    const concurrency = 3

    const instance = new TaskUploader(taskId, sources, chunkSize, concurrency, {
      onProgress: (_id, done) => {
        set((s) => ({ localDone: { ...s.localDone, [taskId]: done } }))
      },
      onFileDone: () => {
        /* 文件级完成由 WS 事件与最终刷新统一反映 */
      },
      onFileFailed: (_id, fileIndex, code, message) => {
        const note = explainErrorCode(code, message)
        set((s) => ({
          uploadNotes: { ...s.uploadNotes, [taskId]: `文件 #${fileIndex + 1} 传输失败：${note}` },
        }))
        toast.error(`文件 #${fileIndex + 1} 传输失败：${note}`)
      },
      onTransientError: (id, message) => {
        set((s) => ({ uploadNotes: { ...s.uploadNotes, [id]: message } }))
      },
      onFatalError: (_id, error) => {
        const message =
          error instanceof ApiError ? explainErrorCode(error.code, error.message) : error.message
        set((s) => ({ uploadNotes: { ...s.uploadNotes, [taskId]: message } }))
        toast.error(message)
      },
      onFinished: () => {
        // 上传完成后拉取权威结果（校验结论只能由服务端给出）。
        void get().refreshTask(taskId)
      },
      isActive: (id) => {
        const t = get().tasks[id]
        if (!t) return false
        return t.status === 'uploading' || t.status === 'queued'
      },
    })

    set((s) => ({ runners: { ...s.runners, [taskId]: instance } }))
    instance.authorize()
    instance
      .run()
      .catch((err) => {
        const message =
          err instanceof ApiError ? explainErrorCode(err.code, err.message) : err.message ?? '上传失败'
        set((s) => ({ uploadNotes: { ...s.uploadNotes, [taskId]: message } }))
        if (!(err instanceof Error && err.name === 'SourceUnavailableError')) {
          toast.error(message)
        }
      })
      .finally(() => {
        set((s) => ({ runners: omit(s.runners, taskId) }))
      })
  },

  stopUpload: (taskId, mode) => {
    const runner = get().runners[taskId]
    if (!runner) return
    if (mode === 'pause') runner.pause()
    else runner.abort()
  },

  dismissIncoming: (id) => {
    set((s) => ({ dismissedIncoming: [...new Set([...s.dismissedIncoming, id])] }))
  },

  pendingIncoming: () => {
    const meId = getMeId()
    const { tasks, dismissedIncoming } = get()
    return Object.values(tasks)
      .filter(
        (t) =>
          t.receiverId === meId && t.status === 'awaiting' && !dismissedIncoming.includes(t.id),
      )
      .sort((a, b) => b.createdAt - a.createdAt)
  },

  sortedTasks: () => {
    const { tasks, order } = get()
    return order.map((id) => tasks[id]).filter(Boolean)
  },

  doneBytesOf: (task) => Math.max(task.doneSize ?? 0, get().localDone[task.id] ?? 0),

  uploadNote: (id) => get().uploadNotes[id],
}))

// ---------------------------------------------------------------- 内部工具 ---

function upsertSnapshot(set: SetFn, get: GetFn, t: Transfer) {
  const prev = get().tasks[t.id]
  const merged: Transfer = prev
    ? { ...prev, ...t, files: mergeFiles(prev.files, t.files) }
    : t
  set((s) => {
    const tasks = { ...s.tasks, [t.id]: merged }
    return { tasks, order: sortOrder(tasks), lastSyncAt: Date.now() }
  })
}

function patch(set: SetFn, get: GetFn, id: string, changes: Partial<Transfer>) {
  const existing = get().tasks[id]
  if (!existing) return
  set((s) => ({
    tasks: { ...s.tasks, [id]: { ...existing, ...changes } },
    lastSyncAt: Date.now(),
  }))
}

function sortOrder(tasks: Record<string, Transfer>): string[] {
  return Object.values(tasks)
    .sort((a, b) => b.createdAt - a.createdAt)
    .map((t) => t.id)
}

function omit<T extends Record<string, unknown>>(obj: T, key: string): T {
  if (!(key in obj)) return obj
  const next = { ...obj }
  delete next[key]
  return next
}

function statusFromEvent(type: string): Transfer['status'] | null {
  switch (type) {
    case WS_EVENTS.transferAccepted:
      return 'queued'
    case WS_EVENTS.transferRejected:
      return 'rejected'
    case WS_EVENTS.transferPaused:
      return 'paused'
    case WS_EVENTS.transferCancelled:
      return 'cancelled'
    case WS_EVENTS.transferFailed:
      return 'failed'
    default:
      return null
  }
}

/**
 * 可信设备自动接收。
 *
 * 前提是用户在设置中开启了「允许可信设备简化接收确认」，且发送方确实已受信任。
 * 未开启时永远弹窗询问——不会出现陌生设备静默写入的情况。
 */
function shouldAutoAccept(t: Transfer): boolean {
  const config = readConfigSnapshot()
  if (!config?.allowTrustedAutoAccept) return false
  const sender = useDevices.getState().byId(t.senderId)
  return Boolean(sender?.trusted)
}

/** 由 config store 注入的配置读取器，避免此处直接依赖 config store（防循环引用）。 */
let configSnapshotReader: () => { allowTrustedAutoAccept: boolean } | null = () => null

export function setConfigSnapshotReader(fn: () => { allowTrustedAutoAccept: boolean } | null) {
  configSnapshotReader = fn
}

function readConfigSnapshot() {
  return configSnapshotReader()
}

/** 供 UI 复用的体积格式化（避免在 store 里引入 UI 依赖）。 */
function prettyBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—'
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  const v = bytes / 1024 ** i
  return `${v.toFixed(i === 0 ? 0 : v >= 100 ? 0 : 1)} ${units[i]}`
}
