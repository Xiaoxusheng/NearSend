// Command nearsend 是局域网快传的服务端入口。
//
// 启动流程：解析参数 -> 打开数据库并迁移 -> 加载配置 -> 启动 WebSocket Hub
// -> 恢复中断任务 -> 启动传输巡检 -> 监听 HTTP。
//
// 监听端口被占用时给出明确提示与替代端口，而不是抛出底层错误。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"nearsend/internal/api"
	"nearsend/internal/config"
	"nearsend/internal/hub"
	"nearsend/internal/model"
	"nearsend/internal/netinfo"
	"nearsend/internal/store"
	"nearsend/internal/transfer"
)

// version 由构建脚本注入（-ldflags "-X main.version=..."），默认 dev。
var version = "dev"

func main() {
	var (
		addrFlag   = flag.String("addr", "", "监听地址，默认 0.0.0.0（可被数据库中的配置覆盖，命令行优先级最高）")
		portFlag   = flag.Int("port", 0, "监听端口，默认 8787")
		dataDirFlg = flag.String("data-dir", "", "数据目录（数据库与默认存储），默认可执行文件旁的 nearsend-data")
		publicFlag = flag.String("public-dir", "", "前端构建产物目录，默认 <项目>/web/dist")
		verbose    = flag.Bool("verbose", false, "输出调试日志")
		showVer    = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("nearsend", version)
		return
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if err := run(*addrFlag, *portFlag, *dataDirFlg, *publicFlag, logger); err != nil {
		logger.Error("启动失败", "err", err)
		os.Exit(1)
	}
}

func run(addrFlag string, portFlag int, dataDirFlag, publicFlag string, logger *slog.Logger) error {
	dataDir := dataDirFlag
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("无法创建数据目录 %s: %w", dataDir, err)
	}

	st, err := store.Open(filepath.Join(dataDir, "nearsend.db"))
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer st.Close()

	cfg, err := config.New(st, config.Options{
		DataDir:    dataDir,
		ListenAddr: addrFlag,
		Port:       portFlag,
		Version:    version,
	})
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	cur := cfg.Get()

	// WebSocket Hub：令牌解析回调直接查库，令牌持久化因此刷新页面不会掉线。
	h := hub.New(logger, func(token string) (*model.Device, error) {
		return st.FindDeviceByToken(token)
	})

	engine := transfer.New(st, cfg, h, logger)
	if err := engine.RecoverOnStart(); err != nil {
		return fmt.Errorf("恢复任务状态失败: %w", err)
	}
	engine.Start()
	defer engine.Stop()

	publicDir := resolvePublicDir(publicFlag)
	srv := api.NewServer(api.Options{
		Store: st, Config: cfg, Hub: h, Engine: engine,
		Log: logger, Version: version, PublicDir: publicDir,
	})

	// 先绑定端口，绑定成功再打印地址，避免打印出无法访问的链接。
	listenAddr := netinfo.NormalizeListenAddr(cur.ListenAddr)
	lnAddr := net.JoinHostPort(listenAddr, fmt.Sprint(cur.Port))
	ln, err := net.Listen("tcp", lnAddr)
	if err != nil {
		printPortHelp(cur.ListenAddr, cur.Port, err)
		return fmt.Errorf("监听 %s 失败", lnAddr)
	}

	httpSrv := &http.Server{
		Handler: srv.Handler(),
		// 大文件分块上传与下载可能持续很久，因此不设置整体读写超时，
		// 改用请求头超时 + 逐请求的 MaxBytesReader 控制风险。
		ReadHeaderTimeout: 20 * time.Second,
		WriteTimeout:      0,
		ReadTimeout:       0,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
		BaseContext: func(net.Listener) context.Context {
			return context.Background()
		},
	}

	errCh := make(chan error, 1)
	go func() {
		printBanner(cur, dataDir, publicDir, logger)
		printAdminToken(cfg, dataDir, logger)
		if serveErr := httpSrv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-stop:
		logger.Info("收到退出信号，正在关闭服务", "signal", sig.String())
	case serveErr := <-errCh:
		logger.Error("HTTP 服务异常退出", "err", serveErr)
		return serveErr
	}

	// 优雅关闭：等待进行中的请求结束，最多 10 秒。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.Shutdown()
	if err := httpSrv.Shutdown(ctx); err != nil {
		logger.Warn("关闭超时，强制退出", "err", err)
	}
	logger.Info("服务已停止")
	return nil
}

// printBanner 打印可访问地址。地址来自真实接口枚举，不是拼凑的字符串。
func printBanner(cfg model.Settings, dataDir, publicDir string, logger *slog.Logger) {
	urls := netinfo.LANAddresses(cfg.Port)
	fmt.Println()
	fmt.Println("  局域网快传  ·  nearsend " + version)
	fmt.Println("  ─────────────────────────────────────────────")
	fmt.Println("  在浏览器中打开以下地址（手机请扫码或直接输入局域网地址）：")
	primary := ""
	for _, a := range urls {
		mark := "  "
		if a.Recommended {
			mark = "→ "
			primary = a.URL
		}
		fmt.Printf("  %s%s   (%s, %s)\n", mark, a.URL, a.Family, a.Iface)
	}
	if primary == "" && len(urls) > 0 {
		primary = urls[0].URL
	}
	fmt.Printf("  本机访问:     http://127.0.0.1:%d\n", cfg.Port)
	fmt.Println("  ─────────────────────────────────────────────")
	fmt.Printf("  接收目录:     %s\n", cfg.ReceiveDir)
	fmt.Printf("  临时目录:     %s\n", cfg.TempDir)
	fmt.Printf("  数据目录:     %s\n", dataDir)
	fmt.Printf("  前端资源:     %s\n", publicDir)
	fmt.Printf("  并发/分块:    %d 个任务 / %s\n", cfg.MaxConcurrent,
		humanBytes(cfg.ChunkSize))
	fmt.Println("  安全提示:     本服务通过明文 HTTP 提供局域网访问，未启用 TLS，")
	fmt.Println("                不具备端到端加密能力。请仅在可信的局域网中使用。")
	fmt.Println()
	if logger != nil {
		logger.Info("服务已启动", "port", cfg.Port, "primary", primary)
	}
}

// printAdminToken 输出管理令牌的位置与内容。
//
// 令牌用于让「非主机上的浏览器」取得管理权限。首次运行必须完整打印，
// 之后只提示文件位置，避免每次启动都把它刷在屏幕上。
func printAdminToken(cfg *config.Manager, dataDir string, logger *slog.Logger) {
	token := cfg.AdminToken()
	if token == "" {
		return
	}
	tokenFile := filepath.Join(dataDir, "admin-token.txt")
	if cfg.AdminTokenGenerated() {
		if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
			logger.Warn("写入管理令牌文件失败", "err", err)
		}
		fmt.Println("  管理令牌（供非本机设备取得管理权限，请妥善保管）:")
		fmt.Printf("    %s\n", token)
		fmt.Printf("  令牌文件:     %s\n", tokenFile)
		fmt.Println("  在本机浏览器打开时无需令牌；在其它设备上首次进入设置页会要求输入。")
		fmt.Println()
		return
	}
	if _, err := os.Stat(tokenFile); err != nil {
		if werr := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); werr != nil {
			logger.Warn("写入管理令牌文件失败", "err", werr)
		}
	}
	fmt.Printf("  管理令牌文件: %s\n\n", tokenFile)
}

// printPortHelp 在端口不可用时给出明确、可执行的建议。
func printPortHelp(addr string, port int, err error) {
	fmt.Println()
	fmt.Println("  无法启动：监听端口失败")
	fmt.Println("  ─────────────────────────────────────────────")
	fmt.Printf("  地址:  %s:%d\n", addr, port)
	fmt.Printf("  原因:  %v\n", err)
	fmt.Println()
	fmt.Println("  可能是端口已被其它程序占用（常见占用者：其它开发服务器、代理软件）。")
	fmt.Println("  可以尝试：")
	fmt.Printf("    1) 换个端口启动：  nearsend -port %d\n", port+1)
	fmt.Println("    2) 查看占用进程：")
	fmt.Printf("       Windows:  netstat -ano | findstr :%d\n", port)
	fmt.Printf("       Linux:    ss -lntp | grep :%d\n", port)
	fmt.Printf("       macOS:    lsof -nP -iTCP:%d -sTCP:LISTEN\n", port)
	fmt.Println("    3) 如果只是想临时改端口，也可以在启动后用管理页面修改并重启服务。")
	fmt.Println()
}

// resolvePublicDir 定位前端构建产物。
func resolvePublicDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(base, "web", "dist"),
			filepath.Join(base, "dist"),
			filepath.Join(base, "..", "web", "dist"),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(wd, "web", "dist"),
			filepath.Join(wd, "dist"),
		)
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "index.html")); err == nil {
			return c
		}
	}
	if len(candidates) > 0 {
		// 都找不到时返回第一个候选，由静态处理器给出「需要先构建」的说明页。
		return candidates[0]
	}
	return ""
}

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

// humanBytes 与服务端日志保持一致的体积格式。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return strings.TrimSpace(fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp]))
}
