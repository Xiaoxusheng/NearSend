/**
 * 传输任务列表与任务条目。
 *
 * 展示规则：
 *  - 进度、速度、剩余时间全部基于真实计数；剩余时间在样本不足时显示「—」。
 *  - 只有一个文件的短名称时直接显示文件名；多文件时显示「首个文件名 等 N 个文件」。
 *  - 操作入口按状态与角色给出，不显示无法执行的操作按钮。
 */

import {
  CloseOutlined,
  DownloadOutlined,
  FolderOpenOutlined,
  PauseOutlined,
  PlayCircleOutlined,
  RedoOutlined,
  RightOutlined,
  WarningOutlined,
} from '@ant-design/icons'
import { Button, Progress, Tooltip } from 'antd'
import { useEffect, useRef, useState } from 'react'

import { ApiError, api } from '../lib/api'
import {
  fileStatusLabel,
  formatBytes,
  formatDateTime,
  formatEta,
  formatPercent,
  formatSpeed,
  truncateMiddle,
} from '../lib/format'
import type { Transfer } from '../lib/types'
import { useDevices } from '../store/devices'
import { getMeId } from '../store/identity'
import { explainErrorCode, toast } from '../store/notify'
import { useTransfers } from '../store/transfers'
import { DeviceIcon, EmptyHint, FileIcon, Tag, TransferStatusTag } from './common'

/**
 * 计算真实速度：基于服务端已确认字节数的变化率。
 *
 * 使用 requestAnimation 之外的普通 effect + 采样节流：只有在字节数变化超过
 * 64KB 或距上次采样超过 0.6 秒时才更新，避免高频进度事件引发过度重渲染。
 * 传输停止 2 秒后速度归零——不显示一个已经不再增长的「假速度」。
 */
export function useSpeedTracker(doneBytes: number): number {
  const ref = useRef({ done: doneBytes, at: Date.now(), speed: 0 })
  const [speed, setSpeed] = useState(0)

  useEffect(() => {
    const now = Date.now()
    const prev = ref.current
    const elapsed = (now - prev.at) / 1000
    const delta = doneBytes - prev.done

    if (delta < 0) {
      ref.current = { done: doneBytes, at: now, speed: 0 }
      setSpeed(0)
      return
    }
    if (elapsed < 0.6 && delta < 65536) return
    if (elapsed <= 0) return

    const instant = delta / elapsed
    // 指数平滑，避免单次抖动造成数值剧烈跳动。
    const smoothed = prev.speed === 0 ? instant : prev.speed * 0.6 + instant * 0.4
    ref.current = { done: doneBytes, at: now, speed: smoothed }
    setSpeed(smoothed)
  }, [doneBytes])

  useEffect(() => {
    const timer = window.setInterval(() => {
      if (ref.current.speed !== 0 && Date.now() - ref.current.at > 2000) {
        ref.current = { ...ref.current, speed: 0 }
        setSpeed(0)
      }
    }, 1000)
    return () => window.clearInterval(timer)
  }, [])

  return speed
}

export function TaskList({
  tasks,
  emptyTitle,
  emptyDescription,
  emptyAction,
}: {
  tasks: Transfer[]
  emptyTitle: string
  emptyDescription?: React.ReactNode
  emptyAction?: React.ReactNode
}) {
  if (tasks.length === 0) {
    return <EmptyHint title={emptyTitle} description={emptyDescription} action={emptyAction} />
  }
  return (
    <div className="ns-stack-sm">
      {tasks.map((t) => (
        <TaskRow key={t.id} task={t} />
      ))}
    </div>
  )
}

export function taskTitle(task: Transfer): string {
  const files = task.files ?? []
  if (files.length === 0) {
    return task.totalFiles === 1 ? '1 个文件' : `${task.totalFiles} 个文件`
  }
  if (files.length === 1) return files[0].name
  return `${files[0].name} 等 ${files.length} 个文件`
}

export function TaskRow({ task, compact }: { task: Transfer; compact?: boolean }) {
  const meId = getMeId()
  const [expanded, setExpanded] = useState(false)
  const [busy, setBusy] = useState<string | null>(null)

  const doneBytesOf = useTransfers((s) => s.doneBytesOf)
  const accept = useTransfers((s) => s.accept)
  const reject = useTransfers((s) => s.reject)
  const cancel = useTransfers((s) => s.cancel)
  const retry = useTransfers((s) => s.retry)
  const pause = useTransfers((s) => s.pause)
  const resume = useTransfers((s) => s.resume)
  const uploadNote = useTransfers((s) => s.uploadNote)
  const sourceAvailable = useTransfers((s) => Boolean(s.sources[task.id]?.length))

  const isSender = task.senderId === meId
  const isReceiver = task.receiverId === meId
  const done = doneBytesOf(task)
  const speed = useSpeedTracker(done)
  const note = uploadNote(task.id)
  const files = task.files ?? []

  const percent = task.totalSize > 0 ? Math.min(100, (done / task.totalSize) * 100) : task.status === 'completed' ? 100 : 0
  const remaining = Math.max(0, task.totalSize - done)
  const eta = formatEta(remaining, speed)

  const tone =
    task.status === 'completed'
      ? 'is-success'
      : task.status === 'failed' || task.status === 'rejected'
        ? 'is-danger'
        : task.status === 'paused'
          ? 'is-warning'
          : ''

  const run = async (label: string, fn: () => Promise<void>) => {
    setBusy(label)
    try {
      await fn()
    } finally {
      setBusy(null)
    }
  }

  const downloadAll = async () => {
    setBusy('download')
    try {
      const downloadable = files.filter((f) => f.status === 'ready' || f.status === 'downloaded')
      if (downloadable.length === 0) {
        toast.warning('目前没有可下载的文件。')
        return
      }
      for (const f of downloadable) {
        // 逐个签发短期票据并触发浏览器原生下载（支持 Range，不占内存）。
        const ticket = await api.createTicket(task.id, f.index)
        const a = document.createElement('a')
        a.href = ticket.url
        a.download = ticket.name
        a.rel = 'noopener'
        document.body.appendChild(a)
        a.click()
        document.body.removeChild(a)
        // 间隔 300ms，避免浏览器把连续下载视为弹窗滥用而拦截。
        await new Promise((r) => setTimeout(r, 300))
      }
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '下载失败'
      toast.error(message)
    } finally {
      setBusy(null)
    }
  }

  const reveal = async (fileIndex: number) => {
    try {
      await api.reveal(task.id, fileIndex)
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '无法打开所在目录'
      toast.error(message)
    }
  }

  const peer = isSender ? task.receiverName : task.senderName
  const peerOnline = isSender ? task.receiverOnline : task.senderOnline
  const peerDevice = useDevices((s) => s.byId(isSender ? task.receiverId : task.senderId))

  const actions: React.ReactNode[] = []

  if (isReceiver && task.status === 'awaiting') {
    actions.push(
      <Button
        key="accept"
        size="small"
        type="primary"
        loading={busy === 'accept'}
        onClick={() => run('accept', () => accept(task.id, task.conflict))}
      >
        接受
      </Button>,
      <Button key="reject" size="small" danger loading={busy === 'reject'} onClick={() => run('reject', () => reject(task.id, '接收方拒绝了本次传输'))}>
        拒绝
      </Button>,
    )
  }

  if (isSender && (task.status === 'uploading' || task.status === 'queued')) {
    actions.push(
      <Button
        key="pause"
        size="small"
        icon={<PauseOutlined />}
        loading={busy === 'pause'}
        onClick={() => run('pause', () => pause(task.id))}
      >
        暂停
      </Button>,
    )
  }

  if (task.status === 'paused') {
    actions.push(
      <Button
        key="resume"
        size="small"
        type="primary"
        icon={<PlayCircleOutlined />}
        loading={busy === 'resume'}
        onClick={() => run('resume', () => resume(task.id))}
      >
        继续
      </Button>,
    )
  }

  if (isSender && (task.status === 'failed' || task.status === 'cancelled' || task.status === 'rejected')) {
    actions.push(
      <Tooltip key="retry-tip" title={task.resumable ? '从已传输的分块继续' : task.nonResumableReason || '需要重新发起'}>
        <Button
          size="small"
          icon={<RedoOutlined />}
          loading={busy === 'retry'}
          onClick={() => run('retry', () => retry(task.id))}
        >
          重试
        </Button>
      </Tooltip>,
    )
  }

  if (isReceiver && (task.status === 'completed' || task.status === 'ready' || task.status === 'uploading' || task.status === 'paused')) {
    const hasDownloadable = files.some((f) => f.status === 'ready' || f.status === 'downloaded')
    if (hasDownloadable) {
      actions.push(
        <Button
          key="download"
          size="small"
          type="primary"
          icon={<DownloadOutlined />}
          loading={busy === 'download'}
          onClick={downloadAll}
        >
          下载
        </Button>,
      )
    }
  }

  if (!task.status.match(/^(completed|failed|cancelled|rejected)$/)) {
    actions.push(
      <Tooltip key="cancel-tip" title="取消任务">
        <Button
          size="small"
          type="text"
          danger
          icon={<CloseOutlined />}
          loading={busy === 'cancel'}
          onClick={() => run('cancel', () => cancel(task.id))}
        />
      </Tooltip>,
    )
  }

  return (
    <div className={`ns-task ${task.status === 'awaiting' ? 'ns-pop' : ''}`}>
      <div className="ns-task-head">
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
          {files.length === 1 ? <FileIcon name={files[0].name} size={16} /> : <FolderOpenOutlined style={{ color: 'var(--ns-text-secondary)' }} />}
        </span>

        <div className="ns-row-main">
          <div className="ns-row-title">
            <Tooltip title={files.length === 1 ? files[0].relPath || files[0].name : taskTitle(task)}>
              <span className="ns-ellipsis" style={{ maxWidth: compact ? 220 : 420 }}>
                {truncateMiddle(taskTitle(task), compact ? 26 : 52)}
              </span>
            </Tooltip>
            <TransferStatusTag status={task.status} />
            {task.retries > 0 ? <Tag tone="neutral">已重试 {task.retries} 次</Tag> : null}
            {task.verified && task.status === 'completed' ? <Tag tone="success">校验通过</Tag> : null}
          </div>
          <div className="ns-row-sub">
            <span className="ns-inline">
              {isSender ? '发送至' : '接收自'}
              <DeviceIcon type={peerDevice?.type ?? 'desktop'} size={12} />
              <strong style={{ fontWeight: 550, color: 'var(--ns-text-secondary)' }}>{peer}</strong>
              <span className={`ns-status-dot ${peerOnline ? 'is-online' : 'is-offline'}`} />
            </span>
            <span>{files.length > 1 ? `${files.length} 个文件 · ` : ''}{formatBytes(task.totalSize)}</span>
            <span>{formatDateTime(task.createdAt)}</span>
          </div>
        </div>

        <div className="ns-row-actions">
          {actions}
          {files.length > 1 || task.error ? (
            <Tooltip title="查看详情">
              <Button
                size="small"
                type="text"
                icon={<RightOutlined rotate={expanded ? 90 : 0} />}
                aria-label="查看任务详情"
                onClick={() => setExpanded((v) => !v)}
              />
            </Tooltip>
          ) : null}
        </div>
      </div>

      {(task.status === 'uploading' || task.status === 'queued' || task.status === 'paused' || task.status === 'completed') && task.totalSize > 0 ? (
        <div className="ns-stack-sm" style={{ gap: 4 }}>
          <div className="ns-progress-track">
            <div className={`ns-progress-fill ${tone}`} style={{ width: `${percent}%` }} />
          </div>
          <div className="ns-task-nums">
            <span>
              {formatBytes(done)} / {formatBytes(task.totalSize)}（{formatPercent(done, task.totalSize)}）
            </span>
            {task.status === 'uploading' ? (
              <>
                <span>速度 {formatSpeed(speed)}</span>
                <span>{eta ? `剩余约 ${eta}` : '剩余时间计算中…'}</span>
              </>
            ) : null}
            {task.status === 'queued' ? <span>等待可用传输通道…</span> : null}
            {task.status === 'paused' ? <span>已暂停，可随时继续</span> : null}
          </div>
        </div>
      ) : null}

      {task.status === 'awaiting' ? (
        <div className="ns-meta">
          {isSender
            ? `等待 ${task.receiverName} 确认接收${peerOnline ? '' : '（对方当前不在线，确认可能延迟）'}`
            : '请确认是否接收本次传输'}
        </div>
      ) : null}

      {task.status === 'failed' && task.error ? (
        <div className="ns-inline" style={{ color: 'var(--ns-danger)', fontSize: 'var(--ns-fs-tiny)' }}>
          <WarningOutlined />
          <span>{explainErrorCode(task.errorCode ?? 'internal', task.error)}</span>
        </div>
      ) : null}

      {task.status === 'rejected' ? (
        <div className="ns-meta">对方拒绝了本次传输{task.error ? `：${task.error}` : ''}</div>
      ) : null}

      {isSender && !sourceAvailable && (task.status === 'failed' || task.status === 'uploading' || task.status === 'paused') ? (
        <div className="ns-meta" style={{ color: 'var(--ns-warning)' }}>
          源文件不在当前页面会话中（页面可能已刷新），无法继续上传。请在「发送与接收」页重新选择同样的文件后重新发送。
        </div>
      ) : null}

      {note ? <div className="ns-meta" style={{ color: 'var(--ns-warning)' }}>{note}</div> : null}

      {expanded ? (
        <div className="ns-stack-sm" style={{ gap: 6, borderTop: '1px solid var(--ns-border-subtle)', paddingTop: 'var(--ns-sp-2)' }}>
          <div className="ns-kv">
            <div className="ns-kv-key">任务 ID</div>
            <div className="ns-kv-val ns-mono">{task.id}</div>
            <div className="ns-kv-key">开始时间</div>
            <div className="ns-kv-val">{formatDateTime(task.startedAt || task.createdAt)}</div>
            <div className="ns-kv-key">完成时间</div>
            <div className="ns-kv-val">{task.completedAt ? formatDateTime(task.completedAt) : '—'}</div>
            <div className="ns-kv-key">重名策略</div>
            <div className="ns-kv-val">
              {task.conflict === 'rename' ? '自动重命名' : task.conflict === 'overwrite' ? '覆盖' : '跳过'}
            </div>
            <div className="ns-kv-key">分块大小</div>
            <div className="ns-kv-val">{formatBytes(task.chunkSize)}</div>
            <div className="ns-kv-key">完整性校验</div>
            <div className="ns-kv-val">
              {task.verified ? 'SHA-256 校验通过' : task.status === 'completed' ? '未标记' : '传输完成后校验'}
            </div>
            <div className="ns-kv-key">断点续传</div>
            <div className="ns-kv-val">{task.resumable ? '支持' : task.nonResumableReason || '不支持'}</div>
          </div>

          {files.length > 0 ? (
            <div
              className="ns-scroll-list"
              style={{ maxHeight: 220, border: '1px solid var(--ns-border-subtle)', borderRadius: 'var(--ns-radius-md)' }}
            >
              {files.map((f) => {
                const fPercent = f.size > 0 ? Math.min(100, (f.doneBytes / f.size) * 100) : f.status === 'ready' || f.status === 'downloaded' ? 100 : 0
                return (
                  <div className="ns-row" key={f.id}>
                    <FileIcon name={f.name} />
                    <div className="ns-row-main">
                      <div className="ns-row-title">
                        <Tooltip title={f.relPath || f.name}>
                          <span className="ns-ellipsis">{truncateMiddle(f.name, 40)}</span>
                        </Tooltip>
                        <Tag tone={f.status === 'ready' || f.status === 'downloaded' ? 'success' : f.status === 'skipped' ? 'neutral' : f.status === 'failed' ? 'danger' : 'info'}>
                          {fileStatusLabel(f.status)}
                        </Tag>
                        {f.finalName && f.finalName !== f.name ? <Tag tone="warning">保存为 {f.finalName}</Tag> : null}
                      </div>
                      <div className="ns-row-sub">
                        <span>{formatBytes(f.size)}</span>
                        {f.chunkCount > 1 ? <span>{f.receivedChunks}/{f.chunkCount} 块</span> : null}
                        {f.error ? <span style={{ color: 'var(--ns-danger)' }}>{f.error}</span> : null}
                      </div>
                      {f.status !== 'ready' && f.status !== 'downloaded' && f.status !== 'skipped' && fPercent > 0 ? (
                        <div className="ns-progress-track" style={{ marginTop: 4, height: 4 }}>
                          <div className="ns-progress-fill" style={{ width: `${fPercent}%` }} />
                        </div>
                      ) : null}
                    </div>
                    {isReceiver && (f.status === 'ready' || f.status === 'downloaded') ? (
                      <div className="ns-row-actions">
                        <Tooltip title="在主机上打开所在文件夹">
                          <Button size="small" type="text" icon={<FolderOpenOutlined />} onClick={() => reveal(f.index)} />
                        </Tooltip>
                      </div>
                    ) : null}
                  </div>
                )
              })}
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  )
}

/** 传输中的整体进度条（用于首页顶部的汇总展示）。 */
export function TaskSummaryBar({ tasks }: { tasks: Transfer[] }) {
  const totalDone = tasks.reduce((s, t) => s + t.doneSize, 0)
  const totalBytes = tasks.reduce((s, t) => s + t.totalSize, 0)
  if (totalBytes === 0) return null
  return (
    <Progress
      percent={Math.round((totalDone / totalBytes) * 100)}
      size="small"
      strokeColor="var(--ns-primary)"
      trailColor="var(--ns-bg-active)"
      format={(p) => `${p}%`}
    />
  )
}
