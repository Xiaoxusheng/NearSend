package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"nearsend/internal/api"
	"nearsend/internal/config"
	"nearsend/internal/hub"
	"nearsend/internal/model"
	"nearsend/internal/protocol"
	"nearsend/internal/store"
	"nearsend/internal/transfer"
)

// ---------------------------------------------------------------- 测试环境 ---

type testEnv struct {
	t          *testing.T
	http       *httptest.Server
	wsBase     string
	st         *store.Store
	cfg        *config.Manager
	engine     *transfer.Engine
	dataDir    string
	receiveDir string
}

// newEnv 起一个真实的 HTTP 服务（含真实 SQLite 与真实文件系统）。
//
// 通过包装 handler 注入 X-Test-Remote 头来模拟「远程设备」，
// 因为 httptest 的所有请求都来自回环地址，而回环地址会被判定为主机自身。
func newEnv(t *testing.T, mutate ...func(*model.Settings)) *testEnv {
	t.Helper()
	dataDir := t.TempDir()
	receiveDir := filepath.Join(dataDir, "received")
	tempDir := filepath.Join(dataDir, "tmp")
	for _, d := range []string{receiveDir, tempDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(dataDir, "api.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

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
	cfg, err := config.New(st, config.Options{DataDir: dataDir, Port: 8787})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	h := hub.New(logger, func(token string) (*model.Device, error) {
		return st.FindDeviceByToken(token)
	})
	engine := transfer.New(st, cfg, h, logger)
	srv := api.NewServer(api.Options{
		Store: st, Config: cfg, Hub: h, Engine: engine,
		Log: logger, Version: "test", PublicDir: "",
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fake := r.Header.Get("X-Test-Remote"); fake != "" {
			r.RemoteAddr = fake
		}
		srv.Handler().ServeHTTP(w, r)
	})
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)

	return &testEnv{
		t: t, http: hs, wsBase: "ws" + strings.TrimPrefix(hs.URL, "http"),
		st: st, cfg: cfg, engine: engine, dataDir: dataDir, receiveDir: receiveDir,
	}
}

// client 是一个带会话令牌的 HTTP 客户端封装。
type client struct {
	t     *testing.T
	env   *testEnv
	token string
	devID string
	name  string
	// remote 为伪造的远端地址；非空表示「局域网内的另一台设备」。
	remote string
}

// newClient 注册一个新会话。
func (e *testEnv) newClient(name, ua, remote string) *client {
	e.t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	req, err := http.NewRequest(http.MethodPost, e.http.URL+"/api/session", bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	if remote != "" {
		req.Header.Set("X-Test-Remote", remote)
	}
	resp, err := e.http.Client().Do(req)
	if err != nil {
		e.t.Fatalf("创建会话失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("创建会话返回 %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Device model.Device `json:"device"`
		Token  string       `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		e.t.Fatal(err)
	}
	if out.Token == "" || out.Device.ID == "" {
		e.t.Fatal("会话响应缺少令牌或设备 ID")
	}
	return &client{t: e.t, env: e, token: out.Token, devID: out.Device.ID, name: name, remote: remote}
}

// do 发起带鉴权的请求。
func (c *client) do(method, path string, body any, extraHeaders map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case []byte:
			reader = bytes.NewReader(v)
		default:
			b, err := json.Marshal(v)
			if err != nil {
				c.t.Fatal(err)
			}
			reader = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequest(method, c.env.http.URL+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.remote != "" {
		req.Header.Set("X-Test-Remote", c.remote)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := c.env.http.Client().Do(req)
	if err != nil {
		c.t.Fatalf("请求 %s %s 失败: %v", method, path, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw
}

// mustJSON 断言状态码并解析 JSON。
func (c *client) mustJSON(method, path string, body any, wantStatus int, dst any) {
	c.t.Helper()
	resp, raw := c.do(method, path, body, nil)
	if resp.StatusCode != wantStatus {
		c.t.Fatalf("%s %s 期望 %d, 实际 %d: %s", method, path, wantStatus, resp.StatusCode, raw)
	}
	if dst != nil {
		if err := json.Unmarshal(raw, dst); err != nil {
			c.t.Fatalf("解析响应失败: %v (%s)", err, raw)
		}
	}
}

// errCode 断言错误响应中的错误码。
func (c *client) errCode(method, path string, body any, wantStatus int, wantCode string) {
	c.t.Helper()
	resp, raw := c.do(method, path, body, nil)
	if resp.StatusCode != wantStatus {
		c.t.Fatalf("%s %s 期望状态 %d, 实际 %d: %s", method, path, wantStatus, resp.StatusCode, raw)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		c.t.Fatalf("错误响应不是合法 JSON: %s", raw)
	}
	if e.Error.Code != wantCode {
		c.t.Fatalf("%s %s 期望错误码 %q, 实际 %q (%s)", method, path, wantCode, e.Error.Code, raw)
	}
	if e.Error.Message == "" {
		c.t.Fatalf("错误响应缺少面向用户的文案: %s", raw)
	}
}

// errCodeH 与 errCode 相同，但可附加自定义请求头（例如声明分块哈希）。
func (c *client) errCodeH(method, path string, body any, headers map[string]string,
	wantStatus int, wantCode string) {
	c.t.Helper()
	resp, raw := c.do(method, path, body, headers)
	if resp.StatusCode != wantStatus {
		c.t.Fatalf("%s %s 期望状态 %d, 实际 %d: %s", method, path, wantStatus, resp.StatusCode, raw)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		c.t.Fatalf("错误响应不是合法 JSON: %s", raw)
	}
	if e.Error.Code != wantCode {
		c.t.Fatalf("%s %s 期望错误码 %q, 实际 %q (%s)", method, path, wantCode, e.Error.Code, raw)
	}
	if e.Error.Message == "" {
		c.t.Fatalf("错误响应缺少面向用户的文案: %s", raw)
	}
}

// wsClient 是测试用的 WebSocket 客户端。
type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
	mu   chan struct{}
	seen []protocol.Envelope
}

// dialWS 建立 WebSocket 连接并完成 client.hello 握手。
func (c *client) dialWS() *wsClient {
	c.t.Helper()
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial(c.env.wsBase+"/ws", nil)
	if err != nil {
		c.t.Fatalf("WebSocket 连接失败: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{
		"type": "client.hello",
		"data": map[string]any{"token": c.token},
	}); err != nil {
		c.t.Fatalf("发送 hello 失败: %v", err)
	}
	wc := &wsClient{t: c.t, conn: conn, mu: make(chan struct{}, 1)}
	wc.mu <- struct{}{}
	c.t.Cleanup(func() { conn.Close() })
	// 读取握手回执，顺带验证鉴权成功。
	if _, ok := wc.waitFor(protocol.EvHello, 3*time.Second); !ok {
		c.t.Fatal("未收到 server.hello，握手可能失败")
	}
	return wc
}

// waitFor 等待指定类型的事件（同时把路过的事件记录下来，便于后续断言）。
func (w *wsClient) waitFor(event string, timeout time.Duration) (protocol.Envelope, bool) {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, e := range w.seen {
			if e.Type == event {
				return e, true
			}
		}
		if time.Now().After(deadline) {
			return protocol.Envelope{}, false
		}
		_ = w.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		var env protocol.Envelope
		if err := w.conn.ReadJSON(&env); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				return protocol.Envelope{}, false
			}
			continue
		}
		w.seen = append(w.seen, env)
	}
}

// waitForDevice 等待针对指定设备的事件。
func waitForDevice(w *wsClient, event, deviceID string, timeout time.Duration) bool {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, e := range w.seen {
			if e.Type == event && e.DeviceID == deviceID {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		_ = w.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		var env protocol.Envelope
		if err := w.conn.ReadJSON(&env); err != nil {
			continue
		}
		w.seen = append(w.seen, env)
	}
}

// sawEvent 判断是否观察到过某类事件。
func (w *wsClient) sawEvent(event string) bool {
	for _, e := range w.seen {
		if e.Type == event {
			return true
		}
	}
	return false
}

// chunkSize 返回测试环境的真实分块大小。
func (e *testEnv) chunkSize() int64 { return e.cfg.Get().ChunkSize }

// content 生成确定性测试内容。
func content(n int) []byte {
	b := make([]byte, n)
	var x uint32 = 0xA5A5A5A5
	for i := range b {
		x = x*1103515245 + 12345
		b[i] = byte(x >> 17)
	}
	return b
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// createTask 通过 API 创建任务并返回任务 ID。
func (c *client) createTask(receiverIDs []string, files []map[string]any, extra map[string]any) string {
	c.t.Helper()
	body := map[string]any{"receiverIds": receiverIDs, "files": files}
	for k, v := range extra {
		body[k] = v
	}
	var out struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	c.mustJSON(http.MethodPost, "/api/transfers", body, http.StatusCreated, &out)
	if len(out.Tasks) == 0 {
		c.t.Fatal("创建任务未返回任务 ID")
	}
	return out.Tasks[0].ID
}

// uploadChunks 通过 HTTP 上传整个文件内容。
func (c *client) uploadChunks(taskID string, fileIndex int, data []byte) {
	c.t.Helper()
	cs := c.env.chunkSize()
	for off := int64(0); off < int64(len(data)); off += cs {
		end := off + cs
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		part := data[off:end]
		path := fmt.Sprintf("/api/transfers/%s/files/%d/chunks/%d", taskID, fileIndex, off/cs)
		resp, raw := c.do(http.MethodPost, path, part, map[string]string{
			"X-Chunk-SHA256": shaHex(part),
		})
		if resp.StatusCode != http.StatusOK {
			c.t.Fatalf("上传分块失败 %d: %d %s", off/cs, resp.StatusCode, raw)
		}
	}
}

// ---------------------------------------------------------------- 基础接口 ---

func TestHealthRequiresNoAuth(t *testing.T) {
	env := newEnv(t)
	resp, err := env.http.Client().Get(env.http.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("健康检查应返回 200, 实际 %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "ok" {
		t.Fatalf("健康状态异常: %v", out)
	}
	if _, ok := out["uptimeSec"]; !ok {
		t.Fatal("健康响应缺少运行时长")
	}
}

func TestAuthRequiredAndTokenValidation(t *testing.T) {
	env := newEnv(t)
	// 无令牌
	resp, err := env.http.Client().Get(env.http.URL + "/api/devices")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌访问应返回 401, 实际 %d", resp.StatusCode)
	}
	// 伪造令牌
	req, _ := http.NewRequest(http.MethodGet, env.http.URL+"/api/devices", nil)
	req.Header.Set("Authorization", "Bearer 伪造的令牌")
	resp2, err := env.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("伪造令牌应返回 401, 实际 %d", resp2.StatusCode)
	}
	// 合法令牌
	c := env.newClient("合法设备", "Mozilla/5.0 (Windows NT 10.0) Chrome/120", "192.168.1.10:5000")
	var out map[string]any
	c.mustJSON(http.MethodGet, "/api/devices", nil, http.StatusOK, &out)
	if out["meId"] != c.devID {
		t.Fatalf("meId 不符: %v != %s", out["meId"], c.devID)
	}
}

func TestUserAgentParsingAndDeviceClassification(t *testing.T) {
	env := newEnv(t)
	cases := []struct {
		name     string
		ua       string
		wantOS   string
		wantType model.DeviceType
	}{
		{"Windows 桌面", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120 Safari/537.36", "Windows", model.DeviceDesktop},
		{"iPhone", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Version/17.0 Mobile/15E148 Safari/604.1", "iOS", model.DeviceMobile},
		{"Android 手机", "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/120 Mobile Safari/537.36", "Android", model.DeviceMobile},
		{"iPad", "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Version/17.0 Safari/604.1", "iOS", model.DeviceTablet},
		{"macOS", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/17.0 Safari/605.1.15", "macOS", model.DeviceDesktop},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := env.newClient("", c.ua, "192.168.1.77:5000")
			var out struct {
				Devices []map[string]any `json:"devices"`
			}
			cl.mustJSON(http.MethodGet, "/api/devices", nil, http.StatusOK, &out)
			var found map[string]any
			for _, d := range out.Devices {
				if d["id"] == cl.devID {
					found = d
				}
			}
			if found == nil {
				t.Fatal("未找到自身设备")
			}
			if found["os"] != c.wantOS {
				t.Fatalf("操作系统解析错误: %v (期望 %s)", found["os"], c.wantOS)
			}
			if found["type"] != string(c.wantType) {
				t.Fatalf("设备类型解析错误: %v (期望 %s)", found["type"], c.wantType)
			}
			// 远程设备的名称应自动生成而不是空
			if found["name"] == "" {
				t.Fatal("设备名不应为空")
			}
		})
	}
}

func TestUnauthenticatedCannotForgeDeviceIdentity(t *testing.T) {
	env := newEnv(t)
	a := env.newClient("A", "ua-a", "192.168.1.10:1")
	b := env.newClient("B", "ua-b", "192.168.1.11:1")
	if a.token == b.token || a.devID == b.devID {
		t.Fatal("不同会话必须拥有不同的设备 ID 与令牌")
	}
	// 同名设备也必须拥有不同会话标识
	a2 := env.newClient("A", "ua-c", "192.168.1.12:1")
	if a2.devID == a.devID {
		t.Fatal("同名设备不应共享会话标识")
	}
	// 令牌不能由客户端指定：用 POST /api/session 传入 token 字段应被拒绝（未知字段）
	body, _ := json.Marshal(map[string]string{"token": "我想指定的令牌"})
	resp, err := env.http.Client().Post(env.http.URL+"/api/session", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("客户端不应能指定会话令牌")
	}
}

// ---------------------------------------------------------------- 端到端流程 ---

func TestEndToEndHTTPTransfer(t *testing.T) {
	env := newEnv(t)
	sender := env.newClient("笔记本", "Mozilla/5.0 (Windows NT 10.0) Chrome/120", "192.168.1.20:4000")
	receiver := env.newClient("手机", "Mozilla/5.0 (Linux; Android 14; Pixel 8) Chrome/120 Mobile Safari/537.36", "192.168.1.21:4000")

	// 接收方建立 WebSocket，用于验证实时事件
	ws := receiver.dialWS()

	// 真实文件内容：跨越多分块
	data := content(int(env.chunkSize()*2 + 7777))
	taskID := sender.createTask([]string{receiver.devID}, []map[string]any{{
		"name":    "季度报告.pdf",
		"relPath": "季度报告.pdf",
		"size":    len(data),
		"mime":    "application/pdf",
		"modTime": time.Now().UnixMilli(),
	}}, map[string]any{"note": "端到端测试"})

	// 1) 接收方必须收到真实的传输请求
	reqEv, ok := ws.waitFor(protocol.EvTransferRequested, 5*time.Second)
	if !ok {
		t.Fatal("接收方未收到 transfer.requested")
	}
	if reqEv.TaskID != taskID {
		t.Fatalf("事件任务 ID 不符: %s != %s", reqEv.TaskID, taskID)
	}

	// 2) 接受前的任务状态必须是 awaiting，且接收方可以查看详情
	var detail struct {
		Task map[string]any `json:"task"`
	}
	receiver.mustJSON(http.MethodGet, "/api/transfers/"+taskID, nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusAwaiting) {
		t.Fatalf("确认前状态应为 awaiting, 实际 %v", detail.Task["status"])
	}

	// 3) 接受
	receiver.mustJSON(http.MethodPost, "/api/transfers/"+taskID+"/accept",
		map[string]any{"conflict": "rename"}, http.StatusOK, nil)
	if _, ok := ws.waitFor(protocol.EvTransferQueued, 5*time.Second); !ok {
		t.Fatal("接受后未收到 transfer.queued（发送方据此开始上传）")
	}

	// 4) 发送方上传全部分块
	sender.uploadChunks(taskID, 0, data)

	// 5) 任务完成且通过校验
	sender.mustJSON(http.MethodGet, "/api/transfers/"+taskID, nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusCompleted) {
		t.Fatalf("任务应完成, 实际 %v (%v)", detail.Task["status"], detail.Task["error"])
	}
	if detail.Task["verified"] != true {
		t.Fatal("已完成任务必须标记为已校验")
	}
	if _, ok := ws.waitFor(protocol.EvTransferCompleted, 5*time.Second); !ok {
		t.Fatal("未收到 transfer.completed")
	}

	// 6) 分块状态查询应显示无缺失
	var cst struct {
		Files []struct {
			Missing []int  `json:"missing"`
			Status  string `json:"status"`
		} `json:"files"`
	}
	sender.mustJSON(http.MethodGet, "/api/transfers/"+taskID+"/chunks", nil, http.StatusOK, &cst)
	if len(cst.Files) != 1 || len(cst.Files[0].Missing) != 0 {
		t.Fatalf("完成后不应有缺失分块: %+v", cst.Files)
	}

	// 7) 接收方取下载票据并真实下载，校验字节完全一致
	var tk struct {
		Ticket string `json:"ticket"`
		URL    string `json:"url"`
		Name   string `json:"name"`
	}
	receiver.mustJSON(http.MethodPost, "/api/transfers/"+taskID+"/files/0/ticket", nil, http.StatusOK, &tk)
	if tk.Ticket == "" || tk.URL == "" {
		t.Fatal("票据签发失败")
	}
	dlResp, err := env.http.Client().Get(env.http.URL + tk.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(dlResp.Body)
	dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK {
		t.Fatalf("下载失败: %d", dlResp.StatusCode)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("下载内容与源文件不一致 (%d != %d 字节)", len(got), len(data))
	}
	if cd := dlResp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("下载响应缺少 attachment: %s", cd)
	}
	if dlResp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatal("下载响应应声明支持 Range，以实现续传下载")
	}

	// 8) 服务端应把文件真实落盘，内容与源一致
	matches, _ := filepath.Glob(filepath.Join(env.receiveDir, "*", "笔记本", "*.pdf"))
	if len(matches) == 0 {
		t.Fatal("接收目录中未找到落盘文件")
	}
	onDisk, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, data) {
		t.Fatal("落盘文件内容与源不一致")
	}

	// 9) 历史记录应包含正确结果
	var hist struct {
		Entries []map[string]any `json:"entries"`
		Total   int              `json:"total"`
	}
	sender.mustJSON(http.MethodGet, "/api/history", nil, http.StatusOK, &hist)
	if len(hist.Entries) == 0 {
		t.Fatal("历史记录为空")
	}
	found := false
	for _, e := range hist.Entries {
		if e["taskId"] == taskID {
			found = true
			if e["status"] != string(model.StatusCompleted) {
				t.Fatalf("历史状态应为 completed, 实际 %v", e["status"])
			}
			if e["direction"] != "out" {
				t.Fatalf("发送方视角方向应为 out, 实际 %v", e["direction"])
			}
			if e["verified"] != true {
				t.Fatal("历史记录应标记为已校验")
			}
			if e["size"].(float64) != float64(len(data)) {
				t.Fatalf("历史大小不符: %v", e["size"])
			}
		}
	}
	if !found {
		t.Fatal("历史记录中未找到该任务")
	}

	// 10) 接收方视角方向应为 in
	receiver.mustJSON(http.MethodGet, "/api/history?dir=in", nil, http.StatusOK, &hist)
	hasIn := false
	for _, e := range hist.Entries {
		if e["taskId"] == taskID && e["direction"] == "in" {
			hasIn = true
		}
	}
	if !hasIn {
		t.Fatal("接收方按 in 筛选应能看到该记录")
	}
}

func TestResumeOverHTTPAfterInterruption(t *testing.T) {
	env := newEnv(t)
	sender := env.newClient("S", "ua", "192.168.1.30:1")
	receiver := env.newClient("R", "ua", "192.168.1.31:1")
	data := content(int(env.chunkSize()*3 + 100)) // 4 个分块

	taskID := sender.createTask([]string{receiver.devID}, []map[string]any{
		{"name": "resume.bin", "size": len(data)},
	}, nil)
	receiver.mustJSON(http.MethodPost, "/api/transfers/"+taskID+"/accept",
		map[string]any{"conflict": "rename"}, http.StatusOK, nil)

	// 只传前 2 块后中断
	cs := env.chunkSize()
	for i := 0; i < 2; i++ {
		part := data[int64(i)*cs : int64(i+1)*cs]
		path := fmt.Sprintf("/api/transfers/%s/files/0/chunks/%d", taskID, i)
		resp, raw := sender.do(http.MethodPost, path, part, map[string]string{"X-Chunk-SHA256": shaHex(part)})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("上传失败: %s", raw)
		}
	}

	// 查询续传状态：必须精确报告缺失的 [2,3]
	var cst struct {
		Files []struct {
			Missing    []int `json:"missing"`
			ChunkCount int   `json:"chunkCount"`
		} `json:"files"`
		Resumable bool `json:"resumable"`
	}
	sender.mustJSON(http.MethodGet, "/api/transfers/"+taskID+"/chunks", nil, http.StatusOK, &cst)
	if !cst.Resumable {
		t.Fatal("中断的任务应支持续传")
	}
	if len(cst.Files[0].Missing) != 2 || cst.Files[0].Missing[0] != 2 || cst.Files[0].Missing[1] != 3 {
		t.Fatalf("缺失分块应为 [2 3], 实际 %v", cst.Files[0].Missing)
	}

	// 补传缺失部分
	for _, i := range []int{2, 3} {
		off := int64(i) * cs
		end := off + cs
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		part := data[off:end]
		path := fmt.Sprintf("/api/transfers/%s/files/0/chunks/%d", taskID, i)
		resp, raw := sender.do(http.MethodPost, path, part, map[string]string{"X-Chunk-SHA256": shaHex(part)})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("补传失败: %s", raw)
		}
	}

	var detail struct {
		Task map[string]any `json:"task"`
	}
	sender.mustJSON(http.MethodGet, "/api/transfers/"+taskID, nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusCompleted) {
		t.Fatalf("续传后应完成, 实际 %v", detail.Task["status"])
	}
}

// ---------------------------------------------------------------- 越权 ---

func TestCrossDeviceAuthorizationEnforced(t *testing.T) {
	env := newEnv(t)
	sender := env.newClient("发送方", "ua", "192.168.1.40:1")
	receiver := env.newClient("接收方", "ua", "192.168.1.41:1")
	attacker := env.newClient("旁观者", "ua", "192.168.1.42:1")

	data := content(2048)
	taskID := sender.createTask([]string{receiver.devID}, []map[string]any{
		{"name": "secret.txt", "size": len(data)},
	}, nil)
	receiver.mustJSON(http.MethodPost, "/api/transfers/"+taskID+"/accept",
		map[string]any{"conflict": "rename"}, http.StatusOK, nil)
	sender.uploadChunks(taskID, 0, data)

	// 前提校验：旁观者必须确实是非特权设备，否则本测试无意义
	var cfgOut struct {
		Privileged bool `json:"privileged"`
	}
	attacker.mustJSON(http.MethodGet, "/api/config", nil, http.StatusOK, &cfgOut)
	if cfgOut.Privileged {
		t.Fatal("远程设备不应被判定为拥有管理权限")
	}
	// 旁观者不能查看任务详情
	attacker.errCode(http.MethodGet, "/api/transfers/"+taskID, nil, http.StatusForbidden, protocol.CodeForbidden)
	// 旁观者不能接受
	attacker.errCode(http.MethodPost, "/api/transfers/"+taskID+"/accept",
		map[string]any{"conflict": "rename"}, http.StatusForbidden, protocol.CodeForbidden)
	// 旁观者不能取消
	attacker.errCode(http.MethodPost, "/api/transfers/"+taskID+"/cancel", nil, http.StatusForbidden, protocol.CodeForbidden)
	// 旁观者不能上传分块（冒充发送方）
	attacker.errCode(http.MethodPost, fmt.Sprintf("/api/transfers/%s/files/0/chunks/0", taskID),
		data, http.StatusForbidden, protocol.CodeForbidden)
	// 旁观者不能签发下载票据
	attacker.errCode(http.MethodPost, fmt.Sprintf("/api/transfers/%s/files/0/ticket", taskID),
		nil, http.StatusForbidden, protocol.CodeForbidden)
	// 旁观者不能在自己的任务列表里看到该任务
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	attacker.mustJSON(http.MethodGet, "/api/transfers", nil, http.StatusOK, &list)
	for _, task := range list.Tasks {
		if task["id"] == taskID {
			t.Fatal("旁观者看到了不属于自己的任务")
		}
	}
	// 发送方不能下载（只有接收方可以）
	sender.errCode(http.MethodPost, fmt.Sprintf("/api/transfers/%s/files/0/ticket", taskID),
		nil, http.StatusForbidden, protocol.CodeForbidden)

	// 伪造票据不能下载
	resp, err := env.http.Client().Get(env.http.URL + "/api/download?ticket=伪造票据")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("伪造票据应被拒绝, 实际 %d", resp.StatusCode)
	}

	// 接收方可以正常签发并下载
	var tk struct {
		URL string `json:"url"`
	}
	receiver.mustJSON(http.MethodPost, fmt.Sprintf("/api/transfers/%s/files/0/ticket", taskID),
		nil, http.StatusOK, &tk)
	if tk.URL == "" {
		t.Fatal("接收方无法获取下载票据")
	}
}

func TestChunkValidationAtHTTPLayer(t *testing.T) {
	env := newEnv(t)
	sender := env.newClient("S", "ua", "192.168.1.50:1")
	receiver := env.newClient("R", "ua", "192.168.1.51:1")
	data := content(4096)
	taskID := sender.createTask([]string{receiver.devID}, []map[string]any{
		{"name": "v.bin", "size": len(data)},
	}, nil)
	receiver.mustJSON(http.MethodPost, "/api/transfers/"+taskID+"/accept",
		map[string]any{"conflict": "rename"}, http.StatusOK, nil)

	// 非法分块索引
	sender.errCode(http.MethodPost, fmt.Sprintf("/api/transfers/%s/files/0/chunks/9999", taskID),
		data, http.StatusBadRequest, protocol.CodeInvalidChunkIndex)
	// 非数字分块索引
	sender.errCode(http.MethodPost, fmt.Sprintf("/api/transfers/%s/files/0/chunks/abc", taskID),
		data, http.StatusBadRequest, protocol.CodeInvalidChunkIndex)
	// 非法文件索引：该任务下不存在该文件
	sender.errCode(http.MethodPost, fmt.Sprintf("/api/transfers/%s/files/99/chunks/0", taskID),
		data, http.StatusNotFound, protocol.CodeNotFound)
	// 分块哈希不符：声明一个与内容不匹配的哈希
	sender.errCodeH(http.MethodPost, fmt.Sprintf("/api/transfers/%s/files/0/chunks/0", taskID),
		data, map[string]string{"X-Chunk-SHA256": shaHex([]byte("这不是这段内容的哈希"))},
		http.StatusBadRequest, protocol.CodeChunkHashMismatch)
	// 超长请求体：声明 4096 字节却发送 1MB
	huge := content(1 << 20)
	sender.errCode(http.MethodPost, fmt.Sprintf("/api/transfers/%s/files/0/chunks/0", taskID),
		huge, http.StatusBadRequest, protocol.CodeBadRequest)
	// 分块长度不足
	sender.errCode(http.MethodPost, fmt.Sprintf("/api/transfers/%s/files/0/chunks/0", taskID),
		data[:100], http.StatusBadRequest, protocol.CodeBadRequest)
	// 长度超出声明（比声明多一个字节也应被拒绝）
	sender.errCode(http.MethodPost, fmt.Sprintf("/api/transfers/%s/files/0/chunks/0", taskID),
		append(append([]byte{}, data...), 0x00), http.StatusBadRequest, protocol.CodeBadRequest)

	// 合法分块仍然可以上传
	sender.uploadChunks(taskID, 0, data)
	var detail struct {
		Task map[string]any `json:"task"`
	}
	sender.mustJSON(http.MethodGet, "/api/transfers/"+taskID, nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusCompleted) {
		t.Fatalf("合法上传后应完成: %v", detail.Task["status"])
	}
}

func TestTaskNotFoundAndInvalidStateResponses(t *testing.T) {
	env := newEnv(t)
	c := env.newClient("C", "ua", "192.168.1.60:1")
	c.errCode(http.MethodGet, "/api/transfers/不存在的任务", nil, http.StatusNotFound, protocol.CodeNotFound)
	c.errCode(http.MethodPost, "/api/transfers/不存在的任务/cancel", nil, http.StatusNotFound, protocol.CodeNotFound)
	c.errCode(http.MethodPost, "/api/transfers/不存在的任务/accept",
		map[string]any{"conflict": "rename"}, http.StatusNotFound, protocol.CodeNotFound)
}

// ---------------------------------------------------------------- 设置与特权 ---

func TestSettingsRequirePrivilegeAndValidate(t *testing.T) {
	env := newEnv(t)
	admin := env.newClient("主机", "ua", "")               // 回环 => 特权
	guest := env.newClient("访客", "ua", "192.168.1.70:1") // 远程 => 非特权

	// 访客读取设置：拿到但不是完整信息
	var gs struct {
		Settings map[string]any `json:"settings"`
		CanEdit  bool           `json:"canEdit"`
	}
	guest.mustJSON(http.MethodGet, "/api/settings", nil, http.StatusOK, &gs)
	if gs.CanEdit {
		t.Fatal("远程访客不应拥有编辑权限")
	}
	if gs.Settings["receiveDir"] != "" || gs.Settings["tempDir"] != "" {
		t.Fatal("非特权设备不应看到主机路径")
	}

	// 访客不能修改设置
	guest.errCode(http.MethodPatch, "/api/settings", map[string]any{"maxConcurrentTransfers": 8},
		http.StatusForbidden, protocol.CodeForbidden)

	// 特权设备可以修改，并立即生效
	var as struct {
		Settings     map[string]any `json:"settings"`
		NeedsRestart bool           `json:"needsRestart"`
	}
	admin.mustJSON(http.MethodPatch, "/api/settings", map[string]any{"maxConcurrentTransfers": 5},
		http.StatusOK, &as)
	if as.Settings["maxConcurrentTransfers"].(float64) != 5 {
		t.Fatalf("设置未生效: %v", as.Settings["maxConcurrentTransfers"])
	}
	if env.cfg.Get().MaxConcurrent != 5 {
		t.Fatal("引擎未同步到新配置")
	}

	// 非法值必须被拒绝且不改变配置
	admin.errCode(http.MethodPatch, "/api/settings", map[string]any{"maxConcurrentTransfers": 999},
		http.StatusBadRequest, protocol.CodeBadRequest)
	if env.cfg.Get().MaxConcurrent != 5 {
		t.Fatal("非法设置不应生效")
	}
	admin.errCode(http.MethodPatch, "/api/settings", map[string]any{"chunkSize": 1},
		http.StatusBadRequest, protocol.CodeBadRequest)
	admin.errCode(http.MethodPatch, "/api/settings", map[string]any{"receiveDir": "相对路径"},
		http.StatusBadRequest, protocol.CodeBadRequest)
	admin.errCode(http.MethodPatch, "/api/settings", map[string]any{"不存在的项": 1},
		http.StatusBadRequest, protocol.CodeBadRequest)
	// 端口变更应提示需要重启
	admin.mustJSON(http.MethodPatch, "/api/settings", map[string]any{"port": 9000},
		http.StatusOK, &as)
	if !as.NeedsRestart {
		t.Fatal("修改端口应提示需要重启")
	}
	// 改回原端口
	admin.mustJSON(http.MethodPatch, "/api/settings", map[string]any{"port": 8787},
		http.StatusOK, &as)
}

func TestDeviceRenameAndPrivilege(t *testing.T) {
	env := newEnv(t)
	admin := env.newClient("主机", "ua", "")
	guest := env.newClient("访客", "ua", "192.168.1.80:1")

	// 改名自己
	var out struct {
		Device map[string]any `json:"device"`
	}
	guest.mustJSON(http.MethodPatch, "/api/devices/"+guest.devID, map[string]any{"name": "我的手机"},
		http.StatusOK, &out)
	if out.Device["name"] != "我的手机" {
		t.Fatalf("改名失败: %v", out.Device["name"])
	}
	// 改名别人：非特权应被拒绝
	guest.errCode(http.MethodPatch, "/api/devices/"+admin.devID, map[string]any{"name": "篡改"},
		http.StatusForbidden, protocol.CodeForbidden)
	// 特权可以改名别人
	admin.mustJSON(http.MethodPatch, "/api/devices/"+guest.devID, map[string]any{"name": "访客手机"},
		http.StatusOK, &out)
	// 非法名称应被拒绝
	admin.errCode(http.MethodPatch, "/api/devices/"+guest.devID, map[string]any{"name": ""},
		http.StatusBadRequest, protocol.CodeInvalidFileName)
	// 改名不存在的设备
	admin.errCode(http.MethodPatch, "/api/devices/不存在", map[string]any{"name": "x"},
		http.StatusNotFound, protocol.CodeNotFound)
}

func TestTrustRevocationDisconnectsDevice(t *testing.T) {
	env := newEnv(t)
	admin := env.newClient("主机", "ua", "")
	guest := env.newClient("访客", "ua", "192.168.1.90:1")
	ws := guest.dialWS()

	// 授予信任
	admin.mustJSON(http.MethodPost, "/api/devices/"+guest.devID+"/trust", nil, http.StatusOK, nil)
	// 撤销信任：连接应被断开
	admin.mustJSON(http.MethodDelete, "/api/devices/"+guest.devID+"/trust", nil, http.StatusOK, nil)

	// 等待连接关闭（服务端主动断开）
	closed := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = ws.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if _, _, err := ws.conn.ReadMessage(); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) ||
				strings.Contains(err.Error(), "close") {
				closed = true
				break
			}
		}
	}
	if !closed {
		t.Fatal("撤销信任后未断开该设备的实时连接")
	}
}

// ---------------------------------------------------------------- WebSocket ---

func TestWebSocketRejectsBadHandshake(t *testing.T) {
	env := newEnv(t)
	// 不带 hello 直接连：服务端应在超时后关闭
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial(env.wsBase+"/ws", nil)
	if err != nil {
		t.Fatalf("连接建立失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return // 连接被关闭即为预期行为
		}
	}
}

func TestWebSocketRejectsInvalidToken(t *testing.T) {
	env := newEnv(t)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial(env.wsBase+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.WriteJSON(map[string]any{"type": "client.hello", "data": map[string]any{"token": "错误令牌"}})
	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	sawError := false
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			// 连接最终必须被关闭
			if !sawError {
				t.Fatal("错误令牌下未收到鉴权失败消息就断开了")
			}
			return
		}
		var env protocol.Envelope
		if json.Unmarshal(raw, &env) == nil && env.Type == "server.error" {
			sawError = true
		}
	}
}

func TestDeviceOnlineOfflineEvents(t *testing.T) {
	env := newEnv(t)
	observer := env.newClient("观察者", "ua", "192.168.1.100:1")
	subject := env.newClient("被观察者", "ua", "192.168.1.101:1")

	ws := observer.dialWS()
	subjectWs := subject.dialWS()

	// 被观察者上线：观察者应收到针对该设备的 device.online
	// （观察者自身建立连接时也会广播自己的上线事件，因此必须按设备 ID 匹配）
	if !waitForDevice(ws, protocol.EvDeviceOnline, subject.devID, 5*time.Second) {
		t.Fatal("未收到被观察者的 device.online")
	}

	// 关闭该设备的所有连接 -> 应广播 device.offline
	subjectWs.conn.Close()
	if !waitForDevice(ws, protocol.EvDeviceOffline, subject.devID, 10*time.Second) {
		t.Fatal("未收到被观察者的 device.offline")
	}

	// 设备列表中的在线状态应随之更新
	subject2 := env.newClient("被观察者2", "ua", "192.168.1.102:1")
	subject2.dialWS()
	var list struct {
		Devices     []map[string]any `json:"devices"`
		OnlineCount int              `json:"onlineCount"`
	}
	observer.mustJSON(http.MethodGet, "/api/devices", nil, http.StatusOK, &list)
	online := map[string]bool{}
	for _, d := range list.Devices {
		online[d["id"].(string)] = d["online"].(bool)
	}
	if !online[observer.devID] {
		t.Fatal("观察者自身应为在线")
	}
	if !online[subject2.devID] {
		t.Fatal("新连接设备应为在线")
	}
	if online[subject.devID] {
		t.Fatal("已断开设备不应显示在线")
	}
}

// ---------------------------------------------------------------- 临时接收箱 ---

func TestDropboxLifecycleAndExpiryEnforcedByBackend(t *testing.T) {
	env := newEnv(t)
	admin := env.newClient("主机", "ua", "")
	visitor := env.newClient("访客手机", "ua", "192.168.1.110:1")

	// 启用 1 分钟有效期
	var db struct {
		Enabled   bool   `json:"enabled"`
		Token     string `json:"token"`
		UploadURL string `json:"uploadUrl"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	admin.mustJSON(http.MethodPost, "/api/dropbox/enable", map[string]any{"ttlMinutes": 1},
		http.StatusOK, &db)
	if !db.Enabled || db.Token == "" || db.UploadURL == "" {
		t.Fatalf("临时入口启用失败: %+v", db)
	}
	if db.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatal("有效期不应已过期")
	}

	// 访客用令牌上传：应自动接受并完成
	data := content(3000)
	var created struct {
		Tasks []map[string]any `json:"tasks"`
	}
	visitor.mustJSON(http.MethodPost, "/api/transfers", map[string]any{
		"dropboxToken": db.Token,
		"files":        []map[string]any{{"name": "临时上传.txt", "size": len(data)}},
	}, http.StatusCreated, &created)
	if len(created.Tasks) != 1 {
		t.Fatal("临时入口应创建 1 个任务")
	}
	taskID := created.Tasks[0]["id"].(string)
	// 临时入口跳过人工确认，直接可上传
	visitor.uploadChunks(taskID, 0, data)

	var detail struct {
		Task map[string]any `json:"task"`
	}
	visitor.mustJSON(http.MethodGet, "/api/transfers/"+taskID, nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusCompleted) {
		t.Fatalf("临时上传应完成: %v", detail.Task["status"])
	}

	// 统计应反映真实收件数
	var st struct {
		ReceivedCount int   `json:"receivedCount"`
		ReceivedBytes int64 `json:"receivedBytes"`
		Enabled       bool  `json:"enabled"`
	}
	admin.mustJSON(http.MethodGet, "/api/dropbox", nil, http.StatusOK, &st)
	if st.ReceivedCount != 1 || st.ReceivedBytes != int64(len(data)) {
		t.Fatalf("临时入口统计不真实: %+v", st)
	}

	// 关闭后令牌立即失效（后端强制，不只是隐藏按钮）
	admin.mustJSON(http.MethodPost, "/api/dropbox/disable", nil, http.StatusOK, nil)
	visitor.errCode(http.MethodPost, "/api/transfers", map[string]any{
		"dropboxToken": db.Token,
		"files":        []map[string]any{{"name": "x.txt", "size": 10}},
	}, http.StatusGone, protocol.CodeDropboxExpired)

	// 伪造令牌同样被拒绝
	visitor.errCode(http.MethodPost, "/api/transfers", map[string]any{
		"dropboxToken": "伪造令牌",
		"files":        []map[string]any{{"name": "x.txt", "size": 10}},
	}, http.StatusGone, protocol.CodeDropboxExpired)

	// 非特权设备不能启用/关闭临时入口
	visitor.errCode(http.MethodPost, "/api/dropbox/enable", map[string]any{"ttlMinutes": 5},
		http.StatusForbidden, protocol.CodeForbidden)
	visitor.errCode(http.MethodPost, "/api/dropbox/disable", nil,
		http.StatusForbidden, protocol.CodeForbidden)
}

// ---------------------------------------------------------------- 历史与存储 ---

func TestClearHistoryDoesNotDeleteFiles(t *testing.T) {
	env := newEnv(t)
	sender := env.newClient("S", "ua", "192.168.1.120:1")
	receiver := env.newClient("R", "ua", "192.168.1.121:1")
	admin := env.newClient("主机", "ua", "")

	data := content(1500)
	taskID := sender.createTask([]string{receiver.devID}, []map[string]any{
		{"name": "keepme.bin", "size": len(data)},
	}, nil)
	receiver.mustJSON(http.MethodPost, "/api/transfers/"+taskID+"/accept",
		map[string]any{"conflict": "rename"}, http.StatusOK, nil)
	sender.uploadChunks(taskID, 0, data)

	matches, _ := filepath.Glob(filepath.Join(env.receiveDir, "*", "S", "keepme.bin"))
	if len(matches) != 1 {
		t.Fatalf("未找到落盘文件: %v", matches)
	}
	before, _ := os.ReadFile(matches[0])

	// 非特权设备不能清空历史
	sender.errCode(http.MethodPost, "/api/history/clear", nil, http.StatusForbidden, protocol.CodeForbidden)

	// 清空历史
	admin.mustJSON(http.MethodPost, "/api/history/clear", nil, http.StatusOK, nil)

	var hist struct {
		Entries []map[string]any `json:"entries"`
	}
	admin.mustJSON(http.MethodGet, "/api/history", nil, http.StatusOK, &hist)
	if len(hist.Entries) != 0 {
		t.Fatalf("历史应已清空, 实际 %d 条", len(hist.Entries))
	}
	// 关键：磁盘文件必须完好无损
	if _, err := os.Stat(matches[0]); err != nil {
		t.Fatal("清空历史不应删除磁盘文件")
	}
	after, _ := os.ReadFile(matches[0])
	if !bytes.Equal(before, after) {
		t.Fatal("清空历史后文件内容发生变化")
	}
}

func TestHistorySearchAndFilter(t *testing.T) {
	env := newEnv(t)
	sender := env.newClient("S", "ua", "192.168.1.130:1")
	receiver := env.newClient("R", "ua", "192.168.1.131:1")

	// 一个成功任务
	okData := content(500)
	okTask := sender.createTask([]string{receiver.devID}, []map[string]any{
		{"name": "成功文件.txt", "size": len(okData)},
	}, nil)
	receiver.mustJSON(http.MethodPost, "/api/transfers/"+okTask+"/accept",
		map[string]any{"conflict": "rename"}, http.StatusOK, nil)
	sender.uploadChunks(okTask, 0, okData)

	// 一个被拒绝的任务
	rejTask := sender.createTask([]string{receiver.devID}, []map[string]any{
		{"name": "被拒文件.txt", "size": 100},
	}, nil)
	receiver.mustJSON(http.MethodPost, "/api/transfers/"+rejTask+"/reject",
		map[string]any{"reason": "不需要"}, http.StatusOK, nil)

	var out struct {
		Entries []map[string]any `json:"entries"`
	}
	// 关键词搜索
	sender.mustJSON(http.MethodGet, "/api/history?keyword=成功文件", nil, http.StatusOK, &out)
	if len(out.Entries) != 1 || !strings.Contains(out.Entries[0]["fileName"].(string), "成功文件") {
		t.Fatalf("关键词搜索失败: %+v", out.Entries)
	}
	// 状态筛选
	sender.mustJSON(http.MethodGet, "/api/history?status=rejected", nil, http.StatusOK, &out)
	if len(out.Entries) != 1 || out.Entries[0]["status"] != string(model.StatusRejected) {
		t.Fatalf("状态筛选失败: %+v", out.Entries)
	}
	// 非法状态应报错而不是静默忽略
	sender.errCode(http.MethodGet, "/api/history?status=乱写", nil,
		http.StatusBadRequest, protocol.CodeBadRequest)
	// 时间范围（未来时间段应为空）
	sender.mustJSON(http.MethodGet, fmt.Sprintf("/api/history?from=%d", time.Now().Add(time.Hour).UnixMilli()),
		nil, http.StatusOK, &out)
	if len(out.Entries) != 0 {
		t.Fatal("未来时间范围不应有记录")
	}
}

func TestStorageAndCleanupEndpoints(t *testing.T) {
	env := newEnv(t)
	admin := env.newClient("主机", "ua", "")
	guest := env.newClient("访客", "ua", "192.168.1.140:1")

	var out map[string]any
	admin.mustJSON(http.MethodGet, "/api/storage", nil, http.StatusOK, &out)
	if _, ok := out["usage"]; !ok {
		t.Fatal("存储接口缺少 usage")
	}
	if _, ok := out["stats"]; !ok {
		t.Fatal("存储接口缺少 stats")
	}
	usage := out["usage"].(map[string]any)
	if _, ok := usage["diskFreeBytes"]; !ok {
		t.Fatal("存储接口缺少真实磁盘可用空间")
	}

	// 非特权设备不能清理
	guest.errCode(http.MethodPost, "/api/storage/cleanup", nil, http.StatusForbidden, protocol.CodeForbidden)
	admin.mustJSON(http.MethodPost, "/api/storage/cleanup", nil, http.StatusOK, &out)
}

func TestDiagnosticsReturnsRealChecks(t *testing.T) {
	env := newEnv(t)
	c := env.newClient("主机", "ua", "")
	// 建立真实 WebSocket 连接，诊断页应如实反映「本客户端在线」
	c.dialWS()
	var out map[string]any
	c.mustJSON(http.MethodGet, "/api/diagnostics", nil, http.StatusOK, &out)

	if _, ok := out["checkedAt"]; !ok {
		t.Fatal("诊断响应缺少检测时间")
	}
	svc, ok := out["service"].(map[string]any)
	if !ok {
		t.Fatal("诊断响应缺少 service 段")
	}
	for _, k := range []string{"status", "listenAddr", "port", "selfTest", "portNote", "uptimeSec"} {
		if _, ok := svc[k]; !ok {
			t.Fatalf("service 段缺少 %s", k)
		}
	}
	client, ok := out["client"].(map[string]any)
	if !ok {
		t.Fatal("诊断响应缺少 client 段")
	}
	if client["wsOnline"] != true {
		t.Fatal("客户端自身的 WebSocket 状态应为在线（已建立连接）")
	}
	if _, ok := out["storage"]; !ok {
		t.Fatal("诊断响应缺少 storage 段")
	}
	tips, ok := out["tips"].([]any)
	if !ok || len(tips) == 0 {
		t.Fatal("诊断响应缺少排查建议")
	}
	if _, ok := out["addresses"]; !ok {
		t.Fatal("诊断响应缺少地址列表")
	}
}

func TestQRCodeGeneration(t *testing.T) {
	env := newEnv(t)
	resp, err := env.http.Client().Get(env.http.URL + "/api/qrcode.png")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("二维码生成失败: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("二维码类型错误: %s", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) < 100 {
		t.Fatalf("二维码数据过小: %d 字节", len(body))
	}
	// PNG 魔数
	if !bytes.HasPrefix(body, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatal("返回的不是 PNG")
	}
	// 外部 URL 应被拒绝，避免被当作开放二维码生成器
	resp2, err := env.http.Client().Get(env.http.URL + "/api/qrcode.png?url=" + "https://example.com/evil")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("外部 URL 应被拒绝, 实际 %d", resp2.StatusCode)
	}
}

func TestAccessInfoListsRealAddresses(t *testing.T) {
	env := newEnv(t)
	c := env.newClient("主机", "ua", "")
	var out map[string]any
	c.mustJSON(http.MethodGet, "/api/session/access", nil, http.StatusOK, &out)
	if out["port"].(float64) != float64(env.cfg.Get().Port) {
		t.Fatalf("端口不符: %v", out["port"])
	}
	if _, ok := out["addresses"]; !ok {
		t.Fatal("缺少地址列表")
	}
	if out["primaryUrl"] == "" {
		t.Fatal("缺少主访问地址")
	}
	// 至少应包含回环地址
	addrs := out["addresses"].([]any)
	if len(addrs) == 0 {
		t.Fatal("未枚举到任何本机地址")
	}
}

func TestStaticHandlerGuidesWhenFrontendMissing(t *testing.T) {
	env := newEnv(t)
	resp, err := env.http.Client().Get(env.http.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// publicDir 为空：应返回带指引的说明页，而不是 404
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("期望 503 指引页, 实际 %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "npm run build") {
		t.Fatal("指引页应说明如何构建前端")
	}
	// API 路径未命中时应返回真正的 404
	resp2, err := env.http.Client().Get(env.http.URL + "/api/不存在的接口")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("未知 API 应返回 404, 实际 %d", resp2.StatusCode)
	}
}

func TestCreateTransferValidationAtHTTP(t *testing.T) {
	env := newEnv(t)
	c := env.newClient("C", "ua", "192.168.1.150:1")
	other := env.newClient("O", "ua", "192.168.1.151:1")

	// 空文件列表
	c.errCode(http.MethodPost, "/api/transfers", map[string]any{
		"receiverIds": []string{other.devID}, "files": []any{},
	}, http.StatusBadRequest, protocol.CodeBadRequest)
	// 无接收方
	c.errCode(http.MethodPost, "/api/transfers", map[string]any{
		"files": []map[string]any{{"name": "a.txt", "size": 1}},
	}, http.StatusBadRequest, protocol.CodeBadRequest)
	// 不存在的接收方
	c.errCode(http.MethodPost, "/api/transfers", map[string]any{
		"receiverIds": []string{"不存在"}, "files": []map[string]any{{"name": "a.txt", "size": 1}},
	}, http.StatusNotFound, protocol.CodeNotFound)
	// 非法重名策略
	c.errCode(http.MethodPost, "/api/transfers", map[string]any{
		"receiverIds": []string{other.devID}, "conflict": "乱写",
		"files": []map[string]any{{"name": "a.txt", "size": 1}},
	}, http.StatusBadRequest, protocol.CodeBadRequest)
	// 未知字段应被拒绝
	c.errCode(http.MethodPost, "/api/transfers", map[string]any{
		"receiverIds": []string{other.devID}, "files": []map[string]any{{"name": "a.txt", "size": 1}},
		"未定义字段": 1,
	}, http.StatusBadRequest, protocol.CodeBadRequest)
	// 文件超限
	env.cfg.Patch(map[string]any{"maxFileSize": 1000})
	c.errCode(http.MethodPost, "/api/transfers", map[string]any{
		"receiverIds": []string{other.devID}, "files": []map[string]any{{"name": "big.bin", "size": 5000}},
	}, http.StatusRequestEntityTooLarge, protocol.CodeFileTooLarge)
}

func TestQueueAndPauseResumeOverHTTP(t *testing.T) {
	env := newEnv(t, func(s *model.Settings) { s.MaxConcurrent = 1 })
	sender := env.newClient("S", "ua", "192.168.1.160:1")
	r1 := env.newClient("R1", "ua", "192.168.1.161:1")
	r2 := env.newClient("R2", "ua", "192.168.1.162:1")

	data := content(int(env.chunkSize() + 100))
	t1 := sender.createTask([]string{r1.devID}, []map[string]any{{"name": "a.bin", "size": len(data)}}, nil)
	t2 := sender.createTask([]string{r2.devID}, []map[string]any{{"name": "b.bin", "size": len(data)}}, nil)

	var detail struct {
		Task map[string]any `json:"task"`
	}
	r1.mustJSON(http.MethodPost, "/api/transfers/"+t1+"/accept", map[string]any{"conflict": "rename"}, http.StatusOK, nil)
	r2.mustJSON(http.MethodPost, "/api/transfers/"+t2+"/accept", map[string]any{"conflict": "rename"}, http.StatusOK, nil)

	sender.mustJSON(http.MethodGet, "/api/transfers/"+t1, nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusUploading) {
		t.Fatalf("第一个任务应占用槽位: %v", detail.Task["status"])
	}
	sender.mustJSON(http.MethodGet, "/api/transfers/"+t2, nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusQueued) {
		t.Fatalf("第二个任务应排队: %v", detail.Task["status"])
	}

	// 暂停第二个（排队中）任务并继续
	sender.mustJSON(http.MethodPost, "/api/transfers/"+t2+"/pause", nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusPaused) {
		t.Fatalf("暂停后应为 paused: %v", detail.Task["status"])
	}
	sender.mustJSON(http.MethodPost, "/api/transfers/"+t2+"/resume", nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusQueued) {
		t.Fatalf("继续后应为 queued: %v", detail.Task["status"])
	}

	// 完成第一个任务后第二个自动开始
	sender.uploadChunks(t1, 0, data)
	sender.mustJSON(http.MethodGet, "/api/transfers/"+t2, nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusUploading) {
		t.Fatalf("槽位释放后第二个任务应开始: %v", detail.Task["status"])
	}

	// 对已完成任务执行非法操作
	sender.errCode(http.MethodPost, "/api/transfers/"+t1+"/pause", nil,
		http.StatusBadRequest, protocol.CodeInvalidState)
	sender.errCode(http.MethodPost, "/api/transfers/"+t1+"/cancel", nil,
		http.StatusBadRequest, protocol.CodeInvalidState)
}

func TestCancelAndRetryOverHTTP(t *testing.T) {
	env := newEnv(t)
	sender := env.newClient("S", "ua", "192.168.1.170:1")
	receiver := env.newClient("R", "ua", "192.168.1.171:1")
	data := content(int(env.chunkSize() + 50))

	taskID := sender.createTask([]string{receiver.devID}, []map[string]any{
		{"name": "cr.bin", "size": len(data)},
	}, nil)
	receiver.mustJSON(http.MethodPost, "/api/transfers/"+taskID+"/accept",
		map[string]any{"conflict": "rename"}, http.StatusOK, nil)

	// 传 1 块后取消
	cs := env.chunkSize()
	sender.uploadChunks(taskID, 0, data[:cs])

	var detail struct {
		Task map[string]any `json:"task"`
	}
	sender.mustJSON(http.MethodPost, "/api/transfers/"+taskID+"/cancel", nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusCancelled) {
		t.Fatalf("取消后应为 cancelled: %v", detail.Task["status"])
	}

	// 重试后应恢复并保留已传分块
	sender.mustJSON(http.MethodPost, "/api/transfers/"+taskID+"/retry", nil, http.StatusOK, &detail)
	st := detail.Task["status"].(string)
	if st != string(model.StatusUploading) && st != string(model.StatusQueued) {
		t.Fatalf("重试后状态异常: %s", st)
	}

	var cst struct {
		Files []struct {
			Missing []int `json:"missing"`
		} `json:"files"`
	}
	sender.mustJSON(http.MethodGet, "/api/transfers/"+taskID+"/chunks", nil, http.StatusOK, &cst)
	if len(cst.Files[0].Missing) != 1 || cst.Files[0].Missing[0] != 1 {
		t.Fatalf("重试后应只需补第 2 块, 实际缺失 %v", cst.Files[0].Missing)
	}

	// 补传后完成
	sender.uploadChunks(taskID, 0, data)
	sender.mustJSON(http.MethodGet, "/api/transfers/"+taskID, nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusCompleted) {
		t.Fatalf("补传后应完成: %v", detail.Task["status"])
	}
}

func TestTransferListStatusFilterAndPagination(t *testing.T) {
	env := newEnv(t)
	sender := env.newClient("S", "ua", "192.168.1.180:1")
	receiver := env.newClient("R", "ua", "192.168.1.181:1")

	for i := 0; i < 3; i++ {
		sender.createTask([]string{receiver.devID}, []map[string]any{
			{"name": fmt.Sprintf("file-%d.txt", i), "size": 100},
		}, nil)
	}
	var list struct {
		Tasks    []map[string]any `json:"tasks"`
		Total    int              `json:"total"`
		Page     int              `json:"page"`
		PageSize int              `json:"pageSize"`
	}
	sender.mustJSON(http.MethodGet, "/api/transfers?page=1&pageSize=2", nil, http.StatusOK, &list)
	if len(list.Tasks) != 2 {
		t.Fatalf("分页应返回 2 条, 实际 %d", len(list.Tasks))
	}
	if list.Total != 3 {
		t.Fatalf("总数应为 3, 实际 %d", list.Total)
	}
	// 状态筛选
	sender.mustJSON(http.MethodGet, "/api/transfers?status=awaiting", nil, http.StatusOK, &list)
	if len(list.Tasks) != 3 {
		t.Fatalf("等待确认应为 3 条, 实际 %d", len(list.Tasks))
	}
	sender.mustJSON(http.MethodGet, "/api/transfers?status=completed", nil, http.StatusOK, &list)
	if len(list.Tasks) != 0 {
		t.Fatalf("不应有已完成任务, 实际 %d", len(list.Tasks))
	}
	// 关键词搜索
	sender.mustJSON(http.MethodGet, "/api/transfers?keyword=file-1", nil, http.StatusOK, &list)
	if len(list.Tasks) != 1 {
		t.Fatalf("关键词应命中 1 条, 实际 %d", len(list.Tasks))
	}
}

func TestRejectReasonPropagatesToSender(t *testing.T) {
	env := newEnv(t)
	sender := env.newClient("S", "ua", "192.168.1.190:1")
	receiver := env.newClient("R", "ua", "192.168.1.191:1")
	ws := sender.dialWS()

	taskID := sender.createTask([]string{receiver.devID}, []map[string]any{
		{"name": "r.txt", "size": 10},
	}, nil)
	receiver.mustJSON(http.MethodPost, "/api/transfers/"+taskID+"/reject",
		map[string]any{"reason": "磁盘空间不足"}, http.StatusOK, nil)

	ev, ok := ws.waitFor(protocol.EvTransferRejected, 5*time.Second)
	if !ok {
		t.Fatal("发送方未收到 transfer.rejected")
	}
	data, _ := json.Marshal(ev.Data)
	if !strings.Contains(string(data), "磁盘空间不足") {
		t.Fatalf("拒绝原因未传递给发送方: %s", data)
	}
	var detail struct {
		Task map[string]any `json:"task"`
	}
	sender.mustJSON(http.MethodGet, "/api/transfers/"+taskID, nil, http.StatusOK, &detail)
	if detail.Task["status"] != string(model.StatusRejected) {
		t.Fatalf("状态应为 rejected: %v", detail.Task["status"])
	}
	if detail.Task["errorCode"] != protocol.CodeRejected {
		t.Fatalf("错误码应为 rejected: %v", detail.Task["errorCode"])
	}
}

func TestConflictPoliciesOverHTTP(t *testing.T) {
	env := newEnv(t)
	sender := env.newClient("同名发送", "ua", "192.168.1.200:1")
	receiver := env.newClient("R", "ua", "192.168.1.201:1")
	data := content(800)

	send := func(policy string) string {
		taskID := sender.createTask([]string{receiver.devID}, []map[string]any{
			{"name": "重复.txt", "size": len(data)},
		}, nil)
		receiver.mustJSON(http.MethodPost, "/api/transfers/"+taskID+"/accept",
			map[string]any{"conflict": policy}, http.StatusOK, nil)
		var detail struct {
			Task struct {
				Status string `json:"status"`
				Files  []struct {
					Skipped   bool   `json:"skipped"`
					FinalName string `json:"finalName"`
				} `json:"files"`
			} `json:"task"`
		}
		sender.mustJSON(http.MethodGet, "/api/transfers/"+taskID, nil, http.StatusOK, &detail)
		if detail.Task.Status == string(model.StatusCompleted) {
			return "(直接完成)"
		}
		sender.uploadChunks(taskID, 0, data)
		sender.mustJSON(http.MethodGet, "/api/transfers/"+taskID, nil, http.StatusOK, &detail)
		if detail.Task.Status != string(model.StatusCompleted) {
			t.Fatalf("策略 %s 下任务未完成: %s", policy, detail.Task.Status)
		}
		return detail.Task.Files[0].FinalName
	}

	if n := send("rename"); n != "重复.txt" {
		t.Fatalf("首次应为 重复.txt, 实际 %s", n)
	}
	if n := send("rename"); n != "重复 (1).txt" {
		t.Fatalf("第二次应为 重复 (1).txt, 实际 %s", n)
	}
	// overwrite：沿用原名
	if n := send("overwrite"); n != "重复.txt" {
		t.Fatalf("overwrite 应沿用原名, 实际 %s", n)
	}
	// skip：跳过且任务直接完成
	if n := send("skip"); n != "(直接完成)" {
		t.Fatalf("skip 应直接完成, 实际 %s", n)
	}
	// rename 两次生成两个文件；overwrite 覆盖了第一个；skip 不落盘。
	// 因此磁盘上应恰好存在 2 个文件。
	found, _ := filepath.Glob(filepath.Join(env.receiveDir, "*", "同名发送", "重复*.txt"))
	if len(found) != 2 {
		t.Fatalf("应存在 2 个文件, 实际 %d: %v", len(found), found)
	}
	// overwrite 后第一个文件的路径不变，且必须仍可读
	firstPaths, _ := filepath.Glob(filepath.Join(env.receiveDir, "*", "同名发送", "重复.txt"))
	if len(firstPaths) != 1 {
		t.Fatalf("overwrite 目标文件应恰好 1 个, 实际 %v", firstPaths)
	}
	first, err := os.ReadFile(firstPaths[0])
	if err != nil {
		t.Fatalf("overwrite 后文件不可读: %v", err)
	}
	if len(first) != len(data) {
		t.Fatalf("overwrite 后大小不符: %d != %d", len(first), len(data))
	}
}
