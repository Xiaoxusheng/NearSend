// Package protocol 定义前后端共享的 WebSocket 事件协议与错误码。
// 事件名必须与前端 web/src/api/ws.ts 中监听的名字完全一致。
package protocol

// 事件类型常量。后端广播、前端监听、测试断言三方共用。
const (
	EvHello             = "server.hello"
	EvPing              = "server.ping"
	EvPong              = "client.pong"
	EvDeviceOnline      = "device.online"
	EvDeviceOffline     = "device.offline"
	EvDeviceUpdated     = "device.updated"
	EvTransferRequested = "transfer.requested"
	EvTransferAccepted  = "transfer.accepted"
	EvTransferRejected  = "transfer.rejected"
	EvTransferQueued    = "transfer.queued"
	EvTransferProgress  = "transfer.progress"
	EvTransferFileDone  = "transfer.file.completed"
	EvTransferCompleted = "transfer.completed"
	EvTransferFailed    = "transfer.failed"
	EvTransferCancelled = "transfer.cancelled"
	EvTransferPaused    = "transfer.paused"
	EvTransferResumed   = "transfer.resumed"
	EvTransferRetrying  = "transfer.retrying"
	EvDropboxChanged    = "dropbox.changed"
	EvSettingsChanged   = "settings.changed"
)

// Envelope 是所有 WebSocket 消息的统一信封。
// TS 为服务端生成时间戳（毫秒），前端据此丢弃明显过期的旧事件。
type Envelope struct {
	Type string `json:"type"`
	TS   int64  `json:"ts"`
	// TaskID 在传输类事件中必填，便于前端按任务定位而不需扫描全表。
	TaskID string `json:"taskId,omitempty"`
	// DeviceID 在设备类事件中必填。
	DeviceID string `json:"deviceId,omitempty"`
	Data     any    `json:"data,omitempty"`
}

// ProgressPayload 是 transfer.progress 的负载。
// 仅推送真实计数：DoneBytes 为服务端已持久化的分块字节数。
type ProgressPayload struct {
	TaskID     string `json:"taskId"`
	Status     string `json:"status"`
	DoneBytes  int64  `json:"doneBytes"`
	TotalSize  int64  `json:"totalSize"`
	TotalFiles int    `json:"totalFiles"`
	// Files 仅在文件数量不大时附带，避免高频进度消息过大。
	Files []FileProgress `json:"files,omitempty"`
}

// FileProgress 单文件进度。
type FileProgress struct {
	Index          int    `json:"index"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	DoneBytes      int64  `json:"doneBytes"`
	Status         string `json:"status"`
	ReceivedChunks int    `json:"receivedChunks"`
	ChunkCount     int    `json:"chunkCount"`
	Error          string `json:"error,omitempty"`
}

// ErrorCode 是稳定的机器可读错误码，前端据此选择恢复操作与文案。
// 错误消息本身面向用户，但错误码决定「能做什么」。
const (
	CodeOK                = ""
	CodeNotFound          = "not_found"
	CodeForbidden         = "forbidden"
	CodeUnauthorized      = "unauthorized"
	CodeSessionExpired    = "session_expired"
	CodeBadRequest        = "bad_request"
	CodeInvalidChunkIndex = "invalid_chunk_index"
	CodeChunkHashMismatch = "chunk_hash_mismatch"
	CodeIntegrityFailed   = "integrity_failed"
	CodeDiskFull          = "disk_full"
	CodePermissionDenied  = "permission_denied"
	CodePathTraversal     = "path_traversal"
	CodeInvalidFileName   = "invalid_file_name"
	CodeFileTooLarge      = "file_too_large"
	CodeTaskTooLarge      = "task_too_large"
	CodeConflictExists    = "conflict_exists"
	CodeNotResumable      = "not_resumable"
	CodeSourceChanged     = "source_changed"
	CodeReceiverOffline   = "receiver_offline"
	CodeRejected          = "rejected"
	CodeCancelled         = "cancelled"
	CodePortInUse         = "port_in_use"
	CodeTooManyConcurrent = "too_many_concurrent"
	CodeDropboxExpired    = "dropbox_expired"
	CodeWriteFailed       = "write_failed"
	CodeInvalidState      = "invalid_state"
	CodeInternal          = "internal"
)

// Message 返回面向用户的中文简短文案。前端也可以用错误码覆盖为更具体的引导。
var Message = map[string]string{
	CodeNotFound:          "任务或资源不存在",
	CodeForbidden:         "没有操作该任务的权限",
	CodeUnauthorized:      "会话无效或已过期，请刷新页面",
	CodeSessionExpired:    "会话已过期，请重新连接",
	CodeBadRequest:        "请求参数不合法",
	CodeInvalidChunkIndex: "分块编号超出范围",
	CodeChunkHashMismatch: "分块校验失败",
	CodeIntegrityFailed:   "文件完整性校验失败",
	CodeDiskFull:          "服务端磁盘空间不足",
	CodePermissionDenied:  "没有写入权限",
	CodePathTraversal:     "文件路径不合法",
	CodeInvalidFileName:   "文件名不合法",
	CodeFileTooLarge:      "文件大小超出配置限制",
	CodeTaskTooLarge:      "任务总大小超出配置限制",
	CodeConflictExists:    "目标位置已存在同名文件",
	CodeNotResumable:      "该任务不支持断点续传",
	CodeSourceChanged:     "源文件已发生变化，无法继续续传",
	CodeReceiverOffline:   "接收设备已离线",
	CodeRejected:          "对方拒绝了本次传输",
	CodeCancelled:         "任务已取消",
	CodePortInUse:         "服务端口被占用",
	CodeTooManyConcurrent: "并发传输数已达上限，任务排队中",
	CodeDropboxExpired:    "临时接收入口已失效",
	CodeWriteFailed:       "文件写入失败",
	CodeInvalidState:      "当前任务状态不允许该操作",
	CodeInternal:          "服务端内部错误",
}
