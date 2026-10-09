/**
 * 文件拖拽 / 选择区。
 *
 * 支持：拖拽（含文件夹，通过 webkitGetAsEntry 递归展开并保留相对路径）、
 * 文件多选、目录选择（使用浏览器支持的 webkitdirectory）。
 * 浏览器不支持目录选择时按钮保持可见但禁用，并说明原因——不做假按钮。
 */

import { DeleteOutlined, FolderOpenOutlined, InboxOutlined } from '@ant-design/icons'
import { Button, Tooltip } from 'antd'
import type { DragEvent } from 'react'
import { useCallback, useMemo, useRef, useState } from 'react'

import { formatBytes, truncateMiddle } from '../lib/format'
import type { SourceFile } from '../lib/uploader'
import { EmptyHint, FileIcon } from './common'

/** 浏览器是否支持目录选择。 */
export const supportsDirectoryPicker = (): boolean => {
  if (typeof document === 'undefined') return false
  const input = document.createElement('input')
  return 'webkitdirectory' in input || 'directory' in input
}

/** 拖拽项读取：优先使用 DataTransferItem 以支持文件夹。 */
async function collectFromDataTransfer(dt: DataTransfer): Promise<SourceFile[]> {
  const out: SourceFile[] = []

  const readEntry = async (entry: FileSystemEntry, prefix: string): Promise<void> => {
    if (entry.isFile) {
      const fileEntry = entry as FileSystemFileEntry
      const file = await new Promise<File | null>((resolve) => {
        fileEntry.file(
          (f) => resolve(f),
          () => resolve(null),
        )
      })
      if (file) out.push({ file, relPath: prefix ? `${prefix}/${file.name}` : file.name })
      return
    }
    const dirEntry = entry as FileSystemDirectoryEntry
    const reader = dirEntry.createReader()
    const entries: FileSystemEntry[] = []
    // readEntries 每次最多返回 100 项，必须循环读取直到为空。
    for (;;) {
      const batch = await new Promise<FileSystemEntry[]>((resolve) => {
        reader.readEntries(
          (res) => resolve(res),
          () => resolve([]),
        )
      })
      if (batch.length === 0) break
      entries.push(...batch)
    }
    const nextPrefix = prefix ? `${prefix}/${entry.name}` : entry.name
    for (const child of entries) {
      await readEntry(child, nextPrefix)
    }
  }

  const items = dt.items
  if (items && items.length > 0 && typeof items[0].webkitGetAsEntry === 'function') {
    const entries: FileSystemEntry[] = []
    for (let i = 0; i < items.length; i++) {
      const entry = items[i].webkitGetAsEntry()
      if (entry) entries.push(entry)
    }
    if (entries.length > 0) {
      for (const entry of entries) await readEntry(entry, '')
      if (out.length > 0) return out
    }
  }

  // 回退：普通文件列表（不支持目录时）。
  for (let i = 0; i < dt.files.length; i++) {
    const f = dt.files[i]
    // webkitRelativePath 在有目录选择时会被浏览器填充。
    const rel = (f as File & { webkitRelativePath?: string }).webkitRelativePath || f.name
    out.push({ file: f, relPath: rel })
  }
  return out
}

export function FileDropZone({
  files,
  onChange,
  disabled,
  disabledReason,
}: {
  files: SourceFile[]
  onChange: (files: SourceFile[]) => void
  disabled?: boolean
  disabledReason?: string
}) {
  const [active, setActive] = useState(false)
  const dragDepth = useRef(0)
  const fileInputRef = useRef<HTMLInputElement>(null)
  const dirInputRef = useRef<HTMLInputElement>(null)
  const dirSupported = useMemo(() => supportsDirectoryPicker(), [])

  const totalSize = useMemo(() => files.reduce((sum, f) => sum + f.file.size, 0), [files])

  const addFiles = useCallback(
    (incoming: SourceFile[]) => {
      if (incoming.length === 0) return
      // 以「相对路径 + 大小 + 修改时间」作为去重键，避免重复拖入同一文件。
      const seen = new Set(files.map((f) => keyOf(f)))
      const merged = [...files]
      for (const item of incoming) {
        const k = keyOf(item)
        if (seen.has(k)) continue
        seen.add(k)
        merged.push(item)
      }
      onChange(merged)
    },
    [files, onChange],
  )

  const onDrop = async (e: DragEvent<HTMLDivElement>) => {
    e.preventDefault()
    dragDepth.current = 0
    setActive(false)
    if (disabled) return
    const items = await collectFromDataTransfer(e.dataTransfer)
    addFiles(items)
  }

  const onDragEnter = (e: DragEvent<HTMLDivElement>) => {
    e.preventDefault()
    if (disabled) return
    dragDepth.current += 1
    setActive(true)
  }

  const onDragLeave = (e: DragEvent<HTMLDivElement>) => {
    e.preventDefault()
    dragDepth.current = Math.max(0, dragDepth.current - 1)
    if (dragDepth.current === 0) setActive(false)
  }

  const removeAt = (index: number) => {
    const next = [...files]
    next.splice(index, 1)
    onChange(next)
  }

  return (
    <div className="ns-stack-sm">
      <div
        className={`ns-dropzone ${active ? 'is-active' : ''}`}
        onDrop={onDrop}
        onDragOver={(e) => e.preventDefault()}
        onDragEnter={onDragEnter}
        onDragLeave={onDragLeave}
        onClick={() => {
          if (disabled) return
          fileInputRef.current?.click()
        }}
        role="button"
        tabIndex={0}
        aria-label="选择要发送的文件"
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault()
            if (!disabled) fileInputRef.current?.click()
          }
        }}
        style={disabled ? { opacity: 0.6, cursor: 'not-allowed' } : undefined}
      >
        <div className="ns-dropzone-icon" aria-hidden>
          <InboxOutlined />
        </div>
        <div className="ns-dropzone-title">
          {disabled ? '当前无法选择文件' : '拖拽文件到这里，或点击选择'}
        </div>
        <div className="ns-dropzone-hint">
          {disabled
            ? (disabledReason ?? '请先完成上一步操作')
            : '支持常见文档、图片、视频、压缩包与整个文件夹'}
        </div>
        <div className="ns-inline" style={{ marginTop: 'var(--ns-sp-2)' }} onClick={(e) => e.stopPropagation()}>
          <Button
            type="primary"
            size="middle"
            disabled={disabled}
            onClick={() => fileInputRef.current?.click()}
          >
            选择文件
          </Button>
          <Tooltip title={dirSupported ? '选择一个文件夹，将保留其内部目录结构' : '当前浏览器不支持文件夹选择'}>
            <Button
              size="middle"
              icon={<FolderOpenOutlined />}
              disabled={disabled || !dirSupported}
              onClick={() => dirInputRef.current?.click()}
            >
              选择文件夹
            </Button>
          </Tooltip>
        </div>
      </div>

      <input
        ref={fileInputRef}
        type="file"
        multiple
        hidden
        onChange={(e) => {
          const list = Array.from(e.target.files ?? []).map((file) => ({
            file,
            relPath: file.name,
          }))
          addFiles(list)
          e.target.value = ''
        }}
      />
      <input
        ref={dirInputRef}
        type="file"
        hidden
        multiple
        // webkitdirectory 使文件选择器切换为目录模式；同时拿到相对路径。
        {...({ webkitdirectory: '', directory: '' } as Record<string, string>)}
        onChange={(e) => {
          const list = Array.from(e.target.files ?? []).map((file) => ({
            file,
            relPath:
              (file as File & { webkitRelativePath?: string }).webkitRelativePath || file.name,
          }))
          addFiles(list)
          e.target.value = ''
        }}
      />

      {files.length === 0 ? (
        <EmptyHint
          title="尚未选择文件"
          description="选择文件后，右侧选择接收设备即可发送。文件只在局域网内传输，不经过外网。"
        />
      ) : (
        <>
          <div className="ns-inline" style={{ justifyContent: 'space-between' }}>
            <div className="ns-meta">
              已选择 <strong className="ns-num">{files.length}</strong> 个文件 · 共{' '}
              <strong className="ns-num">{formatBytes(totalSize)}</strong>
            </div>
            <Button size="small" type="text" onClick={() => onChange([])}>
              清空选择
            </Button>
          </div>
          <div
            className="ns-scroll-list"
            style={{
              maxHeight: 216,
              border: '1px solid var(--ns-border)',
              borderRadius: 'var(--ns-radius-md)',
            }}
          >
            {files.slice(0, 200).map((f, index) => (
              <div className="ns-row ns-row-hover" key={`${keyOf(f)}-${index}`}>
                <FileIcon name={f.file.name} />
                <div className="ns-row-main">
                  <div className="ns-row-title">
                    <Tooltip title={f.relPath}>
                      <span className="ns-ellipsis">{truncateMiddle(f.relPath, 46)}</span>
                    </Tooltip>
                  </div>
                  <div className="ns-row-sub">
                    <span>{formatBytes(f.file.size)}</span>
                    {f.relPath.includes('/') ? <span>保留目录结构</span> : null}
                  </div>
                </div>
                <div className="ns-row-actions">
                  <Tooltip title="从列表移除">
                    <button
                      type="button"
                      className="ns-icon-btn is-danger"
                      aria-label={`移除 ${f.file.name}`}
                      onClick={() => removeAt(index)}
                    >
                      <DeleteOutlined />
                    </button>
                  </Tooltip>
                </div>
              </div>
            ))}
            {files.length > 200 ? (
              <div className="ns-row ns-meta">
                列表仅展示前 200 个文件，发送时会包含全部 {files.length} 个文件。
              </div>
            ) : null}
          </div>
        </>
      )}
    </div>
  )
}

function keyOf(f: SourceFile): string {
  return `${f.relPath}|${f.file.size}|${f.file.lastModified}`
}
