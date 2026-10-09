// Package api 组装 HTTP 路由、会话鉴权与全部 REST 处理函数。
//
// 约定：
//   - 除 /api/health、/api/session、/api/qrcode.png、/api/download 外，
//     所有接口都要求携带 Authorization: Bearer <session token>。
//   - 错误响应统一为 {"error":{"code","message"}}，code 是稳定的机器可读标识。
//   - 权限判定全部在服务端完成，前端隐藏按钮只是体验优化，不构成安全边界。
package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"nearsend/internal/config"
	"nearsend/internal/hub"
	"nearsend/internal/model"
	"nearsend/internal/protocol"
	"nearsend/internal/store"
	"nearsend/internal/transfer"
)

// Server 汇总全部依赖，并作为 http.Handler 使用。
type Server struct {
	st        *store.Store
	cfg       *config.Manager
	hub       *hub.Hub
	engine    *transfer.Engine
	log       *slog.Logger
	version   string
	startedAt time.Time
	publicDir string

	tickets *ticketStore
}

// Options 是构造 Server 的参数。
type Options struct {
	Store     *store.Store
	Config    *config.Manager
	Hub       *hub.Hub
	Engine    *transfer.Engine
	Log       *slog.Logger
	Version   string
	PublicDir string
}

// NewServer 构造 Server。
func NewServer(o Options) *Server {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	s := &Server{
		st:        o.Store,
		cfg:       o.Config,
		hub:       o.Hub,
		engine:    o.Engine,
		log:       o.Log,
		version:   o.Version,
		startedAt: time.Now(),
		publicDir: o.PublicDir,
		tickets:   newTicketStore(),
	}
	o.Hub.SetHooks(hub.Hooks{
		OnOnline:  s.onDeviceOnline,
		OnOffline: s.onDeviceOffline,
		OnMessage: s.onClientMessage,
	})
	return s
}

// Handler 返回完整的路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ---- 无需会话 ----
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/session", s.handleCreateSession)
	mux.HandleFunc("GET /api/qrcode.png", s.handleQRCode)
	mux.HandleFunc("GET /api/download", s.handleDownloadByTicket)
	mux.HandleFunc("GET /ws", s.hub.HandleWS)

	// ---- 会话自身 ----
	mux.Handle("GET /api/session/access", s.auth(http.HandlerFunc(s.handleAccessInfo)))
	mux.Handle("PATCH /api/session", s.auth(http.HandlerFunc(s.handleUpdateSelf)))
	mux.Handle("POST /api/session/claim", s.auth(http.HandlerFunc(s.handleClaimAdmin)))
	mux.Handle("GET /api/config", s.auth(http.HandlerFunc(s.handleGetConfig)))

	// ---- 设备 ----
	mux.Handle("GET /api/devices", s.auth(http.HandlerFunc(s.handleListDevices)))
	mux.Handle("PATCH /api/devices/{id}", s.auth(http.HandlerFunc(s.handlePatchDevice)))
	mux.Handle("POST /api/devices/{id}/trust", s.privileged(http.HandlerFunc(s.handleSetTrust)))
	mux.Handle("DELETE /api/devices/{id}/trust", s.privileged(http.HandlerFunc(s.handleSetTrust)))
	mux.Handle("DELETE /api/devices/{id}", s.privileged(http.HandlerFunc(s.handleDeleteDevice)))
	mux.Handle("POST /api/devices/{id}/disconnect", s.privileged(http.HandlerFunc(s.handleDisconnectDevice)))

	// ---- 传输任务 ----
	mux.Handle("POST /api/transfers", s.auth(http.HandlerFunc(s.handleCreateTransfer)))
	mux.Handle("GET /api/transfers", s.auth(http.HandlerFunc(s.handleListTransfers)))
	mux.Handle("GET /api/transfers/{id}", s.auth(http.HandlerFunc(s.handleGetTransfer)))
	mux.Handle("POST /api/transfers/{id}/accept", s.auth(http.HandlerFunc(s.handleAccept)))
	mux.Handle("POST /api/transfers/{id}/reject", s.auth(http.HandlerFunc(s.handleReject)))
	mux.Handle("POST /api/transfers/{id}/cancel", s.auth(http.HandlerFunc(s.handleCancel)))
	mux.Handle("POST /api/transfers/{id}/retry", s.auth(http.HandlerFunc(s.handleRetry)))
	mux.Handle("POST /api/transfers/{id}/pause", s.auth(http.HandlerFunc(s.handlePause)))
	mux.Handle("POST /api/transfers/{id}/resume", s.auth(http.HandlerFunc(s.handleResume)))
	mux.Handle("GET /api/transfers/{id}/chunks", s.auth(http.HandlerFunc(s.handleChunkStatus)))
	mux.Handle("POST /api/transfers/{id}/files/{index}/chunks/{chunk}", s.auth(http.HandlerFunc(s.handleUploadChunk)))
	mux.Handle("POST /api/transfers/{id}/files/{index}/reset", s.auth(http.HandlerFunc(s.handleResetFile)))
	mux.Handle("POST /api/transfers/{id}/files/{index}/ticket", s.auth(http.HandlerFunc(s.handleCreateTicket)))
	mux.Handle("POST /api/transfers/{id}/files/{index}/reveal", s.auth(http.HandlerFunc(s.handleReveal)))

	// ---- 历史 / 设置 / 诊断 / 临时接收箱 ----
	mux.Handle("GET /api/history", s.auth(http.HandlerFunc(s.handleHistory)))
	mux.Handle("POST /api/history/clear", s.privileged(http.HandlerFunc(s.handleClearHistory)))
	mux.Handle("GET /api/settings", s.auth(http.HandlerFunc(s.handleGetSettings)))
	mux.Handle("PATCH /api/settings", s.privileged(http.HandlerFunc(s.handlePatchSettings)))
	mux.Handle("PUT /api/settings", s.privileged(http.HandlerFunc(s.handlePatchSettings)))
	mux.Handle("GET /api/storage", s.auth(http.HandlerFunc(s.handleStorage)))
	mux.Handle("POST /api/storage/cleanup", s.privileged(http.HandlerFunc(s.handleCleanupStorage)))
	mux.Handle("GET /api/diagnostics", s.auth(http.HandlerFunc(s.handleDiagnostics)))
	mux.Handle("GET /api/dropbox", s.auth(http.HandlerFunc(s.handleGetDropbox)))
	mux.Handle("POST /api/dropbox/enable", s.privileged(http.HandlerFunc(s.handleEnableDropbox)))
	mux.Handle("POST /api/dropbox/disable", s.privileged(http.HandlerFunc(s.handleDisableDropbox)))

	// ---- 前端静态资源（生产构建产物由 Go 直接托管）----
	mux.Handle("/", s.staticHandler())

	return s.withRecover(s.withSecurityHeaders(mux))
}

// ------------------------------------------------------------ 中间件 ---

type ctxKey int

const deviceCtxKey ctxKey = 1

// auth 校验会话令牌并把设备放入请求上下文。
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dev, err := s.authenticate(r)
		if err != nil {
			writeErr(w, s.log, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceCtxKey, dev)))
	})
}

// privileged 在 auth 基础上要求设备已受信任（或本身是服务所在机器）。
// 特权操作包括：修改设置、信任管理、清除历史、清理临时文件。
func (s *Server) privileged(next http.Handler) http.Handler {
	return s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dev := deviceFrom(r.Context())
		if !s.isPrivileged(dev) {
			writeErr(w, s.log, transfer.NewError(protocol.CodeForbidden, "privileged operation"))
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// authenticate 解析令牌并返回设备。
//
// 令牌校验基于设备表，服务重启后依然有效（令牌持久化），
// 因此刷新页面不会被强制重新配对。
func (s *Server) authenticate(r *http.Request) (*model.Device, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, transfer.NewError(protocol.CodeUnauthorized, "missing bearer token")
	}
	dev, err := s.st.FindDeviceByToken(token)
	if err != nil {
		return nil, transfer.NewError(protocol.CodeSessionExpired, "token not found")
	}
	return dev, nil
}

// isPrivileged 判断设备是否可执行特权操作。
//
// 特权来源只有两条，都不可由客户端自行声称：
//  1. 请求来自服务所在机器的回环地址（部署者本人就在主机上）；
//  2. 该设备已被授予信任——由特权设备在设备管理页授权，
//     或凭服务启动时生成的管理令牌一次性认领。
//
// 刻意不提供「没有受信任设备时自动放行」的兜底：在共享局域网里，
// 那等于让任意陌生人取得管理权限。
func (s *Server) isPrivileged(dev *model.Device) bool {
	if dev == nil {
		return false
	}
	return dev.Trusted || dev.Self
}

func deviceFrom(ctx context.Context) *model.Device {
	v, _ := ctx.Value(deviceCtxKey).(*model.Device)
	return v
}

// withRecover 兜底 panic：单个请求出错不能拖垮整个服务。
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered", "path", r.URL.Path, "panic", rec)
				writeJSON(w, http.StatusInternalServerError, apiError{
					Error: struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					}{protocol.CodeInternal, protocol.Message[protocol.CodeInternal]},
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withSecurityHeaders 添加基础安全响应头。
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		// 明确声明：局域网明文 HTTP 不具备传输加密，页面不允许被外部嵌入或加载外部资源。
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: blob:; "+
				"style-src 'self' 'unsafe-inline'; script-src 'self' blob:; "+
				"worker-src 'self' blob:; connect-src 'self' ws: wss:; "+
				"object-src 'none'; base-uri 'self'; frame-ancestors 'self'")
		next.ServeHTTP(w, r)
	})
}

// isLoopbackRequest 判断请求是否来自本机回环地址（用于判定 self 设备）。
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// clientIP 提取客户端 IP，用于记录与诊断（不用于鉴权）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return strings.Trim(host, "[]")
}

// ------------------------------------------------------------ 下载票据 ---

// ticket 是一次受限的下载授权：绑定任务 + 文件索引 + 设备，且带有效期。
//
// 之所以需要它：浏览器触发原生下载（<a download>）时无法携带自定义请求头，
// 若把会话令牌放进 URL 会进入访问日志。票据是短时效、单一用途的替代品，
// 即使被记录也无法用于其它任务或接口。
type ticket struct {
	TaskID    string
	FileIndex int
	DeviceID  string
	ExpiresAt time.Time
	// Name 仅用于 Content-Disposition，不参与授权判断。
	Name string
}

type ticketStore struct {
	mu   sync.Mutex
	data map[string]ticket
}

const ticketTTL = 120 * time.Second

func newTicketStore() *ticketStore {
	return &ticketStore{data: map[string]ticket{}}
}

func (t *ticketStore) issue(tk ticket) string {
	id := uuid.NewString()
	tk.ExpiresAt = time.Now().Add(ticketTTL)
	t.mu.Lock()
	defer t.mu.Unlock()
	// 顺带清理过期票据，避免内存无限增长。
	now := time.Now()
	for k, v := range t.data {
		if v.ExpiresAt.Before(now) {
			delete(t.data, k)
		}
	}
	t.data[id] = tk
	return id
}

func (t *ticketStore) get(id string) (ticket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.data[id]
	if !ok || tk.ExpiresAt.Before(time.Now()) {
		delete(t.data, id)
		return ticket{}, false
	}
	return tk, true
}
