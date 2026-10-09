/**
 * 与后端协议一致的类型定义。
 *
 * 这些类型对应 Go 结构体的 json tag，任何一侧改动都必须同步另一侧。
 * 所有状态字面量都写成联合类型，使 switch 具备穷尽性检查。
 */

export type DeviceType = 'desktop' | 'laptop' | 'tablet' | 'mobile' | 'unknown'

export type TransferStatus =
  | 'awaiting'
  | 'rejected'
  | 'queued'
  | 'uploading'
  | 'paused'
  | 'verifying'
  | 'ready'
  | 'completed'
  | 'failed'
  | 'cancelled'

export type FileStatus =
  | 'pending'
  | 'uploading'
  | 'verifying'
  | 'ready'
  | 'downloaded'
  | 'failed'
  | 'skipped'

export type ConflictPolicy = 'rename' | 'overwrite' | 'skip'

export interface Device {
  id: string
  name: string
  os: string
  browser: string
  type: DeviceType
  online: boolean
  trusted: boolean
  self: boolean
  firstSeen: number
  lastSeen: number
  connectedAt: number
  lastIp: string
}

export interface TransferFile {
  id: string
  taskId: string
  index: number
  name: string
  relPath: string
  size: number
  mime: string
  srcModTime: number
  sha256: string
  status: FileStatus
  doneBytes: number
  chunkSize: number
  chunkCount: number
  receivedChunks: number
  error?: string
  errorCode?: string
  skipped: boolean
  downloadedAt: number
  finalName?: string
}

export interface Transfer {
  id: string
  senderId: string
  senderName: string
  receiverId: string
  receiverName: string
  status: TransferStatus
  note: string
  totalFiles: number
  totalSize: number
  doneSize: number
  chunkSize: number
  conflict: ConflictPolicy
  createdAt: number
  acceptedAt: number
  startedAt: number
  completedAt: number
  updatedAt: number
  error?: string
  errorCode?: string
  verified: boolean
  resumable: boolean
  nonResumableReason?: string
  retries: number
  uploadDone: boolean
  downloadDone: boolean
  files?: TransferFile[]
  historyVisible: boolean
  expiresAt?: number
  /** 是否由临时接收入口发起（接收方免确认）。 */
  viaDropbox: boolean
  /** 由服务端实时附加，表示对方是否有活跃连接 */
  senderOnline?: boolean
  receiverOnline?: boolean
}

export interface HistoryEntry {
  taskId: string
  fileId: string
  fileName: string
  direction: 'in' | 'out'
  senderName: string
  receiverName: string
  size: number
  status: TransferStatus
  fileStatus: FileStatus
  startedAt: number
  completedAt: number
  error?: string
  errorCode?: string
  verified: boolean
  taskStatus: TransferStatus
}

export interface Settings {
  deviceName: string
  receiveDir: string
  tempDir: string
  maxConcurrentTransfers: number
  chunkSize: number
  maxTaskSize: number
  maxFileSize: number
  autoRetryCount: number
  bandwidthLimit: number
  defaultConflictPolicy: ConflictPolicy
  allowTrustedAutoAccept: boolean
  sessionTimeoutSec: number
  trustedTokenTtlHours: number
  dropboxTtlMinutes: number
  historyRetentionDays: number
  tempRetentionHours: number
  autoCleanTemp: boolean
  listenAddr: string
  port: number
  version?: string
  dataDir?: string
  needsRestart?: boolean
}

/** 影响客户端行为的公开配置子集。 */
export interface PublicConfig {
  deviceName: string
  port: number
  chunkSize: number
  maxConcurrentTransfers: number
  maxFileSize: number
  maxTaskSize: number
  autoRetryCount: number
  bandwidthLimit: number
  defaultConflictPolicy: ConflictPolicy
  allowTrustedAutoAccept: boolean
  sessionTimeoutSec: number
  dropboxTtlMinutes: number
  historyRetentionDays: number
  version: string
  selfDeviceId: string
  selfDeviceName: string
  privileged: boolean
}

export interface AccessAddress {
  ip: string
  family: 'ipv4' | 'ipv6'
  iface: string
  url: string
  recommended: boolean
  loopback: boolean
}

export interface AccessInfo {
  port: number
  listenAddr: string
  addresses: AccessAddress[]
  primaryUrl: string
  qrUrl: string
  deviceName: string
}

export interface HealthInfo {
  status: string
  version: string
  uptimeSec: number
  serverTime: number
  onlineDevices: number
  wsConnections: number
}

export interface ChunkProgress {
  index: number
  name: string
  size: number
  srcModTime: number
  chunkSize: number
  chunkCount: number
  receivedChunks: number
  missing: number[]
  status: FileStatus
  skipped: boolean
  sha256?: string
}

export interface ChunkStatus {
  taskId: string
  status: TransferStatus
  chunkSize: number
  resumable: boolean
  nonResumableReason?: string
  doneBytes: number
  totalSize: number
  files: ChunkProgress[]
}

export interface ChunkResult {
  fileIndex: number
  chunkIndex: number
  receivedChunks: number
  chunkCount: number
  fileDoneBytes: number
  fileSize: number
  fileStatus: FileStatus
  taskDoneBytes: number
  taskTotalSize: number
  taskStatus: TransferStatus
  duplicate: boolean
  finalName?: string
}

export interface StorageUsage {
  receiveDir: string
  tempDir: string
  receiveFiles: number
  receiveBytes: number
  tempFiles: number
  tempBytes: number
  diskFreeBytes: number
  diskTotalBytes: number
  diskProbeFailed: boolean
}

export interface StorageStats {
  tasks: number
  completed: number
  active: number
  failed: number
  devices: number
  trusted: number
  totalBytes: number
  storedBytes: number
}

export interface Diagnostics {
  checkedAt: number
  service: {
    status: string
    version: string
    goVersion: string
    uptimeSec: number
    listenAddr: string
    port: number
    selfTest: { ok: boolean; error: string }
    portFree: boolean
    portNote: string
  }
  addresses: AccessAddress[]
  client: {
    ip: string
    deviceId: string
    deviceName: string
    os: string
    browser: string
    wsOnline: boolean
    wsConnections: number
    wsDevices: number
  }
  transfers: { active: number; failed: number; completed: number; tasks: number }
  storage: StorageUsage
  recentError:
    | null
    | {
        taskId: string
        code: string
        message: string
        at: number
        senderName: string
        receiverName: string
      }
  tips: string[]
}

export interface DropboxState {
  exists: boolean
  enabled: boolean
  id?: string
  token?: string
  createdAt?: number
  expiresAt?: number
  remainingMs?: number
  receivedCount?: number
  receivedBytes?: number
  uploadUrl?: string
  fullUrl?: string
  now: number
  ttlMinutes: number
  message?: string
}

/** 单文件进度（transfer.progress 事件的明细负载）。 */
export interface FileProgress {
  index: number
  name: string
  size: number
  doneBytes: number
  status: FileStatus
  receivedChunks: number
  chunkCount: number
  error?: string
}

/** 服务端返回的稳定错误码。 */
export type ErrorCode =
  | 'not_found'
  | 'forbidden'
  | 'unauthorized'
  | 'session_expired'
  | 'bad_request'
  | 'invalid_chunk_index'
  | 'chunk_hash_mismatch'
  | 'integrity_failed'
  | 'disk_full'
  | 'permission_denied'
  | 'path_traversal'
  | 'invalid_file_name'
  | 'file_too_large'
  | 'task_too_large'
  | 'conflict_exists'
  | 'not_resumable'
  | 'source_changed'
  | 'receiver_offline'
  | 'rejected'
  | 'cancelled'
  | 'port_in_use'
  | 'too_many_concurrent'
  | 'dropbox_expired'
  | 'write_failed'
  | 'invalid_state'
  | 'internal'
