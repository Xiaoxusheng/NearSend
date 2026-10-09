/**
 * 接收请求确认。
 *
 * 关键规则：默认必须由接收方确认，绝不静默写入。只有在设置中显式开启
 * 「允许可信设备简化接收确认」后，且发送方确实已受信任，才会自动接受
 * （自动接受逻辑在 transfers store 中，此处只负责人工确认界面）。
 */

import { InboxOutlined } from '@ant-design/icons'
import { Button, Modal, Radio, Space, Typography } from 'antd'
import { useMemo, useState } from 'react'

import { formatBytes, formatDateTime, transferStatusMeta } from '../lib/format'
import type { ConflictPolicy } from '../lib/types'
import { useTransfers } from '../store/transfers'
import { DeviceIcon, FileIcon, Tag } from './common'

export function ReceiveRequestModal() {
  const pending = useTransfers((s) => s.pendingIncoming)
  const accept = useTransfers((s) => s.accept)
  const reject = useTransfers((s) => s.reject)

  // 一次只处理最早的请求，避免多个弹窗互相遮挡。
  const requests = pending()
  const current = requests[requests.length - 1]

  const [conflict, setConflict] = useState<ConflictPolicy>('rename')
  const [busy, setBusy] = useState(false)

  const files = useMemo(() => current?.files ?? [], [current])

  if (!current) return null

  const onAccept = async () => {
    setBusy(true)
    try {
      await accept(current.id, conflict)
    } finally {
      setBusy(false)
    }
  }

  const onReject = async () => {
    setBusy(true)
    try {
      await reject(current.id, '接收方拒绝了本次传输')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open
      maskClosable={false}
      keyboard={false}
      // 关闭按钮在这里没有意义：请求必须由用户明确接受或拒绝。
      // 保留一个点了没反应的 X 属于无效控件，因此直接不渲染。
      closable={false}
      width={520}
      centered
      title={
        <span className="ns-inline">
          <InboxOutlined style={{ color: 'var(--ns-warning)' }} />
          收到文件传输请求
          {requests.length > 1 ? (
            <Tag tone="neutral">还有 {requests.length - 1} 个待处理</Tag>
          ) : null}
        </span>
      }
      footer={[
        <Button key="reject" danger onClick={onReject} loading={busy}>
          拒绝
        </Button>,
        <Button key="accept" type="primary" onClick={onAccept} loading={busy}>
          接受并接收
        </Button>,
      ]}
    >
      <div className="ns-pop ns-stack-sm" style={{ gap: 'var(--ns-sp-3)' }}>
        <div className="ns-card" style={{ padding: 'var(--ns-sp-3)', boxShadow: 'none' }}>
          <div className="ns-stat-row" style={{ marginBottom: 'var(--ns-sp-2)' }}>
            <span className="ns-stat">
              <DeviceIcon type="desktop" size={14} />
              <span className="ns-stat-value" style={{ fontSize: 'var(--ns-fs-small)' }}>
                {current.senderName}
              </span>
            </span>
            <span className="ns-stat">
              <span>文件数</span>
              <span className="ns-stat-value">{current.totalFiles}</span>
            </span>
            <span className="ns-stat">
              <span>总大小</span>
              <span className="ns-stat-value">{formatBytes(current.totalSize)}</span>
            </span>
          </div>
          <div className="ns-meta">发起时间：{formatDateTime(current.createdAt)}</div>
          {current.note ? <div className="ns-meta">备注：{current.note}</div> : null}
          <div style={{ marginTop: 6 }}>
            <Tag tone={transferStatusMeta(current.status).tone}>
              {transferStatusMeta(current.status).label}
            </Tag>
          </div>
        </div>

        {files.length > 0 ? (
          <div
            className="ns-scroll-list"
            style={{ maxHeight: 168, border: '1px solid var(--ns-border)', borderRadius: 'var(--ns-radius-md)' }}
          >
            {files.slice(0, 100).map((f) => (
              <div className="ns-row" key={f.id}>
                <FileIcon name={f.name} />
                <div className="ns-row-main">
                  <div className="ns-row-title">
                    <span className="ns-ellipsis" title={f.relPath || f.name}>
                      {f.name}
                    </span>
                  </div>
                  <div className="ns-row-sub">{formatBytes(f.size)}</div>
                </div>
              </div>
            ))}
            {files.length > 100 ? (
              <div className="ns-row ns-meta">仅显示前 100 个文件，共 {files.length} 个</div>
            ) : null}
          </div>
        ) : null}

        <div>
          <Typography.Text style={{ fontSize: 'var(--ns-fs-small)', fontWeight: 550 }}>
            同名文件处理方式
          </Typography.Text>
          <div style={{ marginTop: 6 }}>
            <Radio.Group
              value={conflict}
              onChange={(e) => setConflict(e.target.value as ConflictPolicy)}
            >
              <Space direction="vertical" size={4}>
                <Radio value="rename">
                  自动重命名
                  <span className="ns-meta" style={{ marginLeft: 6 }}>
                    生成「报告 (1).pdf」这样的新名字，不覆盖已有文件
                  </span>
                </Radio>
                <Radio value="overwrite">
                  覆盖已有文件
                  <span className="ns-meta" style={{ marginLeft: 6 }}>
                    同名文件将被新文件替换
                  </span>
                </Radio>
                <Radio value="skip">
                  跳过同名文件
                  <span className="ns-meta" style={{ marginLeft: 6 }}>
                    已存在的文件保持原样，不接收该文件
                  </span>
                </Radio>
              </Space>
            </Radio.Group>
          </div>
        </div>

        <div className="ns-meta">
          接受后文件会保存在服务端的接收目录中，你可以在本页或「传输记录」中下载。
        </div>
      </div>
    </Modal>
  )
}
