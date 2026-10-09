// Package store 封装 SQLite 持久化。数据库只保存元数据（设备、任务、分块状态、
// 记录、配置），文件正文一律存放在文件系统。
//
// 连接策略：SetMaxOpenConns(1) + WAL + busy_timeout。所有数据库操作都是
// 微秒级的短事务，串行化换取了「绝不出现 SQLITE_BUSY」的确定性；真正的
// 长时间 I/O（分块读写、下载流式响应）全部在连接之外完成。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"nearsend/internal/model"
)

// ErrNotFound 统一的「记录不存在」错误。
var ErrNotFound = errors.New("record not found")

// Store 是 SQLite 数据访问层。
type Store struct {
	db *sql.DB
	mu sync.Mutex // 序列化多步组合写操作，保证状态迁移一致性
}

const schemaVersion = 1

// migrations 按顺序执行，index+1 即目标版本号。新增变更只允许追加，
// 不允许修改已发布的语句。
var migrations = []string{
	// v1: 初始结构
	`
CREATE TABLE devices (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  os            TEXT NOT NULL DEFAULT '',
  browser       TEXT NOT NULL DEFAULT '',
  type          TEXT NOT NULL DEFAULT 'unknown',
  trusted       INTEGER NOT NULL DEFAULT 0,
  first_seen    INTEGER NOT NULL DEFAULT 0,
  last_seen     INTEGER NOT NULL DEFAULT 0,
  connected_at  INTEGER NOT NULL DEFAULT 0,
  last_ip       TEXT NOT NULL DEFAULT '',
  user_agent    TEXT NOT NULL DEFAULT '',
  token         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_devices_last_seen ON devices(last_seen DESC);

CREATE TABLE transfers (
  id            TEXT PRIMARY KEY,
  sender_id     TEXT NOT NULL,
  sender_name   TEXT NOT NULL DEFAULT '',
  receiver_id   TEXT NOT NULL,
  receiver_name TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL,
  note          TEXT NOT NULL DEFAULT '',
  total_files   INTEGER NOT NULL DEFAULT 0,
  total_size    INTEGER NOT NULL DEFAULT 0,
  done_size     INTEGER NOT NULL DEFAULT 0,
  chunk_size    INTEGER NOT NULL DEFAULT 0,
  conflict      TEXT NOT NULL DEFAULT 'rename',
  created_at    INTEGER NOT NULL DEFAULT 0,
  accepted_at   INTEGER NOT NULL DEFAULT 0,
  started_at    INTEGER NOT NULL DEFAULT 0,
  completed_at  INTEGER NOT NULL DEFAULT 0,
  updated_at    INTEGER NOT NULL DEFAULT 0,
  error         TEXT NOT NULL DEFAULT '',
  error_code    TEXT NOT NULL DEFAULT '',
  verified      INTEGER NOT NULL DEFAULT 0,
  resumable     INTEGER NOT NULL DEFAULT 1,
  non_resumable_reason TEXT NOT NULL DEFAULT '',
  retries       INTEGER NOT NULL DEFAULT 0,
  upload_done   INTEGER NOT NULL DEFAULT 0,
  download_done INTEGER NOT NULL DEFAULT 0,
  history_visible INTEGER NOT NULL DEFAULT 1,
  expires_at    INTEGER NOT NULL DEFAULT 0,
  dropbox_id    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_transfers_created ON transfers(created_at DESC);
CREATE INDEX idx_transfers_status ON transfers(status);
CREATE INDEX idx_transfers_receiver ON transfers(receiver_id);

CREATE TABLE files (
  id            TEXT PRIMARY KEY,
  task_id       TEXT NOT NULL REFERENCES transfers(id) ON DELETE CASCADE,
  idx           INTEGER NOT NULL,
  name          TEXT NOT NULL,
  rel_path      TEXT NOT NULL DEFAULT '',
  size          INTEGER NOT NULL DEFAULT 0,
  mime          TEXT NOT NULL DEFAULT '',
  src_mod_time  INTEGER NOT NULL DEFAULT 0,
  sha256        TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL DEFAULT 'pending',
  done_bytes    INTEGER NOT NULL DEFAULT 0,
  chunk_size    INTEGER NOT NULL DEFAULT 0,
  chunk_count   INTEGER NOT NULL DEFAULT 0,
  received_chunks INTEGER NOT NULL DEFAULT 0,
  error         TEXT NOT NULL DEFAULT '',
  error_code    TEXT NOT NULL DEFAULT '',
  skipped       INTEGER NOT NULL DEFAULT 0,
  downloaded_at INTEGER NOT NULL DEFAULT 0,
  final_name    TEXT NOT NULL DEFAULT '',
  temp_path     TEXT NOT NULL DEFAULT '',
  final_path    TEXT NOT NULL DEFAULT '',
  UNIQUE(task_id, idx)
);
CREATE INDEX idx_files_task ON files(task_id, idx);

CREATE TABLE chunks (
  file_id     TEXT NOT NULL,
  task_id     TEXT NOT NULL,
  idx         INTEGER NOT NULL,
  size        INTEGER NOT NULL DEFAULT 0,
  sha256      TEXT NOT NULL DEFAULT '',
  received_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (file_id, idx)
);
CREATE INDEX idx_chunks_task ON chunks(task_id);

CREATE TABLE dropboxes (
  id             TEXT PRIMARY KEY,
  token          TEXT NOT NULL,
  enabled        INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL DEFAULT 0,
  expires_at     INTEGER NOT NULL DEFAULT 0,
  created_by     TEXT NOT NULL DEFAULT '',
  received_count INTEGER NOT NULL DEFAULT 0,
  received_bytes INTEGER NOT NULL DEFAULT 0,
  upload_count   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE app_settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`,
}

// Open 打开（必要时创建）数据库并执行迁移。
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	var cur int
	row := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`)
	var raw string
	if err := row.Scan(&raw); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		cur = 0
	} else {
		fmt.Sscanf(raw, "%d", &cur)
	}
	for i := cur; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d failed: %w", i+1, err)
		}
		ver := i + 1
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES('schema_version', ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, fmt.Sprint(ver)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// ---------------------------------------------------------------- devices ---

// UpsertDevice 写入或更新设备记录。会话令牌不覆盖已有非空值，
// 避免设备重连时丢失已建立的信任关系。
func (s *Store) UpsertDevice(d *model.Device) error {
	_, err := s.db.Exec(`
INSERT INTO devices(id, name, os, browser, type, trusted, first_seen, last_seen, connected_at, last_ip, user_agent, token)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  name = excluded.name,
  os = excluded.os,
  browser = excluded.browser,
  type = excluded.type,
  last_seen = excluded.last_seen,
  connected_at = excluded.connected_at,
  last_ip = excluded.last_ip,
  user_agent = excluded.user_agent,
  token = CASE WHEN excluded.token != '' THEN excluded.token ELSE devices.token END`,
		d.ID, d.Name, d.OS, d.Browser, string(d.Type), b2i(d.Trusted),
		d.FirstSeen, d.LastSeen, d.ConnectedAt, d.LastIP, d.UserAgent, d.Token)
	return err
}

// TouchDevice 仅更新最后活跃时间。
func (s *Store) TouchDevice(id string, ts int64) error {
	_, err := s.db.Exec(`UPDATE devices SET last_seen = ? WHERE id = ?`, ts, id)
	return err
}

// SetDeviceName 重命名设备。
func (s *Store) SetDeviceName(id, name string) error {
	res, err := s.db.Exec(`UPDATE devices SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// SetDeviceTrusted 设置信任标记。信任关系带有签发时间，令牌本身有时效。
func (s *Store) SetDeviceTrusted(id string, trusted bool) error {
	res, err := s.db.Exec(`UPDATE devices SET trusted = ? WHERE id = ?`, b2i(trusted), id)
	if err != nil {
		return err
	}
	return mustAffect(res)
}

// SetDeviceToken 更新会话令牌（重新配对时使用）。
func (s *Store) SetDeviceToken(id, token string) error {
	_, err := s.db.Exec(`UPDATE devices SET token = ? WHERE id = ?`, token, id)
	return err
}

// DeleteDevice 删除设备记录，同时撤销其信任关系。
func (s *Store) DeleteDevice(id string) error {
	_, err := s.db.Exec(`DELETE FROM devices WHERE id = ?`, id)
	return err
}

// GetDevice 读取单个设备。
func (s *Store) GetDevice(id string) (*model.Device, error) {
	row := s.db.QueryRow(deviceCols+` WHERE id = ?`, id)
	d, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

// FindDeviceByToken 通过会话令牌反查设备，用于鉴权。
func (s *Store) FindDeviceByToken(token string) (*model.Device, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRow(deviceCols+` WHERE token = ?`, token)
	d, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

const deviceCols = `SELECT id, name, os, browser, type, trusted, first_seen, last_seen,
	connected_at, last_ip, user_agent, token FROM devices`

// ListDevices 返回全部已知设备，按最近活跃时间倒序（最近使用设备优先展示）。
func (s *Store) ListDevices() ([]*model.Device, error) {
	rows, err := s.db.Query(deviceCols + ` ORDER BY trusted DESC, last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanDevice(sc scanner) (*model.Device, error) {
	var d model.Device
	var typ string
	var trusted int
	err := sc.Scan(&d.ID, &d.Name, &d.OS, &d.Browser, &typ, &trusted, &d.FirstSeen,
		&d.LastSeen, &d.ConnectedAt, &d.LastIP, &d.UserAgent, &d.Token)
	if err != nil {
		return nil, err
	}
	d.Type = model.DeviceType(typ)
	d.Trusted = trusted == 1
	return &d, nil
}

// -------------------------------------------------------------- transfers ---

const transferCols = `SELECT id, sender_id, sender_name, receiver_id, receiver_name, status, note,
	total_files, total_size, done_size, chunk_size, conflict, created_at, accepted_at, started_at,
	completed_at, updated_at, error, error_code, verified, resumable, non_resumable_reason,
	retries, upload_done, download_done, history_visible, expires_at, dropbox_id FROM transfers`

// CreateTransfer 在单个事务中写入任务与全部文件行，避免出现半截任务。
func (s *Store) CreateTransfer(t *model.Transfer, files []*model.TransferFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO transfers(id, sender_id, sender_name, receiver_id, receiver_name,
		status, note, total_files, total_size, done_size, chunk_size, conflict, created_at, accepted_at,
		started_at, completed_at, updated_at, error, error_code, verified, resumable, non_resumable_reason,
		retries, upload_done, download_done, history_visible, expires_at, dropbox_id)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.SenderID, t.SenderName, t.ReceiverID, t.ReceiverName, string(t.Status), t.Note,
		t.TotalFiles, t.TotalSize, t.DoneSize, t.ChunkSize, string(t.Conflict), t.CreatedAt, t.AcceptedAt,
		t.StartedAt, t.CompletedAt, t.UpdatedAt, t.Error, t.ErrorCode, b2i(t.Verified), b2i(t.Resumable),
		t.NonResumableReason, t.Retries, b2i(t.UploadDone), b2i(t.DownloadDone), b2i(t.HistoryVisible),
		t.ExpiresAt, dropboxIDOf(t)); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.Exec(`INSERT INTO files(id, task_id, idx, name, rel_path, size, mime, src_mod_time,
			sha256, status, done_bytes, chunk_size, chunk_count, received_chunks, error, error_code, skipped,
			downloaded_at, final_name, temp_path, final_path)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			f.ID, t.ID, f.Index, f.Name, f.RelPath, f.Size, f.Mime, f.SrcModTime, f.SHA256,
			string(f.Status), f.DoneBytes, f.ChunkSize, f.ChunkCount, f.ReceivedChunks, f.Error,
			f.ErrorCode, b2i(f.Skipped), f.DownloadedAt, f.FinalName, "", ""); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveTransfer 全量更新任务行（不含文件）。
func (s *Store) SaveTransfer(t *model.Transfer) error {
	_, err := s.db.Exec(`UPDATE transfers SET status=?, note=?, done_size=?, conflict=?, accepted_at=?,
		started_at=?, completed_at=?, updated_at=?, error=?, error_code=?, verified=?, resumable=?,
		non_resumable_reason=?, retries=?, upload_done=?, download_done=?, history_visible=?, expires_at=?,
		sender_name=?, receiver_name=? WHERE id=?`,
		string(t.Status), t.Note, t.DoneSize, string(t.Conflict), t.AcceptedAt, t.StartedAt, t.CompletedAt,
		t.UpdatedAt, t.Error, t.ErrorCode, b2i(t.Verified), b2i(t.Resumable), t.NonResumableReason,
		t.Retries, b2i(t.UploadDone), b2i(t.DownloadDone), b2i(t.HistoryVisible), t.ExpiresAt,
		t.SenderName, t.ReceiverName, t.ID)
	return err
}

// UpdateTransferState 是轻量状态迁移，供传输引擎在热点路径调用。
func (s *Store) UpdateTransferState(id string, status model.TransferStatus, doneSize int64,
	errorCode, errMsg string, updatedAt int64) error {
	_, err := s.db.Exec(`UPDATE transfers SET status=?, done_size=?, error_code=?, error=?, updated_at=? WHERE id=?`,
		string(status), doneSize, errorCode, errMsg, updatedAt, id)
	return err
}

// GetTransfer 读取任务及其全部文件。
func (s *Store) GetTransfer(id string) (*model.Transfer, error) {
	row := s.db.QueryRow(transferCols+` WHERE id = ?`, id)
	t, err := scanTransfer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	files, err := s.ListFiles(id)
	if err != nil {
		return nil, err
	}
	t.Files = files
	return t, nil
}

// ListFiles 列出任务下的文件，按索引升序。
func (s *Store) ListFiles(taskID string) ([]*model.TransferFile, error) {
	rows, err := s.db.Query(`SELECT id, task_id, idx, name, rel_path, size, mime, src_mod_time, sha256,
		status, done_bytes, chunk_size, chunk_count, received_chunks, error, error_code, skipped,
		downloaded_at, final_name FROM files WHERE task_id = ? ORDER BY idx ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.TransferFile{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFile 按索引读取单个文件。
func (s *Store) GetFile(taskID string, index int) (*model.TransferFile, error) {
	row := s.db.QueryRow(`SELECT id, task_id, idx, name, rel_path, size, mime, src_mod_time, sha256,
		status, done_bytes, chunk_size, chunk_count, received_chunks, error, error_code, skipped,
		downloaded_at, final_name FROM files WHERE task_id = ? AND idx = ?`, taskID, index)
	f, err := scanFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

// UpdateFile 更新单个文件的状态字段。
func (s *Store) UpdateFile(f *model.TransferFile) error {
	_, err := s.db.Exec(`UPDATE files SET status=?, done_bytes=?, received_chunks=?, error=?, error_code=?,
		skipped=?, downloaded_at=?, final_name=?, sha256=? WHERE id=?`,
		string(f.Status), f.DoneBytes, f.ReceivedChunks, f.Error, f.ErrorCode, b2i(f.Skipped),
		f.DownloadedAt, f.FinalName, f.SHA256, f.ID)
	return err
}

// ListTransfers 按条件查询任务（不含文件明细），用于列表与历史。
func (s *Store) ListTransfers(q model.HistoryQuery) ([]*model.Transfer, int, error) {
	where := []string{"history_visible = 1"}
	args := []any{}
	if q.Keyword != "" {
		where = append(where, `(id LIKE ? OR sender_name LIKE ? OR receiver_name LIKE ? OR EXISTS (
			SELECT 1 FROM files f WHERE f.task_id = transfers.id AND f.name LIKE ?))`)
		kw := "%" + q.Keyword + "%"
		args = append(args, kw, kw, kw, kw)
	}
	if q.From > 0 {
		where = append(where, `created_at >= ?`)
		args = append(args, q.From)
	}
	if q.To > 0 {
		where = append(where, `created_at <= ?`)
		args = append(args, q.To)
	}
	if len(q.Status) > 0 {
		ph := make([]string, len(q.Status))
		for i, st := range q.Status {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "status IN ("+join(ph, ",")+")")
	}
	// 方向相对发起查询的设备：in = 我是接收方，out = 我是发送方。
	// SelfDeviceID 缺失时该筛选无意义，直接返回空集而不是退化为「不过滤」，
	// 避免用户以为筛选生效却看到全部记录。
	if q.Dir == "in" || q.Dir == "out" {
		if q.SelfDeviceID == "" {
			return nil, 0, nil
		}
		if q.Dir == "in" {
			where = append(where, "receiver_id = ?")
		} else {
			where = append(where, "sender_id = ?")
		}
		args = append(args, q.SelfDeviceID)
	}
	clause := " WHERE " + join(where, " AND ")
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transfers`+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page := q.Page
	if page < 1 {
		page = 1
	}
	size := q.PageSize
	if size <= 0 {
		size = 50
	}
	if size > 500 {
		size = 500
	}
	sqlArgs := append(append([]any{}, args...), size, (page-1)*size)
	rows, err := s.db.Query(transferCols+clause+` ORDER BY created_at DESC LIMIT ? OFFSET ?`, sqlArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*model.Transfer{}
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// ListActiveTransfers 返回所有非终态任务，服务启动时用于恢复。
func (s *Store) ListActiveTransfers() ([]*model.Transfer, error) {
	rows, err := s.db.Query(transferCols + ` WHERE status IN ('awaiting','queued','uploading','paused','verifying','ready') ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Transfer{}
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTransfer 删除任务（级联删除文件与分块行），不触碰磁盘文件。
func (s *Store) DeleteTransfer(id string) error {
	_, err := s.db.Exec(`DELETE FROM transfers WHERE id = ?`, id)
	return err
}

// HideHistory 从历史中隐藏终态任务，绝不删除磁盘文件。
func (s *Store) HideHistory(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	_, err := s.db.Exec(`UPDATE transfers SET history_visible = 0 WHERE id IN (`+join(ph, ",")+`)`, args...)
	return err
}

// ClearHistory 清除全部终态历史（只影响记录，不影响文件）。
func (s *Store) ClearHistory() (int64, error) {
	res, err := s.db.Exec(`UPDATE transfers SET history_visible = 0 WHERE status IN ('completed','failed','cancelled','rejected')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeHistoryBefore 按保留天数清理历史记录标记。
func (s *Store) PurgeHistoryBefore(cutoff int64) (int64, error) {
	res, err := s.db.Exec(`UPDATE transfers SET history_visible = 0
		WHERE history_visible = 1 AND created_at < ? AND status IN ('completed','failed','cancelled','rejected')`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Stats 返回真实统计数据（任务数、总字节），用于设置页展示占用。
func (s *Store) Stats() (map[string]int64, error) {
	out := map[string]int64{}
	queries := map[string]string{
		"tasks":       `SELECT COUNT(*) FROM transfers`,
		"completed":   `SELECT COUNT(*) FROM transfers WHERE status='completed'`,
		"active":      `SELECT COUNT(*) FROM transfers WHERE status IN ('awaiting','queued','uploading','paused','verifying','ready')`,
		"failed":      `SELECT COUNT(*) FROM transfers WHERE status='failed'`,
		"devices":     `SELECT COUNT(*) FROM devices`,
		"trusted":     `SELECT COUNT(*) FROM devices WHERE trusted=1`,
		"totalBytes":  `SELECT COALESCE(SUM(size),0) FROM files`,
		"storedBytes": `SELECT COALESCE(SUM(size),0) FROM files WHERE status IN ('ready','downloaded')`,
	}
	for k, q := range queries {
		var v int64
		if err := s.db.QueryRow(q).Scan(&v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// scanTransfer 统一扫描任务行。
func scanTransfer(sc scanner) (*model.Transfer, error) {
	var t model.Transfer
	var status, conflict string
	var verified, resumable, uploadDone, downloadDone, historyVisible int
	var dropboxID string
	err := sc.Scan(&t.ID, &t.SenderID, &t.SenderName, &t.ReceiverID, &t.ReceiverName, &status, &t.Note,
		&t.TotalFiles, &t.TotalSize, &t.DoneSize, &t.ChunkSize, &conflict, &t.CreatedAt, &t.AcceptedAt,
		&t.StartedAt, &t.CompletedAt, &t.UpdatedAt, &t.Error, &t.ErrorCode, &verified, &resumable,
		&t.NonResumableReason, &t.Retries, &uploadDone, &downloadDone, &historyVisible, &t.ExpiresAt,
		&dropboxID)
	if err != nil {
		return nil, err
	}
	t.Status = model.TransferStatus(status)
	t.Conflict = model.ConflictPolicy(conflict)
	t.Verified = verified == 1
	t.Resumable = resumable == 1
	t.UploadDone = uploadDone == 1
	t.DownloadDone = downloadDone == 1
	t.HistoryVisible = historyVisible == 1
	t.ViaDropbox = dropboxID != ""
	return &t, nil
}

func scanFile(sc scanner) (*model.TransferFile, error) {
	var f model.TransferFile
	var status string
	var skipped int
	err := sc.Scan(&f.ID, &f.TaskID, &f.Index, &f.Name, &f.RelPath, &f.Size, &f.Mime, &f.SrcModTime,
		&f.SHA256, &status, &f.DoneBytes, &f.ChunkSize, &f.ChunkCount, &f.ReceivedChunks, &f.Error,
		&f.ErrorCode, &skipped, &f.DownloadedAt, &f.FinalName)
	if err != nil {
		return nil, err
	}
	f.Status = model.FileStatus(status)
	f.Skipped = skipped == 1
	return &f, nil
}

// ---------------------------------------------------------------- chunks ---

// MarkChunk 幂等登记一个已完整落盘且校验通过的分块。
// 重复提交同一分块不会重复计数——这是断点续传正确性的前提。
func (s *Store) MarkChunk(taskID, fileID string, index int, size int64, sha string, ts int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// 先查询该分块是否已登记：SQLite 的 UPSERT 无论走 INSERT 还是 UPDATE
	// 都会报告 1 行受影响，无法据此区分「首次」与「重复」，因此这里显式判定。
	var existed int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM chunks WHERE file_id = ? AND idx = ?`,
		fileID, index).Scan(&existed); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO chunks(file_id, task_id, idx, size, sha256, received_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(file_id, idx) DO UPDATE SET
		size=excluded.size, sha256=excluded.sha256, received_at=excluded.received_at`,
		fileID, taskID, index, size, sha, ts); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE files SET received_chunks = (SELECT COUNT(*) FROM chunks WHERE file_id = ?),
		done_bytes = (SELECT COALESCE(SUM(size),0) FROM chunks WHERE file_id = ?) WHERE id = ?`,
		fileID, fileID, fileID); err != nil {
		return false, err
	}
	var total int64
	if err := tx.QueryRow(`SELECT COALESCE(SUM(size),0) FROM chunks WHERE task_id = ?`, taskID).Scan(&total); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE transfers SET done_size = ?, updated_at = ? WHERE id = ?`, total, ts, taskID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return existed == 0, nil
}

// ChunkIndexes 返回某文件已收到的分块索引集合，供断点续传计算缺失部分。
func (s *Store) ChunkIndexes(fileID string) (map[int]bool, error) {
	rows, err := s.db.Query(`SELECT idx FROM chunks WHERE file_id = ?`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var i int
		if err := rows.Scan(&i); err != nil {
			return nil, err
		}
		out[i] = true
	}
	return out, rows.Err()
}

// ChunkCount 统计某任务已登记的分块总数。
func (s *Store) ChunkCount(taskID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE task_id = ?`, taskID).Scan(&n)
	return n, err
}

// DeleteChunksForFile 清空某文件的分块登记（重试或重置时使用）。
func (s *Store) DeleteChunksForFile(fileID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM chunks WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE files SET received_chunks = 0, done_bytes = 0 WHERE id = ?`, fileID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteChunksForTask 清空某任务的全部分块登记。
func (s *Store) DeleteChunksForTask(taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM chunks WHERE task_id = ?`, taskID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE files SET received_chunks = 0, done_bytes = 0 WHERE task_id = ?`, taskID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE transfers SET done_size = 0 WHERE id = ?`, taskID); err != nil {
		return err
	}
	return tx.Commit()
}

// ----------------------------------------------------------------- dropbox ---

// UpsertDropbox 保存临时接收入口状态。
func (s *Store) UpsertDropbox(d *model.Dropbox) error {
	_, err := s.db.Exec(`INSERT INTO dropboxes(id, token, enabled, created_at, expires_at, created_by,
		received_count, received_bytes, upload_count) VALUES(?,?,?,?,?,?,?,?,0)
		ON CONFLICT(id) DO UPDATE SET token=excluded.token, enabled=excluded.enabled,
		created_at=excluded.created_at, expires_at=excluded.expires_at,
		received_count=excluded.received_count, received_bytes=excluded.received_bytes`,
		d.ID, d.Token, b2i(d.Enabled), d.CreatedAt, d.ExpiresAt, d.CreatedBy, d.ReceivedCount, d.ReceivedBytes)
	return err
}

// GetActiveDropbox 返回当前唯一活跃的临时入口。
func (s *Store) GetActiveDropbox() (*model.Dropbox, error) {
	row := s.db.QueryRow(`SELECT id, token, enabled, created_at, expires_at, created_by, received_count,
		received_bytes FROM dropboxes ORDER BY created_at DESC LIMIT 1`)
	var d model.Dropbox
	var enabled int
	err := row.Scan(&d.ID, &d.Token, &enabled, &d.CreatedAt, &d.ExpiresAt, &d.CreatedBy,
		&d.ReceivedCount, &d.ReceivedBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Enabled = enabled == 1
	return &d, nil
}

// AddDropboxStats 累加临时入口的真实收件统计。
func (s *Store) AddDropboxStats(id string, count int, bytes int64) error {
	_, err := s.db.Exec(`UPDATE dropboxes SET received_count = received_count + ?, 
		received_bytes = received_bytes + ?, upload_count = upload_count + 1 WHERE id = ?`, count, bytes, id)
	return err
}

// ---------------------------------------------------------------- settings ---

// LoadSettings 读取持久化配置；不存在时返回 nil。
func (s *Store) LoadSettings() (*model.Settings, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM app_settings WHERE key = 'app'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st model.Settings
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// SaveSettings 持久化配置。
func (s *Store) SaveSettings(st *model.Settings) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO app_settings(key, value) VALUES('app', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, string(b))
	return err
}

// ----------------------------------------------------------------- helpers ---

// dropboxIDOf 在任务来自临时接收入口时写入一个占位标识，
// 用于记录来源（读取时只判断是否非空，不依赖具体值）。
func dropboxIDOf(t *model.Transfer) string {
	if t.ViaDropbox {
		return "dropbox"
	}
	return ""
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mustAffect(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// NowMillis 便捷时间戳。
func NowMillis() int64 { return time.Now().UnixMilli() }
