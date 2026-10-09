/**
 * 临时接收入口的上传页（/drop?token=...）。
 *
 * 这是给「没有安装任何客户端、只是拿到链接的陌生人」用的极简页面：
 * 拖入文件 -> 直接上传，无需接收方确认。上传权限完全由后端校验令牌，
 * 令牌过期或被关闭后，这里的任何操作都会被后端拒绝（页面也会显示失效提示）。
 */

import { CheckCircleOutlined, CloudUploadOutlined, InboxOutlined, StopOutlined } from '@ant-design/icons'
import { Alert, Button, Result, Spin } from 'antd'
import { useCallback, useEffect, useMemo, useState } from 'react'

import { FileDropZone } from '../components/FileDropZone'
import { Card, FileIcon, Tag } from '../components/common'
import { ApiError, api, setToken } from '../lib/api'
import { formatBytes, formatCountdown, truncateMiddle } from '../lib/format'
import type { SourceFile } from '../lib/uploader'
import { TaskUploader } from '../lib/uploader'

type Stage = 'checking' | 'invalid' | 'ready' | 'uploading' | 'done' | 'error'

interface UploadState {
  taskId: string
  done: number
  total: number
  status: string
  error?: string
}

/** 临时入口页面使用独立的极简会话：登记一个匿名设备即可上传。 */
async function ensureAnonymousSession(): Promise<boolean> {
  try {
    await api.config()
    return true
  } catch {
    // 令牌无效或无会话：尝试登记一个匿名会话后再试。
  }
  try {
    const created = await api.createSession('临时上传设备')
    setToken(created.token)
    return true
  } catch {
    return false
  }
}

export function DropPage() {
  const token = useMemo(() => new URLSearchParams(window.location.search).get('token') ?? '', [])
  const [stage, setStage] = useState<Stage>('checking')
  const [message, setMessage] = useState('')
  const [files, setFiles] = useState<SourceFile[]>([])
  const [upload, setUpload] = useState<UploadState | null>(null)
  // 过期时间来自服务端的绝对时间戳；本地只负责倒计时显示。
  const [expiresAt, setExpiresAt] = useState(0)
  const [now, setNow] = useState(Date.now())

  const totalSize = useMemo(() => files.reduce((s, f) => s + f.file.size, 0), [files])

  /** 校验临时入口是否有效（由后端给出结论，前端不自行判断）。 */
  const check = useCallback(async () => {
    setStage('checking')
    if (!token) {
      setStage('invalid')
      setMessage('这个链接缺少临时入口令牌，请向发送者索取完整的地址。')
      return
    }
    const ok = await ensureAnonymousSession()
    if (!ok) {
      setStage('invalid')
      setMessage('无法连接到服务，请确认你与对方处于同一局域网。')
      return
    }
    try {
      const state = await api.dropbox()
      if (!state.exists || !state.enabled || !state.expiresAt || state.expiresAt <= Date.now()) {
        setStage('invalid')
        setMessage('临时接收入口已失效或被关闭。请让接收方重新开启后再索取新的链接。')
        return
      }
      setExpiresAt(state.expiresAt)
      setStage('ready')
    } catch (err) {
      setStage('invalid')
      setMessage(
        err instanceof ApiError ? err.message : '无法验证临时接收入口，请稍后重试。',
      )
    }
  }, [token])

  useEffect(() => {
    void check()
  }, [check])

  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [])

  const remainingMs = expiresAt > 0 ? Math.max(0, expiresAt - now) : 0
  // 倒计时归零时重新向后端确认一次：以后端结论为准，而不是仅凭本地时钟判断。
  useEffect(() => {
    if (stage === 'ready' && expiresAt > 0 && remainingMs === 0) void check()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [remainingMs === 0, stage])

  const start = async () => {
    if (files.length === 0) return
    setStage('uploading')
    setUpload({ taskId: '', done: 0, total: totalSize, status: 'uploading' })
    try {
      const res = await api.createTransfer({
        receiverIds: [],
        dropboxToken: token,
        conflict: 'rename',
        files: files.map((f) => ({
          name: f.file.name,
          relPath: f.relPath,
          size: f.file.size,
          mime: f.file.type || 'application/octet-stream',
          modTime: f.file.lastModified,
        })),
      })
      const task = res.tasks[0]
      if (!task) throw new Error('服务端未返回任务')

      const chunkSize = task.chunkSize || 8 * 1024 * 1024
      const uploader = new TaskUploader(task.id, files, chunkSize, 3, {
        onProgress: (_id, done, total) =>
          setUpload({ taskId: task.id, done, total, status: 'uploading' }),
        onFileDone: () => undefined,
        onFileFailed: (_id, index, code, msg) =>
          setUpload((prev) => ({ ...(prev ?? { taskId: task.id, done: 0, total: totalSize, status: 'uploading' }), error: `文件 #${index + 1}：${msg}${code ? ` (${code})` : ''}` })),
        onTransientError: (_id, msg) =>
          setUpload((prev) => ({ ...(prev ?? { taskId: task.id, done: 0, total: totalSize, status: 'uploading' }), status: msg })),
        onFatalError: (_id, err) => {
          setStage('error')
          setMessage(err.message)
        },
        onFinished: () => {
          setStage('done')
        },
        isActive: () => true,
      })
      // 临时入口免确认：创建后即可上传。
      uploader.authorize()
      await uploader.run()
      setStage('done')
    } catch (err) {
      setStage('error')
      const msg =
        err instanceof ApiError
          ? err.code === 'dropbox_expired'
            ? '临时接收入口已失效，上传被服务端拒绝。请向接收方索取新的链接。'
            : err.message
          : err instanceof Error
            ? err.message
            : '上传失败'
      setMessage(msg)
    }
  }

  return (
    <div
      style={{
        minHeight: '100vh',
        background: 'var(--ns-bg-app)',
        display: 'flex',
        justifyContent: 'center',
        padding: 'var(--ns-sp-6) var(--ns-sp-4)',
      }}
    >
      <div style={{ width: '100%', maxWidth: 620 }}>
        <div className="ns-inline" style={{ marginBottom: 'var(--ns-sp-4)' }}>
          <div className="ns-brand-mark" aria-hidden>
            <InboxOutlined />
          </div>
          <div>
            <div className="ns-brand-title">临时接收箱</div>
            <div className="ns-brand-sub">对方已开启临时接收，文件会直接送达</div>
          </div>
        </div>

        {stage === 'checking' ? (
          <Card>
            <div style={{ textAlign: 'center', padding: 'var(--ns-sp-6)' }}>
              <Spin />
              <div className="ns-meta" style={{ marginTop: 12 }}>
                正在验证临时接收入口…
              </div>
            </div>
          </Card>
        ) : null}

        {stage === 'invalid' ? (
          <Card>
            <Result
              status="warning"
              icon={<StopOutlined style={{ color: 'var(--ns-warning)' }} />}
              title="临时接收入口不可用"
              subTitle={message}
              extra={
                <Button onClick={() => void check()}>重新验证</Button>
              }
            />
          </Card>
        ) : null}

        {stage === 'error' ? (
          <Card>
            <Result
              status="error"
              title="上传失败"
              subTitle={message}
              extra={[
                <Button key="retry" type="primary" onClick={() => void start()}>
                  重试上传
                </Button>,
                <Button key="recheck" onClick={() => void check()}>
                  重新验证入口
                </Button>,
              ]}
            />
          </Card>
        ) : null}

        {stage === 'done' ? (
          <Card>
            <Result
              status="success"
              icon={<CheckCircleOutlined style={{ color: 'var(--ns-success)' }} />}
              title="文件已送达"
              subTitle="文件已通过完整性校验并保存到对方的接收目录，可以关闭本页面了。"
              extra={
                <Button
                  type="primary"
                  onClick={() => {
                    setFiles([])
                    setUpload(null)
                    setStage('ready')
                  }}
                >
                  再传一批文件
                </Button>
              }
            />
          </Card>
        ) : null}

        {stage === 'ready' || stage === 'uploading' ? (
          <div className="ns-stack">
            <Card>
              <div className="ns-inline" style={{ justifyContent: 'space-between', flexWrap: 'wrap' }}>
                <Tag tone="success">
                  <CheckCircleOutlined /> 入口有效
                </Tag>
                <span className="ns-meta">剩余有效时间 {formatCountdown(remainingMs)}</span>
              </div>
            </Card>

            {stage === 'uploading' && upload ? (
              <Card>
                <div className="ns-section">
                  <span className="ns-section-title">正在上传</span>
                  <div className="ns-section-spacer" />
                  <span className="ns-meta">
                    {formatBytes(upload.done)} / {formatBytes(upload.total)}
                  </span>
                </div>
                <div className="ns-progress-track">
                  <div
                    className="ns-progress-fill"
                    style={{ width: `${upload.total > 0 ? Math.min(100, (upload.done / upload.total) * 100) : 0}%` }}
                  />
                </div>
                <div className="ns-meta" style={{ marginTop: 8 }}>
                  请保持本页面处于前台。移动浏览器在切到后台或息屏后可能暂停上传，
                  回到页面后会自动继续（已上传的分块不会重传）。
                </div>
                {upload.error ? (
                  <Alert style={{ marginTop: 8 }} type="warning" showIcon message={upload.error} />
                ) : null}
              </Card>
            ) : (
              <Card>
                <FileDropZone files={files} onChange={setFiles} disabled={stage !== 'ready'} />

                {files.length > 0 ? (
                  <Button
                    type="primary"
                    size="large"
                    block
                    style={{ marginTop: 'var(--ns-sp-3)' }}
                    icon={<CloudUploadOutlined />}
                    onClick={() => void start()}
                  >
                    发送 {files.length} 个文件 · {formatBytes(totalSize)}
                  </Button>
                ) : null}
              </Card>
            )}

            {files.length > 0 && stage === 'ready' ? (
              <Card>
                <div className="ns-section">
                  <span className="ns-section-title">待发送</span>
                </div>
                <div className="ns-stack-sm" style={{ maxHeight: 200, overflowY: 'auto' }}>
                  {files.map((f, i) => (
                    <div className="ns-row" key={`${f.relPath}-${i}`}>
                      <FileIcon name={f.file.name} />
                      <div className="ns-row-main">
                        <div className="ns-row-title">
                          <span className="ns-ellipsis">{truncateMiddle(f.relPath, 40)}</span>
                        </div>
                        <div className="ns-row-sub">{formatBytes(f.file.size)}</div>
                      </div>
                    </div>
                  ))}
                </div>
              </Card>
            ) : null}

            <div className="ns-meta" style={{ textAlign: 'center' }}>
              文件会先保存到对方的接收目录，再由对方决定如何处理同名文件。
              本入口不使用账号，任何拿到链接的人在有效期内都可以上传，请勿转发给不受信任的人。
            </div>
          </div>
        ) : null}
      </div>
    </div>
  )
}
