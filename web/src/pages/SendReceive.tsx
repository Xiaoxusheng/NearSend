/**
 * 首页：发送与接收。
 *
 * 布局（桌面）：左侧发送区约 58%，右侧设备区约 42%，下方依次是当前任务与最近传输。
 * 移动端：自动切为单栏。
 *
 * 发送按钮的文案与禁用原因都基于真实选择动态计算：条件不满足时明确说明缺什么。
 */

import { CloudUploadOutlined, QrcodeOutlined, ReloadOutlined, SendOutlined } from '@ant-design/icons'
import { Alert, Button, Select, Tooltip } from 'antd'
import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'

import { FileDropZone } from '../components/FileDropZone'
import { NearbyDevices } from '../components/NearbyDevices'
import { QrPanel } from '../components/QrPanel'
import { TaskList } from '../components/TaskList'
import { Card, CopyButton, EmptyHint, PageHeader, Section, Tag } from '../components/common'
import { ApiError, api } from '../lib/api'
import { formatBytes } from '../lib/format'
import type { ConflictPolicy } from '../lib/types'
import type { SourceFile } from '../lib/uploader'
import { useConfigStore } from '../store/config'
import { useDevices } from '../store/devices'
import { explainErrorCode, toast } from '../store/notify'
import { useSession } from '../store/session'
import { isActiveTask, isTerminal, useTransfers } from '../store/transfers'

export function SendReceivePage() {
  const navigate = useNavigate()
  const [files, setFiles] = useState<SourceFile[]>([])
  const [targets, setTargets] = useState<string[]>([])
  const [qrOpen, setQrOpen] = useState(false)
  const [sending, setSending] = useState(false)
  const [conflict, setConflict] = useState<ConflictPolicy>('rename')

  const config = useConfigStore((s) => s.config)
  const tasks = useTransfers((s) => s.tasks)
  const registerSources = useTransfers((s) => s.registerSources)
  const ensureUploading = useTransfers((s) => s.ensureUploading)
  const loadTasks = useTransfers((s) => s.load)
  const loadDevices = useDevices((s) => s.load)
  const onlineCount = useDevices((s) => s.onlineCount)
  const devices = useDevices((s) => s.devices)
  const access = useSession((s) => s.access)
  const deviceName = useSession((s) => s.deviceName)
  const wsStatus = useSession((s) => s.wsStatus)
  const selfDeviceId = useSession((s) => s.deviceId)

  useEffect(() => {
    if (config?.defaultConflictPolicy) setConflict(config.defaultConflictPolicy)
  }, [config?.defaultConflictPolicy])

  const totalSize = useMemo(() => files.reduce((s, f) => s + f.file.size, 0), [files])

  // 目标设备可能已经离线：选择后要重新校验，避免向离线设备发起。
  const onlineIds = useMemo(
    () => new Set(devices.filter((d) => d.online).map((d) => d.id)),
    [devices],
  )

  const validTargets = targets.filter((id) => onlineIds.has(id))
  const staleTargets = targets.filter((id) => !onlineIds.has(id))

  useEffect(() => {
    if (staleTargets.length > 0) {
      toast.warning('部分已选设备已离线，已自动从目标中移除。')
      setTargets(validTargets)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [staleTargets.length])

  const blocking: string[] = []
  if (files.length === 0) blocking.push('尚未选择文件')
  if (validTargets.length === 0) blocking.push('尚未选择接收设备')
  if (wsStatus !== 'online') blocking.push('与服务端的实时连接未就绪')
  if (config) {
    const oversize = files.find((f) => f.file.size > config.maxFileSize)
    if (oversize) blocking.push(`「${oversize.file.name}」超过单文件大小上限 ${formatBytes(config.maxFileSize)}`)
    if (totalSize > config.maxTaskSize) blocking.push(`总量超过单次上限 ${formatBytes(config.maxTaskSize)}`)
  }

  const canSend = blocking.length === 0 && !sending

  const onSend = async () => {
    if (!canSend) return
    if (wsStatus !== 'online') {
      toast.warning('实时连接未就绪，暂时无法发起传输。')
      return
    }
    setSending(true)
    try {
      const res = await api.createTransfer({
        receiverIds: validTargets,
        conflict,
        files: files.map((f) => ({
          name: f.file.name,
          relPath: f.relPath,
          size: f.file.size,
          mime: f.file.type || 'application/octet-stream',
          // 提供修改时间，服务端据此在续传前判断源文件是否已变化。
          modTime: f.file.lastModified,
        })),
      })

      // 为每个接收方的任务登记源文件，后续分块上传统一从这里取。
      for (const t of res.tasks) {
        registerSources(t.id, files)
        if (t.status === 'uploading') ensureUploading(t.id)
      }

      const receiverNames = res.tasks.map((t) => t.receiverName).join('、')
      const awaiting = res.tasks.filter((t) => t.status === 'awaiting').length
      toast.success(
        awaiting > 0
          ? `已向 ${receiverNames} 发起传输请求，等待对方确认。`
          : `已开始向 ${receiverNames} 传输 ${files.length} 个文件。`,
      )
      setFiles([])
      setTargets([])
      await loadTasks()
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '发起传输失败'
      toast.error(message)
    } finally {
      setSending(false)
    }
  }

  const allTasks = useMemo(() => Object.values(tasks), [tasks])
  const activeTasks = useMemo(
    () => allTasks.filter((t) => isActiveTask(t.status)).sort((a, b) => b.createdAt - a.createdAt),
    [allTasks],
  )
  const recentTasks = useMemo(
    () =>
      allTasks
        .filter((t) => isTerminal(t.status))
        .sort((a, b) => (b.completedAt || b.updatedAt) - (a.completedAt || a.updatedAt))
        .slice(0, 3),
    [allTasks],
  )

  return (
    <>
      <PageHeader
        title="局域网快传"
        description="在同一局域网内，快速发送和接收文件。无需账号，不依赖外网与云盘。"
        extra={
          <>
            <Tooltip title={wsStatus === 'online' ? '服务连接正常' : '实时连接未就绪'}>
              <Tag tone={wsStatus === 'online' ? 'success' : 'warning'}>
                <span className={`ns-status-dot ${wsStatus === 'online' ? 'is-online' : 'is-offline'}`} />
                {wsStatus === 'online' ? '服务可用' : '连接中'}
              </Tag>
            </Tooltip>
            {access?.primaryUrl ? <CopyButton text={access.primaryUrl} label="复制访问地址" /> : null}
            <Button icon={<QrcodeOutlined />} onClick={() => setQrOpen(true)}>
              二维码连接
            </Button>
          </>
        }
      />

      <div className="ns-two-col">
        {/* 左侧：文件发送区 */}
        <Card className="ns-rise ns-rise-1">
          <Section
            title="发送文件"
            extra={
              <span className="ns-meta">
                本机：<strong style={{ fontWeight: 550 }}>{deviceName || '—'}</strong>
              </span>
            }
          >
            <FileDropZone files={files} onChange={setFiles} />
          </Section>

          <div className="ns-divider" />

          <div className="ns-inline" style={{ justifyContent: 'space-between', flexWrap: 'wrap' }}>
            <div className="ns-inline">
              <span className="ns-meta">同名文件：</span>
              <Select
                size="small"
                value={conflict}
                onChange={(v) => setConflict(v)}
                style={{ width: 148 }}
                options={[
                  { value: 'rename', label: '自动重命名' },
                  { value: 'overwrite', label: '覆盖已有文件' },
                  { value: 'skip', label: '跳过同名文件' },
                ]}
              />
            </div>
            <div className="ns-inline">
              <Button size="small" icon={<ReloadOutlined />} onClick={() => void loadTasks()}>
                刷新状态
              </Button>
            </div>
          </div>

          <div style={{ marginTop: 'var(--ns-sp-3)' }}>
            <Tooltip
              title={
                canSend
                  ? `将向 ${validTargets.length} 台设备发送`
                  : blocking.join('；')
              }
            >
              <span style={{ display: 'block' }}>
                <Button
                  type="primary"
                  size="large"
                  block
                  icon={<SendOutlined />}
                  disabled={!canSend}
                  loading={sending}
                  onClick={onSend}
                >
                  {files.length === 0
                    ? '请先选择文件'
                    : validTargets.length === 0
                      ? '请选择接收设备'
                      : `发送文件 · ${files.length} 个文件 · ${formatBytes(totalSize)}`}
                </Button>
              </span>
            </Tooltip>
            {!canSend && blocking.length > 0 ? (
              <div className="ns-meta" style={{ marginTop: 6 }}>
                还差：{blocking.join('；')}
              </div>
            ) : (
              <div className="ns-meta" style={{ marginTop: 6 }}>
                发送前会先向对方发起请求，对方确认后才会真正传输。
              </div>
            )}
          </div>
        </Card>

        {/* 右侧：设备选择区 */}
        <Card className="ns-rise ns-rise-2">
          <NearbyDevices selected={targets} onChange={setTargets} onShowQr={() => setQrOpen(true)} />
        </Card>
      </div>

      {/* 第三部分：当前传输任务 */}
      <Section
        title="当前传输任务"
        extra={
          <Button size="small" type="link" onClick={() => navigate('/tasks')}>
            查看全部任务
          </Button>
        }
        style={{ marginTop: 'var(--ns-sp-5)' }}
      >
        <Card>
          {activeTasks.length === 0 ? (
            <EmptyHint
              icon={<CloudUploadOutlined />}
              title="当前没有进行中的传输"
              description="发起传输后，进度、速度与剩余时间会显示在这里。"
            />
          ) : (
            <TaskList tasks={activeTasks} emptyTitle="" />
          )}
        </Card>
      </Section>

      {/* 第四部分：最近传输 */}
      <Section
        title="最近传输"
        extra={
          <Button size="small" type="link" onClick={() => navigate('/history')}>
            查看全部记录
          </Button>
        }
      >
        <Card>
          {recentTasks.length === 0 ? (
            <EmptyHint
              title="还没有传输记录"
              description="完成一次传输后，记录会出现在这里。这里不会展示任何示例数据。"
            />
          ) : (
            <div className="ns-stack-sm">
              {recentTasks.map((t) => (
                <div
                  key={t.id}
                  className="ns-row ns-row-hover ns-row-selectable"
                  onClick={() => navigate('/history')}
                >
                  <div className="ns-row-main">
                    <div className="ns-row-title">
                      <span className="ns-ellipsis">
                        {t.senderId === selfDeviceId ? '发出' : '接收'} · {t.totalFiles} 个文件
                      </span>
                    </div>
                    <div className="ns-row-sub">
                      <span>{t.senderId === selfDeviceId ? `→ ${t.receiverName}` : `← ${t.senderName}`}</span>
                      <span>{formatBytes(t.totalSize)}</span>
                    </div>
                  </div>
                </div>
              ))}
            </div>
          )}
        </Card>
      </Section>

      {onlineCount <= 1 ? (
        <Alert
          style={{ marginTop: 'var(--ns-sp-4)' }}
          type="info"
          showIcon
          message="还没有其它设备连接"
          description={
            <span style={{ fontSize: 'var(--ns-fs-small)' }}>
              让另一台设备用浏览器打开{' '}
              {access?.primaryUrl ? <strong className="ns-mono">{access.primaryUrl}</strong> : '本页地址'}
              ，或扫描二维码。设备打开后会自动出现在「附近在线设备」中。当前服务本身无法主动扫描局域网设备
              ——这是浏览器的安全限制，因此本页面只显示真实连接过的设备。
            </span>
          }
          action={
            <Button size="small" type="primary" icon={<QrcodeOutlined />} onClick={() => setQrOpen(true)}>
              显示二维码
            </Button>
          }
        />
      ) : null}

      <QrPanel open={qrOpen} onClose={() => setQrOpen(false)} />

      {/* 首次进入且没有设备时，也允许手动刷新一次，确保状态是最新的 */}
      <Button
        type="text"
        size="small"
        style={{ marginTop: 'var(--ns-sp-3)', paddingLeft: 0, color: 'var(--ns-text-tertiary)' }}
        icon={<ReloadOutlined spin={false} />}
        onClick={() => {
          void loadDevices()
          void loadTasks()
        }}
      >
        刷新设备与任务状态
      </Button>
    </>
  )
}
