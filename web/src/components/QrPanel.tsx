/**
 * 连接二维码面板。
 *
 * 二维码由服务端生成（GET /api/qrcode.png），内容是本服务真实的局域网地址；
 * 不使用第三方二维码服务，避免地址被外泄。IP 地址列表同样来自服务端枚举的本机网卡。
 */

import { DesktopOutlined, LinkOutlined, QrcodeOutlined } from '@ant-design/icons'
import { Button, Modal, Segmented, Tooltip } from 'antd'
import { useMemo, useState } from 'react'

import { useSession } from '../store/session'
import { CopyButton, Tag } from './common'

export function QrPanel({
  open,
  onClose,
  title = '扫描二维码连接',
}: {
  open: boolean
  onClose: () => void
  title?: string
}) {
  const access = useSession((s) => s.access)
  const reloadAccess = useSession((s) => s.reloadAccess)
  const [chosen, setChosen] = useState<string>('')

  const addresses = useMemo(() => access?.addresses ?? [], [access])
  const primary = access?.primaryUrl ?? ''
  const current = chosen || primary

  const qrSrc = current ? `/api/qrcode.png?url=${encodeURIComponent(current)}` : ''

  return (
    <Modal open={open} onCancel={onClose} footer={null} width={480} title={title} centered>
      <div className="ns-stack" style={{ alignItems: 'center', textAlign: 'center' }}>
        {qrSrc ? (
          <div
            style={{
              padding: 10,
              background: '#fff',
              border: '1px solid var(--ns-border)',
              borderRadius: 'var(--ns-radius-lg)',
            }}
          >
            <img
              src={qrSrc}
              alt={`连接地址二维码：${current}`}
              width={236}
              height={236}
              style={{ display: 'block' }}
            />
          </div>
        ) : (
          <div className="ns-meta">未检测到可用于局域网访问的地址。</div>
        )}

        <div className="ns-inline" style={{ width: '100%', justifyContent: 'center', flexWrap: 'wrap' }}>
          <span className="ns-mono" style={{ fontSize: 'var(--ns-fs-small)' }}>
            {current || '—'}
          </span>
          <CopyButton text={current} label="复制" />
        </div>

        {addresses.length > 1 ? (
          <div style={{ width: '100%' }}>
            <div className="ns-meta" style={{ marginBottom: 6 }}>
              本机检测到多个地址，若手机扫码后打不开，可换一个试试：
            </div>
            <Segmented
              block
              value={current}
              onChange={(v) => setChosen(String(v))}
              options={addresses.map((a) => ({
                label: (
                  <Tooltip title={`${a.iface}（${a.family}）`}>
                    <span>{a.ip}{a.recommended ? ' ·推荐' : ''}</span>
                  </Tooltip>
                ),
                value: a.url,
              }))}
            />
          </div>
        ) : null}

        <div className="ns-card" style={{ width: '100%', padding: 'var(--ns-sp-3)', textAlign: 'left', boxShadow: 'none' }}>
          <div className="ns-inline" style={{ marginBottom: 6 }}>
            <QrcodeOutlined style={{ color: 'var(--ns-primary)' }} />
            <span style={{ fontWeight: 550, fontSize: 'var(--ns-fs-small)' }}>使用方法</span>
          </div>
          <ol
            style={{
              margin: 0,
              paddingLeft: 18,
              fontSize: 'var(--ns-fs-tiny)',
              color: 'var(--ns-text-secondary)',
              lineHeight: 1.75,
            }}
          >
            <li>确保手机与本机连接的是同一个 Wi-Fi。</li>
            <li>用手机相机或微信扫描左侧二维码，在浏览器中打开。</li>
            <li>打开后这台手机就会出现在「附近在线设备」中，可以互相发送文件。</li>
          </ol>
          <div className="ns-inline" style={{ marginTop: 'var(--ns-sp-2)', flexWrap: 'wrap' }}>
            <Tag tone="neutral">
              <DesktopOutlined /> 手机与电脑需在同一局域网
            </Tag>
            <Tag tone="warning">若打不开，请检查是否开启了访客网络隔离</Tag>
          </div>
        </div>

        <div className="ns-inline">
          <Button size="small" icon={<LinkOutlined />} onClick={() => void reloadAccess()}>
            重新检测地址
          </Button>
          <Button size="small" onClick={onClose}>
            关闭
          </Button>
        </div>
      </div>
    </Modal>
  )
}

/** 顶部栏里的一行式地址展示（可复制）。 */
export function AccessLine({ compact }: { compact?: boolean }) {
  const access = useSession((s) => s.access)
  if (!access) return null
  return (
    <div className="ns-inline" style={{ flexWrap: 'wrap', fontSize: 'var(--ns-fs-tiny)' }}>
      {access.addresses.slice(0, compact ? 2 : 4).map((a) => (
        <span key={a.url} className="ns-inline" style={{ color: 'var(--ns-text-tertiary)' }}>
          <span className="ns-mono">{a.url}</span>
          {a.recommended ? <Tag tone="primary">推荐</Tag> : null}
        </span>
      ))}
    </div>
  )
}
