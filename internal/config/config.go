// Package config 管理服务端配置：默认值、持久化、校验与应用。
//
// 配置分为两类：
//   - 可热生效：并发数、分块大小、目录、策略等，保存后立即作用于新任务。
//   - 需重启：监听地址与端口，保存时返回需要重启的标记，不会谎称已生效。
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nearsend/internal/fsutil"
	"nearsend/internal/model"
	"nearsend/internal/store"
)

// DefaultChunkSize 默认分块大小 8 MiB。
const DefaultChunkSize int64 = 8 << 20

// MinChunkSize / MaxChunkSize 分块大小的合法区间。
const (
	MinChunkSize int64 = 256 << 10 // 256 KiB
	MaxChunkSize int64 = 64 << 20  // 64 MiB
)

// Manager 持有当前有效配置，提供并发安全的读写与校验。
type Manager struct {
	st *store.Store

	mu      sync.RWMutex
	current model.Settings
	boot    model.Settings // 启动时冻结的监听参数，用于判断是否需要重启
	// tokenGenerated 表示本次启动是否首次生成管理令牌（首次需要打印给用户）。
	tokenGenerated bool
}

// Options 是启动参数，优先级高于数据库中的配置（命令行 > 数据库 > 默认值）。
type Options struct {
	DataDir    string
	ListenAddr string
	Port       int
	Version    string
}

// New 加载配置：数据库中的值覆盖默认值，命令行参数再覆盖前两者。
func New(st *store.Store, opt Options) (*Manager, error) {
	saved, err := st.LoadSettings()
	if err != nil {
		return nil, err
	}
	def := Defaults(opt)
	if saved != nil {
		merge(&def, saved)
	}
	// 命令行参数拥有最高优先级，且不被持久化值覆盖。
	if opt.ListenAddr != "" {
		def.ListenAddr = opt.ListenAddr
	}
	if opt.Port > 0 {
		def.Port = opt.Port
	}
	if err := validate(&def); err != nil {
		return nil, err
	}
	if err := prepareDirs(&def); err != nil {
		return nil, err
	}
	// 管理令牌：首次运行时生成并持久化。
	// 非本机设备必须凭它才能取得管理权限，避免局域网内任意设备自称管理员。
	m := &Manager{st: st, current: def, boot: def}
	if def.AdminToken == "" {
		def.AdminToken = newAdminToken()
		m.current = def
		m.tokenGenerated = true
		if err := st.SaveSettings(&def); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// newAdminToken 生成 32 字节随机管理令牌。
func newAdminToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// 环境异常时退化为时间派生的高熵值；仍然不可预测到足以防暴力猜测。
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())))
		return hex.EncodeToString(sum[:])
	}
	return hex.EncodeToString(b)
}

// AdminTokenGenerated 返回本次启动是否首次生成了管理令牌。
func (m *Manager) AdminTokenGenerated() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tokenGenerated
}

// AdminToken 返回当前管理令牌（仅用于服务端本地校验与首次打印）。
func (m *Manager) AdminToken() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current.AdminToken
}

// CheckAdminToken 常数时间比较管理令牌，避免通过响应耗时推断令牌内容。
func (m *Manager) CheckAdminToken(candidate string) bool {
	want := m.AdminToken()
	if want == "" || candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(candidate)) == 1
}

// Defaults 构造带默认值的配置。
func Defaults(opt Options) model.Settings {
	dataDir := opt.DataDir
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	return model.Settings{
		DeviceName:             defaultDeviceName(),
		ReceiveDir:             filepath.Join(dataDir, "received"),
		TempDir:                filepath.Join(dataDir, "tmp"),
		MaxConcurrent:          3,
		ChunkSize:              DefaultChunkSize,
		MaxTaskSize:            64 << 30, // 64 GiB
		MaxFileSize:            32 << 30, // 32 GiB
		AutoRetry:              2,
		BandwidthLimit:         0,
		DefaultConflict:        model.ConflictRename,
		AllowTrustedAutoAccept: false,
		SessionTimeoutSec:      120,
		TrustedTokenTTLHours:   720,
		DropboxTTLMinutes:      30,
		HistoryRetentionDays:   30,
		TempRetentionHours:     24,
		AutoCleanTemp:          true,
		ListenAddr:             "0.0.0.0",
		Port:                   8787,
		Version:                opt.Version,
		DataDir:                dataDir,
	}
}

// merge 用持久化的非零值覆盖默认值。字符串为空、数值为 0 表示「未设置」，
// 但对于带宽限制等 0 有语义的字段，我们信任已保存的完整结构，
// 因此除字符串外一律直接覆盖。
func merge(def *model.Settings, saved *model.Settings) {
	s := *saved
	if s.DeviceName != "" {
		def.DeviceName = s.DeviceName
	}
	if s.ReceiveDir != "" {
		def.ReceiveDir = s.ReceiveDir
	}
	if s.TempDir != "" {
		def.TempDir = s.TempDir
	}
	if s.MaxConcurrent > 0 {
		def.MaxConcurrent = s.MaxConcurrent
	}
	if s.ChunkSize > 0 {
		def.ChunkSize = s.ChunkSize
	}
	if s.MaxTaskSize > 0 {
		def.MaxTaskSize = s.MaxTaskSize
	}
	if s.MaxFileSize > 0 {
		def.MaxFileSize = s.MaxFileSize
	}
	if s.AutoRetry >= 0 {
		def.AutoRetry = s.AutoRetry
	}
	def.BandwidthLimit = s.BandwidthLimit
	if s.DefaultConflict != "" {
		def.DefaultConflict = s.DefaultConflict
	}
	def.AllowTrustedAutoAccept = s.AllowTrustedAutoAccept
	if s.SessionTimeoutSec > 0 {
		def.SessionTimeoutSec = s.SessionTimeoutSec
	}
	if s.TrustedTokenTTLHours > 0 {
		def.TrustedTokenTTLHours = s.TrustedTokenTTLHours
	}
	if s.DropboxTTLMinutes > 0 {
		def.DropboxTTLMinutes = s.DropboxTTLMinutes
	}
	if s.HistoryRetentionDays >= 0 {
		def.HistoryRetentionDays = s.HistoryRetentionDays
	}
	if s.TempRetentionHours > 0 {
		def.TempRetentionHours = s.TempRetentionHours
	}
	def.AutoCleanTemp = s.AutoCleanTemp
	if s.ListenAddr != "" {
		def.ListenAddr = s.ListenAddr
	}
	if s.Port > 0 {
		def.Port = s.Port
	}
	if s.AdminToken != "" {
		def.AdminToken = s.AdminToken
	}
}

// Get 返回当前配置的副本（只读使用，避免调用方意外修改内部状态）。
func (m *Manager) Get() model.Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// NeedsRestart 判断相对启动时的监听参数是否发生变更。
func (m *Manager) NeedsRestart() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current.ListenAddr != m.boot.ListenAddr || m.current.Port != m.boot.Port
}

// Update 校验并保存新配置。返回 (是否需要重启, 错误)。
// 校验失败时不修改任何状态，保证「配置保存要么全成功要么全不变」。
func (m *Manager) Update(next *model.Settings) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	candidate := *next
	// 只读字段不允许通过 API 篡改。
	candidate.DataDir = m.current.DataDir
	candidate.Version = m.current.Version
	candidate.NeedsRestart = false
	// 管理令牌只由服务端维护，客户端无法读取或改写。
	candidate.AdminToken = m.current.AdminToken

	if err := validate(&candidate); err != nil {
		return false, err
	}
	if err := prepareDirs(&candidate); err != nil {
		return false, err
	}
	if err := m.st.SaveSettings(&candidate); err != nil {
		return false, err
	}
	m.current = candidate
	// 直接比较启动快照，避免在持有写锁时再次申请读锁造成自锁。
	needsRestart := candidate.ListenAddr != m.boot.ListenAddr || candidate.Port != m.boot.Port
	return needsRestart, nil
}

// Patch 只更新被显式指定的字段，未提供（nil）的字段保持原值。
// 这样前端「修改单个设置项」的请求不会意外重置其它配置。
func (m *Manager) Patch(patch map[string]any) (bool, error) {
	m.mu.RLock()
	next := m.current
	m.mu.RUnlock()

	for k, v := range patch {
		switch k {
		case "deviceName":
			if s, ok := asString(v); ok {
				next.DeviceName = s
			}
		case "receiveDir":
			if s, ok := asString(v); ok {
				next.ReceiveDir = s
			}
		case "tempDir":
			if s, ok := asString(v); ok {
				next.TempDir = s
			}
		case "maxConcurrentTransfers":
			if n, ok := asInt(v); ok {
				next.MaxConcurrent = n
			}
		case "chunkSize":
			if n, ok := asInt64(v); ok {
				next.ChunkSize = n
			}
		case "maxTaskSize":
			if n, ok := asInt64(v); ok {
				next.MaxTaskSize = n
			}
		case "maxFileSize":
			if n, ok := asInt64(v); ok {
				next.MaxFileSize = n
			}
		case "autoRetryCount":
			if n, ok := asInt(v); ok {
				next.AutoRetry = n
			}
		case "bandwidthLimit":
			if n, ok := asInt64(v); ok {
				next.BandwidthLimit = n
			}
		case "defaultConflictPolicy":
			if s, ok := asString(v); ok {
				next.DefaultConflict = model.ConflictPolicy(s)
			}
		case "allowTrustedAutoAccept":
			if b, ok := v.(bool); ok {
				next.AllowTrustedAutoAccept = b
			}
		case "sessionTimeoutSec":
			if n, ok := asInt(v); ok {
				next.SessionTimeoutSec = n
			}
		case "trustedTokenTtlHours":
			if n, ok := asInt(v); ok {
				next.TrustedTokenTTLHours = n
			}
		case "dropboxTtlMinutes":
			if n, ok := asInt(v); ok {
				next.DropboxTTLMinutes = n
			}
		case "historyRetentionDays":
			if n, ok := asInt(v); ok {
				next.HistoryRetentionDays = n
			}
		case "tempRetentionHours":
			if n, ok := asInt(v); ok {
				next.TempRetentionHours = n
			}
		case "autoCleanTemp":
			if b, ok := v.(bool); ok {
				next.AutoCleanTemp = b
			}
		case "adminToken":
			return false, errors.New("管理令牌不可通过接口修改")
		case "listenAddr":
			if s, ok := asString(v); ok {
				next.ListenAddr = s
			}
		case "port":
			if n, ok := asInt(v); ok {
				next.Port = n
			}
		default:
			return false, fmt.Errorf("未知配置项: %s", k)
		}
	}
	return m.Update(&next)
}

// validate 对所有配置项做范围与合法性校验。
func validate(s *model.Settings) error {
	if strings.TrimSpace(s.DeviceName) == "" {
		return errors.New("设备名称不能为空")
	}
	if len([]rune(s.DeviceName)) > 48 {
		return errors.New("设备名称不能超过 48 个字符")
	}
	if s.MaxConcurrent < 1 || s.MaxConcurrent > 32 {
		return errors.New("最大并发传输数需在 1 到 32 之间")
	}
	if s.ChunkSize < MinChunkSize || s.ChunkSize > MaxChunkSize {
		return fmt.Errorf("分块大小需在 %s 到 %s 之间", fsutil.HumanBytes(MinChunkSize), fsutil.HumanBytes(MaxChunkSize))
	}
	if s.MaxFileSize <= 0 {
		return errors.New("单文件大小限制必须大于 0")
	}
	if s.MaxTaskSize < s.MaxFileSize {
		return errors.New("单任务大小限制不能小于单文件大小限制")
	}
	if s.AutoRetry < 0 || s.AutoRetry > 10 {
		return errors.New("自动重试次数需在 0 到 10 之间")
	}
	if s.BandwidthLimit < 0 {
		return errors.New("带宽限制不能为负数")
	}
	switch s.DefaultConflict {
	case model.ConflictRename, model.ConflictOverwrite, model.ConflictSkip:
	default:
		return errors.New("默认重名策略不合法")
	}
	if s.SessionTimeoutSec < 30 || s.SessionTimeoutSec > 86400 {
		return errors.New("会话超时时间需在 30 到 86400 秒之间")
	}
	if s.TrustedTokenTTLHours < 1 || s.TrustedTokenTTLHours > 8760 {
		return errors.New("信任有效期需在 1 到 8760 小时之间")
	}
	if s.DropboxTTLMinutes < 1 || s.DropboxTTLMinutes > 1440 {
		return errors.New("临时入口有效期需在 1 到 1440 分钟之间")
	}
	if s.HistoryRetentionDays < 0 || s.HistoryRetentionDays > 3650 {
		return errors.New("历史记录保留时间需在 0 到 3650 天之间（0 表示永久保留）")
	}
	if s.TempRetentionHours < 1 || s.TempRetentionHours > 8760 {
		return errors.New("临时文件保留时间需在 1 到 8760 小时之间")
	}
	if s.Port < 1 || s.Port > 65535 {
		return errors.New("端口需在 1 到 65535 之间")
	}
	if s.ListenAddr != "" && net.ParseIP(s.ListenAddr) == nil {
		return fmt.Errorf("监听地址不是合法 IP: %s", s.ListenAddr)
	}
	return nil
}

// prepareDirs 创建并验证存储目录可写。
// 目录不可写属于「必须立刻反馈」的错误，因此在此处直接失败而不是等到传输时。
func prepareDirs(s *model.Settings) error {
	if !filepath.IsAbs(s.ReceiveDir) {
		return fmt.Errorf("接收目录必须是绝对路径: %s", s.ReceiveDir)
	}
	if !filepath.IsAbs(s.TempDir) {
		return fmt.Errorf("临时文件目录必须是绝对路径: %s", s.TempDir)
	}
	if samePath(s.ReceiveDir, s.TempDir) {
		return errors.New("临时文件目录不能与接收目录相同")
	}
	for _, dir := range []string{s.ReceiveDir, s.TempDir} {
		if err := fsutil.EnsureDir(dir); err != nil {
			return fmt.Errorf("无法创建目录 %s: %w", dir, err)
		}
		probe := filepath.Join(dir, ".nearsend_write_test")
		if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
			return fmt.Errorf("目录不可写 %s: %w", dir, err)
		}
		_ = os.Remove(probe)
	}
	return nil
}

func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

// ReceiveDir 返回当前接收目录。
func (m *Manager) ReceiveDir() string { return m.Get().ReceiveDir }

// TempDir 返回当前临时目录。
func (m *Manager) TempDir() string { return m.Get().TempDir }

// defaultDataDir 选择默认数据目录：优先可执行文件旁的 data，其次用户主目录。
func defaultDataDir() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(exe), "nearsend-data")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			return dir
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	dir := filepath.Join(home, ".nearsend")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func defaultDeviceName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "未命名设备"
	}
	return h
}

// ---- 类型转换辅助：JSON 反序列化后数字统一为 float64 ----

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}
