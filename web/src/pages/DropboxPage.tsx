/**
 * 临时接收箱。
 *
 * 语义：「临时」由后端强制实现 —— 入口有过期时间，过期或手动关闭后，
 * 后端会立即拒绝该令牌的一切上传请求，而不是只把前端按钮隐藏。
 */

import {
  ClockCircleOutlined,
  CopyOutlined,
  InboxOutlined,
  QrcodeOutlined,
  StopOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons'
import { Alert, Button, Modal, Popconfirm, Select, Tooltip } from 'antd'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'

import { Card, CopyButton, EmptyHint, KeyValues, PageHeader, Tag } from '../components/common'
import { TaskRow } from '../components/TaskList'
import { ApiError, api } from '../lib/api'
import { formatBytes, formatCountdown, formatDateTime } from '../lib/format'
import type { DropboxState } from '../lib/types'
import { isPrivileged } from '../store/identity'
import { explainErrorCode, toast } from '../store/notify'
import { useTransfers } from '../store/transfers'

const TTL_OPTIONS = [
  { value: 5, label: '5 分钟' },
  { value: 15, label: '15 分钟' },
  { value: 30, label: '30 分钟' },
  { value: 60, label: '1 小时' },
  { value: 240, label: '4 小时' },
  { value: 1440, label: '24 小时' },
]

export function DropboxPage() {
  const navigate = useNavigate()
  const [state, setState] = useState<DropboxState | null>(null)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [ttl, setTtl] = useState(30)
  const [qrOpen, setQrOpen] = useState(false)
  const [now, setNow] = useState(Date.now())

  const tasks = useTransfers((s) => s.tasks)
  const loadTasks = useTransfers((s) => s.load)
  const admin = isPrivileged()

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const st = await api.dropbox()
      setState(st)
      setTtl((prev) => (st.ttlMinutes && st.ttlMinutes !== prev ? st.ttlMinutes : prev))
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '无法获取临时入口状态'
      toast.error(message)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
    void loadTasks()
  }, [load, loadTasks])

  // 每秒刷新一次倒计时显示（基于服务端返回的绝对过期时间，不做本地臆测）。
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [])

  const remaining = state?.expiresAt ? Math.max(0, state.expiresAt - now) : 0
  const live = Boolean(state?.enabled && remaining > 0)

  // 进入过期瞬间自动刷新一次，把状态同步为后端真实结论。
  useEffect(() => {
    if (state?.enabled && remaining === 0 && state.expiresAt) {
      void load()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [remaining === 0])

  const enable = async () => {
    setBusy(true)
    try {
      const st = await api.enableDropbox(ttl)
      setState(st)
      toast.success(st.message ?? '临时接收入口已开启')
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '开启失败'
      toast.error(message)
    } finally {
      setBusy(false)
    }
  }

  const disable = async () => {
    setBusy(true)
    try {
      const st = await api.disableDropbox()
      setState((prev) => (prev ? { ...prev, ...st, token: undefined, uploadUrl: undefined, fullUrl: undefined } : st))
      toast.info(st.message ?? '临时接收入口已关闭')
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '关闭失败'
      toast.error(message)
    } finally {
      setBusy(false)
    }
  }

  const received = useMemo(
    () =>
      Object.values(tasks)
        .filter((t) => t.viaDropbox)
        .sort((a, b) => b.createdAt - a.createdAt),
    [tasks],
  )
  const recentDone = received.filter((t) => t.status === 'completed').slice(0, 5)

  const dropUrl = state?.fullUrl ?? ''

  return (
    <>
      <PageHeader
        title="临时接收箱"
        description="开一个临时入口，让其它设备无需确认即可把文件传进来。入口会在设定时间后自动失效。"
        extra={
          <>
            <Tag tone={live ? 'success' : 'neutral'}>
              <span className={`ns-status-dot ${live ? 'is-online' : 'is-offline'}`} />
              {live ? '入口已开启' : '入口未开启'}
            </Tag>
            <Button onClick={() => void load()}>刷新状态</Button>
          </>
        }
      />

      <div className="ns-two-col">
        <Card className="ns-rise ns-rise-1">
          <div className="ns-section">
            <span className="ns-section-title">入口状态</span>
            <div className="ns-section-spacer" />
            {live && state?.expiresAt ? (
              <Tag tone="warning">
                <ClockCircleOutlined /> 剩余 {formatCountdown(remaining)}
              </Tag>
            ) : null}
          </div>

          {!admin ? (
            <Alert
              type="warning"
              showIcon
              message="当前设备没有管理权限"
              description="临时接收入口会开放接收权限，因此只有受信任的设备（或在主机本机打开的页面）才能开启或关闭。请在主机上操作，或使用管理令牌取得权限。"
              action={
                <Button size="small" onClick={() => navigate('/settings')}>
                  去获取权限
                </Button>
              }
            />
          ) : live ? (
            <div className="ns-stack">
              <div className="ns-inline" style={{ justifyContent: 'space-between' }}>
                <span className="ns-meta">把这个地址发给要传文件的设备（或让对方扫码）</span>
                <div className="ns-inline">
                  <CopyButton text={dropUrl} label="复制入口地址" />
                  <Button size="small" icon={<QrcodeOutlined />} onClick={() => setQrOpen(true)}>
                    二维码
                  </Button>
                </div>
              </div>

              <div
                className="ns-card"
                style={{ padding: 'var(--ns-sp-3)', boxShadow: 'none', background: 'var(--ns-bg-subtle)' }}
              >
                <div className="ns-mono" style={{ fontSize: 'var(--ns-fs-small)', wordBreak: 'break-all' }}>
                  {dropUrl || '—'}
                </div>
              </div>

              <KeyValues
                items={[
                  { key: '开启时间', value: formatDateTime(state?.createdAt) },
                  { key: '失效时间', value: formatDateTime(state?.expiresAt) },
                  { key: '已接收', value: `${state?.receivedCount ?? 0} 个任务 / ${formatBytes(state?.receivedBytes ?? 0)}` },
                  { key: '有效期', value: formatCountdown(remaining) },
                ]}
              />

              <div className="ns-inline" style={{ flexWrap: 'wrap' }}>
                <Select
                  size="small"
                  value={ttl}
                  onChange={(v) => setTtl(v)}
                  style={{ width: 132 }}
                  options={TTL_OPTIONS}
                />
                <Button size="small" onClick={() => void enable()} loading={busy}>
                  重新签发（旧地址立即失效）
                </Button>
                <Popconfirm
                  title="关闭临时接收入口？"
                  description="关闭后旧地址会立即失效，正在进行的传输不受影响。"
                  okText="关闭"
                  okButtonProps={{ danger: true }}
                  cancelText="取消"
                  onConfirm={() => void disable()}
                >
                  <Button size="small" danger icon={<StopOutlined />} loading={busy}>
                    立即关闭
                  </Button>
                </Popconfirm>
              </div>

              <div className="ns-meta">
                临时入口只是在有效期内免去人工确认，仍然受「接收目录」「重名策略」「大小限制」等全部规则约束。
              </div>
            </div>
          ) : (
            <div className="ns-stack">
              <EmptyHint
                icon={<InboxOutlined />}
                title="临时入口未开启"
                description="开启后，任何拿到链接的设备都可以直接把文件传进来，无需你再逐个确认。适合「别人给我发文件」的场景。"
              />
              <div className="ns-inline" style={{ justifyContent: 'center', flexWrap: 'wrap' }}>
                <span className="ns-meta">有效期</span>
                <Select
                  size="small"
                  value={ttl}
                  onChange={(v) => setTtl(v)}
                  style={{ width: 132 }}
                  options={TTL_OPTIONS}
                />
                <Button
                  type="primary"
                  icon={<ThunderboltOutlined />}
                  loading={busy || loading}
                  onClick={() => void enable()}
                >
                  开启临时接收
                </Button>
              </div>
            </div>
          )}
        </Card>

        <Card className="ns-rise ns-rise-2">
          <div className="ns-section">
            <span className="ns-section-title">收到的请求</span>
            <div className="ns-section-spacer" />
            <Tag tone={received.length > 0 ? 'primary' : 'neutral'}>{received.length} 个</Tag>
          </div>
          {received.length === 0 ? (
            <EmptyHint
              title="还没有通过临时入口收到文件"
              description="开启入口并让其它设备上传后，任务会显示在这里。"
            />
          ) : (
            <div className="ns-stack-sm" style={{ maxHeight: 420, overflowY: 'auto' }}>
              {received.slice(0, 10).map((t) => (
                <TaskRow key={t.id} task={t} compact />
              ))}
            </div>
          )}
        </Card>
      </div>

      <Card className="ns-rise ns-rise-3" style={{ marginTop: 'var(--ns-sp-4)' }}>
        <div className="ns-section">
          <span className="ns-section-title">最近完成</span>
          <div className="ns-section-spacer" />
          <Button size="small" type="link" onClick={() => navigate('/history')}>
            查看全部记录
          </Button>
        </div>
        {recentDone.length === 0 ? (
          <EmptyHint title="暂无已完成记录" description="通过临时入口完成传输后，这里会显示最近的结果。" />
        ) : (
          <div className="ns-stack-sm">
            {recentDone.map((t) => (
              <TaskRow key={t.id} task={t} compact />
            ))}
          </div>
        )}
      </Card>

      <Modal
        open={qrOpen}
        onCancel={() => setQrOpen(false)}
        footer={null}
        title="临时接收入口二维码"
        width={420}
        centered
      >
        <div className="ns-stack" style={{ alignItems: 'center', textAlign: 'center' }}>
          {dropUrl ? (
            <div style={{ padding: 10, background: '#fff', border: '1px solid var(--ns-border)', borderRadius: 10 }}>
              <img
                src={`/api/qrcode.png?url=${encodeURIComponent(dropUrl)}`}
                alt="临时接收入口二维码"
                width={228}
                height={228}
                style={{ display: 'block' }}
              />
            </div>
          ) : null}
          <div className="ns-mono" style={{ fontSize: 'var(--ns-fs-tiny)', wordBreak: 'break-all' }}>
            {dropUrl}
          </div>
          <div className="ns-meta">
            对方扫码后进入一个极简上传页，可直接拖入文件发送，无需确认。
            <br />
            入口将在 {state?.expiresAt ? formatDateTime(state.expiresAt) : '—'} 失效。
          </div>
          <div className="ns-inline">
            <CopyButton text={dropUrl} label="复制地址" type="default" size="small" />
            <Tooltip title="点击复制">
              <Button size="small" icon={<CopyOutlined />} onClick={() => void load()}>
                刷新状态
              </Button>
            </Tooltip>
          </div>
        </div>
      </Modal>
    </>
  )
}
