package api

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// staticHandler 托管前端构建产物（web/dist）。
//
// 行为：
//   - 命中真实文件 → 直接返回（带长效缓存，文件名含内容哈希由 Vite 保证）。
//   - 未命中且是页面请求 → 回退到 index.html，支持前端路由刷新。
//   - 目录不存在 → 返回一个说明页，告诉用户需要先构建前端，
//     而不是给出 404 让人误以为服务坏了。
func (s *Server) staticHandler() http.Handler {
	if s.publicDir == "" {
		return http.HandlerFunc(s.serveMissingFrontend)
	}
	root, err := filepath.Abs(s.publicDir)
	if err != nil {
		return http.HandlerFunc(s.serveMissingFrontend)
	}
	indexPath := filepath.Join(root, "index.html")
	if _, err := os.Stat(indexPath); err != nil {
		return http.HandlerFunc(s.serveMissingFrontend)
	}
	fileServer := http.FileServer(http.Dir(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		clean := path.Clean("/" + r.URL.Path)

		// 未匹配到路由的 API/WS 路径必须返回 404，绝不能回退成 index.html：
		// 否则调用方拿到的是 HTML，只会得到一个令人困惑的 JSON 解析错误，
		// 而不是「这个接口不存在」这个明确结论。
		if strings.HasPrefix(clean, "/api/") || clean == "/api" ||
			strings.HasPrefix(clean, "/ws") {
			http.NotFound(w, r)
			return
		}

		full := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
		// 双保险：确保拼出的路径没有越出静态根目录。
		if !strings.HasPrefix(full, root) {
			http.NotFound(w, r)
			return
		}
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			if strings.HasPrefix(clean, "/assets/") {
				// Vite 产物文件名带哈希，可长期缓存。
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		// 静态资源未命中：返回真正的 404，避免把 JS/CSS 缺失伪装成 HTML。
		if hasFileExtension(clean) {
			http.NotFound(w, r)
			return
		}
		// 其余路径视作前端路由，回退到 index.html。
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, indexPath)
	})
}

func hasFileExtension(p string) bool {
	base := path.Base(p)
	idx := strings.LastIndex(base, ".")
	return idx > 0 && idx < len(base)-1
}

// serveMissingFrontend 在未找到构建产物时给出可执行的指引。
func (s *Server) serveMissingFrontend(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>局域网快传 · 前端未构建</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI","Microsoft YaHei",sans-serif;background:#f5f6f8;color:#1f2328;margin:0;padding:48px;line-height:1.7}
.card{max-width:680px;margin:0 auto;background:#fff;border:1px solid #e5e7eb;border-radius:12px;padding:28px 32px}
h1{font-size:20px;margin:0 0 8px}p{font-size:14px;color:#57606a;margin:8px 0}
code{background:#f2f4f7;padding:2px 6px;border-radius:4px;font-size:13px}
pre{background:#0d1117;color:#e6edf3;padding:14px 16px;border-radius:8px;overflow:auto;font-size:13px}
.ok{color:#1a7f37;font-weight:600}
</style></head><body><div class="card">
<h1>后端已启动，但前端构建产物缺失</h1>
<p class="ok">服务本身运行正常：<code>/api/health</code>、<code>/ws</code> 等接口均可用。</p>
<p>页面需要先构建前端。在项目根目录执行：</p>
<pre>cd web
npm install
npm run build</pre>
<p>然后重新启动服务，或使用开发模式：在 <code>web</code> 目录执行 <code>npm run dev</code>，开发服务器会把 <code>/api</code> 与 <code>/ws</code> 代理到本服务。</p>
</div></body></html>`))
}

// ensureFS 保留 fs 接口引用，便于后续切换到嵌入式静态资源。
var _ fs.FS
