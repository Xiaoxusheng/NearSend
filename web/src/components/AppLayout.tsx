/**
 * 全局布局。
 *
 * 桌面端：左侧固定 224px 侧边栏 + 顶部栏 + 可伸缩内容区（内容最大宽度受控，
 * 大屏不会被无限拉伸）。
 * 移动端：隐藏侧边栏，改用顶部抽屉 + 底部标签栏；所有页面入口在标签栏与抽屉中
 * 都保留，不会因为隐藏侧边栏而丢失导航。
 *
 * 侧边栏只包含产品标识、导航与版本号，不放头像、假通知数量等装饰模块。
 */

import {
  ApiOutlined,
  CloseOutlined,
  DesktopOutlined,
  HistoryOutlined,
  InboxOutlined,
  MenuOutlined,
  MoonOutlined,
  SettingOutlined,
  SunOutlined,
  SwapOutlined,
  UnorderedListOutlined,
} from '@ant-design/icons'
import { Badge, Button, Drawer, Tooltip } from 'antd'
import type { ReactNode } from 'react'
import { useEffect, useMemo, useState } from 'react'
import { NavLink, Outlet, useLocation } from 'react-router-dom'

import { useSession } from '../store/session'
import { useDevices } from '../store/devices'
import { isActiveTask, useTransfers } from '../store/transfers'
import { useTheme } from '../store/theme'
import { CopyButton, Tag } from './common'
import { ReceiveRequestModal } from './ReceiveRequestModal'

interface NavItem {
  path: string
  label: string
  icon: ReactNode
  /** 是否在移动端底部标签栏直接显示。 */
  inTabbar: boolean
}

const NAV: NavItem[] = [
  { path: '/', label: '发送与接收', icon: <SwapOutlined />, inTabbar: true },
  { path: '/tasks', label: '传输任务', icon: <UnorderedListOutlined />, inTabbar: true },
  { path: '/history', label: '传输记录', icon: <HistoryOutlined />, inTabbar: true },
  { path: '/devices', label: '设备管理', icon: <DesktopOutlined />, inTabbar: true },
  { path: '/dropbox', label: '临时接收箱', icon: <InboxOutlined />, inTabbar: false },
  { path: '/diagnostics', label: '网络诊断', icon: <ApiOutlined />, inTabbar: false },
  { path: '/settings', label: '设置', icon: <SettingOutlined />, inTabbar: false },
]

/** 页面标题用静态映射，避免在各页面之间维护额外的标题状态。 */
const TITLES: Record<string, string> = {
  '/': '发送与接收',
  '/tasks': '传输任务',
  '/history': '传输记录',
  '/devices': '设备管理',
  '/dropbox': '临时接收箱',
  '/diagnostics': '网络诊断',
  '/settings': '设置',
}

export function AppLayout() {
  const location = useLocation()
  const [drawerOpen, setDrawerOpen] = useState(false)

  const wsStatus = useSession((s) => s.wsStatus)
  const wsError = useSession((s) => s.wsError)
  const access = useSession((s) => s.access)
  const deviceName = useSession((s) => s.deviceName)
  const reconnectWs = useSession((s) => s.reconnectWs)
  const health = useSession((s) => s.health)

  const devices = useDevices((s) => s.devices)
  const onlineCount = useDevices((s) => s.onlineCount)

  const tasks = useTransfers((s) => s.tasks)
  const pendingIncoming = useTransfers((s) => s.pendingIncoming)
  const themeMode = useTheme((s) => s.mode)
  const toggleTheme = useTheme((s) => s.toggle)

  const activeCount = useMemo(
    () => Object.values(tasks).filter((t) => isActiveTask(t.status)).length,
    [tasks],
  )
  const incomingCount = pendingIncoming().length

  // 路由切换时关闭移动端抽屉
  useEffect(() => {
    setDrawerOpen(false)
  }, [location.pathname])

  const primaryUrl = access?.primaryUrl ?? ''

  const navList = (compact: boolean) => (
    <nav className="ns-nav">
      {NAV.map((item) => {
        const badge =
          item.path === '/tasks'
            ? activeCount
            : item.path === '/devices'
              ? onlineCount
              : 0
        return (
          <NavLink
            key={item.path}
            to={item.path}
            end={item.path === '/'}
            className={({ isActive }) => `ns-nav-item ${isActive ? 'is-active' : ''}`}
            style={compact ? { padding: '10px 10px' } : undefined}
          >
            {item.icon}
            <span>{item.label}</span>
            {badge > 0 ? <span className="ns-nav-badge">{badge}</span> : null}
          </NavLink>
        )
      })}
    </nav>
  )

  return (
    <div className="ns-shell">
      <aside className="ns-sidebar ns-no-print">
        <div className="ns-brand">
          <div className="ns-brand-mark" aria-hidden>
            <SwapOutlined />
          </div>
          <div className="ns-brand-text">
            <div className="ns-brand-title">局域网快传</div>
            <div className="ns-brand-sub ns-ellipsis">{deviceName || '未命名设备'}</div>
          </div>
        </div>
        {navList(false)}
        <div className="ns-sidebar-foot">
          <span>版本 {health?.version ?? '—'}</span>
          <span className="ns-ellipsis" title={primaryUrl}>
            {primaryUrl || '未检测到局域网地址'}
          </span>
        </div>
      </aside>

      <div className="ns-main">
        <header className="ns-topbar ns-no-print">
          <div className="ns-topbar-title">
            <span>{TITLES[location.pathname] ?? '局域网快传'}</span>
            <ServiceBadge status={wsStatus} error={wsError} onReconnect={reconnectWs} />
          </div>
          <div className="ns-topbar-spacer" />
          <div className="ns-topbar-actions">
            {incomingCount > 0 ? (
              <Tag tone="warning">{incomingCount} 个待确认</Tag>
            ) : null}
            {primaryUrl ? (
              <span className="ns-lan-address">
                <CopyButton text={primaryUrl} label={undefined} />
                <span className="ns-ellipsis ns-lan-address-text" title={primaryUrl}>
                  {primaryUrl.replace(/^https?:\/\//, '')}
                </span>
              </span>
            ) : null}
            <Tooltip title={themeMode === 'dark' ? '切换到浅色主题' : '切换到深色主题'}>
              <Button
                type="text"
                icon={themeMode === 'dark' ? <SunOutlined /> : <MoonOutlined />}
                onClick={toggleTheme}
                aria-label="切换主题"
              />
            </Tooltip>
            <Tooltip title="设置">
              <NavLink to="/settings" aria-label="设置">
                <Button type="text" icon={<SettingOutlined />} />
              </NavLink>
            </Tooltip>
          </div>
        </header>

        <main className="ns-content">
          <Outlet />
        </main>
      </div>

      {/* 移动端底部标签栏 */}
      <nav className="ns-tabbar ns-no-print">
        {NAV.filter((n) => n.inTabbar).map((item) => {
          const badge = item.path === '/tasks' ? activeCount : item.path === '/devices' ? onlineCount : 0
          return (
            <NavLink
              key={item.path}
              to={item.path}
              end={item.path === '/'}
              className={({ isActive }) => `ns-tab ${isActive ? 'is-active' : ''}`}
            >
              <Badge count={badge} size="small" offset={[2, 0]}>
                {item.icon}
              </Badge>
              <span>{item.label}</span>
            </NavLink>
          )
        })}
        <button type="button" className="ns-tab" onClick={() => setDrawerOpen(true)}>
          <MenuOutlined />
          <span>更多</span>
        </button>
      </nav>

      <Drawer
        title="局域网快传"
        placement="left"
        width={252}
        open={drawerOpen}
        onClose={() => setDrawerOpen(false)}
        closeIcon={<CloseOutlined />}
        styles={{ body: { padding: 'var(--ns-sp-3) var(--ns-sp-2)' } }}
      >
        {navList(true)}
        <div className="ns-sidebar-foot" style={{ borderTop: 'none' }}>
          <span>版本 {health?.version ?? '—'}</span>
          <span className="ns-ellipsis">{deviceName || '未命名设备'}</span>
          <span>{devices.length} 台已知设备 · {onlineCount} 台在线</span>
        </div>
      </Drawer>

      <ReceiveRequestModal />
    </div>
  )
}

/** 顶部服务状态：连接中断时点一下即可重连，并说明原因。 */
export function ServiceBadge({
  status,
  error,
  onReconnect,
}: {
  status: string
  error: string
  onReconnect: () => void
}) {
  if (status === 'online') {
    return (
      <Tooltip title="与服务端的实时连接正常">
        <span className="ns-inline" style={{ fontSize: 'var(--ns-fs-tiny)', color: 'var(--ns-text-tertiary)' }}>
          <span className="ns-status-dot is-online" />
          已连接
        </span>
      </Tooltip>
    )
  }
  if (status === 'connecting' || status === 'idle') {
    return (
      <span className="ns-inline" style={{ fontSize: 'var(--ns-fs-tiny)', color: 'var(--ns-text-tertiary)' }}>
        <span className="ns-status-dot" />
        连接中…
      </span>
    )
  }
  return (
    <Tooltip title={error || '实时连接已断开，点击重连'}>
      <Button size="small" danger type="text" onClick={onReconnect}>
        <span className="ns-status-dot is-offline" style={{ marginRight: 6 }} />
        连接中断，点击重连
      </Button>
    </Tooltip>
  )
}
