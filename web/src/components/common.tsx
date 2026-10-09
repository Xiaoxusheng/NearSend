/**
 * 通用展示组件。
 *
 * 所有组件只用 tokens.css 中的 CSS 变量，不硬编码颜色；
 * 图标统一来自 @ant-design/icons，不使用 Emoji 代替 UI 图标。
 */

import {
  ApiOutlined,
  AudioOutlined,
  CheckOutlined,
  CodeOutlined,
  CopyOutlined,
  DesktopOutlined,
  FileExcelOutlined,
  FileImageOutlined,
  FileOutlined,
  FilePdfOutlined,
  FilePptOutlined,
  FileTextOutlined,
  FileZipOutlined,
  FolderOutlined,
  LaptopOutlined,
  MobileOutlined,
  TabletOutlined,
  VideoCameraOutlined,
} from '@ant-design/icons'
import { Button, Tooltip, Typography } from 'antd'
import type { ReactNode } from 'react'
import { useEffect, useState } from 'react'

import { fileKind, transferStatusMeta, type FileKind } from '../lib/format'
import type { DeviceType } from '../lib/types'

// ---------------------------------------------------------------- 状态标签 ---

type Tone = 'neutral' | 'primary' | 'success' | 'warning' | 'danger' | 'info'

export function Tag({
  tone = 'neutral',
  children,
  title,
}: {
  tone?: Tone
  children: ReactNode
  title?: string
}) {
  return (
    <span className={`ns-tag ns-tag-${tone}`} title={title}>
      {children}
    </span>
  )
}

/** 传输状态标签：颜色语义克制，不使用渐变或高饱和荧光色。 */
export function TransferStatusTag({ status }: { status: string }) {
  const meta = transferStatusMeta(status)
  return <Tag tone={meta.tone}>{meta.label}</Tag>
}

// ---------------------------------------------------------------- 页面结构 ---

export function PageHeader({
  title,
  description,
  extra,
}: {
  title: string
  description?: ReactNode
  extra?: ReactNode
}) {
  return (
    <div className="ns-page-head">
      <div
        style={{
          display: 'flex',
          alignItems: 'flex-end',
          gap: 'var(--ns-sp-4)',
          flexWrap: 'wrap',
        }}
      >
        <div style={{ minWidth: 0, flex: '1 1 auto' }}>
          <h1 className="ns-page-title">{title}</h1>
          {description ? <div className="ns-page-desc">{description}</div> : null}
        </div>
        {extra ? <div className="ns-topbar-actions">{extra}</div> : null}
      </div>
    </div>
  )
}

export function Section({
  title,
  extra,
  children,
  style,
}: {
  title?: ReactNode
  extra?: ReactNode
  children?: ReactNode
  style?: React.CSSProperties
}) {
  return (
    <section style={style}>
      {title || extra ? (
        <div className="ns-section">
          <div className="ns-section-title">{title}</div>
          <div className="ns-section-spacer" />
          {extra}
        </div>
      ) : null}
      {children}
    </section>
  )
}

export function Card({
  children,
  padded = true,
  style,
  className,
}: {
  children: ReactNode
  padded?: boolean
  style?: React.CSSProperties
  className?: string
}) {
  return (
    <div className={`ns-card ${padded ? 'ns-card-pad' : ''} ${className ?? ''}`} style={style}>
      {children}
    </div>
  )
}

/** 空状态：说明「为什么空」以及「下一步做什么」，不放置装饰性插图。 */
export function EmptyHint({
  title,
  description,
  action,
  icon,
}: {
  title: string
  description?: ReactNode
  action?: ReactNode
  icon?: ReactNode
}) {
  return (
    <div
      style={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        justifyContent: 'center',
        gap: 'var(--ns-sp-2)',
        padding: 'var(--ns-sp-6) var(--ns-sp-4)',
        textAlign: 'center',
      }}
    >
      {icon ? (
        <div style={{ fontSize: 26, color: 'var(--ns-text-tertiary)', lineHeight: 1 }}>{icon}</div>
      ) : null}
      <div style={{ fontSize: 'var(--ns-fs-small)', fontWeight: 550 }}>{title}</div>
      {description ? (
        <div
          style={{
            fontSize: 'var(--ns-fs-tiny)',
            color: 'var(--ns-text-tertiary)',
            maxWidth: 460,
            lineHeight: 1.6,
          }}
        >
          {description}
        </div>
      ) : null}
      {action ? <div style={{ marginTop: 'var(--ns-sp-2)' }}>{action}</div> : null}
    </div>
  )
}

/** 错误状态：展示可执行的原因与重试入口。 */
export function ErrorHint({
  message,
  onRetry,
  retryText = '重试',
}: {
  message: string
  onRetry?: () => void
  retryText?: string
}) {
  return (
    <EmptyHint
      icon={<ApiOutlined />}
      title="出现了问题"
      description={message}
      action={
        onRetry ? (
          <Button size="small" onClick={onRetry}>
            {retryText}
          </Button>
        ) : undefined
      }
    />
  )
}

// ---------------------------------------------------------------- 键值展示 ---

export function KeyValues({ items }: { items: { key: string; value: ReactNode }[] }) {
  return (
    <div className="ns-kv">
      {items.map((it) => (
        <div key={it.key} style={{ display: 'contents' }}>
          <div className="ns-kv-key">{it.key}</div>
          <div className="ns-kv-val">{it.value}</div>
        </div>
      ))}
    </div>
  )
}

// ---------------------------------------------------------------- 图标 ---

const FILE_ICON: Record<FileKind, { icon: ReactNode; color: string }> = {
  image: { icon: <FileImageOutlined />, color: 'var(--ns-info)' },
  video: { icon: <VideoCameraOutlined />, color: 'var(--ns-primary)' },
  audio: { icon: <AudioOutlined />, color: 'var(--ns-warning)' },
  archive: { icon: <FileZipOutlined />, color: 'var(--ns-warning)' },
  document: { icon: <FileTextOutlined />, color: 'var(--ns-primary)' },
  spreadsheet: { icon: <FileExcelOutlined />, color: 'var(--ns-success)' },
  presentation: { icon: <FilePptOutlined />, color: 'var(--ns-danger)' },
  pdf: { icon: <FilePdfOutlined />, color: 'var(--ns-danger)' },
  code: { icon: <CodeOutlined />, color: 'var(--ns-text-secondary)' },
  folder: { icon: <FolderOutlined />, color: 'var(--ns-warning)' },
  file: { icon: <FileOutlined />, color: 'var(--ns-text-tertiary)' },
}

export function FileIcon({ name, size = 15 }: { name: string; size?: number }) {
  const kind = fileKind(name)
  const meta = FILE_ICON[kind]
  return <span style={{ color: meta.color, fontSize: size, lineHeight: 1 }}>{meta.icon}</span>
}

export function DeviceIcon({ type, size = 16 }: { type: DeviceType | string; size?: number }) {
  const style = { fontSize: size, color: 'var(--ns-text-secondary)' }
  switch (type) {
    case 'mobile':
      return <MobileOutlined style={style} />
    case 'tablet':
      return <TabletOutlined style={style} />
    case 'laptop':
      return <LaptopOutlined style={style} />
    default:
      return <DesktopOutlined style={style} />
  }
}

// ---------------------------------------------------------------- 交互 ---

/** 复制按钮：复制成功/失败都有明确反馈。 */
export function CopyButton({
  text,
  label,
  size = 'small',
  onCopied,
  type = 'text',
}: {
  text: string
  label?: string
  size?: 'small' | 'middle' | 'large'
  onCopied?: (ok: boolean) => void
  type?: 'text' | 'default' | 'primary'
}) {
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    if (!copied) return
    const timer = window.setTimeout(() => setCopied(false), 1800)
    return () => window.clearTimeout(timer)
  }, [copied])

  const doCopy = async () => {
    let ok = false
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(text)
        ok = true
      } else {
        // 非安全上下文（明文 HTTP 局域网）下 clipboard API 可能不可用，走兜底方案。
        const el = document.createElement('textarea')
        el.value = text
        el.style.position = 'fixed'
        el.style.opacity = '0'
        document.body.appendChild(el)
        el.select()
        ok = document.execCommand('copy')
        document.body.removeChild(el)
      }
    } catch {
      ok = false
    }
    setCopied(ok)
    onCopied?.(ok)
  }

  return (
    <Tooltip title={copied ? '已复制' : `复制${label ?? ''}`}>
      <Button size={size} type={type} icon={copied ? <CheckOutlined /> : <CopyOutlined />} onClick={doCopy}>
        {label}
      </Button>
    </Tooltip>
  )
}

export function Mono({ children }: { children: ReactNode }) {
  return (
    <Typography.Text className="ns-mono" style={{ fontSize: 'var(--ns-fs-tiny)' }} copyable={false}>
      {children}
    </Typography.Text>
  )
}

/** 带 Tooltip 的省略文本：长文件名截断后仍可查看完整名称。 */
export function EllipsisText({
  text,
  max,
  className,
  style,
}: {
  text: string
  max?: number
  className?: string
  style?: React.CSSProperties
}) {
  const display = max && text.length > max ? `${text.slice(0, max)}…` : text
  return (
    <Tooltip title={text.length > (max ?? 0) ? text : undefined}>
      <span className={`ns-ellipsis ${className ?? ''}`} style={style}>
        {display}
      </span>
    </Tooltip>
  )
}
