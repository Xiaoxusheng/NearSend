/**
 * 网络诊断。
 *
 * 每一项都是刚执行的真实探测结果，并附带检测时间。
 * 不播放「检测成功」的虚假动画：数据没回来之前显示加载中，回来了就显示真实的结论。
 * 无法确定的结论不会写成确定结论（例如端口占用只说明事实与建议，不猜测占用者身份）。
 */

import {
  ApiOutlined,
  CheckCircleOutlined,
  CloseCircleOutlined,
  CopyOutlined,
  ExclamationCircleOutlined,
  HddOutlined,
  ReloadOutlined,
  WifiOutlined,
} from '@ant-design/icons'
import { Alert, Button, Tooltip } from 'antd'
import { useCallback, useEffect, useState } from 'react'

import { Card, ErrorHint, KeyValues, PageHeader, Tag } from '../components/common'
import { ApiError, api } from '../lib/api'
import { formatBytes, formatDateTimeSec, formatDuration } from '../lib/format'
import type { Diagnostics } from '../lib/types'
import { hashCapability, selfTestSha256 } from '../lib/sha256'
import { explainErrorCode, toast } from '../store/notify'
import { useSession } from '../store/session'

type CheckTone = 'ok' | 'warn' | 'bad'

function toneOf(ok: boolean, warn = false): CheckTone {
  if (ok) return 'ok'
  return warn ? 'warn' : 'bad'
}

function ToneTag({ tone, text }: { tone: CheckTone; text: string }) {
  if (tone === 'ok') {
    return (
      <Tag tone="success">
        <CheckCircleOutlined /> {text}
      </Tag>
    )
  }
  if (tone === 'warn') {
    return (
      <Tag tone="warning">
        <ExclamationCircleOutlined /> {text}
      </Tag>
    )
  }
  return (
    <Tag tone="danger">
      <CloseCircleOutlined /> {text}
    </Tag>
  )
}

const HASH_LABEL: Record<ReturnType<typeof hashCapability>, string> = {
  webcrypto: '浏览器原生（硬件加速）',
  worker: 'Web Worker + 纯 JS（多核并行）',
  'main-thread': '主线程纯 JS（分片让出，界面仍可操作）',
}

export function DiagnosticsPage() {
  const [data, setData] = useState<Diagnostics | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [hashResult, setHashResult] = useState<string | null>(null)
  const [hashChecked, setHashChecked] = useState(false)

  const wsStatus = useSession((s) => s.wsStatus)
  const wsError = useSession((s) => s.wsError)
  const reconnectWs = useSession((s) => s.reconnectWs)
  const lastResyncAt = useSession((s) => s.lastResyncAt)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const d = await api.diagnostics()
      setData(d)
      setError(null)
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '无法获取诊断信息'
      setError(message)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // 前端哈希能力自检：与后端的分块校验直接相关，因此在这里如实展示。
  useEffect(() => {
    let cancelled = false
    void (async () => {
      const result = await selfTestSha256()
      if (!cancelled) {
        setHashResult(result)
        setHashChecked(true)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  const copyAll = () => {
    if (!data) return
    const lines = [
      `局域网快传 诊断信息`,
      `检测时间: ${formatDateTimeSec(data.checkedAt)}`,
      `版本: ${data.service.version} (${data.service.goVersion})`,
      `运行时长: ${formatDuration(data.service.uptimeSec)}`,
      `监听: ${data.service.listenAddr}:${data.service.port}`,
      `本机自检: ${data.service.selfTest.ok ? '成功' : `失败 - ${data.service.selfTest.error}`}`,
      `端口可用: ${data.service.portFree ? '是' : '否'}`,
      `在线设备: ${data.client.wsDevices}，WebSocket 连接数: ${data.client.wsConnections}`,
      `客户端 IP: ${data.client.ip}`,
      `客户端实时连接: ${data.client.wsOnline ? '正常' : '断开'}`,
      `磁盘可用: ${formatBytes(data.storage.diskFreeBytes)} / ${formatBytes(data.storage.diskTotalBytes)}`,
      `接收目录: ${data.storage.receiveDir}`,
      `接收占用: ${data.storage.receiveFiles} 个文件 / ${formatBytes(data.storage.receiveBytes)}`,
      `临时占用: ${data.storage.tempFiles} 个文件 / ${formatBytes(data.storage.tempBytes)}`,
      `任务: 进行中 ${data.transfers.active}，失败 ${data.transfers.failed}，完成 ${data.transfers.completed}`,
      `访问地址:`,
      ...data.addresses.map((a) => `  ${a.url} (${a.family}, ${a.iface})${a.recommended ? ' [推荐]' : ''}`),
      data.recentError
        ? `最近错误: [${data.recentError.code}] ${data.recentError.message} @ ${formatDateTimeSec(data.recentError.at)}`
        : '最近错误: 无',
    ]
    void navigator.clipboard
      ?.writeText(lines.join('\n'))
      .then(() => toast.success('诊断信息已复制到剪贴板'))
      .catch(() => toast.error('复制失败，请手动选择文本复制'))
  }

  return (
    <>
      <PageHeader
        title="网络诊断"
        description="所有检测项都实时执行，并标注检测时间。未经验证的推测不会被写成结论。"
        extra={
          <>
            <span className="ns-meta">
              {data ? `检测于 ${formatDateTimeSec(data.checkedAt)}` : '尚未检测'}
            </span>
            <Button icon={<ReloadOutlined spin={loading} />} onClick={() => void load()} loading={loading}>
              重新检测
            </Button>
            <Button icon={<CopyOutlined />} onClick={copyAll} disabled={!data}>
              复制诊断信息
            </Button>
          </>
        }
      />

      {error ? (
        <Card>
          <ErrorHint message={error} onRetry={() => void load()} retryText="重新检测" />
        </Card>
      ) : null}

      <div className="ns-two-col" style={{ marginTop: error ? 'var(--ns-sp-4)' : 0 }}>
        {/* 服务监听 */}
        <Card className="ns-rise ns-rise-1">
          <div className="ns-section">
            <ApiOutlined style={{ color: 'var(--ns-primary)' }} />
            <span className="ns-section-title">服务监听</span>
            <div className="ns-section-spacer" />
            <ToneTag
              tone={toneOf(data?.service.status === 'ok', false)}
              text={data?.service.status === 'ok' ? '正常' : '异常'}
            />
          </div>
          <KeyValues
            items={[
              { key: '监听地址', value: <span className="ns-mono">{data ? `${data.service.listenAddr}:${data.service.port}` : '—'}</span> },
              { key: '版本', value: `${data?.service.version ?? '—'} (${data?.service.goVersion ?? '—'})` },
              { key: '运行时长', value: data ? formatDuration(data.service.uptimeSec) : '—' },
              {
                key: '本机自检',
                value: data ? (
                  <span className="ns-inline">
                    <ToneTag tone={toneOf(data.service.selfTest.ok)} text={data.service.selfTest.ok ? '连接成功' : '连接失败'} />
                    {!data.service.selfTest.ok && data.service.selfTest.error ? (
                      <Tooltip title={data.service.selfTest.error}>
                        <span className="ns-meta">查看原因</span>
                      </Tooltip>
                    ) : null}
                  </span>
                ) : (
                  '—'
                ),
              },
              {
                key: '端口状态',
                value: <span className="ns-meta">{data?.service.portNote ?? '—'}</span>,
              },
            ]}
          />
          <div className="ns-meta" style={{ marginTop: 'var(--ns-sp-2)' }}>
            本机自检是对 127.0.0.1 发起的一次真实 TCP 连接尝试；失败通常意味着监听异常或本机防火墙策略生效。
          </div>
        </Card>

        {/* 客户端连接 */}
        <Card className="ns-rise ns-rise-2">
          <div className="ns-section">
            <WifiOutlined style={{ color: 'var(--ns-primary)' }} />
            <span className="ns-section-title">客户端连接</span>
            <div className="ns-section-spacer" />
            <ToneTag tone={toneOf(wsStatus === 'online')} text={wsStatus === 'online' ? '正常' : '断开'} />
          </div>
          <KeyValues
            items={[
              { key: '当前设备', value: `${data?.client.deviceName ?? '—'}（${data?.client.os ?? '—'} / ${data?.client.browser ?? '—'}）` },
              { key: '来源 IP', value: <span className="ns-mono">{data?.client.ip ?? '—'}</span> },
              {
                key: 'WebSocket',
                value: (
                  <span className="ns-inline">
                    <ToneTag
                      tone={toneOf(Boolean(data?.client.wsOnline))}
                      text={data?.client.wsOnline ? '已连接' : '未连接'}
                    />
                    {wsError ? <span className="ns-meta">{wsError}</span> : null}
                  </span>
                ),
              },
              {
                key: '连接统计',
                value: data ? `${data.client.wsDevices} 台设备 / ${data.client.wsConnections} 条连接` : '—',
              },
              {
                key: '最近重连同步',
                value: lastResyncAt ? formatDateTimeSec(lastResyncAt) : '本次会话尚未发生过断线重连',
              },
            ]}
          />
          <div className="ns-inline" style={{ marginTop: 'var(--ns-sp-2)' }}>
            <Button size="small" onClick={reconnectWs}>
              重新连接实时通道
            </Button>
            <span className="ns-meta">断线期间页面仍可查看数据，但无法收到实时进度。</span>
          </div>
        </Card>
      </div>

      <div className="ns-two-col" style={{ marginTop: 'var(--ns-sp-4)' }}>
        {/* 存储状态 */}
        <Card className="ns-rise ns-rise-2">
          <div className="ns-section">
            <HddOutlined style={{ color: 'var(--ns-primary)' }} />
            <span className="ns-section-title">存储状态</span>
            <div className="ns-section-spacer" />
            <ToneTag
              tone={
                data?.storage.diskProbeFailed
                  ? 'warn'
                  : toneOf((data?.storage.diskFreeBytes ?? 0) > 512 * 1024 * 1024, true)
              }
              text={
                data?.storage.diskProbeFailed
                  ? '无法探测'
                  : (data?.storage.diskFreeBytes ?? 0) > 512 * 1024 * 1024
                    ? '空间充足'
                    : '空间偏低'
              }
            />
          </div>
          <KeyValues
            items={[
              { key: '磁盘可用', value: data ? `${formatBytes(data.storage.diskFreeBytes)} / ${formatBytes(data.storage.diskTotalBytes)}` : '—' },
              { key: '接收目录', value: <span className="ns-mono">{data?.storage.receiveDir ?? '—'}</span> },
              { key: '接收占用', value: data ? `${data.storage.receiveFiles} 个文件 / ${formatBytes(data.storage.receiveBytes)}` : '—' },
              { key: '临时目录', value: <span className="ns-mono">{data?.storage.tempDir ?? '—'}</span> },
              { key: '临时占用', value: data ? `${data.storage.tempFiles} 个文件 / ${formatBytes(data.storage.tempBytes)}` : '—' },
            ]}
          />
          {data?.storage.diskProbeFailed ? (
            <div className="ns-meta" style={{ marginTop: 6, color: 'var(--ns-warning)' }}>
              当前系统不支持磁盘空间探测接口，因此无法给出可用空间。这不会阻塞传输，真实写入失败时会返回明确错误。
            </div>
          ) : null}
        </Card>

        {/* 完整性与校验能力 */}
        <Card className="ns-rise ns-rise-3">
          <div className="ns-section">
            <CheckCircleOutlined style={{ color: 'var(--ns-primary)' }} />
            <span className="ns-section-title">文件校验能力</span>
            <div className="ns-section-spacer" />
            <ToneTag
              tone={hashChecked ? toneOf(hashResult === null) : 'warn'}
              text={!hashChecked ? '检测中…' : hashResult === null ? '正常工作' : '异常'}
            />
          </div>
          <KeyValues
            items={[
              { key: '计算方式', value: HASH_LABEL[hashCapability()] },
              { key: '安全上下文', value: typeof isSecureContext !== 'undefined' && isSecureContext ? '是（可使用 Web Crypto）' : '否（明文 HTTP，已自动降级）' },
              {
                key: '自检结果',
                value: !hashChecked ? '检测中…' : hashResult === null ? 'SHA-256 测试向量全部通过' : hashResult,
              },
            ]}
          />
          <div className="ns-meta" style={{ marginTop: 'var(--ns-sp-2)' }}>
            为什么要在前端算哈希：局域网通常是明文 HTTP，crypto.subtle 不可用。若不降级实现，
            分块校验会静默失效。这里如实显示当前实际使用的实现方式。
          </div>
        </Card>
      </div>

      {/* 最近错误 */}
      <Card className="ns-rise ns-rise-3" style={{ marginTop: 'var(--ns-sp-4)' }}>
        <div className="ns-section">
          <span className="ns-section-title">最近一次传输失败</span>
        </div>
        {data?.recentError ? (
          <Alert
            type="error"
            showIcon
            message={explainErrorCode(data.recentError.code, data.recentError.message)}
            description={
              <div className="ns-stack-sm" style={{ gap: 4, fontSize: 13 }}>
                <span>
                  涉及设备：{data.recentError.senderName} → {data.recentError.receiverName}
                </span>
                <span>发生时间：{formatDateTimeSec(data.recentError.at)}</span>
                <span className="ns-mono">任务 ID：{data.recentError.taskId}</span>
                <span className="ns-mono">错误码：{data.recentError.code}</span>
              </div>
            }
            action={
              <Button size="small" onClick={() => void load()}>
                重新检测
              </Button>
            }
          />
        ) : (
          <div className="ns-meta">最近没有失败的传输任务（本次检测未发现失败记录）。</div>
        )}
      </Card>

      {/* 常见问题排查 */}
      <Card className="ns-rise ns-rise-3" style={{ marginTop: 'var(--ns-sp-4)' }}>
        <div className="ns-section">
          <span className="ns-section-title">常见问题排查</span>
          <div className="ns-section-spacer" />
          <Tag tone="neutral">按实际检测结果生成</Tag>
        </div>
        <ul
          style={{
            margin: 0,
            paddingLeft: 18,
            fontSize: 'var(--ns-fs-small)',
            color: 'var(--ns-text-secondary)',
            lineHeight: 1.9,
          }}
        >
          {(data?.tips ?? []).map((tip) => (
            <li key={tip}>{tip}</li>
          ))}
        </ul>
        <div className="ns-divider" />
        <div className="ns-meta" style={{ lineHeight: 1.9 }}>
          <strong>关于安全边界：</strong>本服务通过明文 HTTP 在局域网内提供访问，没有启用 TLS，
          因此<strong>不具备端到端加密能力</strong>。同一网络中的其它设备在理论上可以嗅探流量。
          请仅在可信的局域网（例如你自己的家庭网络）中使用；不要在公共 Wi-Fi 或不受信任的网络上传输敏感文件。
        </div>
      </Card>
    </>
  )
}
