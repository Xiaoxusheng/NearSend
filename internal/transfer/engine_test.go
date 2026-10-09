package transfer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"nearsend/internal/config"
	"nearsend/internal/fsutil"
	"nearsend/internal/model"
	"nearsend/internal/protocol"
	"nearsend/internal/store"
	"nearsend/internal/transfer"
)

// ---------------------------------------------------------------- 测试夹具 ---

// fakeEmitter 记录事件并模拟在线状态，用于在不启动 HTTP 的情况下验证引擎行为。
type fakeEmitter struct {
	mu     sync.Mutex
	events []protocol.Envelope
	online map[string]bool
}

func newFakeEmitter() *fakeEmitter {
	return &fakeEmitter{online: map[string]bool{}}
}

func (f *fakeEmitter) Broadcast(env protocol.Envelope) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, env)
}

func (f *fakeEmitter) SendToDevice(deviceID string, env protocol.Envelope) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	env.DeviceID = deviceID
	f.events = append(f.events, env)
	return f.online[deviceID]
}

func (f *fakeEmitter) IsOnline(deviceID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online[deviceID]
}

func (f *fakeEmitter) setOnline(deviceID string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.online[deviceID] = v
}

// hasEvent 判断是否推送过某类事件。
func (f *fakeEmitter) hasEvent(event string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.Type == event {
			return true
		}
	}
	return false
}

// eventCount 统计某类事件出现次数。
func (f *fakeEmitter) eventCount(event string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e.Type == event {
			n++
		}
	}
	return n
}

type harness struct {
	t       *testing.T
	st      *store.Store
	cfg     *config.Manager
	eng     *transfer.Engine
	emit    *fakeEmitter
	dataDir string
}

// newHarness 构建一套完整的引擎测试环境，使用真实文件系统与真实 SQLite。
func newHarness(t *testing.T, mutate ...func(*model.Settings)) *harness {
	t.Helper()
	dataDir := t.TempDir()
	receiveDir := filepath.Join(dataDir, "received")
	tempDir := filepath.Join(dataDir, "tmp")
	for _, d := range []string{receiveDir, tempDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// 用最小分块尺寸，让小文件也能覆盖多分块逻辑；测试仍然是真实分块。
	saved := &model.Settings{
		ChunkSize:     config.MinChunkSize,
		MaxConcurrent: 2,
		ReceiveDir:    receiveDir,
		TempDir:       tempDir,
		MaxFileSize:   1 << 30,
		MaxTaskSize:   4 << 30,
	}
	for _, m := range mutate {
		m(saved)
	}
	if err := st.SaveSettings(saved); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.New(st, config.Options{DataDir: dataDir})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	emit := newFakeEmitter()
	// 静默日志：测试输出的重点在断言结果上。
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	eng := transfer.New(st, cfg, emit, logger)
	return &harness{t: t, st: st, cfg: cfg, eng: eng, emit: emit, dataDir: dataDir}
}

// addDevice 直接在数据库中登记一台在线设备。
func (h *harness) addDevice(name string) *model.Device {
	h.t.Helper()
	d := &model.Device{
		ID:        "dev-" + name,
		Name:      name,
		OS:        "Windows",
		Browser:   "Chrome",
		Type:      model.DeviceDesktop,
		FirstSeen: fsutil.NowMillis(),
		LastSeen:  fsutil.NowMillis(),
		Token:     "token-" + name,
	}
	if err := h.st.UpsertDevice(d); err != nil {
		h.t.Fatalf("写入设备失败: %v", err)
	}
	h.emit.setOnline(d.ID, true)
	return d
}

// spec 构造文件声明。
func spec(name string, size int64) transfer.FileSpec {
	return transfer.FileSpec{Name: name, Size: size, Mime: "application/octet-stream"}
}

// uploadFile 按分块上传整个文件内容，返回最后一次响应。
func (h *harness) uploadFile(taskID string, dev *model.Device, index int, content []byte) (*transfer.ChunkResult, error) {
	h.t.Helper()
	chunkSize := h.cfg.Get().ChunkSize
	var last *transfer.ChunkResult
	for off := int64(0); off < int64(len(content)); off += chunkSize {
		end := off + chunkSize
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		part := content[off:end]
		sum := sha256.Sum256(part)
		res, err := h.eng.WriteChunk(context.Background(), taskID, dev, index,
			int(off/chunkSize), bytes.NewReader(part), hex.EncodeToString(sum[:]))
		if err != nil {
			return nil, err
		}
		last = res
	}
	return last, nil
}

// randomBytes 生成确定性的伪随机内容（不是全 0，便于发现错位写入）。
func randomBytes(n int) []byte {
	b := make([]byte, n)
	var x uint32 = 0x12345678
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 16)
	}
	return b
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ---------------------------------------------------------------- 正常流程 ---

func TestCreateAcceptUploadComplete(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("发送端")
	receiver := h.addDevice("接收端")

	contentA := randomBytes(int(config.MinChunkSize*2 + 4096)) // 3 个分块
	contentB := randomBytes(128)                               // 1 个分块

	tasks, err := h.eng.Create(transfer.CreateRequest{
		Sender:      sender,
		ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{
			spec("a.bin", int64(len(contentA))),
			spec("b.bin", int64(len(contentB))),
		},
	})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("期望 1 个任务, 得到 %d", len(tasks))
	}
	task := tasks[0]
	if task.Status != model.StatusAwaiting {
		t.Fatalf("新建任务状态应为 awaiting, 实际 %s", task.Status)
	}
	if task.TotalSize != int64(len(contentA)+len(contentB)) {
		t.Fatalf("总大小不符: %d", task.TotalSize)
	}
	if !h.emit.hasEvent(protocol.EvTransferRequested) {
		t.Fatal("未向接收方推送 transfer.requested")
	}

	// 接收方接受
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatalf("接受失败: %v", err)
	}
	// 槽位充足，应直接进入 uploading
	v, _ := h.eng.View(task.ID)
	if v.Status != model.StatusUploading {
		t.Fatalf("接受后应进入 uploading, 实际 %s", v.Status)
	}

	// 上传全部内容
	for idx, content := range [][]byte{contentA, contentB} {
		if _, err := h.uploadFile(task.ID, sender, idx, content); err != nil {
			t.Fatalf("上传文件 %d 失败: %v", idx, err)
		}
	}

	final, err := h.eng.View(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != model.StatusCompleted {
		t.Fatalf("全部上传后应为 completed, 实际 %s (error=%s)", final.Status, final.Error)
	}
	if !final.Verified {
		t.Fatal("完成的任务必须通过校验")
	}
	if final.DoneSize != final.TotalSize {
		t.Fatalf("已完成字节数不符: %d/%d", final.DoneSize, final.TotalSize)
	}
	if !h.emit.hasEvent(protocol.EvTransferCompleted) {
		t.Fatal("未推送 transfer.completed")
	}
	if !h.emit.hasEvent(protocol.EvTransferProgress) {
		t.Fatal("未推送 transfer.progress")
	}

	// 校验磁盘上的真实内容
	for idx, want := range [][]byte{contentA, contentB} {
		p, f, _, ferr := h.eng.FilePath(task.ID, idx)
		if ferr != nil {
			t.Fatalf("取文件路径失败: %v", ferr)
		}
		got, rerr := os.ReadFile(p)
		if rerr != nil {
			t.Fatalf("读取落盘文件失败: %v", rerr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("文件 %d 内容不一致 (len %d != %d)", idx, len(got), len(want))
		}
		if f.SHA256 != sha256Hex(want) {
			t.Fatalf("文件 %d 记录的 SHA-256 不匹配", idx)
		}
		if f.Status != model.FileReady {
			t.Fatalf("文件 %d 状态应为 ready, 实际 %s", idx, f.Status)
		}
		// 落盘路径必须位于接收目录内
		if !fsutil.WithinRoot(h.cfg.ReceiveDir(), p) {
			t.Fatalf("落盘路径逃出了接收目录: %s", p)
		}
	}

	// 临时文件应已全部提交，不留 .part 残留
	tempRoot := filepath.Join(h.cfg.TempDir(), "transfers", task.ID)
	if entries, err := os.ReadDir(tempRoot); err == nil {
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".part" {
				t.Fatalf("残留临时文件: %s", e.Name())
			}
		}
	}
}

func TestZeroByteFileCompletesWithoutChunks(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	tasks, err := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("empty.txt", 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	v, _ := h.eng.View(task.ID)
	if v.Status != model.StatusCompleted {
		t.Fatalf("空文件应直接完成, 实际 %s", v.Status)
	}
	p, _, _, err := h.eng.FilePath(task.ID, 0)
	if err != nil {
		t.Fatalf("空文件应可下载: %v", err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Size() != 0 {
		t.Fatalf("空文件落盘异常: %v size=%d", err, st.Size())
	}
}

// ---------------------------------------------------------------- 分块健壮性 ---

func TestDuplicateChunkIsIdempotent(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(int(config.MinChunkSize*2 + 100))
	tasks, err := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("dup.bin", int64(len(content)))},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}

	chunkSize := h.cfg.Get().ChunkSize
	first := content[:chunkSize]
	sum := sha256Hex(first)

	res1, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, 0,
		bytes.NewReader(first), sum)
	if err != nil {
		t.Fatal(err)
	}
	if res1.Duplicate {
		t.Fatal("首次提交不应被标记为重复")
	}
	if res1.FileDoneBytes != int64(len(first)) {
		t.Fatalf("已传字节不符: %d", res1.FileDoneBytes)
	}

	// 重复提交同一分块：必须幂等，已传字节数不能翻倍。
	res2, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, 0,
		bytes.NewReader(first), sum)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Duplicate {
		t.Fatal("重复提交应被识别为重复")
	}
	if res2.FileDoneBytes != res1.FileDoneBytes {
		t.Fatalf("重复提交改变了已传字节: %d -> %d", res1.FileDoneBytes, res2.FileDoneBytes)
	}
	if res2.ReceivedChunks != 1 {
		t.Fatalf("重复提交改变了分块计数: %d", res2.ReceivedChunks)
	}
}

func TestMissingChunkBlocksCompletionAndReportsIndex(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(int(config.MinChunkSize*3 + 10)) // 4 个分块
	tasks, err := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("partial.bin", int64(len(content)))},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	chunkSize := h.cfg.Get().ChunkSize
	// 故意漏掉第 2 个分块（索引 2，从 0 开始）
	for _, idx := range []int{0, 1, 3} {
		off := int64(idx) * chunkSize
		end := off + chunkSize
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		part := content[off:end]
		if _, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, idx,
			bytes.NewReader(part), sha256Hex(part)); err != nil {
			t.Fatalf("上传分块 %d 失败: %v", idx, err)
		}
	}

	v, _ := h.eng.View(task.ID)
	if v.Status == model.StatusCompleted {
		t.Fatal("缺失分块的任务绝不能标记为完成")
	}
	st, err := h.eng.Chunks(task.ID, sender)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Files) != 1 {
		t.Fatalf("期望 1 个文件, 得到 %d", len(st.Files))
	}
	missing := st.Files[0].Missing
	if len(missing) != 1 || missing[0] != 2 {
		t.Fatalf("缺失分块索引应为 [2], 实际 %v", missing)
	}
	if st.Files[0].ChunkCount != 4 {
		t.Fatalf("分块总数应为 4, 实际 %d", st.Files[0].ChunkCount)
	}

	// 补上缺失分块后应完成
	off := int64(2) * chunkSize
	end := off + chunkSize
	part := content[off:end]
	if _, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, 2,
		bytes.NewReader(part), sha256Hex(part)); err != nil {
		t.Fatal(err)
	}
	v, _ = h.eng.View(task.ID)
	if v.Status != model.StatusCompleted {
		t.Fatalf("补齐后应完成, 实际 %s", v.Status)
	}
}

func TestChunkHashMismatchRejectedAndNotPersisted(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(1024)
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("hash.bin", int64(len(content)))},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	_, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, 0,
		bytes.NewReader(content), sha256Hex([]byte("错误的哈希")))
	if err == nil {
		t.Fatal("哈希不符必须被拒绝")
	}
	if code := transfer.CodeOf(err); code != protocol.CodeChunkHashMismatch {
		t.Fatalf("错误码应为 chunk_hash_mismatch, 实际 %s", code)
	}
	// 被拒绝的分块不得计入进度
	f, _ := h.st.GetFile(task.ID, 0)
	if f.ReceivedChunks != 0 || f.DoneBytes != 0 {
		t.Fatalf("校验失败的分块被错误记录: chunks=%d bytes=%d", f.ReceivedChunks, f.DoneBytes)
	}
	// 临时文件也不应含有该数据
	part := filepath.Join(h.cfg.TempDir(), "transfers", task.ID, f.ID+".part")
	if st, serr := os.Stat(part); serr == nil && st.Size() > 0 {
		t.Fatalf("校验失败的数据被写入磁盘: %d 字节", st.Size())
	}
}

func TestInvalidChunkIndexRejected(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(2048)
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("idx.bin", int64(len(content)))},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	// 只应有 1 个分块（索引 0）
	for _, bad := range []int{-1, 1, 99} {
		_, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, bad,
			bytes.NewReader(content), sha256Hex(content))
		if err == nil {
			t.Fatalf("非法分块索引 %d 未被拒绝", bad)
		}
		if code := transfer.CodeOf(err); code != protocol.CodeInvalidChunkIndex {
			t.Fatalf("索引 %d 错误码应为 invalid_chunk_index, 实际 %s", bad, code)
		}
	}
	// 文件索引越界
	if _, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 7, 0,
		bytes.NewReader(content), ""); err == nil {
		t.Fatal("超范围文件索引未被拒绝")
	}
}

func TestOversizedChunkRejected(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	// 声明 1000 字节的文件，但试图上传 100000 字节
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("small.bin", 1000)},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	_, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, 0,
		bytes.NewReader(randomBytes(100000)), "")
	if err == nil {
		t.Fatal("超长分块必须被拒绝")
	}
	if code := transfer.CodeOf(err); code != protocol.CodeBadRequest {
		t.Fatalf("错误码应为 bad_request, 实际 %s", code)
	}
}

func TestWholeFileIntegrityFailureMarksTaskFailed(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(4096)
	// 声明一个与实际内容不符的整文件哈希
	wrong := sha256Hex([]byte("完全不同的内容"))
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{{
			Name: "bad.bin", Size: int64(len(content)), SHA256: wrong,
		}},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	_, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, 0,
		bytes.NewReader(content), sha256Hex(content))
	if err == nil {
		t.Fatal("整文件校验失败必须报错")
	}
	if code := transfer.CodeOf(err); code != protocol.CodeIntegrityFailed {
		t.Fatalf("错误码应为 integrity_failed, 实际 %s", code)
	}
	v, _ := h.eng.View(task.ID)
	if v.Status != model.StatusFailed {
		t.Fatalf("任务应标记为 failed, 实际 %s", v.Status)
	}
	if v.Verified {
		t.Fatal("失败的任务不得标记为已验证")
	}
	// 校验失败的文件绝不能出现在接收目录里
	if _, _, _, ferr := h.eng.FilePath(task.ID, 0); ferr == nil {
		t.Fatal("校验失败的文件不应可下载")
	}
}

// ---------------------------------------------------------------- 恢复与重试 ---

func TestResumeAfterInterruptionAndRetry(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(int(config.MinChunkSize*3 + 500)) // 4 个分块
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("resume.bin", int64(len(content)))},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	chunkSize := h.cfg.Get().ChunkSize

	// 只传前 2 块，然后模拟服务重启
	for _, idx := range []int{0, 1} {
		off := int64(idx) * chunkSize
		part := content[off : off+chunkSize]
		if _, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, idx,
			bytes.NewReader(part), sha256Hex(part)); err != nil {
			t.Fatal(err)
		}
	}

	// 重启恢复：未完成的任务必须被标记为失败，而不是成功
	if err := h.eng.RecoverOnStart(); err != nil {
		t.Fatal(err)
	}
	v, _ := h.eng.View(task.ID)
	if v.Status != model.StatusFailed {
		t.Fatalf("重启后中断任务应为 failed, 实际 %s", v.Status)
	}
	if !v.Resumable {
		t.Fatal("中断任务应保留续传能力")
	}
	if v.Verified {
		t.Fatal("中断任务不得标记为已验证")
	}

	// 续传状态应保留已收到的分块，只要求补缺失部分
	st, err := h.eng.Chunks(task.ID, sender)
	if err != nil {
		t.Fatal(err)
	}
	missing := st.Files[0].Missing
	if len(missing) != 2 || missing[0] != 2 || missing[1] != 3 {
		t.Fatalf("缺失分块应为 [2 3], 实际 %v", missing)
	}

	// 重试
	if _, err := h.eng.Retry(task.ID, sender); err != nil {
		t.Fatalf("重试失败: %v", err)
	}
	v, _ = h.eng.View(task.ID)
	if v.Status != model.StatusUploading && v.Status != model.StatusQueued {
		t.Fatalf("重试后状态异常: %s", v.Status)
	}
	if v.Retries != 1 {
		t.Fatalf("重试次数应为 1, 实际 %d", v.Retries)
	}

	// 只补缺失的两块
	for _, idx := range []int{2, 3} {
		off := int64(idx) * chunkSize
		end := off + chunkSize
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		part := content[off:end]
		if _, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, idx,
			bytes.NewReader(part), sha256Hex(part)); err != nil {
			t.Fatalf("补传分块 %d 失败: %v", idx, err)
		}
	}
	v, _ = h.eng.View(task.ID)
	if v.Status != model.StatusCompleted {
		t.Fatalf("续传完成后应为 completed, 实际 %s (err=%s)", v.Status, v.Error)
	}
	p, _, _, _ := h.eng.FilePath(task.ID, 0)
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, content) {
		t.Fatal("续传后文件内容与源不一致")
	}
}

func TestResetFileClearsProgress(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(int(config.MinChunkSize * 2))
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("reset.bin", int64(len(content)))},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	chunkSize := h.cfg.Get().ChunkSize
	if _, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, 0,
		bytes.NewReader(content[:chunkSize]), sha256Hex(content[:chunkSize])); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ResetFile(task.ID, 0, sender); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	st, _ := h.eng.Chunks(task.ID, sender)
	if st.Files[0].ReceivedChunks != 0 {
		t.Fatalf("重置后已收分块应为 0, 实际 %d", st.Files[0].ReceivedChunks)
	}
	if len(st.Files[0].Missing) != st.Files[0].ChunkCount {
		t.Fatal("重置后应要求全部重传")
	}
}

// ---------------------------------------------------------------- 权限 ---

func TestOnlySenderCanUpload(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	other := h.addDevice("X")
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("auth.bin", 100)},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	// 第三人上传：必须被拒绝
	if _, err := h.eng.WriteChunk(context.Background(), task.ID, other, 0, 0,
		bytes.NewReader(randomBytes(100)), ""); err == nil {
		t.Fatal("非发送方上传未被拒绝")
	} else if transfer.CodeOf(err) != protocol.CodeForbidden {
		t.Fatalf("错误码应为 forbidden, 实际 %s", transfer.CodeOf(err))
	}
	// 接收方也不应能上传（只有发送方持有源文件）
	if _, err := h.eng.WriteChunk(context.Background(), task.ID, receiver, 0, 0,
		bytes.NewReader(randomBytes(100)), ""); err == nil {
		t.Fatal("接收方上传未被拒绝")
	}
	// 第三人读取分块状态：必须被拒绝
	if _, err := h.eng.Chunks(task.ID, other); err == nil {
		t.Fatal("非参与者读取任务状态未被拒绝")
	}
}

func TestOnlyReceiverCanAcceptReject(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	other := h.addDevice("X")
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("f.txt", 10)},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, sender, model.ConflictRename); err == nil {
		t.Fatal("发送方不应能代为接受")
	}
	if _, err := h.eng.Accept(task.ID, other, model.ConflictRename); err == nil {
		t.Fatal("第三方不应能接受")
	}
	if _, err := h.eng.Reject(task.ID, sender, ""); err == nil {
		t.Fatal("发送方不应能代为拒绝")
	}
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatalf("接收方接受失败: %v", err)
	}
	// 已接受后再次接受应报状态错误
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err == nil {
		t.Fatal("重复接受未被拒绝")
	}
}

func TestOnlyReceiverCanDownload(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(64)
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("dl.bin", int64(len(content)))},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	if _, err := h.uploadFile(task.ID, sender, 0, content); err != nil {
		t.Fatal(err)
	}
	// 发送方不是接收方，不能下载
	if err := h.eng.MarkDownloaded(task.ID, 0, sender); err == nil {
		t.Fatal("发送方下载未被拒绝")
	} else if transfer.CodeOf(err) != protocol.CodeForbidden {
		t.Fatalf("错误码应为 forbidden, 实际 %s", transfer.CodeOf(err))
	}
	// 接收方可以下载并登记
	if err := h.eng.MarkDownloaded(task.ID, 0, receiver); err != nil {
		t.Fatalf("接收方下载失败: %v", err)
	}
	f, _ := h.st.GetFile(task.ID, 0)
	if f.Status != model.FileDownloaded || f.DownloadedAt == 0 {
		t.Fatalf("下载登记未生效: %s at=%d", f.Status, f.DownloadedAt)
	}
	v, _ := h.eng.View(task.ID)
	if !v.DownloadDone {
		t.Fatal("任务应标记为已下载完成")
	}
}

func TestRejectFlowNotifiesSender(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("r.txt", 10)},
	})
	task := tasks[0]
	if _, err := h.eng.Reject(task.ID, receiver, "暂时不需要"); err != nil {
		t.Fatal(err)
	}
	v, _ := h.eng.View(task.ID)
	if v.Status != model.StatusRejected {
		t.Fatalf("状态应为 rejected, 实际 %s", v.Status)
	}
	if v.ErrorCode != protocol.CodeRejected {
		t.Fatalf("错误码应为 rejected, 实际 %s", v.ErrorCode)
	}
	if !h.emit.hasEvent(protocol.EvTransferRejected) {
		t.Fatal("未推送 transfer.rejected")
	}
	// 发送方重试应回到 awaiting（需要接收方重新确认）
	if _, err := h.eng.Retry(task.ID, sender); err != nil {
		t.Fatal(err)
	}
	v, _ = h.eng.View(task.ID)
	if v.Status != model.StatusAwaiting {
		t.Fatalf("被拒绝任务重试后应为 awaiting, 实际 %s", v.Status)
	}
}

// ---------------------------------------------------------------- 取消与暂停 ---

func TestCancelBlocksUploadAndRetainsPartialDataForResume(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(int(config.MinChunkSize * 2))
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("cancel.bin", int64(len(content)))},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	chunkSize := h.cfg.Get().ChunkSize
	if _, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, 0,
		bytes.NewReader(content[:chunkSize]), sha256Hex(content[:chunkSize])); err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.Cancel(task.ID, sender); err != nil {
		t.Fatal(err)
	}
	v, _ := h.eng.View(task.ID)
	if v.Status != model.StatusCancelled {
		t.Fatalf("状态应为 cancelled, 实际 %s", v.Status)
	}
	if v.UploadDone || v.Verified {
		t.Fatal("取消的任务不得标记为已上传或已校验")
	}
	if !h.emit.hasEvent(protocol.EvTransferCancelled) {
		t.Fatal("未推送 transfer.cancelled")
	}
	// 取消后不得再接受上传
	if _, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, 1,
		bytes.NewReader(content[chunkSize:]), sha256Hex(content[chunkSize:])); err == nil {
		t.Fatal("取消后仍可上传")
	}
	// 续传能力：已收分块被保留，重试时只需补缺失部分，而不是全部重传
	if _, err := os.Stat(filepath.Join(h.cfg.TempDir(), "transfers", task.ID)); err != nil {
		t.Fatalf("取消后应保留临时数据以便续传: %v", err)
	}
	if _, err := h.eng.Retry(task.ID, sender); err != nil {
		t.Fatalf("重试失败: %v", err)
	}
	st, _ := h.eng.Chunks(task.ID, sender)
	if len(st.Files[0].Missing) != 1 || st.Files[0].Missing[0] != 1 {
		t.Fatalf("重试后应只需补第 2 块, 实际缺失 %v", st.Files[0].Missing)
	}
	if _, err := h.uploadFile(task.ID, sender, 0, content); err != nil {
		t.Fatal(err)
	}
	v, _ = h.eng.View(task.ID)
	if v.Status != model.StatusCompleted {
		t.Fatalf("续传后应完成, 实际 %s", v.Status)
	}
	// 完成后临时分块文件应已被原子提交走，只留下空目录
	partFiles := 0
	_ = filepath.WalkDir(filepath.Join(h.cfg.TempDir(), "transfers", task.ID),
		func(_ string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".part") {
				partFiles++
			}
			return nil
		})
	if partFiles != 0 {
		t.Fatalf("完成后仍残留 %d 个临时分块文件", partFiles)
	}
	// 已完成任务的临时目录可被立即回收
	if n, _, err := h.eng.CleanupTemp(); err != nil {
		t.Fatal(err)
	} else if n == 0 {
		t.Fatal("已完成任务的残留临时目录应被回收")
	}
}

func TestPauseBlocksChunkWritesAndResumeRestores(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(int(config.MinChunkSize + 100))
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("p.bin", int64(len(content)))},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.Pause(task.ID, sender); err != nil {
		t.Fatalf("暂停失败: %v", err)
	}
	v, _ := h.eng.View(task.ID)
	if v.Status != model.StatusPaused {
		t.Fatalf("状态应为 paused, 实际 %s", v.Status)
	}
	// 暂停期间写入必须被拒绝——「暂停」因此具有真实语义
	_, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, 0,
		bytes.NewReader(content), sha256Hex(content))
	if err == nil {
		t.Fatal("暂停期间仍可写入分块")
	}
	if _, err := h.eng.Resume(task.ID, sender); err != nil {
		t.Fatalf("继续失败: %v", err)
	}
	v, _ = h.eng.View(task.ID)
	if v.Status != model.StatusUploading && v.Status != model.StatusQueued {
		t.Fatalf("继续后状态异常: %s", v.Status)
	}
	if _, err := h.uploadFile(task.ID, sender, 0, content); err != nil {
		t.Fatalf("继续后上传失败: %v", err)
	}
	v, _ = h.eng.View(task.ID)
	if v.Status != model.StatusCompleted {
		t.Fatalf("继续后应完成, 实际 %s", v.Status)
	}
}

// ---------------------------------------------------------------- 重名策略 ---

func TestConflictRenameProducesReadableUniqueName(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("同名发送端")
	receiver := h.addDevice("R")
	content := []byte("第一次的内容")

	upload := func() (string, *model.Transfer) {
		tasks, err := h.eng.Create(transfer.CreateRequest{
			Sender: sender, ReceiverIDs: []string{receiver.ID},
			Files: []transfer.FileSpec{spec("报告.txt", int64(len(content)))},
		})
		if err != nil {
			t.Fatal(err)
		}
		task := tasks[0]
		if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
			t.Fatal(err)
		}
		if _, err := h.uploadFile(task.ID, sender, 0, content); err != nil {
			t.Fatal(err)
		}
		p, _, _, err := h.eng.FilePath(task.ID, 0)
		if err != nil {
			t.Fatalf("取路径失败: %v", err)
		}
		return p, task
	}

	first, _ := upload()
	if filepath.Base(first) != "报告.txt" {
		t.Fatalf("首次落盘名应为 报告.txt, 实际 %s", filepath.Base(first))
	}
	second, _ := upload()
	if filepath.Base(second) != "报告 (1).txt" {
		t.Fatalf("第二次应自动重命名为 报告 (1).txt, 实际 %s", filepath.Base(second))
	}
	third, _ := upload()
	if filepath.Base(third) != "报告 (2).txt" {
		t.Fatalf("第三次应为 报告 (2).txt, 实际 %s", filepath.Base(third))
	}
	// 三次内容都必须存在且可读，不能发生静默覆盖
	for _, p := range []string{first, second, third} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("文件丢失: %s", p)
		}
	}
}

func TestConflictOverwriteReplacesExisting(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("覆盖发送端")
	receiver := h.addDevice("R")

	upload := func(content []byte, policy model.ConflictPolicy) string {
		tasks, err := h.eng.Create(transfer.CreateRequest{
			Sender: sender, ReceiverIDs: []string{receiver.ID},
			Files: []transfer.FileSpec{spec("data.txt", int64(len(content)))},
		})
		if err != nil {
			t.Fatal(err)
		}
		task := tasks[0]
		if _, err := h.eng.Accept(task.ID, receiver, policy); err != nil {
			t.Fatal(err)
		}
		if _, err := h.uploadFile(task.ID, sender, 0, content); err != nil {
			t.Fatal(err)
		}
		p, _, _, err := h.eng.FilePath(task.ID, 0)
		if err != nil {
			t.Fatalf("取路径失败: %v", err)
		}
		return p
	}

	p1 := upload([]byte("旧内容"), model.ConflictOverwrite)
	p2 := upload([]byte("新内容!!"), model.ConflictOverwrite)
	if p1 != p2 {
		t.Fatalf("overwrite 策略下路径应相同: %s vs %s", p1, p2)
	}
	b, _ := os.ReadFile(p2)
	if string(b) != "新内容!!" {
		t.Fatalf("覆盖后内容不符: %q", b)
	}
}

func TestConflictSkipMarksFileSkippedWithoutUpload(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("跳过发送端")
	receiver := h.addDevice("R")

	// 先放一个已有文件到接收目录的对应位置
	first := func() string {
		tasks, _ := h.eng.Create(transfer.CreateRequest{
			Sender: sender, ReceiverIDs: []string{receiver.ID},
			Files: []transfer.FileSpec{spec("keep.txt", 5)},
		})
		task := tasks[0]
		if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
			t.Fatal(err)
		}
		if _, err := h.uploadFile(task.ID, sender, 0, []byte("hello")); err != nil {
			t.Fatal(err)
		}
		p, _, _, _ := h.eng.FilePath(task.ID, 0)
		return p
	}()

	// 再发同名文件，选择 skip
	tasks, err := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("keep.txt", int64(len("world!!!")))},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictSkip); err != nil {
		t.Fatal(err)
	}
	v, _ := h.eng.View(task.ID)
	if v.Status != model.StatusCompleted {
		t.Fatalf("全跳过任务应直接完成, 实际 %s", v.Status)
	}
	f, _ := h.st.GetFile(task.ID, 0)
	if !f.Skipped || f.Status != model.FileSkipped {
		t.Fatalf("文件应被标记为跳过, 实际 skipped=%v status=%s", f.Skipped, f.Status)
	}
	// 原文件内容不能被改动
	b, _ := os.ReadFile(first)
	if string(b) != "hello" {
		t.Fatalf("跳过策略下原文件被修改: %q", b)
	}
}

// ---------------------------------------------------------------- 路径安全 ---

func TestFileNameTraversalCannotEscapeReceiveDir(t *testing.T) {
	h := newHarness(t)
	receiver := h.addDevice("R")
	content := []byte("payload")

	// wantBase 为空表示只断言安全性，不断言具体净化结果（例如 %2F 这类
	// 不含真实分隔符的字符串可以保留，它无法构成路径逃逸）。
	cases := []struct {
		in       string
		wantBase string
	}{
		{"../../../evil.txt", "evil.txt"},
		{`..\..\..\evil.txt`, "evil.txt"},
		{"/etc/passwd", "passwd"},
		{"sub/../../evil.txt", "evil.txt"},
		{"..%2F..%2Fevil.txt", ""},
		{"a/b/../../../c.txt", "c.txt"},
	}
	for idx, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			// 每个用例使用独立的发送方，使其落到各自的目录，
			// 避免同名文件因重名策略被改名而干扰断言。
			sender := h.addDevice(fmt.Sprintf("attacker-%d", idx))
			tasks, err := h.eng.Create(transfer.CreateRequest{
				Sender: sender, ReceiverIDs: []string{receiver.ID},
				Files: []transfer.FileSpec{{Name: c.in, RelPath: c.in, Size: int64(len(content))}},
			})
			if err != nil {
				t.Fatalf("创建失败: %v", err)
			}
			task := tasks[0]
			if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
				t.Fatalf("接受失败: %v", err)
			}
			if _, err := h.uploadFile(task.ID, sender, 0, content); err != nil {
				t.Fatalf("上传失败: %v", err)
			}
			p, f, _, err := h.eng.FilePath(task.ID, 0)
			if err != nil {
				t.Fatalf("路径不可用: %v", err)
			}
			// 核心安全断言 1：落盘路径必须位于接收目录内。
			if !fsutil.WithinRoot(h.cfg.ReceiveDir(), p) {
				t.Fatalf("逃出了接收目录: %s", p)
			}
			// 核心安全断言 2：相对接收目录的路径不得含有上级引用段。
			relPath, rerr := filepath.Rel(h.cfg.ReceiveDir(), p)
			if rerr != nil {
				t.Fatalf("无法计算相对路径: %v", rerr)
			}
			if strings.HasPrefix(relPath, "..") {
				t.Fatalf("相对路径以 .. 开头: %s", relPath)
			}
			for _, seg := range strings.Split(filepath.ToSlash(relPath), "/") {
				if seg == ".." {
					t.Fatalf("路径段含上级引用: %s", relPath)
				}
			}
			// 核心安全断言 3：入库的相对路径不得是绝对路径。
			if filepath.IsAbs(f.RelPath) {
				t.Fatalf("相对路径净化后仍为绝对路径: %s", f.RelPath)
			}
			if c.wantBase != "" && filepath.Base(p) != c.wantBase {
				t.Fatalf("期望落盘名 %q, 实际 %q", c.wantBase, filepath.Base(p))
			}
		})
	}
}

func TestNestedDirectoryStructurePreserved(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("目录发送端")
	receiver := h.addDevice("R")
	content := []byte("nested content")

	tasks, err := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{
			{Name: "a.txt", RelPath: "project/src/a.txt", Size: int64(len(content))},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	if _, err := h.uploadFile(task.ID, sender, 0, content); err != nil {
		t.Fatal(err)
	}
	p, _, _, err := h.eng.FilePath(task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filepath.ToSlash(p), "project/src/a.txt") {
		t.Fatalf("目录结构未保留: %s", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("嵌套文件未落盘: %v", err)
	}
}

// ---------------------------------------------------------------- 大小限制 ---

func TestSizeLimitsEnforcedAtCreate(t *testing.T) {
	h := newHarness(t, func(s *model.Settings) {
		s.MaxFileSize = 1024
		s.MaxTaskSize = 2048
	})
	sender := h.addDevice("S")
	receiver := h.addDevice("R")

	// 单文件超限
	_, err := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("big.bin", 4096)},
	})
	if err == nil || transfer.CodeOf(err) != protocol.CodeFileTooLarge {
		t.Fatalf("单文件超限应返回 file_too_large, 实际 %v", err)
	}

	// 任务总量超限
	_, err = h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{
			spec("a.bin", 1000), spec("b.bin", 1000), spec("c.bin", 1000),
		},
	})
	if err == nil || transfer.CodeOf(err) != protocol.CodeTaskTooLarge {
		t.Fatalf("任务超限应返回 task_too_large, 实际 %v", err)
	}

	// 边界内应成功
	if _, err := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("ok.bin", 1024)},
	}); err != nil {
		t.Fatalf("限制内文件被拒绝: %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")

	if _, err := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: nil, Files: []transfer.FileSpec{spec("a", 1)},
	}); err == nil {
		t.Fatal("无接收方应报错")
	}
	if _, err := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID}, Files: nil,
	}); err == nil {
		t.Fatal("空文件列表应报错")
	}
	// 只给自己发：没有有效接收方
	if _, err := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{sender.ID}, Files: []transfer.FileSpec{spec("a", 1)},
	}); err == nil {
		t.Fatal("给自己发送应报错")
	}
	// 不存在的接收方
	if _, err := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{"不存在"}, Files: []transfer.FileSpec{spec("a", 1)},
	}); err == nil {
		t.Fatal("不存在的接收方应报错")
	}
}

// ---------------------------------------------------------------- 多接收方 ---

func TestMultiReceiverCreatesSeparateTasks(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	r1 := h.addDevice("R1")
	r2 := h.addDevice("R2")
	content := []byte("multi receiver payload")

	tasks, err := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{r1.ID, r2.ID, r1.ID}, // 含重复项
		Files: []transfer.FileSpec{spec("m.txt", int64(len(content)))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("应为 2 个独立任务（重复接收方去重）, 实际 %d", len(tasks))
	}
	// 各任务独立确认
	if _, err := h.eng.Accept(tasks[0].ID, r1, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	v1, _ := h.eng.View(tasks[0].ID)
	v2, _ := h.eng.View(tasks[1].ID)
	if v1.Status != model.StatusUploading && v1.Status != model.StatusQueued {
		t.Fatalf("任务1状态异常: %s", v1.Status)
	}
	if v2.Status != model.StatusAwaiting {
		t.Fatalf("任务2应仍在等待确认: %s", v2.Status)
	}
}

// ---------------------------------------------------------------- 并发槽位 ---

func TestConcurrencySlotsLimitUploadingTasks(t *testing.T) {
	h := newHarness(t, func(s *model.Settings) {
		s.MaxConcurrent = 1
	})
	sender := h.addDevice("S")
	r1 := h.addDevice("R1")
	r2 := h.addDevice("R2")
	content := []byte("concurrency payload")

	mk := func(rid string) *model.Transfer {
		tasks, err := h.eng.Create(transfer.CreateRequest{
			Sender: sender, ReceiverIDs: []string{rid},
			Files: []transfer.FileSpec{spec("c.txt", int64(len(content)))},
		})
		if err != nil {
			t.Fatal(err)
		}
		return tasks[0]
	}
	t1 := mk(r1.ID)
	t2 := mk(r2.ID)

	if _, err := h.eng.Accept(t1.ID, r1, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.Accept(t2.ID, r2, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	v1, _ := h.eng.View(t1.ID)
	v2, _ := h.eng.View(t2.ID)
	if v1.Status != model.StatusUploading {
		t.Fatalf("第一个任务应占用槽位: %s", v1.Status)
	}
	if v2.Status != model.StatusQueued {
		t.Fatalf("第二个任务应排队等待: %s", v2.Status)
	}

	// 完成第一个任务后，第二个应自动获得槽位
	if _, err := h.uploadFile(t1.ID, sender, 0, content); err != nil {
		t.Fatal(err)
	}
	v1, _ = h.eng.View(t1.ID)
	if v1.Status != model.StatusCompleted {
		t.Fatalf("任务1应完成: %s", v1.Status)
	}
	v2, _ = h.eng.View(t2.ID)
	if v2.Status != model.StatusUploading {
		t.Fatalf("任务2应在前者完成后自动开始: %s", v2.Status)
	}
}

// ---------------------------------------------------------------- 持久化 ---

func TestPersistenceAcrossReopen(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(int(config.MinChunkSize + 10))
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("persist.bin", int64(len(content)))},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	chunkSize := h.cfg.Get().ChunkSize
	if _, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, 0,
		bytes.NewReader(content[:chunkSize]), sha256Hex(content[:chunkSize])); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(h.dataDir, "test.db")
	h.st.Close()

	// 重新打开同一个数据库：分块进度与任务状态必须完整保留
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("重新打开数据库失败: %v", err)
	}
	defer st2.Close()
	got, err := st2.GetTransfer(task.ID)
	if err != nil {
		t.Fatalf("任务丢失: %v", err)
	}
	if got.Status != model.StatusUploading {
		t.Fatalf("状态未持久化: %s", got.Status)
	}
	if len(got.Files) != 1 {
		t.Fatalf("文件记录丢失: %d", len(got.Files))
	}
	if got.Files[0].ReceivedChunks != 1 {
		t.Fatalf("分块进度未持久化: %d", got.Files[0].ReceivedChunks)
	}
	if got.Files[0].DoneBytes != chunkSize {
		t.Fatalf("已传字节未持久化: %d", got.Files[0].DoneBytes)
	}
	idx, err := st2.ChunkIndexes(got.Files[0].ID)
	if err != nil || !idx[0] || len(idx) != 1 {
		t.Fatalf("分块索引未持久化: %v %v", idx, err)
	}
}

func TestRecoverOnStartKeepsReadyAndAwaitingTasks(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")

	// awaiting 任务（未接受）
	pending, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("pending.txt", 4)},
	})

	// 已完成任务
	content := []byte("done")
	done, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("done.txt", int64(len(content)))},
	})
	if _, err := h.eng.Accept(done[0].ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	if _, err := h.uploadFile(done[0].ID, sender, 0, content); err != nil {
		t.Fatal(err)
	}

	if err := h.eng.RecoverOnStart(); err != nil {
		t.Fatal(err)
	}
	v1, _ := h.eng.View(pending[0].ID)
	if v1.Status != model.StatusAwaiting {
		t.Fatalf("等待确认的任务不应被改动: %s", v1.Status)
	}
	v2, _ := h.eng.View(done[0].ID)
	if v2.Status != model.StatusCompleted {
		t.Fatalf("已完成任务必须保持完成: %s", v2.Status)
	}
	if !v2.Verified {
		t.Fatal("已完成任务的校验标记丢失")
	}
	// 已完成文件的下载能力在重启后仍然可用
	if _, _, _, err := h.eng.FilePath(done[0].ID, 0); err != nil {
		t.Fatalf("重启后文件不可下载: %v", err)
	}
}

// ---------------------------------------------------------------- 限速 ---

func TestLimiterBasics(t *testing.T) {
	// 不限速时 Wait 应立即返回
	l := transfer.NewLimiter(0)
	if err := l.Wait(context.Background(), 1<<20); err != nil {
		t.Fatalf("不限速时出错: %v", err)
	}
	if l.Limit() != 0 {
		t.Fatalf("Limit() 应为 0, 实际 %d", l.Limit())
	}
	// 设置限速后 Limit 应反映新值（用于设置页展示真实生效值）
	l.SetLimit(1 << 20)
	if l.Limit() != 1<<20 {
		t.Fatalf("限速值未更新: %d", l.Limit())
	}
	// 取消 context 应能打断等待
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	small := transfer.NewLimiter(1) // 每秒 1 字节
	if err := small.Wait(ctx, 1<<20); err == nil {
		t.Fatal("已取消的上下文应返回错误")
	}
}

// ---------------------------------------------------------------- 工具 ---

func TestChunkCountCalculation(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	chunkSize := h.cfg.Get().ChunkSize

	cases := []struct {
		size  int64
		count int
	}{
		{0, 0},
		{1, 1},
		{chunkSize, 1},
		{chunkSize + 1, 2},
		{chunkSize * 5, 5},
		{chunkSize*5 + 7, 6},
	}
	for _, c := range cases {
		tasks, err := h.eng.Create(transfer.CreateRequest{
			Sender: sender, ReceiverIDs: []string{receiver.ID},
			Files: []transfer.FileSpec{spec(fmt.Sprintf("c-%d.bin", c.size), c.size)},
		})
		if err != nil {
			t.Fatalf("size=%d 创建失败: %v", c.size, err)
		}
		f, err := h.st.GetFile(tasks[0].ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if f.ChunkCount != c.count {
			t.Fatalf("size=%d 分块数应为 %d, 实际 %d", c.size, c.count, f.ChunkCount)
		}
	}
}

func TestCleanupTempSparesActiveTasks(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(int(config.MinChunkSize * 2))
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("active.bin", int64(len(content)))},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	chunkSize := h.cfg.Get().ChunkSize
	// 上传 1 块，形成一个"正在进行中"的临时文件
	if _, err := h.eng.WriteChunk(context.Background(), task.ID, sender, 0, 0,
		bytes.NewReader(content[:chunkSize]), sha256Hex(content[:chunkSize])); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(h.cfg.TempDir(), "transfers", task.ID)
	if _, err := os.Stat(part); err != nil {
		t.Fatalf("临时目录不存在: %v", err)
	}
	// 强制清理（保留期设为 0 也生效，因为还在进行中的任务必须被跳过）
	removed, _, err := h.eng.CleanupTemp()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("正在进行中的任务临时数据被清理了: %d", removed)
	}
	if _, err := os.Stat(part); err != nil {
		t.Fatal("进行中任务的临时目录被删除")
	}
}

func TestStorageUsageReportsRealNumbers(t *testing.T) {
	h := newHarness(t)
	sender := h.addDevice("S")
	receiver := h.addDevice("R")
	content := randomBytes(5000)
	tasks, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{receiver.ID},
		Files: []transfer.FileSpec{spec("usage.bin", int64(len(content)))},
	})
	task := tasks[0]
	if _, err := h.eng.Accept(task.ID, receiver, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	if _, err := h.uploadFile(task.ID, sender, 0, content); err != nil {
		t.Fatal(err)
	}
	usage := h.eng.StorageUsage()
	rb, ok := usage["receiveBytes"].(int64)
	if !ok {
		t.Fatal("receiveBytes 缺失")
	}
	if rb < int64(len(content)) {
		t.Fatalf("接收目录占用应至少为文件大小, 实际 %d", rb)
	}
	rf, _ := usage["receiveFiles"].(int)
	if rf < 1 {
		t.Fatalf("接收文件数应 >= 1, 实际 %d", rf)
	}
}

// ---------------------------------------------------------------- 僵死任务回收 ---

func TestStaleUploadingTaskIsReapedAndSlotReleased(t *testing.T) {
	h := newHarness(t, func(s *model.Settings) { s.MaxConcurrent = 1 })
	sender := h.addDevice("S")
	r1 := h.addDevice("R1")
	r2 := h.addDevice("R2")
	content := []byte("stale payload")

	t1, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{r1.ID},
		Files: []transfer.FileSpec{spec("stale.bin", int64(len(content)))},
	})
	if _, err := h.eng.Accept(t1[0].ID, r1, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	if v, _ := h.eng.View(t1[0].ID); v.Status != model.StatusUploading {
		t.Fatalf("第一个任务应占用唯一的槽位: %s", v.Status)
	}

	// 把 updated_at 改到很久以前，模拟「发送方消失，没有任何分块进展」。
	old := fsutil.NowMillis() - 30*60*1000
	if err := h.st.UpdateTransferState(t1[0].ID, model.StatusUploading, 0, "", "", old); err != nil {
		t.Fatal(err)
	}

	h.eng.Sweep()

	v, _ := h.eng.View(t1[0].ID)
	if v.Status != model.StatusFailed {
		t.Fatalf("长时间无进展的任务应被回收为 failed, 实际 %s", v.Status)
	}
	if !v.Resumable {
		t.Fatal("被回收的任务必须保留续传能力")
	}
	if !h.emit.hasEvent(protocol.EvTransferFailed) {
		t.Fatal("回收时应推送 transfer.failed")
	}

	// 关键：槽位必须被释放，否则后续任务会永远排队。
	t2, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{r2.ID},
		Files: []transfer.FileSpec{spec("after.bin", int64(len(content)))},
	})
	if _, err := h.eng.Accept(t2[0].ID, r2, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	v2, _ := h.eng.View(t2[0].ID)
	if v2.Status != model.StatusUploading {
		t.Fatalf("回收槽位后新任务应能开始传输, 实际 %s", v2.Status)
	}
}

func TestReaperIgnoresQueuedTasks(t *testing.T) {
	h := newHarness(t, func(s *model.Settings) { s.MaxConcurrent = 1 })
	sender := h.addDevice("S")
	r1 := h.addDevice("R1")
	r2 := h.addDevice("R2")

	// 占住唯一槽位的任务：保持「最近有更新」，因此不应被回收。
	t1, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{r1.ID},
		Files: []transfer.FileSpec{spec("active.bin", 32)},
	})
	if _, err := h.eng.Accept(t1[0].ID, r1, model.ConflictRename); err != nil {
		t.Fatal(err)
	}

	// 第二个任务排队，并把它的 updated_at 改旧。
	t2, _ := h.eng.Create(transfer.CreateRequest{
		Sender: sender, ReceiverIDs: []string{r2.ID},
		Files: []transfer.FileSpec{spec("queued.bin", 32)},
	})
	if _, err := h.eng.Accept(t2[0].ID, r2, model.ConflictRename); err != nil {
		t.Fatal(err)
	}
	if v2, _ := h.eng.View(t2[0].ID); v2.Status != model.StatusQueued {
		t.Fatalf("第二个任务应排队: %s", v2.Status)
	}
	old := fsutil.NowMillis() - 30*60*1000
	if err := h.st.UpdateTransferState(t2[0].ID, model.StatusQueued, 0, "", "", old); err != nil {
		t.Fatal(err)
	}

	h.eng.Sweep()

	// 排队任务不占槽位，因此不该被回收——等待是它的正常状态。
	v2, _ := h.eng.View(t2[0].ID)
	if v2.Status != model.StatusQueued {
		t.Fatalf("排队中的任务不应被回收, 实际 %s", v2.Status)
	}
	// 持有槽位且最近有更新的任务同样不该被回收。
	v1, _ := h.eng.View(t1[0].ID)
	if v1.Status != model.StatusUploading {
		t.Fatalf("最近有更新的传输中任务不应被回收, 实际 %s", v1.Status)
	}
}
