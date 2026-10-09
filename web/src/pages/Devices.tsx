/**
 * 设备管理页面。
 *
 * 分组：当前在线 / 最近连接 / 已信任设备。每个设备都有详情抽屉。
 * 明确一点：设备名称不是安全凭据，真正的可信关系由服务端签发并可由用户随时撤销。
 */

import {
  DeleteOutlined,
  DisconnectOutlined,
  EditOutlined,
  QrcodeOutlined,
  ReloadOutlined,
  SafetyCertificateOutlined,
  SearchOutlined,
} from '@ant-design/icons'
import { Button, Drawer, Empty, Input, Popconfirm, Segmented, Tooltip } from 'antd'
import { useMemo, useState } from 'react'

import { QrPanel } from '../components/QrPanel'
import { Card, DeviceIcon, EmptyHint, KeyValues, PageHeader, Tag } from '../components/common'
import { deviceTypeLabel, formatDateTime, formatRelative } from '../lib/format'
import type { Device } from '../lib/types'
import { useDevices } from '../store/devices'
import { isPrivileged } from '../store/identity'
import { useSession } from '../store/session'

type Group = 'online' | 'recent' | 'trusted'

const GROUP_OPTIONS: { label: string; value: Group }[] = [
  { label: '当前在线', value: 'online' },
  { label: '最近连接', value: 'recent' },
  { label: '已信任设备', value: 'trusted' },
]

export function DevicesPage() {
  const devices = useDevices((s) => s.devices)
  const loading = useDevices((s) => s.loading)
  const load = useDevices((s) => s.load)
  const rename = useDevices((s) => s.rename)
  const setTrust = useDevices((s) => s.setTrust)
  const remove = useDevices((s) => s.remove)
  const disconnect = useDevices((s) => s.disconnect)
  const meId = useDevices((s) => s.meId)
  const canAdmin = useDevices((s) => s.canAdmin)

  const renameSelf = useSession((s) => s.rename)

  const [group, setGroup] = useState<Group>('online')
  const [keyword, setKeyword] = useState('')
  const [detail, setDetail] = useState<Device | null>(null)
  const [renaming, setRenaming] = useState<Device | null>(null)
  const [renameValue, setRenameValue] = useState('')
  const [qrOpen, setQrOpen] = useState(false)

  const admin = canAdmin || isPrivileged()

  const counts = useMemo(
    () => ({
      online: devices.filter((d) => d.online).length,
      recent: devices.filter((d) => !d.online).length,
      trusted: devices.filter((d) => d.trusted).length,
    }),
    [devices],
  )

  const filtered = useMemo(() => {
    let list = devices
    if (group === 'online') list = list.filter((d) => d.online)
    else if (group === 'recent') list = list.filter((d) => !d.online)
    else list = list.filter((d) => d.trusted)
    const k = keyword.trim().toLowerCase()
    if (k) {
      list = list.filter(
        (d) =>
          d.name.toLowerCase().includes(k) ||
          d.os.toLowerCase().includes(k) ||
          d.browser.toLowerCase().includes(k) ||
          d.lastIp.includes(k),
      )
    }
    return [...list].sort((a, b) => {
      if (a.id === meId) return -1
      if (b.id === meId) return 1
      if (a.online !== b.online) return a.online ? -1 : 1
      return b.lastSeen - a.lastSeen
    })
  }, [devices, group, keyword, meId])

  const submitRename = async () => {
    if (!renaming) return
    const name = renameValue.trim()
    if (!name) return
    if (renaming.id === meId) await renameSelf(name)
    else await rename(renaming.id, name)
    setRenaming(null)
  }

  return (
    <>
      <PageHeader
        title="设备管理"
        description="只有真实连接到本服务的设备才会出现在这里。在线状态由实时连接决定，不会伪造未知设备。"
        extra={
          <>
            <Tooltip title="重新获取设备状态">
              <Button icon={<ReloadOutlined spin={loading} />} onClick={() => void load()}>
                刷新
              </Button>
            </Tooltip>
            <Button icon={<QrcodeOutlined />} onClick={() => setQrOpen(true)}>
              添加设备
            </Button>
          </>
        }
      />

      <Card style={{ marginBottom: 'var(--ns-sp-4)' }} className="ns-rise ns-rise-1">
        <div className="ns-inline" style={{ justifyContent: 'space-between', flexWrap: 'wrap', gap: 'var(--ns-sp-3)' }}>
          <Segmented
            value={group}
            onChange={(v) => setGroup(v as Group)}
            options={GROUP_OPTIONS.map((o) => ({
              label: (
                <span>
                  {o.label}
                  <span className="ns-num" style={{ marginLeft: 6, opacity: 0.7 }}>
                    {counts[o.value]}
                  </span>
                </span>
              ),
              value: o.value,
            }))}
          />
          <Input
            size="small"
            allowClear
            prefix={<SearchOutlined style={{ color: 'var(--ns-text-tertiary)' }} />}
            placeholder="搜索设备名称、系统或 IP"
            value={keyword}
            onChange={(e) => setKeyword(e.target.value)}
            style={{ width: 260, maxWidth: '100%' }}
          />
        </div>
      </Card>

      <Card className="ns-rise ns-rise-2">
        {filtered.length === 0 ? (
          <EmptyHint
            title={
              keyword
                ? '没有匹配的设备'
                : group === 'online'
                  ? '当前没有其它设备在线'
                  : group === 'trusted'
                    ? '还没有受信任的设备'
                    : '没有历史设备'
            }
            description={
              keyword
                ? '换个关键词试试。'
                : group === 'online'
                  ? '让其它设备打开本页的局域网地址或扫码连接，连接成功后会自动出现在这里。'
                  : group === 'trusted'
                    ? '在你的设备点击「设为可信」后，它会出现在这里，并可在设置中启用简化接收确认。'
                    : '设备断开并超过保留期后，记录仍会保留在这里。'
            }
            action={
              group === 'online' ? (
                <Button size="small" type="primary" icon={<QrcodeOutlined />} onClick={() => setQrOpen(true)}>
                  显示二维码
                </Button>
              ) : undefined
            }
          />
        ) : (
          <div className="ns-stack-sm">
            {filtered.map((d) => (
              <div className="ns-row ns-row-hover" key={d.id}>
                <span
                  style={{
                    width: 32,
                    height: 32,
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
                    {d.id === meId ? <Tag tone="primary">本机</Tag> : null}
                    {d.trusted ? <Tag tone="success">可信</Tag> : null}
                    <Tag tone={d.online ? 'success' : 'neutral'}>
                      <span className={`ns-status-dot ${d.online ? 'is-online' : 'is-offline'}`} />
                      {d.online ? '在线' : '离线'}
                    </Tag>
                  </div>
                  <div className="ns-row-sub">
                    <span>{deviceTypeLabel(d.type)}</span>
                    <span>{d.os}</span>
                    {d.browser ? <span>{d.browser}</span> : null}
                    <span>最近活跃 {formatRelative(d.lastSeen)}</span>
                  </div>
                </div>
                <div className="ns-row-actions">
                  <Tooltip title="查看详情">
                    <Button size="small" type="text" icon={<EditOutlined />} onClick={() => setDetail(d)}>
                      详情
                    </Button>
                  </Tooltip>
                  {d.id === meId ? (
                    <Tooltip title="重命名本机">
                      <Button
                        size="small"
                        type="text"
                        onClick={() => {
                          setRenaming(d)
                          setRenameValue(d.name)
                        }}
                      >
                        重命名
                      </Button>
                    </Tooltip>
                  ) : null}
                </div>
              </div>
            ))}
          </div>
        )}
      </Card>

      <div className="ns-meta" style={{ marginTop: 'var(--ns-sp-3)' }}>
        关于安全：设备名称只是显示用的标签，不作为身份凭据。真正的信任关系由服务端记录，
        可在详情抽屉中随时撤销；撤销后该设备的实时连接会立即断开。
      </div>

      <QrPanel open={qrOpen} onClose={() => setQrOpen(false)} title="添加设备" />

      {/* 设备详情抽屉 */}
      <Drawer
        open={Boolean(detail)}
        onClose={() => setDetail(null)}
        width={400}
        title={detail?.name ?? '设备详情'}
      >
        {detail ? (
          <div className="ns-stack">
            <KeyValues
              items={[
                { key: '设备类型', value: deviceTypeLabel(detail.type) },
                { key: '操作系统', value: detail.os || '未知' },
                { key: '浏览器', value: detail.browser || '未知' },
                {
                  key: '连接状态',
                  value: (
                    <span className="ns-inline">
                      <span className={`ns-status-dot ${detail.online ? 'is-online' : 'is-offline'}`} />
                      {detail.online ? '在线' : '离线'}
                    </span>
                  ),
                },
                { key: '最近来源 IP', value: <span className="ns-mono">{detail.lastIp || '—'}</span> },
                { key: '首次连接', value: formatDateTime(detail.firstSeen) },
                { key: '最近活跃', value: formatDateTime(detail.lastSeen) },
                {
                  key: '本次连接时间',
                  value: detail.connectedAt ? formatDateTime(detail.connectedAt) : '—',
                },
                { key: '信任状态', value: detail.trusted ? '已信任' : '未信任' },
                { key: '设备标识', value: <span className="ns-mono">{detail.id}</span> },
              ]}
            />

            <div className="ns-divider" />

            <div className="ns-inline" style={{ flexWrap: 'wrap' }}>
              <Button
                icon={<EditOutlined />}
                size="small"
                onClick={() => {
                  setRenaming(detail)
                  setRenameValue(detail.name)
                  setDetail(null)
                }}
              >
                重命名
              </Button>

              {detail.id !== meId ? (
                <>
                  {detail.trusted ? (
                    <Popconfirm
                      title="撤销信任？"
                      description="撤销后该设备将失去管理权限，其实时连接会立即断开，需要重新连接。"
                      okText="撤销"
                      okButtonProps={{ danger: true }}
                      cancelText="取消"
                      onConfirm={() => {
                        void setTrust(detail.id, false)
                        setDetail(null)
                      }}
                    >
                      <Button size="small" danger icon={<SafetyCertificateOutlined />}>
                        撤销信任
                      </Button>
                    </Popconfirm>
                  ) : (
                    <Tooltip title={admin ? '授予管理权限' : '只有受信任的设备可以授予信任'}>
                      <Button
                        size="small"
                        icon={<SafetyCertificateOutlined />}
                        disabled={!admin}
                        onClick={() => {
                          void setTrust(detail.id, true)
                          setDetail(null)
                        }}
                      >
                        设为可信
                      </Button>
                    </Tooltip>
                  )}

                  {detail.online ? (
                    <Button
                      size="small"
                      icon={<DisconnectOutlined />}
                      disabled={!admin}
                      onClick={() => {
                        void disconnect(detail.id)
                        setDetail(null)
                      }}
                    >
                      断开连接
                    </Button>
                  ) : null}

                  <Popconfirm
                    title="移除该设备记录？"
                    description="设备的会话令牌会同时失效，它需要重新打开页面才能再次连接。"
                    okText="移除"
                    okButtonProps={{ danger: true }}
                    cancelText="取消"
                    onConfirm={() => {
                      void remove(detail.id)
                      setDetail(null)
                    }}
                  >
                    <Button size="small" danger icon={<DeleteOutlined />} disabled={!admin}>
                      移除设备
                    </Button>
                  </Popconfirm>
                </>
              ) : (
                <span className="ns-meta">这是你当前使用的设备，无法从本机移除。</span>
              )}
            </div>

            {detail.id !== meId ? (
              <div className="ns-card" style={{ padding: 'var(--ns-sp-3)', boxShadow: 'none' }}>
                <div className="ns-meta" style={{ lineHeight: 1.8 }}>
                  「设为可信」会授予该设备修改设置、管理其它设备与清除记录的权限。
                  请只对你自己控制的设备这样做。若需要更严格的配对方式，可在设置页配置
                  「允许可信设备简化接收确认」的开关。
                </div>
              </div>
            ) : null}
          </div>
        ) : (
          <Empty />
        )}
      </Drawer>

      {/* 重命名弹窗 */}
      <Drawer
        open={Boolean(renaming)}
        onClose={() => setRenaming(null)}
        width={380}
        title="重命名设备"
        footer={
          <div className="ns-inline" style={{ justifyContent: 'flex-end' }}>
            <Button onClick={() => setRenaming(null)}>取消</Button>
            <Button type="primary" disabled={!renameValue.trim()} onClick={() => void submitRename()}>
              保存
            </Button>
          </div>
        }
      >
        <div className="ns-stack-sm">
          <div className="ns-meta">设备名称会展示给局域网内的其它设备，用于区分不同客户端。</div>
          <Input
            value={renameValue}
            maxLength={48}
            showCount
            placeholder="例如：客厅的笔记本"
            onChange={(e) => setRenameValue(e.target.value)}
            onPressEnter={() => void submitRename()}
          />
        </div>
      </Drawer>
    </>
  )
}
