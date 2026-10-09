package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"

	"nearsend/internal/fsutil"
	"nearsend/internal/model"
	"nearsend/internal/netinfo"
	"nearsend/internal/protocol"
	"nearsend/internal/transfer"
)

// newToken 生成 32 字节随机会话令牌。
func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属于不可恢复的环境问题，退化为 UUID 组合仍保持不可预测性。
		return uuid.NewString() + uuid.NewString()
	}
	return hex.EncodeToString(b)
}

// handleHealth 是唯一完全无需鉴权的接口，用于「打开页面就知道服务是否可用」。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	conns, _ := s.hub.ConnectionCount()
	_, onlineDevices := s.hub.ConnectionCount()
	_ = conns
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"version":       s.version,
		"uptimeSec":     int64(time.Since(s.startedAt).Seconds()),
		"serverTime":    fsutil.NowMillis(),
		"onlineDevices": onlineDevices,
		"wsConnections": s.hubOnlineConnections(),
	})
}

func (s *Server) hubOnlineConnections() int {
	conns, _ := s.hub.ConnectionCount()
	return conns
}

// handleCreateSession 登记一个新的设备会话。
//
// 这是唯一无需令牌的写接口，但它只创建「自己的」会话：设备 ID 与令牌
// 都由服务端随机生成，客户端无法指定或冒充其它设备。
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	// 允许空请求体。
	if r.Body != nil && r.ContentLength != 0 {
		if err := readJSON(w, r, &body); err != nil {
			writeErr(w, s.log, err)
			return
		}
	}
	ua := r.Header.Get("User-Agent")
	osName, browser, dtype := parseUserAgent(ua)
	now := fsutil.NowMillis()
	self := isLoopbackRequest(r)

	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = s.cfg.Get().DeviceName
		if !self {
			name = suggestDeviceName(dtype, osName)
		}
		// 同一台机器上打开多个浏览器（或同一浏览器多份配置）时，
		// 若直接沿用主机名，会出现多个同名设备，用户无法区分。
		// 这里在重名时追加系统/浏览器信息加以区分。
		name = s.dedupeDeviceName(name, osName, browser)
	}
	if sanitized, err := fsutil.SanitizeName(name); err == nil {
		name = sanitized
	}
	if len([]rune(name)) > 48 {
		name = string([]rune(name)[:48])
	}

	dev := &model.Device{
		ID:          uuid.NewString(),
		Name:        name,
		OS:          osName,
		Browser:     browser,
		Type:        dtype,
		Online:      false, // 以 WebSocket 真实连接为准，HTTP 登记本身不算在线
		Trusted:     self,  // 服务所在机器上的浏览器默认受信任
		Self:        self,
		FirstSeen:   now,
		LastSeen:    now,
		ConnectedAt: now,
		LastIP:      clientIP(r),
		UserAgent:   truncate(ua, 300),
		Token:       newToken(),
	}
	if err := s.st.UpsertDevice(dev); err != nil {
		writeErr(w, s.log, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"device": dev,
		"token":  dev.Token,
		"access": s.accessInfo(),
	})
}

// dedupeDeviceName 在名称已被其它设备占用时追加系统与浏览器信息。
//
// 只做展示层面的区分，不承担身份识别职责——设备身份始终由随机会话 ID 决定。
func (s *Server) dedupeDeviceName(base, osName, browser string) string {
	devices, err := s.st.ListDevices()
	if err != nil {
		return base
	}
	used := map[string]bool{}
	for _, d := range devices {
		used[d.Name] = true
	}
	if !used[base] {
		return base
	}
	candidates := []string{
		base + " (" + browser + ")",
		base + " (" + osName + ")",
		base + " (" + osName + " · " + browser + ")",
	}
	for _, c := range candidates {
		if !used[c] {
			return c
		}
	}
	// 仍然冲突时退化为带短随机后缀的名字，保证一定可区分。
	return base + " (" + uuid.NewString()[:6] + ")"
}

// suggestDeviceName 依据 UA 生成一个可读的设备名，用户随后可在设备管理中修改。
func suggestDeviceName(t model.DeviceType, osName string) string {
	switch t {
	case model.DeviceMobile:
		return "手机 (" + osName + ")"
	case model.DeviceTablet:
		return "平板 (" + osName + ")"
	case model.DeviceLaptop:
		return "笔记本 (" + osName + ")"
	default:
		return "电脑 (" + osName + ")"
	}
}

// handleClaimAdmin 用管理令牌为当前设备取得管理权限。
//
// 这是无头部署（服务跑在另一台机器上）时的授权入口：令牌由服务端在
// 首次启动时生成并打印在控制台 / 写入数据目录，只有能访问主机的人拿得到。
// 成功一次即把该设备标记为受信任，之后无需再输入。
func (s *Server) handleClaimAdmin(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	var body struct {
		AdminToken string `json:"adminToken"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeErr(w, s.log, err)
		return
	}
	if !s.cfg.CheckAdminToken(strings.TrimSpace(body.AdminToken)) {
		// 不区分「令牌为空」「令牌错误」，避免成为令牌探测的旁路。
		s.log.Warn("管理令牌校验失败", "device", dev.ID, "ip", clientIP(r))
		writeErr(w, s.log, transfer.NewError(protocol.CodeForbidden, "admin token mismatch"))
		return
	}
	if err := s.st.SetDeviceTrusted(dev.ID, true); err != nil {
		writeErr(w, s.log, err)
		return
	}
	updated, err := s.st.GetDevice(dev.ID)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	s.broadcastDevice(protocol.EvDeviceUpdated, updated)
	writeJSON(w, http.StatusOK, map[string]any{
		"device":  s.deviceView(updated),
		"message": "已取得管理权限。",
	})
}

// handleUpdateSelf 修改当前设备名称。
func (s *Server) handleUpdateSelf(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeErr(w, s.log, err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, s.log, transfer.NewError(protocol.CodeBadRequest, "empty name"))
		return
	}
	sanitized, err := fsutil.SanitizeName(name)
	if err != nil {
		writeErr(w, s.log, transfer.NewError(protocol.CodeInvalidFileName, err.Error()))
		return
	}
	if len([]rune(sanitized)) > 48 {
		sanitized = string([]rune(sanitized)[:48])
	}
	if err := s.st.SetDeviceName(dev.ID, sanitized); err != nil {
		writeErr(w, s.log, err)
		return
	}
	updated, err := s.st.GetDevice(dev.ID)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	s.broadcastDevice(protocol.EvDeviceUpdated, updated)
	writeJSON(w, http.StatusOK, map[string]any{"device": s.deviceView(updated)})
}

// handleAccessInfo 返回当前服务的访问方式（局域网地址、端口、二维码地址）。
func (s *Server) handleAccessInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.accessInfo())
}

func (s *Server) accessInfo() map[string]any {
	cfg := s.cfg.Get()
	addrs := netinfo.LANAddresses(cfg.Port)
	primary := netinfo.PrimaryURL(cfg.Port)
	items := make([]map[string]any, 0, len(addrs))
	for _, a := range addrs {
		items = append(items, map[string]any{
			"ip": a.IP, "family": a.Family, "iface": a.Iface,
			"url": a.URL, "recommended": a.Recommended, "loopback": a.IsLoopback,
		})
	}
	return map[string]any{
		"port":       cfg.Port,
		"listenAddr": cfg.ListenAddr,
		"addresses":  items,
		"primaryUrl": primary,
		"qrUrl":      primary,
		"deviceName": cfg.DeviceName,
	}
}

// handleQRCode 生成当前访问地址的二维码 PNG。
//
// 只编码本机 primaryUrl，不接受任意外部 URL 参数，避免被当作开放二维码生成器。
func (s *Server) handleQRCode(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	target := netinfo.PrimaryURL(cfg.Port)
	if q := strings.TrimSpace(r.URL.Query().Get("url")); q != "" {
		// 仅允许同源地址，防止被用来为第三方内容生成二维码。
		if !isSameOrigin(q, cfg.Port) {
			writeErr(w, s.log, transfer.NewError(protocol.CodeBadRequest, "url not allowed"))
			return
		}
		target = q
	}
	png, err := qrPNG(target, 320)
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// isSameOrigin 校验 url 的主机部分是否指向本服务。
func isSameOrigin(raw string, port int) bool {
	for _, a := range netinfo.LANAddresses(port) {
		if strings.HasPrefix(raw, a.URL) {
			return true
		}
	}
	return strings.HasPrefix(raw, netinfo.BuildURL("127.0.0.1", "ipv4", port))
}

// ------------------------------------------------------------ 设备 ---

func (s *Server) deviceView(d *model.Device) map[string]any {
	return map[string]any{
		"id": d.ID, "name": d.Name, "os": d.OS, "browser": d.Browser, "type": d.Type,
		"online": s.hub.IsOnline(d.ID), "trusted": d.Trusted, "self": d.Self,
		"firstSeen": d.FirstSeen, "lastSeen": d.LastSeen, "connectedAt": d.ConnectedAt,
		"lastIp": d.LastIP,
	}
}

// handleListDevices 返回真实在线状态：online 完全由 WebSocket 连接决定。
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	list, err := s.st.ListDevices()
	if err != nil {
		writeErr(w, s.log, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	online := 0
	for _, d := range list {
		v := s.deviceView(d)
		if v["online"].(bool) {
			online++
		}
		out = append(out, v)
	}
	canAdmin := s.isPrivileged(dev)
	writeJSON(w, http.StatusOK, map[string]any{
		"devices":     out,
		"meId":        dev.ID,
		"onlineCount": online,
		"canAdmin":    canAdmin,
	})
}

// handlePatchDevice 重命名设备。仅允许改自己的名字，或由特权设备改任意设备。
func (s *Server) handlePatchDevice(w http.ResponseWriter, r *http.Request) {
	dev := deviceFrom(r.Context())
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, s.log, transfer.NewError(protocol.CodeBadRequest, "missing id"))
		return
	}
	if id != dev.ID && !s.isPrivileged(dev) {
		writeErr(w, s.log, transfer.NewError(protocol.CodeForbidden, "rename others"))
		return
	}
	target, err := s.st.GetDevice(id)
	if err != nil {
		writeErr(w, s.log, transfer.NewError(protocol.CodeNotFound, "device not found"))
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeErr(w, s.log, err)
		return
	}
	sanitized, err := fsutil.SanitizeName(strings.TrimSpace(body.Name))
	if err != nil {
		writeErr(w, s.log, transfer.NewError(protocol.CodeInvalidFileName, err.Error()))
		return
	}
	if len([]rune(sanitized)) > 48 {
		sanitized = string([]rune(sanitized)[:48])
	}
	if err := s.st.SetDeviceName(target.ID, sanitized); err != nil {
		writeErr(w, s.log, err)
		return
	}
	updated, _ := s.st.GetDevice(target.ID)
	s.broadcastDevice(protocol.EvDeviceUpdated, updated)
	writeJSON(w, http.StatusOK, map[string]any{"device": s.deviceView(updated)})
}

// handleSetTrust 授予或撤销信任（幂等：按 HTTP 方法决定目标状态）。
func (s *Server) handleSetTrust(w http.ResponseWriter, r *http.Request) {
	trust := r.Method == http.MethodPost
	id := r.PathValue("id")
	target, err := s.st.GetDevice(id)
	if err != nil {
		writeErr(w, s.log, transfer.NewError(protocol.CodeNotFound, "device not found"))
		return
	}
	me := deviceFrom(r.Context())
	if target.ID == me.ID && !trust {
		writeErr(w, s.log, transfer.NewError(protocol.CodeBadRequest, "cannot untrust current device"))
		return
	}
	if err := s.st.SetDeviceTrusted(id, trust); err != nil {
		writeErr(w, s.log, err)
		return
	}
	if !trust {
		// 撤销信任立即断开该设备的实时连接：信任不是前端标记，而是连接资格。
		s.hub.CloseDevice(id)
	}
	updated, _ := s.st.GetDevice(id)
	s.broadcastDevice(protocol.EvDeviceUpdated, updated)
	writeJSON(w, http.StatusOK, map[string]any{"device": s.deviceView(updated)})
}

// handleDeleteDevice 移除设备记录并断开其连接，令牌随记录一并失效。
func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := deviceFrom(r.Context())
	if id == me.ID {
		writeErr(w, s.log, transfer.NewError(protocol.CodeBadRequest, "cannot remove current device"))
		return
	}
	if _, err := s.st.GetDevice(id); err != nil {
		writeErr(w, s.log, transfer.NewError(protocol.CodeNotFound, "device not found"))
		return
	}
	s.hub.CloseDevice(id)
	if err := s.st.DeleteDevice(id); err != nil {
		writeErr(w, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": id})
}

// handleDisconnectDevice 只断开实时连接，保留设备记录与信任关系。
func (s *Server) handleDisconnectDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.st.GetDevice(id); err != nil {
		writeErr(w, s.log, transfer.NewError(protocol.CodeNotFound, "device not found"))
		return
	}
	s.hub.CloseDevice(id)
	writeJSON(w, http.StatusOK, map[string]any{"disconnected": id})
}

// broadcastDevice 广播设备变更事件。
func (s *Server) broadcastDevice(event string, d *model.Device) {
	if d == nil {
		return
	}
	s.hub.Broadcast(protocol.Envelope{
		Type: event, TS: fsutil.NowMillis(), DeviceID: d.ID, Data: s.deviceView(d),
	})
}

// ------------------------------------------------------------ 连接回调 ---

// onDeviceOnline 在设备建立第一条 WebSocket 连接时触发。
func (s *Server) onDeviceOnline(deviceID string) {
	now := fsutil.NowMillis()
	if err := s.st.TouchDevice(deviceID, now); err != nil {
		s.log.Debug("touch device failed", "device", deviceID, "err", err)
	}
	dev, err := s.st.GetDevice(deviceID)
	if err != nil {
		s.log.Warn("online for unknown device", "device", deviceID)
		return
	}
	s.broadcastDevice(protocol.EvDeviceOnline, dev)
}

// onDeviceOffline 在该设备最后一条连接断开时触发。
func (s *Server) onDeviceOffline(deviceID string) {
	now := fsutil.NowMillis()
	_ = s.st.TouchDevice(deviceID, now)
	dev, err := s.st.GetDevice(deviceID)
	if err != nil {
		return
	}
	s.broadcastDevice(protocol.EvDeviceOffline, dev)
}

// onClientMessage 处理客户端上报的控制消息。
// 目前只接受心跳与进度查询请求；未知类型一律忽略，不做任何隐式状态修改，
// 避免客户端通过 WebSocket 绕过 REST 层的权限校验。
func (s *Server) onClientMessage(deviceID string, env protocol.Envelope) {
	switch env.Type {
	case "client.ping":
		s.hub.SendToDevice(deviceID, protocol.Envelope{Type: protocol.EvPong, TS: fsutil.NowMillis()})
	case "client.heartbeat":
		_ = s.st.TouchDevice(deviceID, fsutil.NowMillis())
	default:
		s.log.Debug("ignored client message", "type", env.Type, "device", deviceID)
	}
}

// ------------------------------------------------------------ UA 解析 ---

// parseUserAgent 依据 UA 推断操作系统、浏览器与设备形态。
// 结果只用于展示与默认命名，绝不参与任何权限判断。
func parseUserAgent(ua string) (string, string, model.DeviceType) {
	lower := strings.ToLower(ua)

	osName := "未知系统"
	switch {
	case strings.Contains(lower, "windows nt 10"):
		osName = "Windows"
	case strings.Contains(lower, "windows"):
		osName = "Windows"
	case strings.Contains(lower, "android"):
		osName = "Android"
	case strings.Contains(lower, "iphone") || strings.Contains(lower, "ipad") || strings.Contains(lower, "ios"):
		osName = "iOS"
	case strings.Contains(lower, "mac os x") || strings.Contains(lower, "macintosh"):
		osName = "macOS"
	case strings.Contains(lower, "cros"):
		osName = "ChromeOS"
	case strings.Contains(lower, "linux"):
		osName = "Linux"
	case strings.Contains(lower, "harmony"):
		osName = "HarmonyOS"
	}

	browser := "浏览器"
	switch {
	case strings.Contains(lower, "edg/"):
		browser = "Edge"
	case strings.Contains(lower, "opr/") || strings.Contains(lower, "opera"):
		browser = "Opera"
	case strings.Contains(lower, "micromessenger"):
		browser = "微信"
	case strings.Contains(lower, "firefox"):
		browser = "Firefox"
	case strings.Contains(lower, "chrome") && !strings.Contains(lower, "chromium"):
		browser = "Chrome"
	case strings.Contains(lower, "safari"):
		browser = "Safari"
	}

	dtype := model.DeviceUnknown
	switch {
	case strings.Contains(lower, "ipad") || (strings.Contains(lower, "android") && !strings.Contains(lower, "mobile")):
		dtype = model.DeviceTablet
	case strings.Contains(lower, "mobile") || strings.Contains(lower, "iphone") ||
		strings.Contains(lower, "android"):
		dtype = model.DeviceMobile
	case strings.Contains(lower, "macintosh") || strings.Contains(lower, "windows") ||
		strings.Contains(lower, "linux") || strings.Contains(lower, "cros"):
		// 无触摸提示时按桌面处理；笔记本与台式机在 UA 中无法区分。
		dtype = model.DeviceDesktop
	}
	if dtype == model.DeviceUnknown && ua == "" {
		dtype = model.DeviceDesktop
	}
	return osName, browser, dtype
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// osArch 供诊断页展示服务端运行环境。
func osArch() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}
