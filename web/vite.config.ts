import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 开发模式下把 /api 与 /ws 代理到 Go 服务，生产构建产物直接由 Go 服务托管，
// 因此不需要在生产环境配置任何反向代理。
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // 单文件体积较大时按块拆分，首屏只加载必要的部分。
    chunkSizeWarningLimit: 1200,
    rollupOptions: {
      output: {
        // 用函数形式拆分公共依赖：React 与 Ant Design 各自独立成块，
        // 业务代码变更时这两块仍能命中浏览器缓存。
        manualChunks(id: string) {
          if (!id.includes('node_modules')) return undefined
          if (
            id.includes('node_modules/react/') ||
            id.includes('node_modules/react-dom/') ||
            id.includes('node_modules/react-router') ||
            id.includes('node_modules/scheduler/')
          ) {
            return 'react'
          }
          if (id.includes('node_modules/antd') || id.includes('node_modules/@ant-design')) {
            return 'antd'
          }
          return undefined
        },
      },
    },
  },
  server: {
    host: true,
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8787',
        changeOrigin: true,
      },
      '/ws': {
        target: 'ws://127.0.0.1:8787',
        ws: true,
      },
    },
  },
})
