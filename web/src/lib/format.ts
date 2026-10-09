/** 展示层格式化工具。所有数值都来自真实数据，不做任何估算占位。 */

import dayjs from 'dayjs'
import relativeTime from 'dayjs/plugin/relativeTime'
import 'dayjs/locale/zh-cn'

dayjs.extend(relativeTime)
dayjs.locale('zh-cn')

/** 字节数格式化：使用 1024 进制，保留合理精度。 */
export function formatBytes(bytes: number, digits = 1): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—'
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  const value = bytes / 1024 ** i
  const fixed = i === 0 ? 0 : value >= 100 ? 0 : value >= 10 ? 1 : digits
  return `${value.toFixed(fixed)} ${units[i]}`
}

/** 传输速度：字节/秒。 */
export function formatSpeed(bytesPerSecond: number): string {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return '—'
  return `${formatBytes(bytesPerSecond)}/s`
}

/**
 * 剩余时间。
 * 数据不足时返回 null，由调用方显示占位符，绝不编造一个「看起来合理」的数值。
 */
export function formatEta(remainingBytes: number, bytesPerSecond: number): string | null {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return null
  if (!Number.isFinite(remainingBytes) || remainingBytes <= 0) return null
  const seconds = remainingBytes / bytesPerSecond
  if (!Number.isFinite(seconds) || seconds <= 0) return null
  if (seconds > 86400 * 7) return null
  return formatDuration(seconds)
}

/** 时长格式化，输入为秒。 */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '—'
  if (seconds < 1) return '<1 秒'
  const s = Math.round(seconds)
  if (s < 60) return `${s} 秒`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} 分 ${s % 60} 秒`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} 小时 ${m % 60} 分`
  return `${Math.floor(h / 24)} 天 ${h % 24} 小时`
}

/** 简洁时长：用于传输用时（只取最大单位）。 */
export function formatElapsed(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—'
  const s = ms / 1000
  if (s < 1) return '不到 1 秒'
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)} 秒`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} 分 ${Math.round(s % 60)} 秒`
  const h = Math.floor(m / 60)
  return `${h} 小时 ${m % 60} 分`
}

/** 绝对时间：2026-10-09 20:43 */
export function formatDateTime(ms?: number): string {
  if (!ms) return '—'
  return dayjs(ms).format('YYYY-MM-DD HH:mm')
}

/** 含秒的绝对时间。 */
export function formatDateTimeSec(ms?: number): string {
  if (!ms) return '—'
  return dayjs(ms).format('YYYY-MM-DD HH:mm:ss')
}

/** 只有时分秒，用于日志式的短提示。 */
export function formatTime(ms?: number): string {
  if (!ms) return '—'
  return dayjs(ms).format('HH:mm:ss')
}

/** 相对时间：3 分钟前。 */
export function formatRelative(ms?: number): string {
  if (!ms) return '—'
  return dayjs(ms).fromNow()
}

/** 倒计时展示：用于临时入口剩余时间。 */
export function formatCountdown(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '已过期'
  const total = Math.floor(ms / 1000)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  if (h > 0) return `${h} 小时 ${m} 分`
  if (m > 0) return `${m} 分 ${s} 秒`
  return `${s} 秒`
}

/** 百分比：0-100，保留一位小数但整数时不显示 .0。 */
export function formatPercent(done: number, total: number): string {
  if (!total || total <= 0) return '0%'
  const p = Math.max(0, Math.min(100, (done / total) * 100))
  if (p >= 100) return '100%'
  return `${p >= 10 ? p.toFixed(0) : p.toFixed(1)}%`
}

/** 从文件名推断一个用于图标选择的分类。 */
export type FileKind =
  | 'image'
  | 'video'
  | 'audio'
  | 'archive'
  | 'document'
  | 'code'
  | 'spreadsheet'
  | 'presentation'
  | 'pdf'
  | 'folder'
  | 'file'

const EXT_MAP: Record<string, FileKind> = {
  jpg: 'image', jpeg: 'image', png: 'image', gif: 'image', webp: 'image', bmp: 'image',
  svg: 'image', heic: 'image', avif: 'image', tif: 'image', tiff: 'image',
  mp4: 'video', mkv: 'video', mov: 'video', avi: 'video', webm: 'video', flv: 'video', wmv: 'video',
  mp3: 'audio', wav: 'audio', flac: 'audio', aac: 'audio', ogg: 'audio', m4a: 'audio',
  zip: 'archive', rar: 'archive', '7z': 'archive', tar: 'archive', gz: 'archive', bz2: 'archive',
  xz: 'archive', tgz: 'archive',
  doc: 'document', docx: 'document', txt: 'document', md: 'document', rtf: 'document',
  odt: 'document', log: 'document',
  xls: 'spreadsheet', xlsx: 'spreadsheet', csv: 'spreadsheet', tsv: 'spreadsheet', ods: 'spreadsheet',
  ppt: 'presentation', pptx: 'presentation', odp: 'presentation', key: 'presentation',
  pdf: 'pdf',
  js: 'code', ts: 'code', tsx: 'code', jsx: 'code', go: 'code', py: 'code', java: 'code',
  c: 'code', cpp: 'code', h: 'code', hpp: 'code', rs: 'code', rb: 'code', php: 'code',
  html: 'code', css: 'code', scss: 'code', json: 'code', yaml: 'code', yml: 'code',
  xml: 'code', sh: 'code', sql: 'code', vue: 'code', toml: 'code', ini: 'code',
}

export function fileKind(name: string): FileKind {
  const idx = name.lastIndexOf('.')
  if (idx <= 0) return 'file'
  const ext = name.slice(idx + 1).toLowerCase()
  return EXT_MAP[ext] ?? 'file'
}

/**
 * 超长文件名中间省略：保留头尾，中间用 … 代替。
 * 用于必须一眼看出扩展名和大致名称的场景（表格、任务标题）。
 */
export function truncateMiddle(name: string, max = 42): string {
  if (name.length <= max) return name
  const dot = name.lastIndexOf('.')
  const ext = dot > 0 && name.length - dot <= 12 ? name.slice(dot) : ''
  const stem = ext ? name.slice(0, dot) : name
  const keep = Math.max(4, max - ext.length - 1)
  const head = Math.ceil(keep * 0.6)
  const tail = keep - head
  return `${stem.slice(0, head)}…${tail > 0 ? stem.slice(-tail) : ''}${ext}`
}

/** 设备形态的中文名。 */
export function deviceTypeLabel(t: string): string {
  switch (t) {
    case 'desktop':
      return '台式机'
    case 'laptop':
      return '笔记本'
    case 'tablet':
      return '平板'
    case 'mobile':
      return '手机'
    default:
      return '未知设备'
  }
}

/** 任务状态的中文名与语义色。 */
export function transferStatusMeta(status: string): {
  label: string
  tone: 'neutral' | 'primary' | 'success' | 'warning' | 'danger' | 'info'
} {
  switch (status) {
    case 'awaiting':
      return { label: '等待确认', tone: 'warning' }
    case 'queued':
      return { label: '排队中', tone: 'info' }
    case 'uploading':
      return { label: '传输中', tone: 'primary' }
    case 'verifying':
      return { label: '校验中', tone: 'primary' }
    case 'paused':
      return { label: '已暂停', tone: 'neutral' }
    case 'ready':
      return { label: '待接收方下载', tone: 'info' }
    case 'completed':
      return { label: '已完成', tone: 'success' }
    case 'failed':
      return { label: '失败', tone: 'danger' }
    case 'cancelled':
      return { label: '已取消', tone: 'neutral' }
    case 'rejected':
      return { label: '已拒绝', tone: 'danger' }
    default:
      return { label: status, tone: 'neutral' }
  }
}

/** 文件状态的中文名。 */
export function fileStatusLabel(status: string): string {
  switch (status) {
    case 'pending':
      return '等待中'
    case 'uploading':
      return '传输中'
    case 'verifying':
      return '校验中'
    case 'ready':
      return '已落盘'
    case 'downloaded':
      return '已下载'
    case 'failed':
      return '失败'
    case 'skipped':
      return '已跳过'
    default:
      return status
  }
}

/** 把时间戳转成 dayjs 可用的值；0 视为未设置。 */
export function tsOrUndefined(ms?: number): number | undefined {
  return ms && ms > 0 ? ms : undefined
}
