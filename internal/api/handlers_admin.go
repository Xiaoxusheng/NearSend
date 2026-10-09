package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"nearsend/internal/fsutil"
	"nearsend/internal/model"
	"nearsend/internal/netinfo"
	"nearsend/internal/protocol"
	"nearsend/internal/transfer"
)

// ------------------------------------------------------------ 设置 ---

// handleGetConfig 返回影响客户端行为的公开配置子集。
//
// 这是页面加载后最先调用的接口之一：前端据此决定分块大小、并发上传数、
// 大小上限与默认重名策略。不含任何主机路径或监听信息。
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	cur := s.cfg.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"deviceName":             cur.DeviceName,
		"port":                   cur.Port,
		"chunkSize":              cur.ChunkSize,
		"maxConcurrentTransfers": cur.MaxConcurrent,
		"maxFileSize":            cur.MaxFileSize,
		"maxTaskSize":            cur.MaxTaskSize,
		"autoRetryCount":         cur.AutoRetry,
		"bandwidthLimit":         cur.BandwidthLimit,
		"defaultConflictPolicy":  cur.DefaultConflict,
		"allowTrustedAutoAccept": cur.AllowTrustedAutoAccept,
		"sessionTimeoutSec":      cur.SessionTimeoutSec,
		"dropboxTtlMinutes":      cur.DropboxTTLMinutes,
		"historyRetentionDays":   cur.HistoryRetentionDays,
		"version":                s.version,
		"selfDeviceId":           dev.ID,
		"selfDeviceName":         dev.Name,
		"privileged":             s.isPrivileged(dev),
	})
}

// handleGetSettings 返回有效配置。
//
// 非特权设备拿不到绝对路径与监听信息（这些属于主机信息），
// 但可以读到影响自身传输行为的参数（分块大小、大小上限等）。
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	canEdit := s.isPrivileged(dev)
	cur := s.cfg.Get()
	// 管理令牌永不进入任何响应体。
	cur.AdminToken = ""
	if !canEdit {
		cur.ReceiveDir = ""
		cur.TempDir = ""
		cur.ListenAddr = ""
		cur.DataDir = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":   cur,
		"canEdit":    canEdit,
		"deviceId":   dev.ID,
		"deviceName": dev.Name,
	})
}

// handlePatchSettings 更新设置。写入前完成全量校验，任一字段非法则整体不生效。
func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	var patch map[string]any
	if err := readJSON(w, r, &patch); err != nil {
		writeErr(w, s.log, err)
		return
	}
	needsRestart, err := s.cfg.Patch(patch)
	if err != nil {
		writeErr(w, s.log, transfer.NewError(protocol.CodeBadRequest, err.Error()))
		return
	}
	// 让传输引擎立即采用新的并发数与限速值。
	s.engine.SyncRuntimeConfig()
	s.hub.Broadcast(protocol.Envelope{
		Type: protocol.EvSettingsChanged, TS: fsutil.NowMillis(),
		Data: map[string]any{"needsRestart": needsRestart},
	})
	cur := s.cfg.Get()
	cur.NeedsRestart = needsRestart
	cur.AdminToken = ""
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": cur, "needsRestart": needsRestart,
		"message": messageForRestart(needsRestart),
	})
}

func messageForRestart(needs bool) string {
	if needs {
		return "设置已保存。监听地址或端口已变更，需要重启服务后生效。"
	}
	return "设置已保存并立即生效。"
}

// ------------------------------------------------------------ 历史记录 ---

// handleHistory 返回按文件展开的历史记录，直接对应记录页表格。
//
// 分页以「任务」为单位，因此同一任务的文件会出现在同一页，不会跨页割裂。
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	qy := model.HistoryQuery{
		Keyword:      strings.TrimSpace(r.URL.Query().Get("keyword")),
		From:         queryInt64(r, "from", 0),
		To:           queryInt64(r, "to", 0),
		Page:         queryInt(r, "page", 1, 1, 100000),
		PageSize:     queryInt(r, "pageSize", 20, 1, 200),
		Dir:          normalizeDir(r.URL.Query().Get("dir")),
		SelfDeviceID: dev.ID,
	}
	if raw := r.URL.Query().Get("status"); raw != "" {
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
	// 非特权设备只能看与自己相关的记录。
	scopeAll := r.URL.Query().Get("scope") == "all" && s.isPrivileged(dev)
	tasks, total, err := s.st.ListTransfers(qy)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	entries := []model.HistoryEntry{}
	for _, t := range tasks {
		if !scopeAll && t.SenderID != dev.ID && t.ReceiverID != dev.ID {
			continue
		}
		files, ferr := s.st.ListFiles(t.ID)
		if ferr != nil {
			continue
		}
		dir := "out"
		if t.ReceiverID == dev.ID {
			dir = "in"
		}
		for _, f := range files {
			entries = append(entries, model.HistoryEntry{
				TaskID: t.ID, FileID: f.ID, FileName: f.Name, Direction: dir,
				SenderName: t.SenderName, ReceiverName: t.ReceiverName, Size: f.Size,
				Status: t.Status, FileStatus: f.Status, StartedAt: t.CreatedAt,
				CompletedAt: t.CompletedAt, Error: t.Error, ErrorCode: t.ErrorCode,
				Verified:   t.Verified && (f.Status == model.FileReady || f.Status == model.FileDownloaded),
				TaskStatus: t.Status,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries, "total": total, "page": qy.Page, "pageSize": qy.PageSize,
	})
}

// handleClearHistory 清除历史记录。
//
// 关键语义：只把记录标记为不可见，绝不删除磁盘上的任何文件。
func (s *Server) handleClearHistory(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.ClearHistory()
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cleared": n,
		"message": "已清除历史记录。接收目录中的文件未被删除。",
	})
}

// ------------------------------------------------------------ 存储 ---

// handleStorage 返回真实存储占用。
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	usage := s.engine.StorageUsage()
	stats, err := s.st.Stats()
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"usage": usage, "stats": stats,
	})
}

// handleCleanupStorage 清理过期临时数据。不动接收目录中的用户文件。
func (s *Server) handleCleanupStorage(w http.ResponseWriter, r *http.Request) {
	dirs, freed, err := s.engine.CleanupTemp()
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	orphans, _ := s.engine.CleanupOrphans()
	writeJSON(w, http.StatusOK, map[string]any{
		"removedDirs": dirs + orphans,
		"freedBytes":  freed,
		"message":     "已清理临时文件。接收目录中的文件未被删除。",
	})
}

// ------------------------------------------------------------ 诊断 ---

// handleDiagnostics 返回全部检测项及其检测时间。
// 每一项都是刚执行的真实探测结果，不存在预设的「成功」状态。
func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	cfg := s.cfg.Get()
	now := fsutil.NowMillis()

	conns, onlineDevices := s.hub.ConnectionCount()
	selfTestOK, selfTestErr := netinfo.SelfTest("127.0.0.1", cfg.Port)

	// 端口占用：服务自身在监听，用自检结果判断监听是否真正可用。
	portFree := netinfo.IsPortFree(cfg.ListenAddr, cfg.Port)

	stats, _ := s.st.Stats()
	usage := s.engine.StorageUsage()

	// 最近一次失败：从真实任务记录中取，不伪造。
	var recentError any
	if tasks, _, err := s.st.ListTransfers(model.HistoryQuery{
		Status: []model.TransferStatus{model.StatusFailed}, Page: 1, PageSize: 1,
	}); err == nil && len(tasks) > 0 {
		t := tasks[0]
		recentError = map[string]any{
			"taskId": t.ID, "code": t.ErrorCode, "message": t.Error,
			"at": t.UpdatedAt, "senderName": t.SenderName, "receiverName": t.ReceiverName,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"checkedAt": now,
		"service": map[string]any{
			"status":     "ok",
			"version":    s.version,
			"goVersion":  osArch(),
			"uptimeSec":  int64(time.Since(s.startedAt).Seconds()),
			"listenAddr": cfg.ListenAddr,
			"port":       cfg.Port,
			"selfTest":   map[string]any{"ok": selfTestOK, "error": selfTestErr},
			"portFree":   portFree,
			"portNote":   netinfo.DescribePortUse(cfg.ListenAddr, cfg.Port),
		},
		"addresses": s.accessInfo()["addresses"],
		"client": map[string]any{
			"ip": clientIP(r), "deviceId": dev.ID, "deviceName": dev.Name,
			"os": dev.OS, "browser": dev.Browser,
			"wsOnline":      s.hub.IsOnline(dev.ID),
			"wsConnections": conns, "wsDevices": onlineDevices,
		},
		"transfers": map[string]any{
			"active": stats["active"], "failed": stats["failed"],
			"completed": stats["completed"], "tasks": stats["tasks"],
		},
		"storage":     usage,
		"recentError": recentError,
		"tips":        netinfo.Tips(cfg.Port, true, selfTestOK),
	})
}

// ------------------------------------------------------------ 临时接收箱 ---

// handleGetDropbox 返回当前临时入口状态。
// 即使从未启用过也返回一个「未启用」的结构，前端无需处理 404。
func (s *Server) handleGetDropbox(w http.ResponseWriter, r *http.Request) {
	cur, err := s.st.GetActiveDropbox()
	now := fsutil.NowMillis()
	if err != nil || cur == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false, "exists": false, "now": now,
			"ttlMinutes": s.cfg.Get().DropboxTTLMinutes,
		})
		return
	}
	enabled := cur.Enabled && cur.ExpiresAt > now
	writeJSON(w, http.StatusOK, map[string]any{
		"exists":        true,
		"enabled":       enabled,
		"id":            cur.ID,
		"token":         cur.Token,
		"createdAt":     cur.CreatedAt,
		"expiresAt":     cur.ExpiresAt,
		"remainingMs":   max64(cur.ExpiresAt-now, 0),
		"receivedCount": cur.ReceivedCount,
		"receivedBytes": cur.ReceivedBytes,
		"uploadUrl":     "/drop?token=" + cur.Token,
		"fullUrl":       netinfo.PrimaryURL(s.cfg.Get().Port) + "/drop?token=" + cur.Token,
		"now":           now,
		"ttlMinutes":    s.cfg.Get().DropboxTTLMinutes,
	})
}

// handleEnableDropbox 启用（或重新签发）临时接收入口。
func (s *Server) handleEnableDropbox(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	var body struct {
		TTLMinutes int `json:"ttlMinutes"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(w, r, &body); err != nil {
			writeErr(w, s.log, err)
			return
		}
	}
	ttl := body.TTLMinutes
	if ttl <= 0 {
		ttl = s.cfg.Get().DropboxTTLMinutes
	}
	if ttl < 1 || ttl > 1440 {
		writeErr(w, s.log, transfer.NewError(protocol.CodeBadRequest, "有效期需在 1 到 1440 分钟之间"))
		return
	}
	now := fsutil.NowMillis()
	db := &model.Dropbox{
		ID:        uuid.NewString(),
		Token:     newToken(),
		Enabled:   true,
		CreatedAt: now,
		ExpiresAt: now + int64(ttl)*60*1000,
		CreatedBy: dev.ID,
	}
	if err := s.st.UpsertDropbox(db); err != nil {
		writeErr(w, s.log, err)
		return
	}
	s.hub.Broadcast(protocol.Envelope{Type: protocol.EvDropboxChanged, TS: now})
	writeJSON(w, http.StatusOK, map[string]any{
		"exists": true, "enabled": true, "id": db.ID, "token": db.Token,
		"createdAt": db.CreatedAt, "expiresAt": db.ExpiresAt,
		"remainingMs": int64(ttl) * 60 * 1000, "now": now,
		"uploadUrl": "/drop?token=" + db.Token,
		"fullUrl":   netinfo.PrimaryURL(s.cfg.Get().Port) + "/drop?token=" + db.Token,
		"message":   "临时接收入口已开启，过期后自动失效。",
	})
}

// handleDisableDropbox 关闭临时接收入口。关闭后后端立即拒绝该令牌的一切请求。
func (s *Server) handleDisableDropbox(w http.ResponseWriter, r *http.Request) {
	cur, err := s.st.GetActiveDropbox()
	now := fsutil.NowMillis()
	if err != nil || cur == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "exists": false, "now": now})
		return
	}
	cur.Enabled = false
	// 立即过期，而不是等待自然到期。
	cur.ExpiresAt = now
	if err := s.st.UpsertDropbox(cur); err != nil {
		writeErr(w, s.log, err)
		return
	}
	s.hub.Broadcast(protocol.Envelope{Type: protocol.EvDropboxChanged, TS: now})
	writeJSON(w, http.StatusOK, map[string]any{
		"exists": true, "enabled": false, "now": now,
		"message": "临时接收入口已关闭。",
	})
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
