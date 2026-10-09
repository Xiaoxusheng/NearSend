/**
 * 附近在线设备（接收方选择器）。
 *
 * 只展示「真实连接到本服务」的设备：在线状态来自 WebSocket 连接，
 * 未登录本页面的设备不会出现在这里，也绝不会被伪造出来。
 */

import { DesktopOutlined, QrcodeOutlined, ReloadOutlined, SearchOutlined } from '@ant-design/icons'
import { Button, Input, Tooltip } from 'antd'
import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'

import { formatRelative } from '../lib/format'
import type { Device } from '../lib/types'
import { useDevices } from '../store/devices'
import { getMeId } from '../store/identity'
import { useSession } from '../store/session'
import { DeviceIcon, EmptyHint, Tag } from './common'

export function NearbyDevices({
  selected,
  onChange,
  onShowQr,
}: {
  selected: string[]
  onChange: (ids: string[]) => void
  onShowQr: () => void
}) {
  const devices = useDevices((s) => s.devices)
  const loading = useDevices((s) => s.loading)
  const load = useDevices((s) => s.load)
  const onlineCount = useDevices((s) => s.onlineCount)
  const navigate = useNavigate()

  const [keyword, setKeyword] = useState('')
  const meId = getMeId()

  /** 可接收的设备：在线、且不是自己。最近使用优先由服务端排序保证。 */
  const candidates = useMemo(() => {
    const online = devices.filter((d) => d.online && d.id !== meId)
    if (!keyword.trim()) return online
    const k = keyword.trim().toLowerCase()
    return online.filter(
      (d) =>
        d.name.toLowerCase().includes(k) ||
        d.os.toLowerCase().includes(k) ||
        d.browser.toLowerCase().includes(k),
    )
  }, [devices, keyword, meId])

  const offlineHistory = useMemo(
    () => devices.filter((d) => !d.online && d.id !== meId).slice(0, 6),
    [devices, meId],
  )

  const toggle = (d: Device) => {
    if (selected.includes(d.id)) onChange(selected.filter((id) => id !== d.id))
    else onChange([...selected, d.id])
  }

  return (
    <div className="ns-stack-sm">
      <div className="ns-inline" style={{ justifyContent: 'space-between' }}>
        <div className="ns-inline">
          <span className="ns-section-title">附近在线设备</span>
          <Tag tone={onlineCount > 1 ? 'success' : 'neutral'}>
            {Math.max(0, onlineCount - (devices.some((d) => d.id === meId && d.online) ? 1 : 0))} 台可接收
          </Tag>
        </div>
        <div className="ns-inline">
          <Tooltip title="重新获取连接状态">
            <Button
              size="small"
              type="text"
              icon={<ReloadOutlined spin={loading} />}
              onClick={() => void load()}
              aria-label="刷新在线设备"
            />
          </Tooltip>
          <Tooltip title="用手机扫码连接">
            <Button size="small" type="text" icon={<QrcodeOutlined />} onClick={onShowQr} aria-label="显示二维码" />
          </Tooltip>
        </div>
      </div>

      {devices.length > 6 ? (
        <Input
          size="small"
          allowClear
          prefix={<SearchOutlined style={{ color: 'var(--ns-text-tertiary)' }} />}
          placeholder="搜索设备名称或系统"
          value={keyword}
          onChange={(e) => setKeyword(e.target.value)}
        />
      ) : null}

      {candidates.length === 0 ? (
        <EmptyHint
          icon={<DesktopOutlined />}
          title={keyword ? '没有匹配的设备' : '暂时没有其它在线设备'}
          description={
            keyword ? (
              '换个关键词试试。'
            ) : (
              <>
                让另一台设备用浏览器打开本页的访问地址（或扫描二维码）即可出现在这里。
                <br />
                若一直不显示，常见原因是：访客 Wi-Fi 隔离、两台设备不在同一网段、系统防火墙拦截了入站连接。
              </>
            )
          }
          action={
            keyword ? (
              <Button size="small" onClick={() => setKeyword('')}>
                清除搜索
              </Button>
            ) : (
              <div className="ns-inline">
                <Button size="small" type="primary" icon={<QrcodeOutlined />} onClick={onShowQr}>
                  显示二维码
                </Button>
                <Button size="small" onClick={() => navigate('/diagnostics')}>
                  排查网络问题
                </Button>
              </div>
            )
          }
        />
      ) : (
        <div
          className="ns-scroll-list"
          style={{
            maxHeight: 300,
            border: '1px solid var(--ns-border)',
            borderRadius: 'var(--ns-radius-md)',
          }}
        >
          {candidates.map((d) => {
            const isSelected = selected.includes(d.id)
            return (
              <div
                key={d.id}
                className={`ns-row ns-row-hover ns-row-selectable ${isSelected ? 'is-selected' : ''}`}
                onClick={() => toggle(d)}
                role="checkbox"
                aria-checked={isSelected}
                tabIndex={0}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault()
                    toggle(d)
                  }
                }}
              >
                <span
                  style={{
                    width: 30,
                    height: 30,
                    borderRadius: 'var(--ns-radius-md)',
                    background: 'var(--ns-bg-subtle)',
                    display: 'grid',
                    placeItems: 'center',
                    flex: '0 0 auto',
                  }}
                >
                  <DeviceIcon type={d.type} size={16} />
                </span>
                <div className="ns-row-main">
                  <div className="ns-row-title">
                    <span className="ns-ellipsis" title={d.name}>
                      {d.name}
                    </span>
                    {d.trusted ? <Tag tone="primary">可信</Tag> : null}
                  </div>
                  <div className="ns-row-sub">
                    <span className="ns-status-dot is-online" />
                    <span>
                      {d.os}
                      {d.browser ? ` · ${d.browser}` : ''}
                    </span>
                    <span>最近活跃 {formatRelative(d.lastSeen)}</span>
                    {d.lastIp ? <span className="ns-mono">{d.lastIp}</span> : null}
                  </div>
                </div>
                {isSelected ? <Tag tone="primary">已选择</Tag> : null}
              </div>
            )
          })}
        </div>
      )}

      {offlineHistory.length > 0 && !keyword ? (
        <div>
          <div className="ns-section" style={{ marginTop: 'var(--ns-sp-2)', marginBottom: 'var(--ns-sp-2)' }}>
            <span className="ns-meta">最近连接过（当前离线，无法接收）</span>
          </div>
          <div className="ns-stack-sm">
            {offlineHistory.map((d) => (
              <div className="ns-row" key={d.id} style={{ opacity: 0.66 }}>
                <DeviceIcon type={d.type} size={14} />
                <div className="ns-row-main">
                  <div className="ns-row-title">
                    <span className="ns-ellipsis" title={d.name}>
                      {d.name}
                    </span>
                  </div>
                  <div className="ns-row-sub">
                    <span className="ns-status-dot is-offline" />
                    <span>已离线 · 最近 {formatRelative(d.lastSeen)}</span>
                  </div>
                </div>
              </div>
            ))}
          </div>
          <div className="ns-meta" style={{ marginTop: 6 }}>
            当前离线设备无法接收文件；请让它在浏览器中重新打开本页。
          </div>
        </div>
      ) : null}

      <div className="ns-inline" style={{ justifyContent: 'space-between', marginTop: 'var(--ns-sp-2)' }}>
        <span className="ns-meta">本机名称：{useSession.getState().deviceName || '—'}</span>
        <Button size="small" type="link" style={{ padding: 0 }} onClick={() => navigate('/devices')}>
          在设备管理中修改
        </Button>
      </div>
    </div>
  )
}
