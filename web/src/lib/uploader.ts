/**
 * 分块上传引擎。
 *
 * 设计要点：
 *  - 只上传服务端「缺失」的分块，因此断点续传是真实生效的（服务端提供缺失清单）。
 *  - 源文件校验：续传前比对本地 File.size / lastModified 与任务记录中的值，
 *    不一致时先调用 reset 再整体重传，避免「源文件已改却续传」造成数据损坏。
 *  - 并发受两个约束：服务端的传输槽位（授权后才开始）+ 本地上传并发数。
 *  - 暂停是真实的：暂停后不再发出任何新的分块请求，在途请求的结果仍会被服务端
 *    幂等接受，不会产生脏数据。
 *  - 失败重试有上限与退避，且只在可重试的错误码上重试。
 *
 * 浏览器限制的如实处理：
 *  页面刷新或标签页被回收后，内存中的 File 对象即失效，无法继续上传。
 *  此时不谎称「正在续传」，而是抛出 SourceUnavailableError，由 UI 明确提示
 *  「请重新选择文件」。
 */

import { ApiError, api } from './api'
import { sha256Hex } from './sha256'
import type { ChunkStatus, FileStatus } from './types'

export interface SourceFile {
  file: File
  /** 相对路径，保留文件夹结构。 */
  relPath: string
}

/** 源文件已不在内存中（页面刷新后）导致无法继续。 */
export class SourceUnavailableError extends Error {
  constructor(message = '本次页面会话中的源文件已不可用，请重新选择文件后再试。') {
    super(message)
    this.name = 'SourceUnavailableError'
  }
}

export interface UploaderHooks {
  /** 本地估算进度，服务端事件到达后会被权威数值覆盖。 */
  onProgress: (taskId: string, doneBytes: number, totalBytes: number) => void
  onFileDone: (taskId: string, fileIndex: number, finalName: string) => void
  onFileFailed: (taskId: string, fileIndex: number, code: string, message: string) => void
  onTransientError: (taskId: string, message: string) => void
  onFatalError: (taskId: string, error: Error) => void
  onFinished: (taskId: string) => void
  /** 返回 false 表示应停止（任务已被取消或状态已终态）。 */
  isActive: (taskId: string) => boolean
}

/** 可重试的错误码：网络问题、服务端临时故障。 */
const RETRYABLE = new Set(['internal', 'write_failed', 'too_many_concurrent'])

const MAX_CHUNK_RETRY = 3

interface ChunkJob {
  fileIndex: number
  chunkIndex: number
}

export class TaskUploader {
  private authorized = false
  private authWaiters: Array<() => void> = []
  private paused = false
  private pauseWaiters: Array<() => void> = []
  private aborted = false
  private running = false
  private controllers = new Set<AbortController>()

  private fileDone: number[] = []
  private chunkSize: number
  private totalBytes = 0

  constructor(
    private taskId: string,
    private sources: SourceFile[],
    serverChunkSize: number,
    private concurrency: number,
    private hooks: UploaderHooks,
  ) {
    this.chunkSize = serverChunkSize
    this.fileDone = sources.map(() => 0)
  }

  /** 服务端分配了传输槽位后调用，之后才真正开始推送分块。 */
  authorize() {
    if (this.authorized) return
    this.authorized = true
    this.authWaiters.splice(0).forEach((fn) => fn())
  }

  pause() {
    this.paused = true
  }

  resume() {
    if (!this.paused) return
    this.paused = false
    this.pauseWaiters.splice(0).forEach((fn) => fn())
  }

  /** 取消上传：中止在途请求并停止后续分块。 */
  abort() {
    this.aborted = true
    this.authWaiters.splice(0).forEach((fn) => fn())
    this.pauseWaiters.splice(0).forEach((fn) => fn())
    for (const c of this.controllers) {
      try {
        c.abort()
      } catch {
        /* 忽略 */
      }
    }
    this.controllers.clear()
  }

  isRunning(): boolean {
    return this.running
  }

  getTaskId(): string {
    return this.taskId
  }

  /** 计算本地已完成字节数，用于「暂停后立即反馈」这一真实场景。 */
  localDoneBytes(): number {
    return this.fileDone.reduce((a, b) => a + b, 0)
  }

  /**
   * 执行上传。返回时任务的分块已全部发出（或被中止/暂停）。
   * 幂等：可重复调用以「续传」，服务端按缺失清单只要求必要分块。
   */
  async run(): Promise<void> {
    if (this.running) return
    this.running = true
    try {
      await this.execute()
    } finally {
      this.running = false
    }
  }

  private async execute() {
    if (this.sources.length === 0) throw new SourceUnavailableError()

    // 1) 询问服务端当前分块状态，得到权威的缺失清单与源文件指纹。
    const status = await api.chunkStatus(this.taskId)
    if (!status.resumable && status.status !== 'uploading') {
      throw new ApiError(
        'not_resumable',
        status.nonResumableReason || '该任务不支持断点续传，请重新发起。',
        409,
      )
    }

    let queue = await this.buildQueue(status)
    this.totalBytes = this.sources.reduce((sum, s) => sum + s.file.size, 0)
    if (queue.length === 0) {
      this.hooks.onProgress(this.taskId, this.totalBytes, this.totalBytes)
      this.hooks.onFinished(this.taskId)
      return
    }

    // 2) 等待传输槽位授权（并发限制由服务端裁定）。
    await this.waitForAuthorization()
    if (this.aborted || !this.hooks.isActive(this.taskId)) return

    // 3) 并发上传
    let cursor = 0
    const worker = async () => {
      for (;;) {
        if (this.aborted || !this.hooks.isActive(this.taskId)) return
        await this.waitIfPaused()
        if (this.aborted || !this.hooks.isActive(this.taskId)) return

        const index = cursor++
        if (index >= queue.length) return
        const job = queue[index]
        await this.uploadOne(job)
      }
    }

    const workers = Array.from({ length: Math.max(1, Math.min(this.concurrency, queue.length)) })
    await Promise.all(workers.map(() => worker()))

    if (this.aborted) return
    if (!this.hooks.isActive(this.taskId)) return
    if (this.paused) {
      // 暂停中：不算完成，等待上层在继续后重新调用 run()。
      return
    }
    this.hooks.onProgress(this.taskId, this.totalBytes, this.totalBytes)
    this.hooks.onFinished(this.taskId)
  }

  /**
   * 构造待上传分块队列。
   * 对每个文件：先校验源指纹，必要时重置；再只排入缺失的分块。
   */
  private async buildQueue(status: ChunkStatus): Promise<ChunkJob[]> {
    const queue: ChunkJob[] = []
    for (const info of status.files) {
      if (info.skipped || info.status === 'ready' || info.status === 'downloaded') {
        this.fileDone[info.index] = info.size
        continue
      }
      const source = this.sources[info.index]
      if (!source) {
        // 服务端有该文件但本地没有对应的源文件：无法上传。
        throw new SourceUnavailableError(
          `缺少源文件「${info.name}」。页面刷新后需要重新选择文件才能继续。`,
        )
      }
      const sizeMatches = source.file.size === info.size
      // srcModTime 为 0 表示创建任务时未提供修改时间，此时只比较大小。
      const timeMatches = info.srcModTime === 0 || source.file.lastModified === info.srcModTime
      if (!sizeMatches || !timeMatches) {
        // 源文件已变化：清空服务端进度，整体重传，绝不拼接两份不同内容的字节。
        await api.resetFile(this.taskId, info.index)
        this.fileDone[info.index] = 0
        for (let i = 0; i < info.chunkCount; i++) {
          queue.push({ fileIndex: info.index, chunkIndex: i })
        }
        continue
      }
      this.fileDone[info.index] = info.receivedChunks * info.chunkSize
      if (this.fileDone[info.index] > info.size) this.fileDone[info.index] = info.size
      const missing = info.missing ?? []
      if (missing.length === 0 && info.chunkCount > 0) {
        // 服务端没有返回缺失清单，但该文件也尚未就绪（前面已跳过 ready/downloaded）：
        // 保守地全量重传，避免因为清单缺失而漏传分块。
        for (let i = 0; i < info.chunkCount; i++) {
          queue.push({ fileIndex: info.index, chunkIndex: i })
        }
        continue
      }
      for (const idx of missing) queue.push({ fileIndex: info.index, chunkIndex: idx })
    }
    return queue
  }

  private async uploadOne(job: ChunkJob): Promise<void> {
    const source = this.sources[job.fileIndex]
    if (!source) throw new SourceUnavailableError()
    const file = source.file
    const offset = job.chunkIndex * this.chunkSize
    const end = Math.min(offset + this.chunkSize, file.size)
    if (end <= offset && file.size > 0) return

    // 空文件没有分块，不会有 job 进入这里。
    const slice = file.slice(offset, end)

    let attempt = 0
    for (;;) {
      if (this.aborted || !this.hooks.isActive(this.taskId)) return
      const controller = new AbortController()
      this.controllers.add(controller)
      try {
        // 读入内存一次，既用于计算哈希也作为请求体，避免重复读取同一个分块。
        const buf = await slice.arrayBuffer()
        const hash = await sha256Hex(buf)
        await api.uploadChunk(this.taskId, job.fileIndex, job.chunkIndex, buf, hash, controller.signal)
        this.fileDone[job.fileIndex] += end - offset
        if (this.fileDone[job.fileIndex] > file.size) this.fileDone[job.fileIndex] = file.size
        this.hooks.onProgress(this.taskId, this.localDoneBytes(), this.totalBytes)
        return
      } catch (err) {
        if (this.aborted) return
        const e = err instanceof Error ? err : new Error(String(err))
        if (e.name === 'AbortError') return

        const code = e instanceof ApiError ? e.code : 'internal'
        const message = e instanceof ApiError ? e.message : e.message || '上传分块失败'

        // 任务已被取消 / 状态非法 / 无权限：立刻停止，不做无意义重试。
        if (code === 'invalid_state' || code === 'forbidden' || code === 'not_found') {
          this.hooks.onFatalError(this.taskId, e)
          this.abort()
          return
        }
        // 文件级失败：记录该文件并继续其它文件，避免一个坏文件阻塞整个任务。
        if (code === 'integrity_failed' || code === 'chunk_hash_mismatch') {
          this.hooks.onFileFailed(this.taskId, job.fileIndex, code, message)
          return
        }
        if (code === 'disk_full' || code === 'permission_denied' || code === 'file_too_large') {
          this.hooks.onFatalError(this.taskId, e)
          this.abort()
          return
        }

        attempt += 1
        if (RETRYABLE.has(code) && attempt <= MAX_CHUNK_RETRY) {
          this.hooks.onTransientError(this.taskId, `分块 ${job.chunkIndex} 上传失败，正在重试…`)
          await delay(600 * attempt)
          continue
        }
        this.hooks.onFileFailed(this.taskId, job.fileIndex, code, message)
        // 不中断整个任务：其余文件仍可完成。
        return
      } finally {
        this.controllers.delete(controller)
      }
    }
  }

  private waitForAuthorization(): Promise<void> {
    if (this.authorized || this.aborted) return Promise.resolve()
    return new Promise((resolve) => {
      this.authWaiters.push(resolve)
    })
  }

  private waitIfPaused(): Promise<void> {
    if (!this.paused || this.aborted) return Promise.resolve()
    return new Promise((resolve) => {
      this.pauseWaiters.push(resolve)
    })
  }
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

/** 任务状态是否允许继续上传。 */
export function statusAllowsUpload(status: string): boolean {
  return status === 'uploading'
}

/** 文件状态是否表示已完成。 */
export function isFileFinished(status: FileStatus): boolean {
  return status === 'ready' || status === 'downloaded' || status === 'skipped'
}
