/**
 * 传输记录页面。
 *
 * 桌面端使用紧凑表格（表头与内容对齐，长文本截断不造成列错位）；
 * 移动端自动切换为信息分组清晰的任务列表，而不是强制横向滚动整张表格。
 *
 * 「清除记录」只清除元数据，绝不删除接收目录中的文件——确认弹窗中会明确说明。
 */

import { ClearOutlined, ReloadOutlined, SearchOutlined } from '@ant-design/icons'
import { Button, DatePicker, Input, Popconfirm, Segmented, Select, Table, Tooltip } from 'antd'
import type { TableColumnsType } from 'antd'
import type { Dayjs } from 'dayjs'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'

import { Card, EmptyHint, FileIcon, PageHeader, Tag, TransferStatusTag } from '../components/common'
import { ApiError, api } from '../lib/api'
import { fileStatusLabel, formatBytes, formatDateTime, truncateMiddle } from '../lib/format'
import type { HistoryEntry, TransferStatus } from '../lib/types'
import { isPrivileged } from '../store/identity'
import { explainErrorCode, toast } from '../store/notify'

type DirFilter = 'all' | 'in' | 'out'

const STATUS_OPTIONS: { value: TransferStatus; label: string }[] = [
  { value: 'awaiting', label: '等待确认' },
  { value: 'queued', label: '排队中' },
  { value: 'uploading', label: '传输中' },
  { value: 'paused', label: '已暂停' },
  { value: 'completed', label: '已完成' },
  { value: 'failed', label: '失败' },
  { value: 'cancelled', label: '已取消' },
  { value: 'rejected', label: '已拒绝' },
]

function useIsNarrow(): boolean {
  const [narrow, setNarrow] = useState(() =>
    typeof window === 'undefined' ? false : window.innerWidth <= 860,
  )
  useEffect(() => {
    const onResize = () => setNarrow(window.innerWidth <= 860)
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [])
  return narrow
}

export function HistoryPage() {
  const navigate = useNavigate()
  const narrow = useIsNarrow()

  const [entries, setEntries] = useState<HistoryEntry[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [keyword, setKeyword] = useState('')
  const [dir, setDir] = useState<DirFilter>('all')
  const [statuses, setStatuses] = useState<TransferStatus[]>([])
  const [range, setRange] = useState<[Dayjs, Dayjs] | null>(null)
  const [loading, setLoading] = useState(false)
  const [loadedOnce, setLoadedOnce] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [clearing, setClearing] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const res = await api.history({
        keyword: keyword.trim() || undefined,
        dir,
        status: statuses.length ? statuses : undefined,
        from: range ? range[0].startOf('day').valueOf() : undefined,
        to: range ? range[1].endOf('day').valueOf() : undefined,
        page,
        pageSize,
      })
      setEntries(res.entries)
      setTotal(res.total)
      setError(null)
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '无法获取记录'
      setError(message)
      setEntries([])
      setTotal(0)
    } finally {
      setLoading(false)
      setLoadedOnce(true)
    }
  }, [keyword, dir, statuses, range, page, pageSize])

  useEffect(() => {
    void load()
  }, [load])

  const onClear = async () => {
    setClearing(true)
    try {
      const res = await api.clearHistory()
      toast.success(`${res.message}（清除 ${res.cleared} 条记录）`)
      setPage(1)
      await load()
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '清除失败'
      toast.error(message)
    } finally {
      setClearing(false)
    }
  }

  const statusLabel = (e: HistoryEntry) => <TransferStatusTag status={e.status} />

  const columns: TableColumnsType<HistoryEntry> = useMemo(
    () => [
      {
        title: '文件',
        dataIndex: 'fileName',
        key: 'file',
        width: '30%',
        render: (_: string, e: HistoryEntry) => (
          <div className="ns-cell-file">
            <FileIcon name={e.fileName} />
            <Tooltip title={e.fileName}>
              <span className="ns-ellipsis" style={{ maxWidth: 320 }}>
                {truncateMiddle(e.fileName, 40)}
              </span>
            </Tooltip>
          </div>
        ),
      },
      {
        title: '方向',
        dataIndex: 'direction',
        key: 'direction',
        width: 76,
        render: (d: string) => (d === 'in' ? <Tag tone="info">接收</Tag> : <Tag tone="primary">发送</Tag>),
      },
      {
        title: '对方设备',
        key: 'peer',
        width: '18%',
        render: (_: unknown, e: HistoryEntry) => (
          <Tooltip title={e.direction === 'in' ? e.senderName : e.receiverName}>
            <span className="ns-ellipsis" style={{ maxWidth: 180 }}>
              {e.direction === 'in' ? e.senderName : e.receiverName}
            </span>
          </Tooltip>
        ),
      },
      {
        title: '大小',
        dataIndex: 'size',
        key: 'size',
        width: 96,
        align: 'right',
        render: (v: number) => <span className="ns-num">{formatBytes(v)}</span>,
      },
      {
        title: '时间',
        key: 'time',
        width: 148,
        render: (_: unknown, e: HistoryEntry) => (
          <span className="ns-num">{formatDateTime(e.completedAt || e.startedAt)}</span>
        ),
      },
      {
        title: '状态',
        key: 'status',
        width: 128,
        render: (_: unknown, e: HistoryEntry) => (
          <div className="ns-inline" style={{ gap: 4, flexWrap: 'wrap' }}>
            {statusLabel(e)}
            {e.errorCode ? (
              <Tooltip title={explainErrorCode(e.errorCode, e.error ?? '')}>
                <Tag tone="danger">原因</Tag>
              </Tooltip>
            ) : null}
          </div>
        ),
      },
      {
        title: '校验',
        key: 'verified',
        width: 84,
        render: (_: unknown, e: HistoryEntry) =>
          e.verified ? <Tag tone="success">已校验</Tag> : <span className="ns-meta">—</span>,
      },
      {
        title: '操作',
        key: 'action',
        width: 96,
        render: () => (
          <Button size="small" type="link" style={{ padding: 0 }} onClick={() => navigate('/tasks')}>
            查看任务
          </Button>
        ),
      },
    ],
    [navigate],
  )

  const mobileList = (
    <div className="ns-mobile-card-list">
      {entries.map((e, i) => (
        <div className="ns-row ns-row-hover" key={`${e.taskId}-${e.fileId}-${i}`} style={{ alignItems: 'flex-start' }}>
          <FileIcon name={e.fileName} />
          <div className="ns-row-main">
            <div className="ns-row-title">
              <Tooltip title={e.fileName}>
                <span className="ns-ellipsis" style={{ maxWidth: 190 }}>
                  {truncateMiddle(e.fileName, 26)}
                </span>
              </Tooltip>
            </div>
            <div className="ns-row-sub">
              <Tag tone={e.direction === 'in' ? 'info' : 'primary'}>
                {e.direction === 'in' ? '接收' : '发送'}
              </Tag>
              <span>{e.direction === 'in' ? e.senderName : e.receiverName}</span>
              <span>{formatBytes(e.size)}</span>
            </div>
            <div className="ns-row-sub">
              {statusLabel(e)}
              <span>{formatDateTime(e.completedAt || e.startedAt)}</span>
              {e.verified ? <Tag tone="success">已校验</Tag> : null}
              <span className="ns-meta">{fileStatusLabel(e.fileStatus)}</span>
            </div>
            {e.errorCode ? <div className="ns-meta" style={{ color: 'var(--ns-danger)' }}>{explainErrorCode(e.errorCode, e.error ?? '')}</div> : null}
          </div>
          <Button size="small" type="link" style={{ padding: 0 }} onClick={() => navigate('/tasks')}>
            详情
          </Button>
        </div>
      ))}
    </div>
  )

  const canClear = isPrivileged()

  return (
    <>
      <PageHeader
        title="传输记录"
        description="按文件展开的历史记录。清除记录只删除元数据，不会删除接收目录中的任何文件。"
        extra={
          <>
            <Tooltip title="重新获取记录">
              <Button icon={<ReloadOutlined spin={loading} />} onClick={() => void load()}>
                刷新
              </Button>
            </Tooltip>
            {canClear ? (
              <Popconfirm
                title="清除全部历史记录？"
                description={
                  <div style={{ maxWidth: 300, fontSize: 13 }}>
                    将清除所有已完成、失败、取消的记录条目。
                    <strong>接收目录中的文件不会被删除。</strong>
                  </div>
                }
                okText="确认清除"
                okButtonProps={{ danger: true, loading: clearing }}
                cancelText="取消"
                onConfirm={onClear}
              >
                <Button danger icon={<ClearOutlined />}>
                  清除记录
                </Button>
              </Popconfirm>
            ) : (
              <Tooltip title="只有受信任的设备可以清除记录">
                <Button danger icon={<ClearOutlined />} disabled>
                  清除记录
                </Button>
              </Tooltip>
            )}
          </>
        }
      />

      <Card style={{ marginBottom: 'var(--ns-sp-4)' }} className="ns-rise ns-rise-1">
        <div className="ns-stack-sm" style={{ gap: 'var(--ns-sp-3)' }}>
          <div className="ns-inline" style={{ flexWrap: 'wrap', gap: 'var(--ns-sp-2)' }}>
            <Segmented
              value={dir}
              onChange={(v) => {
                setDir(v as DirFilter)
                setPage(1)
              }}
              options={[
                { label: '全部方向', value: 'all' },
                { label: '我接收的', value: 'in' },
                { label: '我发送的', value: 'out' },
              ]}
            />
            <Select
              mode="multiple"
              allowClear
              size="middle"
              placeholder="按状态筛选"
              value={statuses}
              onChange={(v) => {
                setStatuses(v)
                setPage(1)
              }}
              style={{ minWidth: 200, maxWidth: '100%' }}
              options={STATUS_OPTIONS}
              maxTagCount="responsive"
            />
          </div>
          <div className="ns-inline" style={{ flexWrap: 'wrap', gap: 'var(--ns-sp-2)' }}>
            <Input
              allowClear
              prefix={<SearchOutlined style={{ color: 'var(--ns-text-tertiary)' }} />}
              placeholder="搜索文件名、设备名或任务 ID"
              value={keyword}
              onChange={(e) => setKeyword(e.target.value)}
              onPressEnter={() => setPage(1)}
              style={{ width: 280, maxWidth: '100%' }}
            />
            <DatePicker.RangePicker
              value={range}
              onChange={(v) => {
                setRange(v as [Dayjs, Dayjs] | null)
                setPage(1)
              }}
              allowClear
            />
            <Button
              onClick={() => {
                setPage(1)
                void load()
              }}
            >
              应用筛选
            </Button>
            <Button
              type="text"
              onClick={() => {
                setKeyword('')
                setDir('all')
                setStatuses([])
                setRange(null)
                setPage(1)
              }}
            >
              重置
            </Button>
          </div>
        </div>
      </Card>

      <Card padded={narrow} className="ns-rise ns-rise-2">
        {error ? (
          <EmptyHint
            title="无法获取记录"
            description={error}
            action={
              <Button size="small" onClick={() => void load()}>
                重试
              </Button>
            }
          />
        ) : !loadedOnce ? (
          <div className="ns-meta">正在加载记录…</div>
        ) : entries.length === 0 ? (
          <EmptyHint
            title={keyword || statuses.length || range ? '没有符合条件的记录' : '还没有传输记录'}
            description={
              keyword || statuses.length || range
                ? '调整筛选条件后再试。'
                : '完成一次传输后，记录会出现在这里。页面不会展示任何示例数据。'
            }
            action={
              <Button size="small" type="primary" onClick={() => navigate('/')}>
                去发送文件
              </Button>
            }
          />
        ) : narrow ? (
          mobileList
        ) : (
          <div className="ns-table-wrap">
            <Table<HistoryEntry>
              rowKey={(e) => `${e.taskId}-${e.fileId}`}
              columns={columns}
              dataSource={entries}
              size="small"
              loading={loading}
              pagination={{
                current: page,
                pageSize,
                total,
                showSizeChanger: true,
                pageSizeOptions: [10, 20, 50, 100],
                showTotal: (t, r) => `第 ${r[0]}-${r[1]} 条，共 ${t} 条`,
                onChange: (p, ps) => {
                  setPage(p)
                  setPageSize(ps)
                },
              }}
              scroll={{ x: 900 }}
            />
          </div>
        )}

        {/* 记录较多时提示当前页的汇总（用真实条数，不做估算） */}
        {entries.length > 0 && narrow ? (
          <div className="ns-meta" style={{ marginTop: 'var(--ns-sp-3)' }}>
            当前显示 {entries.length} 条，共 {total} 条 · 本页合计{' '}
            {formatBytes(entries.reduce((s, e) => s + e.size, 0))}
          </div>
        ) : null}
      </Card>

      <div className="ns-meta" style={{ marginTop: 'var(--ns-sp-3)' }}>
        状态含义：<strong>已完成</strong> = 分块全部到达并通过 SHA-256 校验，文件已保存到接收目录；
        <strong>已拒绝 / 已取消</strong> = 未产生任何落盘文件；
        <strong>失败</strong> = 未通过校验，可在任务页重试或重新发起。
      </div>
    </>
  )
}
