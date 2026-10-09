/**
 * 全局提示（message）的桥接。
 *
 * antd 的 message 需要在 App 上下文内使用，但 store 与工具函数里也要能提示。
 * 因此由根组件把实例注册到这里，其余代码通过本模块调用，避免到处传 ref。
 */

export type MessageKind = 'success' | 'error' | 'info' | 'warning' | 'loading'

export interface MessageApi {
  success: (content: string, duration?: number) => void
  error: (content: string, duration?: number) => void
  info: (content: string, duration?: number) => void
  warning: (content: string, duration?: number) => void
  loading: (content: string, duration?: number) => void
}

let api: MessageApi | null = null

export function registerMessageApi(instance: MessageApi | null) {
  api = instance
}

function emit(kind: MessageKind, content: string, duration?: number) {
  if (!api) {
    // 根组件尚未挂载时的兜底：至少把问题暴露在控制台，不静默吞掉。
    console.warn(`[${kind}] ${content}`)
    return
  }
  api[kind](content, duration)
}

export const toast = {
  success: (content: string, duration?: number) => emit('success', content, duration),
  error: (content: string, duration?: number) => emit('error', content, duration ?? 4),
  info: (content: string, duration?: number) => emit('info', content, duration),
  warning: (content: string, duration?: number) => emit('warning', content, duration),
  loading: (content: string, duration?: number) => emit('loading', content, duration),
}

/**
 * 把错误码翻译成「可执行的下一步」。
 *
 * 目的：不让用户只看到「操作失败」。每个码都对应一个具体动作或原因。
 */
export function explainErrorCode(code: string, fallback: string): string {
  switch (code) {
    case 'not_found':
      return '任务或资源不存在，可能已被清理。请刷新列表后重试。'
    case 'forbidden':
      return '没有操作该任务的权限。该任务可能属于其它设备。'
    case 'unauthorized':
    case 'session_expired':
      return '会话已过期，请刷新页面重新连接。'
    case 'invalid_chunk_index':
    case 'bad_request':
      return '请求参数不合法。请重试；若持续出现，请重新选择文件发起传输。'
    case 'chunk_hash_mismatch':
      return '分块校验失败，网络传输可能被截断。系统会自动重传该分块。'
    case 'integrity_failed':
      return '文件完整性校验失败，源文件可能在传输过程中发生了变化。'
    case 'disk_full':
      return '接收端磁盘空间不足，请在设置中更换接收目录或清理空间。'
    case 'permission_denied':
      return '目录没有写入权限。请在设置中更换到可写目录。'
    case 'path_traversal':
    case 'invalid_file_name':
      return '文件名不合法，已被安全策略拦截。请重命名后重试。'
    case 'file_too_large':
      return '单个文件超过服务端配置的大小上限。可在设置中调整「单文件大小限制」。'
    case 'task_too_large':
      return '本次任务总大小超过上限。请减少文件数量或调整「单任务大小限制」。'
    case 'conflict_exists':
      return '目标位置已存在同名文件。请选择「自动重命名」或「覆盖」策略。'
    case 'not_resumable':
      return '该任务不支持断点续传，需要重新发起传输。'
    case 'source_changed':
      return '源文件已发生变化，无法继续续传。请重新选择文件。'
    case 'receiver_offline':
      return '接收设备已离线。请确认对方仍停留在本页面。'
    case 'rejected':
      return '对方拒绝了本次传输。'
    case 'cancelled':
      return '任务已取消。'
    case 'port_in_use':
      return '服务端口被占用，请在设置中更换端口后重启服务。'
    case 'too_many_concurrent':
      return '并发传输数已达上限，任务正在排队，稍后会自动开始。'
    case 'dropbox_expired':
      return '临时接收入口已失效。请在主机上重新开启。'
    case 'write_failed':
      return '文件写入失败，请检查磁盘与目录权限后重试。'
    case 'invalid_state':
      return '当前任务状态不允许该操作。请刷新后查看最新状态。'
    default:
      return fallback
  }
}
