import { App as AntApp, ConfigProvider, theme as antdTheme } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import { useEffect, useMemo } from 'react'
import { BrowserRouter, Navigate, Route, Routes, useLocation } from 'react-router-dom'

import { AppLayout } from './components/AppLayout'
import { BootGate } from './components/BootGate'
import { registerMessageApi } from './store/notify'
import { useSession } from './store/session'
import { useTheme } from './store/theme'

import { DevicesPage } from './pages/Devices'
import { DiagnosticsPage } from './pages/Diagnostics'
import { DropPage } from './pages/DropPage'
import { DropboxPage } from './pages/DropboxPage'
import { HistoryPage } from './pages/History'
import { SendReceivePage } from './pages/SendReceive'
import { SettingsPage } from './pages/Settings'
import { TasksPage } from './pages/Tasks'

/** 路由切换时给内容区加一次淡入动画，避免页面「硬切」。 */
function AnimatedRoutes() {
  const location = useLocation()
  return (
    <div key={location.pathname} className="ns-page-enter">
      <Routes location={location}>
        <Route path="/" element={<SendReceivePage />} />
        <Route path="/tasks" element={<TasksPage />} />
        <Route path="/history" element={<HistoryPage />} />
        <Route path="/devices" element={<DevicesPage />} />
        <Route path="/dropbox" element={<DropboxPage />} />
        <Route path="/diagnostics" element={<DiagnosticsPage />} />
        <Route path="/settings" element={<SettingsPage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </div>
  )
}

/** 用 antd 的 App 组件提供 message 上下文，并注册给全局提示桥接。 */
function MessageBridge({ children }: { children: React.ReactNode }) {
  const { message } = AntApp.useApp()
  useEffect(() => {
    registerMessageApi({
      success: (c, d) => void message.success(c, d),
      error: (c, d) => void message.error(c, d),
      info: (c, d) => void message.info(c, d),
      warning: (c, d) => void message.warning(c, d),
      loading: (c, d) => void message.loading(c, d),
    })
    return () => registerMessageApi(null)
  }, [message])
  return <>{children}</>
}

export function AppRoot() {
  const mode = useTheme((s) => s.mode)
  const boot = useSession((s) => s.boot)

  useEffect(() => {
    void boot()
  }, [boot])

  const themeConfig = useMemo(
    () => ({
      algorithm: mode === 'dark' ? antdTheme.darkAlgorithm : antdTheme.defaultAlgorithm,
      token: {
        fontFamily:
          'system-ui, -apple-system, "Segoe UI", "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif',
        fontSize: 14,
        borderRadius: 8,
        colorPrimary: mode === 'dark' ? '#3b8cff' : '#1677ff',
        colorBgBase: mode === 'dark' ? '#14161a' : '#ffffff',
        colorBgContainer: mode === 'dark' ? '#1c1f24' : '#ffffff',
        colorBgElevated: mode === 'dark' ? '#22262c' : '#ffffff',
        colorBorder: mode === 'dark' ? '#2f343c' : '#e4e6eb',
        colorBorderSecondary: mode === 'dark' ? '#262a31' : '#eef0f4',
        colorText: mode === 'dark' ? '#e6e8eb' : '#1f2328',
        colorTextSecondary: mode === 'dark' ? '#a9b1bc' : '#5b6472',
        colorTextTertiary: mode === 'dark' ? '#7d8694' : '#8a929e',
        controlHeight: 32,
        wireframe: false,
      },
      components: {
        Card: { paddingLG: 20 },
        Table: { headerBg: mode === 'dark' ? '#22262c' : '#f8f9fb' },
        Layout: { headerHeight: 60 },
      },
    }),
    [mode],
  )

  return (
    <ConfigProvider locale={zhCN} theme={themeConfig}>
      <AntApp>
        <MessageBridge>
          <BrowserRouter>
            <Routes>
              {/* 匿名上传页：临时接收入口的落地页，不需要进入主框架 */}
              <Route path="/drop" element={<DropPage />} />
              <Route element={<BootGate />}>
                <Route element={<AppLayout />}>
                  <Route path="*" element={<AnimatedRoutes />} />
                </Route>
              </Route>
            </Routes>
          </BrowserRouter>
        </MessageBridge>
      </AntApp>
    </ConfigProvider>
  )
}
