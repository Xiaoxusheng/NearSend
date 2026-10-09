// Package hub 实现 WebSocket 连接管理：会话鉴权、心跳、在线状态广播与定向推送。
//
// 鉴权方式：浏览器无法为 WebSocket 设置自定义请求头，把令牌放进 URL 查询串
// 又会进入各类访问日志。因此改为「连接建立后立即握手」——客户端必须在
// helloTimeout 内发送 client.hello 携带令牌，否则连接被关闭。令牌从不进入 URL。
package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"nearsend/internal/model"
	"nearsend/internal/protocol"
)

const (
	// writeWait 单条消息写超时。
	writeWait = 10 * time.Second
	// pongWait 允许的客户端静默时长，超过则判定掉线。
	pongWait = 60 * time.Second
	// pingPeriod 服务端主动探测间隔，必须显著小于 pongWait。
	pingPeriod = 25 * time.Second
	// helloTimeout 等待 client.hello 的时间上限。
	helloTimeout = 8 * time.Second
	// maxMessageSize 客户端消息上限；本协议客户端只发小控制帧。
	maxMessageSize = 8 << 10
	// sendBuffer 单连接待发队列长度。溢出即判定客户端消费不过来并断开，
	// 避免慢客户端拖垮整个服务。
	sendBuffer = 64
)

// Hooks 是连接状态变化回调，由 api 层注入以便在设备上下线时更新数据库与广播。
type Hooks struct {
	OnOnline  func(deviceID string)
	OnOffline func(deviceID string)
	// OnMessage 处理客户端发来的自定义控制消息（如 transfer.progress 上报）。
	OnMessage func(deviceID string, env protocol.Envelope)
}

// Client 是一条 WebSocket 连接。
type Client struct {
	DeviceID  string
	conn      *websocket.Conn
	send      chan []byte
	hub       *Hub
	closeOnce sync.Once
	done      chan struct{}
	// remoteAddr 仅用于诊断展示，不用于鉴权。
	remoteAddr string
}

// Hub 管理全部在线连接。
type Hub struct {
	mu      sync.RWMutex
	clients map[string]map[*Client]bool // deviceID -> 该设备的全部连接（多标签页）

	upgrader websocket.Upgrader
	hooks    Hooks
	log      *slog.Logger

	// resolveToken 把令牌解析为设备；返回 error 表示鉴权失败。
	resolveToken func(token string) (*model.Device, error)

	closed chan struct{}
	once   sync.Once
}

// New 创建 Hub。resolveToken 必须由调用方提供，Hub 自身不接触数据库。
func New(log *slog.Logger, resolveToken func(string) (*model.Device, error)) *Hub {
	return &Hub{
		clients:      map[string]map[*Client]bool{},
		resolveToken: resolveToken,
		log:          log,
		closed:       make(chan struct{}),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// 同源校验：仅接受来自本服务的页面。局域网内使用 http://<lan-ip>:<port>，
			// 因此比对 Host 即可，不使用通配。
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					// 非浏览器客户端（如测试工具）没有 Origin。
					return true
				}
				host := r.Host
				for _, prefix := range []string{"http://", "https://"} {
					if len(origin) > len(prefix) && origin[:len(prefix)] == prefix {
						return origin[len(prefix):] == host
					}
				}
				return false
			},
		},
	}
}

// SetHooks 注入连接状态回调。
func (h *Hub) SetHooks(hk Hooks) {
	h.mu.Lock()
	h.hooks = hk
	h.mu.Unlock()
}

// HandleWS 是 http.HandlerFunc，完成升级与握手。
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade 内部已写过响应。
		h.log.Debug("websocket upgrade failed", "err", err, "remote", r.RemoteAddr)
		return
	}

	// 等待 client.hello。此阶段不接受任何其它业务消息。
	_ = conn.SetReadDeadline(time.Now().Add(helloTimeout))
	conn.SetReadLimit(maxMessageSize)
	var hello protocol.Envelope
	if err := conn.ReadJSON(&hello); err != nil {
		h.log.Debug("websocket hello read failed", "err", err)
		conn.Close()
		return
	}
	if hello.Type != "client.hello" {
		_ = conn.WriteJSON(protocol.Envelope{Type: "server.error", TS: nowMillis(),
			Data: map[string]string{"code": protocol.CodeUnauthorized, "message": "首帧必须是 client.hello"}})
		conn.Close()
		return
	}
	token := ""
	if m, ok := hello.Data.(map[string]any); ok {
		if s, ok := m["token"].(string); ok {
			token = s
		}
	}
	device, err := h.resolveToken(token)
	if err != nil || device == nil {
		_ = conn.WriteJSON(protocol.Envelope{Type: "server.error", TS: nowMillis(),
			Data: map[string]string{"code": protocol.CodeUnauthorized, "message": protocol.Message[protocol.CodeUnauthorized]}})
		conn.Close()
		return
	}

	c := &Client{
		DeviceID:   device.ID,
		conn:       conn,
		send:       make(chan []byte, sendBuffer),
		hub:        h,
		done:       make(chan struct{}),
		remoteAddr: r.RemoteAddr,
	}
	first := h.register(c)
	if first {
		if cb := h.hookOnline(); cb != nil {
			cb(device.ID)
		}
	}

	go c.writePump()
	go c.readPump()

	// 握手成功后立即下发服务端信息与当前在线快照，前端不必再额外请求。
	h.SendToDevice(device.ID, protocol.Envelope{
		Type: protocol.EvHello,
		TS:   nowMillis(),
		Data: map[string]any{"deviceId": device.ID},
	})
}

func (h *Hub) hookOnline() func(string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.hooks.OnOnline
}

func (h *Hub) hookOffline() func(string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.hooks.OnOffline
}

func (h *Hub) hookMessage() func(string, protocol.Envelope) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.hooks.OnMessage
}

// register 登记连接，返回该设备是否由离线转为在线。
func (h *Hub) register(c *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.clients[c.DeviceID]
	if !ok {
		set = map[*Client]bool{}
		h.clients[c.DeviceID] = set
	}
	wasEmpty := len(set) == 0
	set[c] = true
	return wasEmpty
}

// unregister 移除连接，返回该设备是否已完全离线。
func (h *Hub) unregister(c *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.clients[c.DeviceID]
	if !ok {
		return false
	}
	if _, ok := set[c]; !ok {
		return false
	}
	delete(set, c)
	if len(set) == 0 {
		delete(h.clients, c.DeviceID)
		return true
	}
	return false
}

// IsOnline 判断设备是否有活跃连接。
func (h *Hub) IsOnline(deviceID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients[deviceID]) > 0
}

// OnlineDeviceIDs 返回当前在线的全部设备 ID。
func (h *Hub) OnlineDeviceIDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.clients))
	for id := range h.clients {
		out = append(out, id)
	}
	return out
}

// ConnectionCount 返回连接总数与设备数，用于诊断页展示真实数据。
func (h *Hub) ConnectionCount() (conns int, devices int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, set := range h.clients {
		conns += len(set)
	}
	return conns, len(h.clients)
}

// Broadcast 向所有在线连接推送事件。
func (h *Hub) Broadcast(env protocol.Envelope) {
	b, err := json.Marshal(env)
	if err != nil {
		h.log.Error("broadcast marshal failed", "err", err)
		return
	}
	h.mu.RLock()
	targets := make([]*Client, 0, len(h.clients))
	for _, set := range h.clients {
		for c := range set {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.enqueue(b)
	}
}

// SendToDevice 向指定设备的全部连接推送事件。设备离线时静默丢弃——
// 调用方应通过 IsOnline 判断并给出「对方离线」的业务反馈。
func (h *Hub) SendToDevice(deviceID string, env protocol.Envelope) bool {
	h.mu.RLock()
	set := h.clients[deviceID]
	targets := make([]*Client, 0, len(set))
	for c := range set {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	if len(targets) == 0 {
		return false
	}
	b, err := json.Marshal(env)
	if err != nil {
		h.log.Error("send marshal failed", "err", err)
		return false
	}
	for _, c := range targets {
		c.enqueue(b)
	}
	return true
}

// CloseDevice 强制断开某设备的全部连接（用于撤销信任或移除设备）。
func (h *Hub) CloseDevice(deviceID string) {
	h.mu.RLock()
	set := h.clients[deviceID]
	targets := make([]*Client, 0, len(set))
	for c := range set {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.close()
	}
}

// Shutdown 关闭全部连接。
func (h *Hub) Shutdown() {
	h.once.Do(func() { close(h.closed) })
	h.mu.RLock()
	targets := []*Client{}
	for _, set := range h.clients {
		for c := range set {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.close()
	}
}

func (c *Client) enqueue(b []byte) {
	select {
	case c.send <- b:
	case <-c.done:
	default:
		// 发送队列已满：客户端消费过慢，主动断开以免影响其他用户。
		c.hub.log.Warn("websocket send buffer full, closing", "device", c.DeviceID)
		c.close()
	}
}

func (c *Client) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

// readPump 读取客户端消息并维持 pong 截止时间。
func (c *Client) readPump() {
	defer func() {
		if last := c.hub.unregister(c); last {
			if cb := c.hub.hookOffline(); cb != nil {
				cb(c.DeviceID)
			}
		}
		c.close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		select {
		case <-c.hub.closed:
			return
		case <-c.done:
			return
		default:
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		var env protocol.Envelope
		if err := c.conn.ReadJSON(&env); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.hub.log.Debug("websocket closed", "device", c.DeviceID, "err", err)
			}
			return
		}
		switch env.Type {
		case protocol.EvPong, "client.ping":
			// 心跳，已由读截止时间处理。
			if env.Type == "client.ping" {
				c.enqueue(mustJSON(protocol.Envelope{Type: protocol.EvPong, TS: nowMillis()}))
			}
			continue
		}
		if cb := c.hub.hookMessage(); cb != nil {
			cb(c.DeviceID, env)
		}
	}
}

// writePump 串行发送消息并周期性下发 ping。
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.close()
	}()
	for {
		select {
		case <-c.done:
			return
		case <-c.hub.closed:
			_ = c.conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down"),
				time.Now().Add(writeWait))
			return
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func nowMillis() int64 { return time.Now().UnixMilli() }
