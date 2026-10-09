// Package model 定义局域网快传的核心领域模型。
// 这些结构同时用于 SQLite 持久化、REST 序列化与 WebSocket 事件负载，
// 字段的 json tag 即对外协议的一部分，修改时需前后端同步。
package model

// DeviceType 描述设备形态，由 User-Agent 解析得出，不作为安全凭据。
type DeviceType string

const (
	DeviceDesktop DeviceType = "desktop"
	DeviceLaptop  DeviceType = "laptop"
	DeviceTablet  DeviceType = "tablet"
	DeviceMobile  DeviceType = "mobile"
	DeviceUnknown DeviceType = "unknown"
)

// Device 表示一个登记到当前服务的在线/历史客户端会话。
// ID 是随机会话标识，同名设备也拥有不同 ID，因此不能以名称判定身份。
type Device struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	OS          string     `json:"os"`
	Browser     string     `json:"browser"`
	Type        DeviceType `json:"type"`
	Online      bool       `json:"online"`
	Trusted     bool       `json:"trusted"`
	Self        bool       `json:"self"`
	FirstSeen   int64      `json:"firstSeen"`
	LastSeen    int64      `json:"lastSeen"`
	ConnectedAt int64      `json:"connectedAt"`
	LastIP      string     `json:"lastIp"`
	UserAgent   string     `json:"userAgent"`
	// Token 为会话令牌，仅在下发给自己设备时返回，绝不包含在设备列表中。
	Token string `json:"-"`
}

// TransferStatus 传输任务的生命周期状态。
//
// awaiting  -> 等待接收方确认
// rejected  -> 接收方拒绝
// queued    -> 已接受，等待传输槽位
// uploading -> 发送方正在推送分块
// paused    -> 用户主动暂停
// verifying -> 服务端正在进行完整性校验
// ready     -> 文件已入库并校验通过，等待接收方下载
// completed -> 接收方已取走全部文件
// failed / cancelled -> 终态
type TransferStatus string

const (
	StatusAwaiting  TransferStatus = "awaiting"
	StatusRejected  TransferStatus = "rejected"
	StatusQueued    TransferStatus = "queued"
	StatusUploading TransferStatus = "uploading"
	StatusPaused    TransferStatus = "paused"
	StatusVerifying TransferStatus = "verifying"
	StatusReady     TransferStatus = "ready"
	StatusCompleted TransferStatus = "completed"
	StatusFailed    TransferStatus = "failed"
	StatusCancelled TransferStatus = "cancelled"
)

// Terminal 判断状态是否为终态（不会再自动变化）。
func (s TransferStatus) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusRejected:
		return true
	}
	return false
}

// Active 判断任务是否占用传输资源。
func (s TransferStatus) Active() bool {
	switch s {
	case StatusQueued, StatusUploading, StatusVerifying:
		return true
	}
	return false
}

// FileStatus 单个文件在任务内的状态。
type FileStatus string

const (
	FilePending    FileStatus = "pending"
	FileUploading  FileStatus = "uploading"
	FileVerifying  FileStatus = "verifying"
	FileReady      FileStatus = "ready"
	FileDownloaded FileStatus = "downloaded"
	FileFailed     FileStatus = "failed"
	FileSkipped    FileStatus = "skipped"
)

// ConflictPolicy 同名文件处理策略（由接收方决定）。
type ConflictPolicy string

const (
	// ConflictRename 自动重命名，例如 "报告 (1).pdf"。默认值，绝不静默覆盖。
	ConflictRename ConflictPolicy = "rename"
	// ConflictOverwrite 覆盖已有文件。
	ConflictOverwrite ConflictPolicy = "overwrite"
	// ConflictSkip 跳过该文件。
	ConflictSkip ConflictPolicy = "skip"
)

// TransferFile 任务内的一个文件。
type TransferFile struct {
	ID      string `json:"id"`
	TaskID  string `json:"taskId"`
	Index   int    `json:"index"`
	Name    string `json:"name"`
	RelPath string `json:"relPath"`
	Size    int64  `json:"size"`
	Mime    string `json:"mime"`
	// SrcModTime 为源文件的修改时间（毫秒），用于断点续传时校验源文件是否被改动。
	SrcModTime int64      `json:"srcModTime"`
	SHA256     string     `json:"sha256"`
	Status     FileStatus `json:"status"`
	// DoneBytes 已落盘的有效字节数（分块累加）。
	DoneBytes  int64 `json:"doneBytes"`
	ChunkSize  int64 `json:"chunkSize"`
	ChunkCount int   `json:"chunkCount"`
	// ReceivedChunks 服务端已完整落盘的分块数量。
	ReceivedChunks int    `json:"receivedChunks"`
	Error          string `json:"error,omitempty"`
	ErrorCode      string `json:"errorCode,omitempty"`
	Skipped        bool   `json:"skipped"`
	DownloadedAt   int64  `json:"downloadedAt"`
	// FinalName 是在接收目录中实际落地的文件名（可能与原名不同，取决于重名策略）。
	FinalName string `json:"finalName,omitempty"`
}

// Transfer 一次传输任务。任务总是属于「发送方 -> 接收方」两个真实会话。
// 服务端作为中转：发送方上传分块，服务端校验入库，接收方再下载。
type Transfer struct {
	ID           string         `json:"id"`
	SenderID     string         `json:"senderId"`
	SenderName   string         `json:"senderName"`
	ReceiverID   string         `json:"receiverId"`
	ReceiverName string         `json:"receiverName"`
	Status       TransferStatus `json:"status"`
	// Direction 是相对于「本机（服务端所在设备）视角」的方向：
	// out = 本机发出，in = 本机接收。服务不假定设备身份，仅用于记录展示。
	Note       string `json:"note"`
	TotalFiles int    `json:"totalFiles"`
	TotalSize  int64  `json:"totalSize"`
	DoneSize   int64  `json:"doneSize"`
	ChunkSize  int64  `json:"chunkSize"`
	// Conflict 为接收方选定的重名策略。
	Conflict    ConflictPolicy `json:"conflict"`
	CreatedAt   int64          `json:"createdAt"`
	AcceptedAt  int64          `json:"acceptedAt"`
	StartedAt   int64          `json:"startedAt"`
	CompletedAt int64          `json:"completedAt"`
	UpdatedAt   int64          `json:"updatedAt"`
	Error       string         `json:"error,omitempty"`
	ErrorCode   string         `json:"errorCode,omitempty"`
	Verified    bool           `json:"verified"`
	// Resumable 为 false 时 NonResumableReason 说明原因，前端据此隐藏续传入口。
	Resumable          bool   `json:"resumable"`
	NonResumableReason string `json:"nonResumableReason,omitempty"`
	Retries            int    `json:"retries"`
	// UploadDone 表示发送侧分块已全部到达并通过校验。
	UploadDone bool `json:"uploadDone"`
	// DownloadDone 表示接收侧已取走全部文件。
	DownloadDone bool            `json:"downloadDone"`
	Files        []*TransferFile `json:"files,omitempty"`
	// HistoryVisible 为 false 表示该记录已被用户从历史中清除（不影响磁盘文件）。
	HistoryVisible bool `json:"historyVisible"`
	// ExpiresAt 用于临时接收入口创建的任务，过期后下载授权失效。
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	// ViaDropbox 表示该任务由临时接收入口发起（接收方免确认）。
	ViaDropbox bool `json:"viaDropbox"`
}

// HistoryEntry 是历史记录的扁平视图，供记录页表格直接渲染。
type HistoryEntry struct {
	TaskID       string         `json:"taskId"`
	FileID       string         `json:"fileId"`
	FileName     string         `json:"fileName"`
	Direction    string         `json:"direction"`
	SenderName   string         `json:"senderName"`
	ReceiverName string         `json:"receiverName"`
	Size         int64          `json:"size"`
	Status       TransferStatus `json:"status"`
	FileStatus   FileStatus     `json:"fileStatus"`
	StartedAt    int64          `json:"startedAt"`
	CompletedAt  int64          `json:"completedAt"`
	Error        string         `json:"error,omitempty"`
	ErrorCode    string         `json:"errorCode,omitempty"`
	Verified     bool           `json:"verified"`
	TaskStatus   TransferStatus `json:"taskStatus"`
}

// HistoryQuery 历史记录查询条件。
//
// Dir 的方向是相对于「发起查询的设备」而言的，不是相对于服务端：
//   - in  -> 该设备作为接收方
//   - out -> 该设备作为发送方
//
// 因此 SelfDeviceID 必须由服务端从会话推导，前端无法伪造。
type HistoryQuery struct {
	Keyword      string
	From         int64
	To           int64
	Status       []TransferStatus
	Dir          string // all | in | out
	SelfDeviceID string
	Page         int
	PageSize     int
}

// Dropbox 临时接收箱会话。临时入口具备有效期与撤销机制，
// 过期后后端拒绝一切写入与下载，而不是仅隐藏前端按钮。
type Dropbox struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	CreatedBy string `json:"createdBy"`
	// ReceivedCount / ReceivedBytes 统计通过临时入口收到的内容，均为真实计数。
	ReceivedCount int   `json:"receivedCount"`
	ReceivedBytes int64 `json:"receivedBytes"`
}

// Settings 是服务端的有效配置。监听地址与端口变更需要重启服务才能生效，
// API 在保存时会返回 needsRestart 标记并有明确提示。
type Settings struct {
	// --- 设备 ---
	DeviceName string `json:"deviceName"`
	// --- 传输 ---
	ReceiveDir      string         `json:"receiveDir"`
	TempDir         string         `json:"tempDir"`
	MaxConcurrent   int            `json:"maxConcurrentTransfers"`
	ChunkSize       int64          `json:"chunkSize"`
	MaxTaskSize     int64          `json:"maxTaskSize"`
	MaxFileSize     int64          `json:"maxFileSize"`
	AutoRetry       int            `json:"autoRetryCount"`
	BandwidthLimit  int64          `json:"bandwidthLimit"`
	DefaultConflict ConflictPolicy `json:"defaultConflictPolicy"`
	// --- 安全 ---
	AllowTrustedAutoAccept bool `json:"allowTrustedAutoAccept"`
	SessionTimeoutSec      int  `json:"sessionTimeoutSec"`
	TrustedTokenTTLHours   int  `json:"trustedTokenTtlHours"`
	DropboxTTLMinutes      int  `json:"dropboxTtlMinutes"`
	// --- 存储 ---
	HistoryRetentionDays int  `json:"historyRetentionDays"`
	TempRetentionHours   int  `json:"tempRetentionHours"`
	AutoCleanTemp        bool `json:"autoCleanTemp"`
	// --- 服务 ---
	ListenAddr string `json:"listenAddr"`
	Port       int    `json:"port"`
	// AdminToken 是管理令牌：非本机设备凭它一次性取得信任。
	//
	// 这是「服务跑在无头机器上」时唯一的安全引导手段，因此必须持久化；
	// 但它绝不出现在任何 API 响应里（返回前显式清空），也不允许通过设置接口修改。
	AdminToken string `json:"adminToken,omitempty"`
	// --- 只读运行时信息（保存时忽略）---
	Version      string `json:"version,omitempty"`
	DataDir      string `json:"dataDir,omitempty"`
	NeedsRestart bool   `json:"needsRestart,omitempty"`
}
