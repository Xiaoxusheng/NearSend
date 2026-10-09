// Package transfer 实现文件传输引擎：任务生命周期、分块落盘与断点续传、
// SHA-256 完整性校验、并发槽位控制、取消/重试/暂停以及临时数据清理。
//
// 数据流向（浏览器无法接受入站连接，因此采用中转架构）：
//
//	发送方 --分块 HTTP PUT--> 服务端临时目录 --校验+原子提交--> 接收目录
//	接收目录 --授权 GET(支持 Range)--> 接收方浏览器下载
//
// 服务端只保存元数据于 SQLite，文件正文始终在文件系统上流式处理，
// 任何路径都不会把整个文件读进内存。
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"nearsend/internal/config"
	"nearsend/internal/fsutil"
	"nearsend/internal/model"
	"nearsend/internal/protocol"
	"nearsend/internal/store"
)

// Emitter 抽象事件推送，避免引擎与 hub 包相互依赖。
type Emitter interface {
	Broadcast(env protocol.Envelope)
	SendToDevice(deviceID string, env protocol.Envelope) bool
	IsOnline(deviceID string) bool
}

// Error 是带错误码的业务错误。Detail 仅供日志，不下发给客户端。
type Error struct {
	Code   string
	Msg    string
	Detail string
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Msg + ": " + e.Detail
	}
	return e.Msg
}

// NewError 构造业务错误，消息取协议中的标准中文文案。
func NewError(code string, detail string) *Error {
	msg := protocol.Message[code]
	if msg == "" {
		msg = protocol.Message[protocol.CodeInternal]
	}
	return &Error{Code: code, Msg: msg, Detail: detail}
}

// CodeOf 提取错误码，未识别的错误一律归为 internal。
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return protocol.CodeOK
	}
	if c := fsutil.ClassifyWriteError(err); c != "" {
		return c
	}
	return protocol.CodeInternal
}

// Engine 是传输引擎。
type Engine struct {
	st   *store.Store
	cfg  *config.Manager
	emit Emitter
	log  *slog.Logger

	// slots 是并发传输槽位，容量等于 MaxConcurrentTransfers。
	slots   chan struct{}
	slotsMu sync.Mutex
	granted map[string]bool // 已获得槽位的任务
	waiting []string        // 按创建顺序等待槽位的任务

	limiter *Limiter

	// progressMu 保护进度节流状态。
	progressMu sync.Mutex
	lastEmit   map[string]int64

	// fileLocks 串行化同一文件的收尾流程，避免并发分块触发重复校验/提交。
	fileLocks sync.Map // fileID -> *sync.Mutex

	closed chan struct{}
	wg     sync.WaitGroup
}

// New 创建引擎。
func New(st *store.Store, cfg *config.Manager, emit Emitter, log *slog.Logger) *Engine {
	e := &Engine{
		st:       st,
		cfg:      cfg,
		emit:     emit,
		log:      log,
		slots:    make(chan struct{}, cfg.Get().MaxConcurrent),
		granted:  map[string]bool{},
		limiter:  NewLimiter(cfg.Get().BandwidthLimit),
		lastEmit: map[string]int64{},
		closed:   make(chan struct{}),
	}
	return e
}

// Start 启动后台巡检协程：槽位容量同步、过期清理、临时文件与历史清理。
func (e *Engine) Start() {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		// 启动后立即跑一次，让重启后的状态尽快收敛。
		e.sweep()
		for {
			select {
			case <-e.closed:
				return
			case <-ticker.C:
				e.sweep()
			}
		}
	}()
}

// Stop 停止巡检。
func (e *Engine) Stop() {
	close(e.closed)
	e.wg.Wait()
}

// SyncRuntimeConfig 在设置变更后同步槽位容量与限速值。
func (e *Engine) SyncRuntimeConfig() {
	s := e.cfg.Get()
	e.slotsMu.Lock()
	if cap(e.slots) != s.MaxConcurrent {
		old := e.slots
		next := make(chan struct{}, s.MaxConcurrent)
		// 把已占用的槽位搬到新通道，保持正在进行的任务不被中断。
	drain:
		for {
			select {
			case <-old:
				select {
				case next <- struct{}{}:
				default:
					// 新容量更小且已填满，多余的槽位被丢弃（任务继续跑完）。
				}
			default:
				break drain
			}
		}
		e.slots = next
	}
	e.slotsMu.Unlock()
	e.limiter.SetLimit(s.BandwidthLimit)
	// 容量变大时可能有等待中的任务可以获得槽位。
	e.dispatchNext()
}

// ------------------------------------------------------------ 任务创建 ---

// FileSpec 是发送方声明的单个文件元数据。
// SHA256 可选：提供时服务端会在提交前强制比对，不提供则以服务端计算结果为准。
type FileSpec struct {
	Name    string `json:"name"`
	RelPath string `json:"relPath"`
	Size    int64  `json:"size"`
	Mime    string `json:"mime"`
	ModTime int64  `json:"modTime"`
	SHA256  string `json:"sha256"`
}

// CreateRequest 创建传输请求的参数。
type CreateRequest struct {
	Sender      *model.Device
	ReceiverIDs []string
	Files       []FileSpec
	Note        string
	Conflict    model.ConflictPolicy
	// DropboxToken 非空时走临时接收入口：跳过接收确认，但令牌必须有效。
	DropboxToken string
}

// Create 创建任务，返回每个接收设备对应的任务。
//
// 多接收方被拆成多个独立任务，因为它们需要各自确认、各自处理重名策略；
// 把多接收方塞进一个任务会让「部分接受」无法表达。
func (e *Engine) Create(req CreateRequest) ([]*model.Transfer, error) {
	if req.Sender == nil {
		return nil, NewError(protocol.CodeUnauthorized, "missing sender session")
	}
	if len(req.Files) == 0 {
		return nil, NewError(protocol.CodeBadRequest, "empty file list")
	}
	if len(req.ReceiverIDs) == 0 {
		return nil, NewError(protocol.CodeBadRequest, "no receiver selected")
	}
	s := e.cfg.Get()

	// 全局大小约束在创建期就拒绝，避免接收方确认后才失败。
	var total int64
	for _, f := range req.Files {
		if f.Size < 0 {
			return nil, NewError(protocol.CodeBadRequest, "negative file size")
		}
		if f.Size > s.MaxFileSize {
			return nil, NewError(protocol.CodeFileTooLarge,
				fmt.Sprintf("file %s size %d > limit %d", f.Name, f.Size, s.MaxFileSize))
		}
		total += f.Size
	}
	if total > s.MaxTaskSize {
		return nil, NewError(protocol.CodeTaskTooLarge,
			fmt.Sprintf("task size %d > limit %d", total, s.MaxTaskSize))
	}

	conflict := req.Conflict
	if conflict == "" {
		conflict = s.DefaultConflict
	}
	switch conflict {
	case model.ConflictRename, model.ConflictOverwrite, model.ConflictSkip:
	default:
		return nil, NewError(protocol.CodeBadRequest, "invalid conflict policy")
	}

	// 临时接收入口校验：令牌必须存在、已启用且未过期。
	var dropbox *model.Dropbox
	if req.DropboxToken != "" {
		db, err := e.st.GetActiveDropbox()
		if err != nil || db == nil || db.Token != req.DropboxToken || !db.Enabled || db.ExpiresAt < fsutil.NowMillis() {
			return nil, NewError(protocol.CodeDropboxExpired, "dropbox token invalid or expired")
		}
		dropbox = db
	}

	now := fsutil.NowMillis()
	out := make([]*model.Transfer, 0, len(req.ReceiverIDs))
	seen := map[string]bool{}
	for _, rid := range req.ReceiverIDs {
		if rid == "" || rid == req.Sender.ID || seen[rid] {
			// 不允许给自己发；重复接收方只处理一次。
			continue
		}
		seen[rid] = true
		receiver, err := e.st.GetDevice(rid)
		if err != nil {
			return nil, NewError(protocol.CodeNotFound, "receiver device not found: "+rid)
		}

		status := model.StatusAwaiting
		expires := int64(0)
		if dropbox != nil {
			// 临时入口：由接收目录所有者代表接收，跳过人工确认。
			status = model.StatusQueued
			expires = dropbox.ExpiresAt
		} else if !e.emit.IsOnline(rid) {
			// 接收设备离线：仍然建单，但任务会停在 awaiting，
			// 前端据此提示「对方离线」，而不是假装已送达。
			status = model.StatusAwaiting
		}

		t := &model.Transfer{
			ID:             uuid.NewString(),
			SenderID:       req.Sender.ID,
			SenderName:     req.Sender.Name,
			ReceiverID:     receiver.ID,
			ReceiverName:   receiver.Name,
			Status:         status,
			Note:           strings.TrimSpace(req.Note),
			TotalFiles:     len(req.Files),
			TotalSize:      total,
			ChunkSize:      s.ChunkSize,
			Conflict:       conflict,
			CreatedAt:      now,
			UpdatedAt:      now,
			Resumable:      true,
			HistoryVisible: true,
			ExpiresAt:      expires,
			// 记录来源：临时接收入口发起的任务在界面上会有明确标注。
			ViaDropbox: dropbox != nil,
		}
		if status == model.StatusQueued {
			t.AcceptedAt = now
		}

		files := make([]*model.TransferFile, 0, len(req.Files))
		for i, spec := range req.Files {
			// 路径净化发生在创建期：之后全流程都使用净化后的名字，
			// 避免「先入库后净化」导致的路径不一致。
			rel, err := fsutil.SanitizeRelPath(pick(spec.RelPath, spec.Name))
			if err != nil {
				return nil, NewError(protocol.CodeInvalidFileName, "file "+spec.Name)
			}
			name := rel
			if i := strings.LastIndex(rel, "/"); i >= 0 {
				name = rel[i+1:]
			}
			chunkCount := int((spec.Size + s.ChunkSize - 1) / s.ChunkSize)
			if spec.Size == 0 {
				chunkCount = 0 // 空文件不需要任何分块，收尾时直接提交
			}
			files = append(files, &model.TransferFile{
				ID:         uuid.NewString(),
				TaskID:     t.ID,
				Index:      i,
				Name:       name,
				RelPath:    rel,
				Size:       spec.Size,
				Mime:       spec.Mime,
				SrcModTime: spec.ModTime,
				SHA256:     normalizeHash(spec.SHA256),
				Status:     model.FilePending,
				ChunkSize:  s.ChunkSize,
				ChunkCount: chunkCount,
			})
		}
		// 全零大小的任务没有分块可传，创建后立即校验收尾。
		if err := e.st.CreateTransfer(t, files); err != nil {
			return nil, err
		}
		t.Files = files
		out = append(out, t)

		if dropbox != nil {
			_ = e.st.AddDropboxStats(dropbox.ID, 1, total)
			e.emit.Broadcast(protocol.Envelope{Type: protocol.EvDropboxChanged, TS: now})
			// 同步推进：让 Create 返回时任务已经进入确定的调度状态，
			// 避免调用方拿到任务后立即查询却看到尚未调度的中间态。
			e.afterAccept(t.ID, conflict)
			t, _ = e.load(t.ID)
			out[len(out)-1] = t
		} else {
			e.emitTo(t.ReceiverID, protocol.EvTransferRequested, t.ID, map[string]any{
				"task": e.view(t, files),
			})
		}
		e.emitTo(t.SenderID, protocol.EvTransferRequested, t.ID, map[string]any{
			"task": e.view(t, files),
		})
	}
	if len(out) == 0 {
		return nil, NewError(protocol.CodeBadRequest, "no valid receiver")
	}
	return out, nil
}

func pick(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func normalizeHash(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return ""
	}
	for _, c := range h {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ""
		}
	}
	if len(h) != 64 {
		return ""
	}
	return h
}

// ------------------------------------------------------------ 状态迁移 ---

// Accept 接收方接受请求。conflict 为空时使用任务创建时的策略。
func (e *Engine) Accept(taskID string, dev *model.Device, conflict model.ConflictPolicy) (*model.Transfer, error) {
	t, err := e.load(taskID)
	if err != nil {
		return nil, err
	}
	if dev == nil || t.ReceiverID != dev.ID {
		return nil, NewError(protocol.CodeForbidden, "only receiver can accept")
	}
	if t.Status != model.StatusAwaiting {
		return nil, NewError(protocol.CodeInvalidState, string(t.Status))
	}
	if conflict == "" {
		conflict = t.Conflict
	}
	switch conflict {
	case model.ConflictRename, model.ConflictOverwrite, model.ConflictSkip:
	default:
		return nil, NewError(protocol.CodeBadRequest, "invalid conflict policy")
	}
	now := fsutil.NowMillis()
	// 先落库再执行后续动作：即使进程在中间崩溃，重启后也能看到一致状态。
	if err := e.st.UpdateTransferState(taskID, model.StatusQueued, t.DoneSize, "", "", now); err != nil {
		return nil, err
	}
	t.Status = model.StatusQueued
	t.Conflict = conflict
	t.AcceptedAt = now
	if err := e.st.SaveTransfer(t); err != nil {
		return nil, err
	}
	e.emitTo(t.SenderID, protocol.EvTransferAccepted, taskID, map[string]any{"conflict": conflict})
	e.emitTo(t.ReceiverID, protocol.EvTransferAccepted, taskID, map[string]any{"conflict": conflict})
	e.afterAccept(taskID, conflict)
	return t, nil
}

// afterAccept 在接收后解析重名策略、处理空文件并申请传输槽位。
func (e *Engine) afterAccept(taskID string, conflict model.ConflictPolicy) {
	t, err := e.load(taskID)
	if err != nil {
		e.log.Error("afterAccept load failed", "task", taskID, "err", err)
		return
	}
	skipped := 0
	for _, f := range t.Files {
		// 跳过判定必须先于一切落盘动作：用户选择「跳过」就不应该占用任何带宽或磁盘。
		if conflict == model.ConflictSkip && e.destExists(t, f) {
			f.Skipped = true
			f.Status = model.FileSkipped
			if err := e.st.UpdateFile(f); err != nil {
				e.log.Error("mark skip failed", "err", err)
			}
			skipped++
			continue
		}
		// 空文件没有分块可传，在接收确认时直接完成提交。
		if f.Size == 0 {
			if _, ferr := e.finalizeFile(t, f); ferr != nil {
				e.failTask(t, ferr, "finalize empty file")
				return
			}
		}
	}
	if skipped == len(t.Files) {
		// 全部文件被跳过：任务直接完成，发送方无需上传任何数据。
		e.completeTask(t)
		return
	}
	if err := e.st.SaveTransfer(t); err != nil {
		e.log.Error("save transfer failed", "err", err)
	}
	t, err = e.load(taskID)
	if err != nil {
		return
	}
	if allFilesReady(t) {
		// 例如任务只包含空文件：无需等待上传即可判定完成。
		e.completeTask(t)
		return
	}
	e.tryDispatch(taskID)
}

// Reject 接收方拒绝。
func (e *Engine) Reject(taskID string, dev *model.Device, reason string) (*model.Transfer, error) {
	t, err := e.load(taskID)
	if err != nil {
		return nil, err
	}
	if dev == nil || t.ReceiverID != dev.ID {
		return nil, NewError(protocol.CodeForbidden, "only receiver can reject")
	}
	if t.Status.Terminal() {
		return nil, NewError(protocol.CodeInvalidState, string(t.Status))
	}
	now := fsutil.NowMillis()
	t.Status = model.StatusRejected
	t.ErrorCode = protocol.CodeRejected
	t.Error = strings.TrimSpace(reason)
	t.CompletedAt = now
	t.UpdatedAt = now
	if err := e.st.SaveTransfer(t); err != nil {
		return nil, err
	}
	e.releaseIfGranted(taskID)
	// 临时数据不在此处删除：任务可能被重试，保留已收分块才能续传。
	// 真正的清理由巡检按「临时文件保留时间」执行。
	e.emitBoth(t, protocol.EvTransferRejected, map[string]any{"reason": t.Error})
	return t, nil
}

// Cancel 取消任务。发送方与接收方都可以取消；终态任务不可取消。
func (e *Engine) Cancel(taskID string, dev *model.Device) (*model.Transfer, error) {
	t, err := e.load(taskID)
	if err != nil {
		return nil, err
	}
	if dev == nil || (t.SenderID != dev.ID && t.ReceiverID != dev.ID) {
		return nil, NewError(protocol.CodeForbidden, "not a participant of this task")
	}
	if t.Status.Terminal() {
		return nil, NewError(protocol.CodeInvalidState, string(t.Status))
	}
	now := fsutil.NowMillis()
	t.Status = model.StatusCancelled
	t.ErrorCode = protocol.CodeCancelled
	t.CompletedAt = now
	t.UpdatedAt = now
	if err := e.st.SaveTransfer(t); err != nil {
		return nil, err
	}
	e.releaseIfGranted(taskID)
	// 同上：取消保留已收分块，重试时可以续传而不是从头再来。
	e.emitBoth(t, protocol.EvTransferCancelled, map[string]any{"by": dev.ID})
	return t, nil
}

// Pause 暂停。仅在任务占用传输槽位时有意义，其它状态返回明确错误码。
func (e *Engine) Pause(taskID string, dev *model.Device) (*model.Transfer, error) {
	t, err := e.load(taskID)
	if err != nil {
		return nil, err
	}
	if dev == nil || (t.SenderID != dev.ID && t.ReceiverID != dev.ID) {
		return nil, NewError(protocol.CodeForbidden, "not a participant of this task")
	}
	if t.Status != model.StatusUploading && t.Status != model.StatusQueued {
		return nil, NewError(protocol.CodeInvalidState, string(t.Status))
	}
	now := fsutil.NowMillis()
	t.Status = model.StatusPaused
	t.UpdatedAt = now
	if err := e.st.SaveTransfer(t); err != nil {
		return nil, err
	}
	e.releaseIfGranted(taskID)
	e.emitBoth(t, protocol.EvTransferPaused, nil)
	return t, nil
}

// Resume 继续被暂停的任务。
func (e *Engine) Resume(taskID string, dev *model.Device) (*model.Transfer, error) {
	t, err := e.load(taskID)
	if err != nil {
		return nil, err
	}
	if dev == nil || (t.SenderID != dev.ID && t.ReceiverID != dev.ID) {
		return nil, NewError(protocol.CodeForbidden, "not a participant of this task")
	}
	if t.Status != model.StatusPaused {
		return nil, NewError(protocol.CodeInvalidState, string(t.Status))
	}
	now := fsutil.NowMillis()
	t.Status = model.StatusQueued
	t.UpdatedAt = now
	if err := e.st.SaveTransfer(t); err != nil {
		return nil, err
	}
	e.emitBoth(t, protocol.EvTransferResumed, nil)
	e.tryDispatch(taskID)
	return t, nil
}

// Retry 重试失败任务。不具备恢复条件的任务会被明确拒绝，而不是假装可以重试。
func (e *Engine) Retry(taskID string, dev *model.Device) (*model.Transfer, error) {
	t, err := e.load(taskID)
	if err != nil {
		return nil, err
	}
	if dev == nil || t.SenderID != dev.ID {
		return nil, NewError(protocol.CodeForbidden, "only sender can retry")
	}
	switch t.Status {
	case model.StatusFailed, model.StatusCancelled:
	case model.StatusRejected:
		// 被拒绝的任务需要接收方重新确认。
	default:
		return nil, NewError(protocol.CodeInvalidState, string(t.Status))
	}
	if !t.Resumable && t.Status == model.StatusFailed {
		return nil, NewError(protocol.CodeNotResumable, t.NonResumableReason)
	}
	// 校验源文件是否仍然与原始声明一致，避免「文件已改却续传」造成数据损坏。
	for _, f := range t.Files {
		if f.Status == model.FileReady || f.Status == model.FileDownloaded || f.Skipped {
			continue
		}
		part := e.partPath(t.ID, f.ID)
		if st, serr := os.Stat(part); serr == nil {
			if st.Size() > f.Size {
				// 临时文件比声明的还大，说明字节数与源不一致，只能重来。
				_ = os.Remove(part)
				if derr := e.st.DeleteChunksForFile(f.ID); derr != nil {
					return nil, derr
				}
				f.Status = model.FilePending
				f.DoneBytes = 0
				f.ReceivedChunks = 0
			}
		} else {
			// 分块记录存在但临时文件已丢失：清理记录防止误判为已有进度。
			if f.ReceivedChunks > 0 || f.DoneBytes > 0 {
				if derr := e.st.DeleteChunksForFile(f.ID); derr != nil {
					return nil, derr
				}
				f.Status = model.FilePending
				f.DoneBytes = 0
				f.ReceivedChunks = 0
			}
		}
		if err := e.st.UpdateFile(f); err != nil {
			return nil, err
		}
	}
	now := fsutil.NowMillis()
	t.Retries++
	t.Error = ""
	t.ErrorCode = ""
	t.CompletedAt = 0
	if t.Status == model.StatusRejected {
		t.Status = model.StatusAwaiting
		t.AcceptedAt = 0
	} else {
		t.Status = model.StatusQueued
	}
	t.UpdatedAt = now
	if err := e.st.SaveTransfer(t); err != nil {
		return nil, err
	}
	e.emitBoth(t, protocol.EvTransferRetrying, map[string]any{"attempt": t.Retries})
	if t.Status == model.StatusAwaiting {
		e.emitTo(t.ReceiverID, protocol.EvTransferRequested, t.ID, map[string]any{"task": e.view(t, t.Files)})
	} else {
		e.tryDispatch(taskID)
	}
	return t, nil
}

// ResetFile 清空某文件的分块进度（源文件改变时由发送方主动调用）。
func (e *Engine) ResetFile(taskID string, fileIndex int, dev *model.Device) error {
	t, err := e.load(taskID)
	if err != nil {
		return err
	}
	if dev == nil || t.SenderID != dev.ID {
		return NewError(protocol.CodeForbidden, "only sender can reset")
	}
	if t.Status.Terminal() {
		return NewError(protocol.CodeInvalidState, string(t.Status))
	}
	f, err := e.st.GetFile(taskID, fileIndex)
	if err != nil {
		return NewError(protocol.CodeNotFound, "file index")
	}
	if f.Status == model.FileReady || f.Status == model.FileDownloaded {
		return NewError(protocol.CodeInvalidState, "file already committed")
	}
	part := e.partPath(t.ID, f.ID)
	if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
		return NewError(protocol.CodeWriteFailed, err.Error())
	}
	if err := e.st.DeleteChunksForFile(f.ID); err != nil {
		return err
	}
	f.Status = model.FilePending
	f.DoneBytes = 0
	f.ReceivedChunks = 0
	f.Error = ""
	f.ErrorCode = ""
	return e.st.UpdateFile(f)
}

// ------------------------------------------------------------ 分块写入 ---

// ChunkResult 是一次分块上传后的真实状态快照，前端据此更新进度而不必等待 WS。
type ChunkResult struct {
	FileIndex      int    `json:"fileIndex"`
	ChunkIndex     int    `json:"chunkIndex"`
	ReceivedChunks int    `json:"receivedChunks"`
	ChunkCount     int    `json:"chunkCount"`
	FileDoneBytes  int64  `json:"fileDoneBytes"`
	FileSize       int64  `json:"fileSize"`
	FileStatus     string `json:"fileStatus"`
	TaskDoneBytes  int64  `json:"taskDoneBytes"`
	TaskTotalSize  int64  `json:"taskTotalSize"`
	TaskStatus     string `json:"taskStatus"`
	Duplicate      bool   `json:"duplicate"`
	FinalName      string `json:"finalName,omitempty"`
}

// WriteChunk 接收一个分块。
//
// 幂等性：重复提交同一索引会覆盖同一区间并重新登记，最终字节数不变。
// 校验顺序：先算 SHA-256 再落盘，校验不过的字节永远不会写进临时文件，
// 因此不会污染已完成的进度。
func (e *Engine) WriteChunk(ctx context.Context, taskID string, dev *model.Device, fileIndex, chunkIndex int,
	body io.Reader, declaredSHA string) (*ChunkResult, error) {

	t, err := e.load(taskID)
	if err != nil {
		return nil, err
	}
	if dev == nil || t.SenderID != dev.ID {
		return nil, NewError(protocol.CodeForbidden, "only sender can upload chunks")
	}
	switch t.Status {
	case model.StatusUploading:
		// 正常路径：已获得并发槽位。
	case model.StatusQueued:
		// 排队中尚未获得槽位。这里必须拒绝，否则并发上限形同虚设：
		// 排队任务只要能发分块就等于绕过了限制。
		return nil, NewError(protocol.CodeTooManyConcurrent,
			"task is queued, waiting for a transfer slot")
	case model.StatusPaused:
		// 暂停期间拒绝写入，让「暂停」具有真实语义。
		return nil, NewError(protocol.CodeInvalidState, "task is paused")
	default:
		return nil, NewError(protocol.CodeInvalidState, string(t.Status))
	}
	f, err := e.st.GetFile(taskID, fileIndex)
	if err != nil {
		return nil, NewError(protocol.CodeNotFound, "file index out of range")
	}
	if f.Status == model.FileReady || f.Status == model.FileDownloaded || f.Skipped {
		return nil, NewError(protocol.CodeInvalidState, "file already finalized")
	}
	if chunkIndex < 0 || chunkIndex >= f.ChunkCount {
		return nil, NewError(protocol.CodeInvalidChunkIndex,
			fmt.Sprintf("index %d not in [0,%d)", chunkIndex, f.ChunkCount))
	}
	offset := int64(chunkIndex) * f.ChunkSize
	expected := f.ChunkSize
	if remain := f.Size - offset; remain < expected {
		expected = remain
	}
	if expected <= 0 {
		return nil, NewError(protocol.CodeInvalidChunkIndex, "zero-length chunk position")
	}

	// 先读进内存再落盘：单块上限 64MiB，属可控范围，且必须完整校验后才能写。
	// 多读 1 字节用于检测超长请求，避免客户端用超大块绕过校验。
	data, err := io.ReadAll(io.LimitReader(body, expected+1))
	if err != nil {
		return nil, NewError(protocol.CodeWriteFailed, "read body: "+err.Error())
	}
	if int64(len(data)) != expected {
		return nil, NewError(protocol.CodeBadRequest,
			fmt.Sprintf("chunk size %d != expected %d", len(data), expected))
	}
	sum := fsutil.HashBytes(data)
	if declaredSHA != "" && !strings.EqualFold(normalizeHash(declaredSHA), sum) {
		return nil, NewError(protocol.CodeChunkHashMismatch, "declared sha256 mismatch")
	}

	if err := fsutil.EnsureDir(filepath.Dir(e.partPath(taskID, f.ID))); err != nil {
		return nil, NewError(protocol.CodeWriteFailed, err.Error())
	}
	part := e.partPath(taskID, f.ID)
	fh, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, NewError(fsutil.ClassifyWriteError(err), err.Error())
	}
	if err := lf(ctx, e.limiter, fh, data, offset); err != nil {
		fh.Close()
		return nil, NewError(fsutil.ClassifyWriteError(err), err.Error())
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		return nil, NewError(fsutil.ClassifyWriteError(err), err.Error())
	}
	if err := fh.Close(); err != nil {
		return nil, NewError(protocol.CodeWriteFailed, err.Error())
	}

	duplicate, err := e.st.MarkChunk(taskID, f.ID, chunkIndex, int64(len(data)), sum, fsutil.NowMillis())
	if err != nil {
		return nil, err
	}
	res := &ChunkResult{
		FileIndex:  fileIndex,
		ChunkIndex: chunkIndex,
		Duplicate:  !duplicate,
	}

	// 重新读取文件行以获得最新的已收分块数。
	f, err = e.st.GetFile(taskID, fileIndex)
	if err != nil {
		return nil, err
	}
	res.ReceivedChunks = f.ReceivedChunks
	res.ChunkCount = f.ChunkCount
	res.FileDoneBytes = f.DoneBytes
	res.FileSize = f.Size
	res.FileStatus = string(f.Status)

	if f.ReceivedChunks >= f.ChunkCount {
		final, ferr := e.finalizeFile(t, f)
		if ferr != nil {
			e.failTask(t, ferr, "finalize file")
			return nil, ferr
		}
		res.FileStatus = string(model.FileReady)
		res.FinalName = final
		e.emitBoth(t, protocol.EvTransferFileDone, map[string]any{
			"fileIndex": f.Index, "name": f.Name, "finalName": final,
		})
	}

	// 全部文件就绪则收尾整个任务。
	t, err = e.load(taskID)
	if err != nil {
		return nil, err
	}
	res.TaskDoneBytes = t.DoneSize
	res.TaskTotalSize = t.TotalSize
	if allFilesReady(t) {
		e.completeTask(t)
		t, _ = e.load(taskID)
	}
	res.TaskStatus = string(t.Status)
	e.emitProgress(t, false)
	return res, nil
}

// lf 执行一次限速写入。
func lf(ctx context.Context, l *Limiter, f *os.File, data []byte, offset int64) error {
	if l.Limit() <= 0 {
		_, err := f.WriteAt(data, offset)
		return err
	}
	if err := l.Wait(ctx, len(data)); err != nil {
		return err
	}
	_, err := f.WriteAt(data, offset)
	return err
}

// finalizeFile 校验临时文件并原子提交到接收目录。
//
// 提交前会重新解析目标名，因此即使「接受」之后出现了新的同名文件，
// 也不会发生静默覆盖（rename 策略下会再次改名）。
func (e *Engine) finalizeFile(t *model.Transfer, f *model.TransferFile) (string, error) {
	lock := e.fileMutex(f.ID)
	lock.Lock()
	defer lock.Unlock()

	if f.Status == model.FileReady || f.Status == model.FileDownloaded {
		return f.FinalName, nil
	}
	part := e.partPath(t.ID, f.ID)
	if f.Size == 0 {
		// 空文件没有任何分块，但仍需创建临时文件，才能统一走
		// 「校验 -> 原子提交」的同一条路径，避免出现特例分支。
		if err := fsutil.EnsureDir(filepath.Dir(part)); err != nil {
			return "", NewError(fsutil.ClassifyWriteError(err), err.Error())
		}
		fh, cerr := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if cerr != nil {
			return "", NewError(fsutil.ClassifyWriteError(cerr), cerr.Error())
		}
		if cerr := fh.Close(); cerr != nil {
			return "", NewError(protocol.CodeWriteFailed, cerr.Error())
		}
	}
	st, err := os.Stat(part)
	if err != nil {
		return "", NewError(protocol.CodeWriteFailed, "temp file missing: "+err.Error())
	}
	if st.Size() != f.Size {
		return "", NewError(protocol.CodeIntegrityFailed,
			fmt.Sprintf("size %d != declared %d", st.Size(), f.Size))
	}
	sum, err := fsutil.HashFile(part)
	if err != nil {
		return "", NewError(protocol.CodeWriteFailed, err.Error())
	}
	if f.SHA256 != "" && !strings.EqualFold(f.SHA256, sum) {
		return "", NewError(protocol.CodeIntegrityFailed,
			fmt.Sprintf("sha256 %s != declared %s", sum[:12], f.SHA256[:12]))
	}
	f.SHA256 = sum

	destRoot := e.receiveRoot(t)
	destDir, finalName, skip, err := e.resolveDestination(destRoot, f.RelPath, t.Conflict)
	if err != nil {
		return "", err
	}
	if skip {
		f.Skipped = true
		f.Status = model.FileSkipped
		f.FinalName = ""
		_ = os.Remove(part)
		return "", e.st.UpdateFile(f)
	}
	finalPath, err := fsutil.SafeJoin(destDir, finalName)
	if err != nil {
		return "", NewError(protocol.CodePathTraversal, err.Error())
	}
	allowReplace := t.Conflict == model.ConflictOverwrite
	if err := fsutil.AtomicCommit(part, finalPath, allowReplace); err != nil {
		if os.IsExist(err) {
			// 竞态导致目标出现：按 rename 策略再试一次唯一名。
			alt, _, uerr := fsutil.UniqueName(destDir, finalName, "rename", true)
			if uerr != nil {
				return "", NewError(protocol.CodeConflictExists, uerr.Error())
			}
			finalPath, err = fsutil.SafeJoin(destDir, alt)
			if err != nil {
				return "", NewError(protocol.CodePathTraversal, err.Error())
			}
			if err = fsutil.AtomicCommit(part, finalPath, false); err != nil {
				return "", NewError(fsutil.ClassifyWriteError(err), err.Error())
			}
			finalName = alt
		} else {
			return "", NewError(fsutil.ClassifyWriteError(err), err.Error())
		}
	}
	f.Status = model.FileReady
	f.FinalName = finalName
	f.DoneBytes = f.Size
	f.Error = ""
	f.ErrorCode = ""
	if err := e.st.UpdateFile(f); err != nil {
		return "", err
	}
	return finalName, nil
}

// destExists 判断文件在接收目录中是否已存在（用于 skip 策略的预览）。
func (e *Engine) destExists(t *model.Transfer, f *model.TransferFile) bool {
	destRoot := e.receiveRoot(t)
	destDir, _, _, err := e.resolveDestination(destRoot, f.RelPath, model.ConflictRename)
	if err != nil {
		return false
	}
	p, err := fsutil.SafeJoin(destDir, f.Name)
	if err != nil {
		return false
	}
	_, serr := os.Lstat(p)
	return serr == nil
}

// resolveDestination 计算落盘目录与最终文件名，并处理重名与目录嵌套。
// 返回 skip=true 表示按 skip 策略应放弃该文件。
func (e *Engine) resolveDestination(destRoot, relPath string, policy model.ConflictPolicy) (dir, name string, skip bool, err error) {
	rel, err := fsutil.SanitizeRelPath(relPath)
	if err != nil {
		return "", "", false, NewError(protocol.CodeInvalidFileName, err.Error())
	}
	parts := strings.Split(rel, "/")
	sub := parts[:len(parts)-1]
	base := parts[len(parts)-1]

	dir, err = fsutil.SafeJoin(destRoot, append([]string{}, sub...)...)
	if err != nil {
		return "", "", false, NewError(protocol.CodePathTraversal, err.Error())
	}
	if err := fsutil.EnsureDir(dir); err != nil {
		return "", "", false, NewError(fsutil.ClassifyWriteError(err), err.Error())
	}
	neverOverwrite := policy != model.ConflictOverwrite
	final, existed, err := fsutil.UniqueName(dir, base, string(policy), neverOverwrite)
	if err != nil {
		return "", "", false, NewError(protocol.CodeInvalidFileName, err.Error())
	}
	if existed && policy == model.ConflictSkip {
		return dir, final, true, nil
	}
	return dir, final, false, nil
}

// receiveRoot 是任务文件的落盘根目录：接收目录/日期/发送方名称。
// 保留发送方目录让多设备共用服务时文件天然分开，也便于事后追溯。
func (e *Engine) receiveRoot(t *model.Transfer) string {
	s := e.cfg.Get()
	// 固定使用创建时间：同一任务的所有文件始终落在同一个日期目录，
	// 不会因为提交跨过午夜而被拆到两个目录。
	day := time.UnixMilli(orNow(t.CreatedAt, t.CreatedAt)).Format("2006-01-02")
	sender, err := fsutil.SanitizeName(t.SenderName)
	if err != nil || sender == "" {
		sender = "unknown"
	}
	return filepath.Join(s.ReceiveDir, day, sender)
}

func orNow(a, b int64) int64 {
	if a > 0 {
		return a
	}
	if b > 0 {
		return b
	}
	return fsutil.NowMillis()
}

// partPath 返回某文件分块的临时落盘路径。
// 命名包含序号与净化后的文件名，便于人工识别残留临时文件。
func (e *Engine) partPath(taskID, fileID string) string {
	return filepath.Join(e.cfg.Get().TempDir, "transfers", taskID, fileID+".part")
}

// ------------------------------------------------------------ 槽位调度 ---

// tryDispatch 尝试为任务申请传输槽位；无可用槽位时保持排队。
func (e *Engine) tryDispatch(taskID string) {
	t, err := e.load(taskID)
	if err != nil {
		return
	}
	// 只有「排队中」才需要调度；「传输中」表示已经持有槽位。
	if t.Status != model.StatusQueued && t.Status != model.StatusUploading {
		return
	}

	e.slotsMu.Lock()
	if e.granted[taskID] {
		if t.Status == model.StatusUploading {
			e.slotsMu.Unlock()
			return
		}
		// 槽位仍在册但任务已不在传输态：说明状态被其它路径改写却没有回收槽位
		// （例如重启恢复把 uploading 改成 failed）。此时主动归还并重新申请，
		// 否则槽位会被慢慢吃光，并发上限最终退化为 0。
		e.releaseGrantedLocked(taskID)
	}
	select {
	case e.slots <- struct{}{}:
		e.granted[taskID] = true
		e.slotsMu.Unlock()
		e.markUploading(taskID)
		return
	default:
	}
	// 无空闲槽位：加入等待队列（去重）。
	for _, id := range e.waiting {
		if id == taskID {
			e.slotsMu.Unlock()
			return
		}
	}
	e.waiting = append(e.waiting, taskID)
	e.slotsMu.Unlock()
	e.markQueued(taskID)
}

// releaseGrantedLocked 在已持有 slotsMu 的前提下归还槽位。
func (e *Engine) releaseGrantedLocked(taskID string) bool {
	if !e.granted[taskID] {
		return false
	}
	delete(e.granted, taskID)
	select {
	case <-e.slots:
	default:
		// 通道里已无令牌（理论上不应发生），保守忽略。
	}
	return true
}

// resetSlots 清空全部槽位记账，并把槽位通道恢复为空闲。
// 用于进程启动后的状态收敛，确保内存中的槽位与数据库状态一致。
func (e *Engine) resetSlots() {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	e.granted = map[string]bool{}
	e.waiting = nil
drain:
	for {
		select {
		case <-e.slots:
		default:
			break drain
		}
	}
}

// dispatchNext 在槽位释放后调度最早排队的任务。
func (e *Engine) dispatchNext() {
	for {
		e.slotsMu.Lock()
		if len(e.waiting) == 0 {
			e.slotsMu.Unlock()
			return
		}
		next := e.waiting[0]
		e.waiting = e.waiting[1:]
		select {
		case e.slots <- struct{}{}:
			e.granted[next] = true
			e.slotsMu.Unlock()
			e.markUploading(next)
		default:
			// 仍然没有空位，放回队首。
			e.waiting = append([]string{next}, e.waiting...)
			e.slotsMu.Unlock()
			return
		}
	}
}

// releaseIfGranted 归还槽位。
func (e *Engine) releaseIfGranted(taskID string) {
	e.slotsMu.Lock()
	wasGranted := e.releaseGrantedLocked(taskID)
	// 从等待队列移除。
	for i, id := range e.waiting {
		if id == taskID {
			e.waiting = append(e.waiting[:i], e.waiting[i+1:]...)
			break
		}
	}
	e.slotsMu.Unlock()
	if wasGranted {
		e.dispatchNext()
	}
}

func (e *Engine) markQueued(taskID string) {
	t, err := e.load(taskID)
	if err != nil {
		return
	}
	if t.Status == model.StatusQueued {
		e.emitBoth(t, protocol.EvTransferQueued, map[string]any{"waiting": true})
	}
}

func (e *Engine) markUploading(taskID string) {
	t, err := e.load(taskID)
	if err != nil {
		return
	}
	now := fsutil.NowMillis()
	if t.StartedAt == 0 {
		t.StartedAt = now
	}
	t.Status = model.StatusUploading
	t.UpdatedAt = now
	if err := e.st.SaveTransfer(t); err != nil {
		e.log.Error("mark uploading failed", "task", taskID, "err", err)
		return
	}
	// 发送方收到该事件后才真正开始推送分块。
	// granted=true 明确表示「并发槽位已到手，可以开始上传」，
	// 与 waiting=true（仅排队、尚不可上传）区分开，避免前端误判。
	payload := map[string]any{"waiting": false, "granted": true, "status": string(model.StatusUploading)}
	e.emitTo(t.SenderID, protocol.EvTransferQueued, taskID, payload)
	e.emitTo(t.ReceiverID, protocol.EvTransferQueued, taskID, payload)
	e.emitProgress(t, true)
}

// ------------------------------------------------------------ 收尾 ---

func allFilesReady(t *model.Transfer) bool {
	for _, f := range t.Files {
		if f.Status != model.FileReady && f.Status != model.FileDownloaded && !f.Skipped {
			return false
		}
	}
	return true
}

// completeTask 在所有文件就绪后把任务置为完成。
// 完成条件严格依赖「每块已校验 + 整文件 SHA-256 已比对」，不存在跳过校验的成功路径。
func (e *Engine) completeTask(t *model.Transfer) {
	cur, err := e.load(t.ID)
	if err != nil {
		return
	}
	if cur.Status == model.StatusCompleted {
		return
	}
	now := fsutil.NowMillis()
	cur.Status = model.StatusCompleted
	cur.Verified = true
	cur.UploadDone = true
	// 已传字节按「实际需要传输的文件」统计，被跳过的文件不虚增进度。
	var transferred int64
	for _, f := range cur.Files {
		if f.Skipped {
			continue
		}
		transferred += f.Size
	}
	cur.DoneSize = transferred
	cur.CompletedAt = now
	cur.UpdatedAt = now
	cur.Error = ""
	cur.ErrorCode = ""
	if err := e.st.SaveTransfer(cur); err != nil {
		e.log.Error("complete task failed", "task", cur.ID, "err", err)
		return
	}
	e.releaseIfGranted(cur.ID)
	e.emitBoth(cur, protocol.EvTransferCompleted, map[string]any{"task": e.view(cur, cur.Files)})
}

// failTask 记录失败原因并释放资源。临时文件保留以便重试；由巡检按保留期清理。
func (e *Engine) failTask(t *model.Transfer, cause error, ctxNote string) {
	cur, err := e.load(t.ID)
	if err != nil {
		return
	}
	code := CodeOf(cause)
	now := fsutil.NowMillis()
	cur.Status = model.StatusFailed
	cur.ErrorCode = code
	cur.Error = protocol.Message[code]
	if cur.Error == "" {
		cur.Error = "传输失败"
	}
	// 只有确定性错误才判定为不可恢复；其余保留续传能力。
	switch code {
	case protocol.CodeDiskFull, protocol.CodePermissionDenied, protocol.CodeIntegrityFailed:
		cur.Resumable = true
	case protocol.CodeNotResumable:
		cur.Resumable = false
		cur.NonResumableReason = protocol.Message[protocol.CodeNotResumable]
	}
	cur.CompletedAt = now
	cur.UpdatedAt = now
	if err := e.st.SaveTransfer(cur); err != nil {
		e.log.Error("fail task save failed", "task", cur.ID, "err", err)
	}
	e.releaseIfGranted(cur.ID)
	e.log.Warn("transfer failed", "task", cur.ID, "code", code, "note", ctxNote,
		"detail", cause.Error())
	e.emitBoth(cur, protocol.EvTransferFailed, map[string]any{
		"code": code, "message": cur.Error, "resumable": cur.Resumable,
	})
}

// MarkDownloaded 在接收方完整取走某文件后登记。
func (e *Engine) MarkDownloaded(taskID string, fileIndex int, dev *model.Device) error {
	t, err := e.load(taskID)
	if err != nil {
		return err
	}
	if dev == nil || t.ReceiverID != dev.ID {
		return NewError(protocol.CodeForbidden, "only receiver can download")
	}
	f, err := e.st.GetFile(taskID, fileIndex)
	if err != nil {
		return NewError(protocol.CodeNotFound, "file index")
	}
	if f.Status != model.FileReady && f.Status != model.FileDownloaded {
		return NewError(protocol.CodeInvalidState, string(f.Status))
	}
	if f.Status != model.FileDownloaded {
		f.Status = model.FileDownloaded
	}
	f.DownloadedAt = fsutil.NowMillis()
	if err := e.st.UpdateFile(f); err != nil {
		return err
	}
	files, err := e.st.ListFiles(taskID)
	if err != nil {
		return err
	}
	all := true
	for _, x := range files {
		if x.Skipped {
			continue
		}
		if x.Status != model.FileDownloaded {
			all = false
			break
		}
	}
	if all && !t.DownloadDone {
		t.DownloadDone = true
		t.UpdatedAt = fsutil.NowMillis()
		if err := e.st.SaveTransfer(t); err != nil {
			return err
		}
		e.emitBoth(t, protocol.EvTransferProgress, map[string]any{"downloaded": true})
	}
	return nil
}

// FilePath 返回某文件最终落盘路径，供下载接口读取。
// 授权检查在 API 层完成；这里只做状态与存在性校验。
func (e *Engine) FilePath(taskID string, fileIndex int) (string, *model.TransferFile, *model.Transfer, error) {
	t, err := e.load(taskID)
	if err != nil {
		return "", nil, nil, err
	}
	f, err := e.st.GetFile(taskID, fileIndex)
	if err != nil {
		return "", nil, nil, NewError(protocol.CodeNotFound, "file index")
	}
	if f.Status != model.FileReady && f.Status != model.FileDownloaded {
		return "", nil, nil, NewError(protocol.CodeInvalidState, "file not ready")
	}
	destDir, _, _, derr := e.resolveDestination(e.receiveRoot(t), f.RelPath, model.ConflictOverwrite)
	if derr != nil {
		return "", nil, nil, derr
	}
	name := f.FinalName
	if name == "" {
		name = f.Name
	}
	p, jerr := fsutil.SafeJoin(destDir, name)
	if jerr != nil {
		return "", nil, nil, NewError(protocol.CodePathTraversal, jerr.Error())
	}
	if _, serr := os.Stat(p); serr != nil {
		return "", nil, nil, NewError(protocol.CodeNotFound, "stored file missing")
	}
	return p, f, t, nil
}

// ChunkStatus 返回任务的分块与恢复状态，供发送方计算断点续传。
type ChunkStatus struct {
	TaskID    string       `json:"taskId"`
	Status    string       `json:"status"`
	ChunkSize int64        `json:"chunkSize"`
	Resumable bool         `json:"resumable"`
	Reason    string       `json:"nonResumableReason,omitempty"`
	DoneBytes int64        `json:"doneBytes"`
	TotalSize int64        `json:"totalSize"`
	Files     []FileChunks `json:"files"`
}

// FileChunks 单文件的续传信息。
type FileChunks struct {
	Index          int    `json:"index"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	SrcModTime     int64  `json:"srcModTime"`
	ChunkSize      int64  `json:"chunkSize"`
	ChunkCount     int    `json:"chunkCount"`
	ReceivedChunks int    `json:"receivedChunks"`
	Missing        []int  `json:"missing"`
	Status         string `json:"status"`
	Skipped        bool   `json:"skipped"`
	SHA256         string `json:"sha256,omitempty"`
}

// Chunks 计算某任务当前缺失的分块索引列表。
func (e *Engine) Chunks(taskID string, dev *model.Device) (*ChunkStatus, error) {
	t, err := e.load(taskID)
	if err != nil {
		return nil, err
	}
	if dev == nil || (t.SenderID != dev.ID && t.ReceiverID != dev.ID) {
		return nil, NewError(protocol.CodeForbidden, "not a participant of this task")
	}
	out := &ChunkStatus{
		TaskID:    t.ID,
		Status:    string(t.Status),
		ChunkSize: t.ChunkSize,
		Resumable: t.Resumable,
		Reason:    t.NonResumableReason,
		DoneBytes: t.DoneSize,
		TotalSize: t.TotalSize,
		Files:     make([]FileChunks, 0, len(t.Files)),
	}
	for _, f := range t.Files {
		fc := FileChunks{
			Index: f.Index, Name: f.Name, Size: f.Size, SrcModTime: f.SrcModTime,
			ChunkSize: f.ChunkSize, ChunkCount: f.ChunkCount,
			ReceivedChunks: f.ReceivedChunks, Status: string(f.Status),
			Skipped: f.Skipped, SHA256: f.SHA256,
		}
		if f.Status != model.FileReady && f.Status != model.FileDownloaded && !f.Skipped {
			have, cerr := e.st.ChunkIndexes(f.ID)
			if cerr != nil {
				return nil, cerr
			}
			missing := make([]int, 0, f.ChunkCount)
			for i := 0; i < f.ChunkCount; i++ {
				if !have[i] {
					missing = append(missing, i)
				}
			}
			fc.Missing = missing
		}
		out.Files = append(out.Files, fc)
	}
	return out, nil
}

// ------------------------------------------------------------ 查询 ---

func (e *Engine) load(taskID string) (*model.Transfer, error) {
	t, err := e.st.GetTransfer(taskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, NewError(protocol.CodeNotFound, "task not found")
	}
	return t, err
}

// View 对外暴露任务视图（设备在线状态实时附加）。
type View struct {
	*model.Transfer
	SenderOnline   bool `json:"senderOnline"`
	ReceiverOnline bool `json:"receiverOnline"`
}

func (e *Engine) view(t *model.Transfer, files []*model.TransferFile) *View {
	cp := *t
	cp.Files = files
	return &View{
		Transfer:       &cp,
		SenderOnline:   e.emit.IsOnline(t.SenderID),
		ReceiverOnline: e.emit.IsOnline(t.ReceiverID),
	}
}

// View 返回单任务视图。
func (e *Engine) View(taskID string) (*View, error) {
	t, err := e.load(taskID)
	if err != nil {
		return nil, err
	}
	return e.view(t, t.Files), nil
}

// ------------------------------------------------------------ 事件 ---

func (e *Engine) emitTo(deviceID, event, taskID string, data any) {
	if deviceID == "" {
		return
	}
	e.emit.SendToDevice(deviceID, protocol.Envelope{
		Type: event, TS: fsutil.NowMillis(), TaskID: taskID, DeviceID: deviceID, Data: data,
	})
}

func (e *Engine) emitBoth(t *model.Transfer, event string, data any) {
	env := protocol.Envelope{Type: event, TS: fsutil.NowMillis(), TaskID: t.ID, Data: data}
	e.emit.SendToDevice(t.SenderID, env)
	if t.ReceiverID != t.SenderID {
		e.emit.SendToDevice(t.ReceiverID, env)
	}
}

// emitProgress 推送节流后的进度事件。force 为 true 时忽略节流
// （状态变更、文件完成等关键节点必须立即送达）。
func (e *Engine) emitProgress(t *model.Transfer, force bool) {
	const minInterval = 350 * time.Millisecond
	now := fsutil.NowMillis()
	e.progressMu.Lock()
	last := e.lastEmit[t.ID]
	if !force && now-last < minInterval.Milliseconds() {
		e.progressMu.Unlock()
		return
	}
	e.lastEmit[t.ID] = now
	e.progressMu.Unlock()

	// 文件很多时不带明细，避免高频事件体积膨胀。
	var files []protocol.FileProgress
	if len(t.Files) <= 40 {
		files = make([]protocol.FileProgress, 0, len(t.Files))
		for _, f := range t.Files {
			files = append(files, protocol.FileProgress{
				Index: f.Index, Name: f.Name, Size: f.Size, DoneBytes: f.DoneBytes,
				Status: string(f.Status), ReceivedChunks: f.ReceivedChunks,
				ChunkCount: f.ChunkCount, Error: f.Error,
			})
		}
	}
	env := protocol.Envelope{
		Type: protocol.EvTransferProgress, TS: now, TaskID: t.ID,
		Data: protocol.ProgressPayload{
			TaskID: t.ID, Status: string(t.Status), DoneBytes: t.DoneSize,
			TotalSize: t.TotalSize, TotalFiles: t.TotalFiles, Files: files,
		},
	}
	e.emit.SendToDevice(t.SenderID, env)
	if t.ReceiverID != t.SenderID {
		e.emit.SendToDevice(t.ReceiverID, env)
	}
}

func (e *Engine) fileMutex(fileID string) *sync.Mutex {
	v, _ := e.fileLocks.LoadOrStore(fileID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// ------------------------------------------------------------ 清理 ---

// cleanupTemp 删除某任务的临时分块目录。只在任务进入终态且不再需要续传时调用。
func (e *Engine) cleanupTemp(t *model.Transfer) {
	dir := filepath.Join(e.cfg.Get().TempDir, "transfers", t.ID)
	if err := os.RemoveAll(dir); err != nil {
		e.log.Debug("cleanup temp failed", "task", t.ID, "err", err)
	}
}

// CleanupTemp 扫描并删除过期临时数据，返回删除的文件数与释放的字节数。
// 只处理临时目录，绝不触碰接收目录中的用户文件。
func (e *Engine) CleanupTemp() (files int, freed int64, err error) {
	s := e.cfg.Get()
	root := filepath.Join(s.TempDir, "transfers")
	entries, derr := os.ReadDir(root)
	if derr != nil {
		if os.IsNotExist(derr) {
			return 0, 0, nil
		}
		return 0, 0, derr
	}
	cutoff := fsutil.NowMillis() - int64(s.TempRetentionHours)*3600*1000
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		taskID := ent.Name()
		dir := filepath.Join(root, taskID)
		t, lerr := e.st.GetTransfer(taskID)

		switch {
		case errors.Is(lerr, store.ErrNotFound):
			// 未登记的任务目录：可能是任务记录已被彻底删除，只按目录时间判断，
			// 避免刚创建但尚未登记的状态被立刻清掉。
			if st, serr := os.Stat(dir); serr == nil && st.ModTime().UnixMilli() > cutoff {
				continue
			}
		case lerr != nil:
			// 数据库异常时保守跳过，宁可留下临时文件也不误删。
			continue
		case t.Status.Active() || t.Status == model.StatusPaused || t.Status == model.StatusAwaiting:
			// 进行中、排队、暂停、等待确认的任务数据绝不触碰。
			continue
		case t.Status == model.StatusCompleted:
			// 已完成任务的分块都已提交，残留的只是空目录与日志，可立即回收。
		default:
			// 失败/取消/被拒：保留到「临时文件保留时间」到期，让用户还能重试续传。
			if t.UpdatedAt > cutoff {
				continue
			}
		}

		size := dirSize(dir)
		if rerr := os.RemoveAll(dir); rerr != nil {
			e.log.Debug("remove temp dir failed", "dir", dir, "err", rerr)
			continue
		}
		files++
		freed += size
	}
	return files, freed, nil
}

// CleanupOrphans 删除无对应任务记录的临时目录（例如任务已从历史中被彻底删除）。
func (e *Engine) CleanupOrphans() (int, error) {
	root := filepath.Join(e.cfg.Get().TempDir, "transfers")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		if _, lerr := e.st.GetTransfer(ent.Name()); errors.Is(lerr, store.ErrNotFound) {
			if rerr := os.RemoveAll(filepath.Join(root, ent.Name())); rerr == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// DirUsage 统计目录占用的真实字节数。
func DirUsage(dir string) (files int, bytes int64) {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			files++
			bytes += info.Size()
		}
		return nil
	})
	return
}

func dirSize(dir string) int64 {
	_, b := DirUsage(dir)
	return b
}

// StorageUsage 返回各存储目录的真实占用，用于设置页与诊断页。
func (e *Engine) StorageUsage() map[string]any {
	s := e.cfg.Get()
	rf, rb := DirUsage(s.ReceiveDir)
	tf, tb := DirUsage(filepath.Join(s.TempDir, "transfers"))
	free, total, err := fsutil.DiskSpace(s.ReceiveDir)
	out := map[string]any{
		"receiveDir":      s.ReceiveDir,
		"tempDir":         s.TempDir,
		"receiveFiles":    rf,
		"receiveBytes":    rb,
		"tempFiles":       tf,
		"tempBytes":       tb,
		"diskFreeBytes":   free,
		"diskTotalBytes":  total,
		"diskProbeFailed": err != nil,
	}
	return out
}

// ------------------------------------------------------------ 巡检 ---

// Sweep 立即执行一次巡检。用于测试与需要主动触发清理的调用方；
// 正常运行由 Start 启动的定时器调用。
func (e *Engine) Sweep() { e.sweep() }

// sweep 周期性任务：同步槽位、过期临时入口、回收僵死任务、清理临时数据、按保留期清理历史。
func (e *Engine) sweep() {
	e.SyncRuntimeConfig()
	s := e.cfg.Get()
	now := fsutil.NowMillis()

	e.reapStaleTasks(now)

	// 临时接收入口过期：后端真正失效，而不是仅隐藏前端按钮。
	if db, err := e.st.GetActiveDropbox(); err == nil && db != nil {
		if db.Enabled && db.ExpiresAt > 0 && db.ExpiresAt < now {
			db.Enabled = false
			if uerr := e.st.UpsertDropbox(db); uerr != nil {
				e.log.Error("expire dropbox failed", "err", uerr)
			} else {
				e.emit.Broadcast(protocol.Envelope{Type: protocol.EvDropboxChanged, TS: now})
			}
		}
	}

	if s.AutoCleanTemp {
		if files, freed, err := e.CleanupTemp(); err != nil {
			e.log.Error("cleanup temp failed", "err", err)
		} else if files > 0 {
			e.log.Info("cleaned temp data", "dirs", files, "freed", fsutil.HumanBytes(freed))
		}
	}
	if s.HistoryRetentionDays > 0 {
		cutoff := now - int64(s.HistoryRetentionDays)*24*3600*1000
		if n, err := e.st.PurgeHistoryBefore(cutoff); err != nil {
			e.log.Error("purge history failed", "err", err)
		} else if n > 0 {
			e.log.Info("purged history records", "n", n)
		}
	}
}

// staleAfter 判定「传输中」任务失去进展的阈值。
//
// 判定依据是 updated_at：每次分块落盘都会刷新它，因此它是真实的存活信号。
// 发送方关闭浏览器、断网或宕机后，任务会永远停在 uploading 并占着一个并发槽位，
// 最终把并发上限吃光（表现为所有新任务都排队且传不动）。定期回收是必要的。
const staleAfter = 10 * time.Minute

// reapStaleTasks 把长时间没有任何分块进展的「传输中」任务标记为失败并释放槽位。
//
// 刻意不处理 queued：排队任务本来就不会推进，且不占用槽位。
// 刻意不处理 awaiting：那是在等接收方确认，属于正常等待。
func (e *Engine) reapStaleTasks(now int64) {
	active, err := e.st.ListActiveTransfers()
	if err != nil {
		e.log.Error("reap: list active failed", "err", err)
		return
	}
	for _, t := range active {
		if t.Status != model.StatusUploading && t.Status != model.StatusVerifying {
			continue
		}
		if t.UpdatedAt == 0 || now-t.UpdatedAt < staleAfter.Milliseconds() {
			continue
		}
		idleMinutes := (now - t.UpdatedAt) / 60000
		t.Status = model.StatusFailed
		t.ErrorCode = protocol.CodeWriteFailed
		t.Error = "传输长时间没有进展，已释放传输通道"
		t.Resumable = true
		t.UpdatedAt = now
		if err := e.st.SaveTransfer(t); err != nil {
			e.log.Error("reap: save failed", "task", t.ID, "err", err)
			continue
		}
		e.releaseIfGranted(t.ID)
		e.log.Warn("reaped stale transfer", "task", t.ID, "idleMinutes", idleMinutes)
		e.emitBoth(t, protocol.EvTransferFailed, map[string]any{
			"code": protocol.CodeWriteFailed, "message": t.Error, "resumable": true,
		})
	}
}

// RecoverOnStart 处理服务重启后的残留状态。
//
// 关键原则：绝不在重启后把未完成的任务标记为成功。
// 正在上传/校验的任务无法在无客户端参与的情况下继续，因此标记为失败并保留
// 续传能力，由发送方决定是否重试；已完成的 ready 状态任务保持可用。
func (e *Engine) RecoverOnStart() error {
	// 进程刚启动：内存中的并发槽位必须是干净的，不能沿用任何残留记账。
	e.resetSlots()
	active, err := e.st.ListActiveTransfers()
	if err != nil {
		return err
	}
	now := fsutil.NowMillis()
	for _, t := range active {
		switch t.Status {
		case model.StatusCompleted, model.StatusFailed, model.StatusCancelled, model.StatusRejected:
			continue
		case model.StatusReady:
			continue
		case model.StatusUploading, model.StatusVerifying, model.StatusQueued:
			// 需要客户端参与的阶段：中断即视为失败，但允许续传。
			t.Status = model.StatusFailed
			t.ErrorCode = protocol.CodeWriteFailed
			t.Error = "服务重启，传输中断"
			t.Resumable = true
			t.UpdatedAt = now
			if err := e.st.SaveTransfer(t); err != nil {
				return err
			}
			e.log.Info("recovered interrupted transfer", "task", t.ID)
		case model.StatusAwaiting, model.StatusPaused:
			// 等待确认/暂停的任务可以原样保留，无需清理。
			continue
		}
	}
	// 清理孤儿临时目录。
	if n, err := e.CleanupOrphans(); err != nil {
		e.log.Error("cleanup orphans failed", "err", err)
	} else if n > 0 {
		e.log.Info("removed orphan temp dirs", "count", n)
	}
	return nil
}

// SortedMissing 便于测试与调试：返回排序后的缺失分块索引。
func SortedMissing(m map[int]bool, total int) []int {
	out := make([]int, 0)
	for i := 0; i < total; i++ {
		if !m[i] {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// ParseChunkIndex 解析路径中的分块索引并做范围检查。
func ParseChunkIndex(raw string) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, NewError(protocol.CodeInvalidChunkIndex, "chunk index: "+raw)
	}
	return v, nil
}
