package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nearsend/internal/fsutil"
	"nearsend/internal/model"
	"nearsend/internal/protocol"
	"nearsend/internal/transfer"
)

// ------------------------------------------------------------ 创建与查询 ---

type createTransferReq struct {
	ReceiverIDs  []string             `json:"receiverIds"`
	Files        []transfer.FileSpec  `json:"files"`
	Note         string               `json:"note"`
	Conflict     model.ConflictPolicy `json:"conflict"`
	DropboxToken string               `json:"dropboxToken"`
}

// handleCreateTransfer 创建传输请求（一个接收方对应一个任务）。
func (s *Server) handleCreateTransfer(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	var body createTransferReq
	if err := readJSON(w, r, &body); err != nil {
		writeErr(w, s.log, err)
		return
	}
	// 临时接收入口场景下 senders 是匿名会话，receiverIds 可以为空，
	// 此时自动指向接收目录的所有者。
	if body.DropboxToken != "" && len(body.ReceiverIDs) == 0 {
		db, err := s.st.GetActiveDropbox()
		if err != nil || !db.Enabled || db.Token != body.DropboxToken || db.ExpiresAt < fsutil.NowMillis() {
			writeErr(w, s.log, transfer.NewError(protocol.CodeDropboxExpired, "dropbox invalid"))
			return
		}
		owner, oerr := s.st.GetDevice(db.CreatedBy)
		if oerr != nil {
			writeErr(w, s.log, transfer.NewError(protocol.CodeNotFound, "dropbox owner gone"))
			return
		}
		body.ReceiverIDs = []string{owner.ID}
	}
	tasks, err := s.engine.Create(transfer.CreateRequest{
		Sender:       dev,
		ReceiverIDs:  body.ReceiverIDs,
		Files:        body.Files,
		Note:         body.Note,
		Conflict:     body.Conflict,
		DropboxToken: body.DropboxToken,
	})
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	views := make([]any, 0, len(tasks))
	for _, t := range tasks {
		v, verr := s.engine.View(t.ID)
		if verr == nil {
			views = append(views, v)
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tasks": views})
}

// handleListTransfers 查询任务列表。
func (s *Server) handleListTransfers(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	q := r.URL.Query()

	qy := model.HistoryQuery{
		Keyword:  strings.TrimSpace(q.Get("keyword")),
		From:     queryInt64(r, "from", 0),
		To:       queryInt64(r, "to", 0),
		Page:     queryInt(r, "page", 1, 1, 100000),
		PageSize: queryInt(r, "pageSize", 50, 1, 200),
		Dir:      normalizeDir(q.Get("dir")),
	}
	if raw := q.Get("status"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !validStatus(part) {
				writeErr(w, s.log, transfer.NewError(protocol.CodeBadRequest, "invalid status: "+part))
				return
			}
			qy.Status = append(qy.Status, model.TransferStatus(part))
		}
	}
	// scope=involved 只返回与当前设备相关的任务；这是默认视图，
	// 避免局域网内所有设备互相看到对方的传输内容。
	scope := q.Get("scope")
	tasks, total, err := s.st.ListTransfers(qy)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	out := make([]any, 0, len(tasks))
	for _, t := range tasks {
		if scope == "all" && !s.isPrivileged(dev) {
			scope = "involved" // 非特权设备无法查看全局任务
		}
		if scope != "all" && t.SenderID != dev.ID && t.ReceiverID != dev.ID {
			continue
		}
		v, verr := s.engine.View(t.ID)
		if verr != nil {
			continue
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": out, "total": total, "page": qy.Page, "pageSize": qy.PageSize,
	})
}

func normalizeDir(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "in", "receive":
		return "in"
	case "out", "send":
		return "out"
	default:
		return "all"
	}
}

func validStatus(s string) bool {
	switch model.TransferStatus(s) {
	case model.StatusAwaiting, model.StatusRejected, model.StatusQueued, model.StatusUploading,
		model.StatusPaused, model.StatusVerifying, model.StatusReady, model.StatusCompleted,
		model.StatusFailed, model.StatusCancelled:
		return true
	}
	return false
}

// handleGetTransfer 查看任务详情。
func (s *Server) handleGetTransfer(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	v, err := s.engine.View(r.PathValue("id"))
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	// 只有任务参与者或特权设备可以查看详情。
	if v.SenderID != dev.ID && v.ReceiverID != dev.ID && !s.isPrivileged(dev) {
		writeErr(w, s.log, transfer.NewError(protocol.CodeForbidden, "not a participant"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": v})
}

// ------------------------------------------------------------ 状态迁移 ---

func (s *Server) handleAccept(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	var body struct {
		Conflict model.ConflictPolicy `json:"conflict"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(w, r, &body); err != nil {
			writeErr(w, s.log, err)
			return
		}
	}
	t, err := s.engine.Accept(r.PathValue("id"), dev, body.Conflict)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	s.respondTask(w, t.ID)
}

func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(w, r, &body); err != nil {
			writeErr(w, s.log, err)
			return
		}
	}
	t, err := s.engine.Reject(r.PathValue("id"), dev, body.Reason)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	s.respondTask(w, t.ID)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	t, err := s.engine.Cancel(r.PathValue("id"), dev)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	s.respondTask(w, t.ID)
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	t, err := s.engine.Retry(r.PathValue("id"), dev)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	s.respondTask(w, t.ID)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	t, err := s.engine.Pause(r.PathValue("id"), dev)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	s.respondTask(w, t.ID)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	t, err := s.engine.Resume(r.PathValue("id"), dev)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	s.respondTask(w, t.ID)
}

func (s *Server) respondTask(w http.ResponseWriter, taskID string) {
	v, err := s.engine.View(taskID)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": v})
}

// ------------------------------------------------------------ 分块 ---

// handleChunkStatus 返回分块与恢复状态，供发送方计算断点续传。
func (s *Server) handleChunkStatus(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	st, err := s.engine.Chunks(r.PathValue("id"), dev)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// maxChunkOverhead 允许分块体积在期望值之上的额外余量（协议开销与探测字节）。
const maxChunkOverhead = 4096

// handleUploadChunk 接收单个分块。
//
// 请求体是原始二进制，SHA-256 通过 X-Chunk-SHA256 头声明（可选但推荐）。
// 服务端在写入临时文件之前完成校验，因此校验失败的字节永远不会落盘。
func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	taskID := r.PathValue("id")
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 {
		writeErr(w, s.log, transfer.NewError(protocol.CodeBadRequest, "invalid file index"))
		return
	}
	chunk, err := transfer.ParseChunkIndex(r.PathValue("chunk"))
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	chunkSize := s.cfg.Get().ChunkSize
	r.Body = http.MaxBytesReader(w, r.Body, chunkSize+maxChunkOverhead)
	res, err := s.engine.WriteChunk(r.Context(), taskID, dev, index, chunk, r.Body,
		r.Header.Get("X-Chunk-SHA256"))
	if err != nil {
		// 关键：在返回错误前把请求体读干净。
		//
		// 分块请求体最大可达数十 MiB，服务端的失败判定（暂停、状态非法、无权限等）
		// 发生在读取 body 之前。若此时直接写响应，客户端可能仍在发送数据，
		// 会收到「连接被重置」而不是一个带错误码的响应——浏览器就只能显示
		// 一个无法解释的网络错误。丢弃 body 让连接可以正常复用并返回结构化的错误。
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, chunkSize+maxChunkOverhead+1))
		writeErr(w, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleResetFile 清空某文件的已收分块（源文件发生变化时使用）。
func (s *Server) handleResetFile(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 {
		writeErr(w, s.log, transfer.NewError(protocol.CodeBadRequest, "invalid file index"))
		return
	}
	if err := s.engine.ResetFile(r.PathValue("id"), index, dev); err != nil {
		writeErr(w, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reset": index})
}

// ------------------------------------------------------------ 下载 ---

// handleCreateTicket 为某个文件签发短期下载票据。
// 只有接收方可以取走文件；发送方与旁观者都会在此被拒绝。
func (s *Server) handleCreateTicket(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	taskID := r.PathValue("id")
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 {
		writeErr(w, s.log, transfer.NewError(protocol.CodeBadRequest, "invalid file index"))
		return
	}
	v, verr := s.engine.View(taskID)
	if verr != nil {
		writeErr(w, s.log, verr)
		return
	}
	// 接收方下载，或特权设备（服务所在机器）代取。
	if v.ReceiverID != dev.ID && !s.isPrivileged(dev) {
		writeErr(w, s.log, transfer.NewError(protocol.CodeForbidden, "only receiver can download"))
		return
	}
	_, f, _, ferr := s.engine.FilePath(taskID, index)
	if ferr != nil {
		writeErr(w, s.log, ferr)
		return
	}
	id := s.tickets.issue(ticket{
		TaskID: taskID, FileIndex: index, DeviceID: dev.ID, Name: f.Name,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":    id,
		"url":       "/api/download?ticket=" + id,
		"name":      f.Name,
		"size":      f.Size,
		"expiresIn": int(ticketTTL.Seconds()),
	})
}

// handleDownloadByTicket 依据票据流式下发文件。
//
// 使用 http.ServeContent，因此原生支持 Range 请求与断点续传下载。
// 响应头显式设置 Content-Disposition: attachment，浏览器走原生下载而非内存缓冲。
func (s *Server) handleDownloadByTicket(w http.ResponseWriter, r *http.Request) {
	tk, ok := s.tickets.get(strings.TrimSpace(r.URL.Query().Get("ticket")))
	if !ok {
		writeErr(w, s.log, transfer.NewError(protocol.CodeForbidden, "invalid or expired ticket"))
		return
	}
	path, f, t, err := s.engine.FilePath(tk.TaskID, tk.FileIndex)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	fh, err := os.Open(path)
	if err != nil {
		writeErr(w, s.log, transfer.NewError(protocol.CodeNotFound, "stored file unreadable"))
		return
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		writeErr(w, s.log, err)
		return
	}

	displayName := f.FinalName
	if displayName == "" {
		displayName = f.Name
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition(displayName))
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("Cache-Control", "no-store")
	// 显式暴露头部，便于前端展示真实文件名与大小（Range 场景下需要）。
	w.Header().Set("Accept-Ranges", "bytes")

	// 统计真实写出的字节数：只有整份文件被完整送出时才记「已下载」，
	// 中途断开或分段下载不会被误记为成功。
	cw := &countingWriter{ResponseWriter: w}
	isRange := r.Header.Get("Range") != ""
	http.ServeContent(cw, r, displayName, st.ModTime(), fh)

	if !isRange && cw.written >= st.Size() && !t.DownloadDone {
		dev, derr := s.st.GetDevice(tk.DeviceID)
		if derr == nil {
			if merr := s.engine.MarkDownloaded(tk.TaskID, tk.FileIndex, dev); merr != nil {
				s.log.Debug("mark downloaded failed", "task", tk.TaskID, "err", merr)
			}
		}
	}
}

type countingWriter struct {
	http.ResponseWriter
	written int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.written += int64(n)
	return n, err
}

// contentDisposition 生成兼容中文文件名的下载头。
// 同时提供 ASCII 回退名与 RFC 5987 的 UTF-8 名，避免中文名在旧浏览器下丢失。
func contentDisposition(name string) string {
	safe := strings.ReplaceAll(name, "\"", "'")
	ascii := make([]rune, 0, len(safe))
	for _, c := range safe {
		if c < 128 && c != '\\' {
			ascii = append(ascii, c)
		} else {
			ascii = append(ascii, '_')
		}
	}
	fallback := strings.TrimSpace(string(ascii))
	if fallback == "" {
		fallback = "download"
	}
	return "attachment; filename=\"" + fallback + "\"; filename*=UTF-8''" + urlEncode(safe)
}

// urlEncode 按 RFC 5987 对文件名编码。
func urlEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// handleReveal 在服务所在机器的文件管理器中定位已接收的文件。
//
// 只在请求来自本机回环地址时允许：这台机器就是存文件的机器，
// 让用户直接看到文件落在哪是合理的；远程设备不触发任何本地进程。
func (s *Server) handleReveal(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		writeErr(w, s.log, transfer.NewError(protocol.CodeForbidden, "only available on the host machine"))
		return
	}
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 {
		writeErr(w, s.log, transfer.NewError(protocol.CodeBadRequest, "invalid file index"))
		return
	}
	path, _, _, ferr := s.engine.FilePath(r.PathValue("id"), index)
	if ferr != nil {
		writeErr(w, s.log, ferr)
		return
	}
	if err := revealInFileManager(path); err != nil {
		writeErr(w, s.log, transfer.NewError(protocol.CodeWriteFailed, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revealed": filepath.Base(path)})
}

// readBodyLimited 读取受限的请求体，用于需要原始字节的小接口。
func readBodyLimited(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, transfer.NewError(protocol.CodeBadRequest, "empty body")
	}
	if limit <= 0 {
		limit = 1 << 20
	}
	return io.ReadAll(io.LimitReader(r.Body, limit+1))
}

// parseTimeParam 解析毫秒时间戳查询参数。
func parseTimeParam(raw string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// since 用于诊断页展示的耗时文本。
func since(t time.Time) string {
	return time.Since(t).Round(time.Second).String()
}
