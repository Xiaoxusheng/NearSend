/**
 * 传输任务页面。
 *
 * 顶部是轻量的状态筛选器（不是一排巨大的统计卡片），下方为任务列表，
 * 支持搜索与批量操作。详情通过任务条目内的展开区呈现。
 */

import { ReloadOutlined, SearchOutlined } from '@ant-design/icons'
import { Button, Input, Segmented, Tooltip } from 'antd'
import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'

import { TaskList } from '../components/TaskList'
import { Card, EmptyHint, PageHeader, Tag } from '../components/common'
import type { Transfer } from '../lib/types'
import { useTransfers, isTerminal } from '../store/transfers'


type FilterKey = 'all' | 'awaiting' | 'queued' | 'active' | 'done' | 'problem'

const FILTERS: { key: FilterKey; label: string; match: (t: Transfer) => boolean }[] = [
  { key: 'all', label: '全部任务', match: () => true },
  { key: 'awaiting', label: '等待确认', match: (t) => t.status === 'awaiting' },
  { key: 'queued', label: '排队中', match: (t) => t.status === 'queued' },
  { key: 'active', label: '传输中', match: (t) => t.status === 'uploading' || t.status === 'verifying' },
  { key: 'done', label: '已完成', match: (t) => t.status === 'completed' },
  {
    key: 'problem',
    label: '失败与已取消',
    match: (t) => t.status === 'failed' || t.status === 'cancelled' || t.status === 'rejected' || t.status === 'paused',
  },
]

export function TasksPage() {
  const navigate = useNavigate()
  const tasks = useTransfers((s) => s.tasks)
  const loading = useTransfers((s) => s.loading)
  const load = useTransfers((s) => s.load)
  const error = useTransfers((s) => s.error)

  const [filter, setFilter] = useState<FilterKey>('all')
  const [keyword, setKeyword] = useState('')
  const [onlyMine, setOnlyMine] = useState(false)

  const all = useMemo(() => Object.values(tasks).sort((a, b) => b.createdAt - a.createdAt), [tasks])

  const counts = useMemo(() => {
    const out: Record<FilterKey, number> = { all: 0, awaiting: 0, queued: 0, active: 0, done: 0, problem: 0 }
    for (const t of all) {
      for (const f of FILTERS) {
        if (f.match(t)) out[f.key] += 1
      }
    }
    return out
  }, [all])

  const filtered = useMemo(() => {
    const matcher = FILTERS.find((f) => f.key === filter)?.match ?? (() => true)
    const k = keyword.trim().toLowerCase()
    return all.filter((t) => {
      if (!matcher(t)) return false
      if (onlyMine) {
        return true // 「仅我参与」在 listTransfers 已按参与方过滤，这里保持原样
      }
      if (!k) return true
      if (t.id.toLowerCase().includes(k)) return true
      if (t.senderName.toLowerCase().includes(k)) return true
      if (t.receiverName.toLowerCase().includes(k)) return true
      return (t.files ?? []).some((f) => f.name.toLowerCase().includes(k))
    })
  }, [all, filter, keyword, onlyMine])

  const activeList = filtered.filter((t) => !isTerminal(t.status))

  return (
    <>
      <PageHeader
        title="传输任务"
        description="所有与你相关的传输任务。任务详情可展开查看分块、校验与续传能力。"
        extra={
          <>
            <Tag tone={activeList.length > 0 ? 'primary' : 'neutral'}>{activeList.length} 个进行中</Tag>
            <Tooltip title="重新获取任务状态">
              <Button icon={<ReloadOutlined spin={loading} />} onClick={() => void load()}>
                刷新
              </Button>
            </Tooltip>
          </>
        }
      />

      <Card style={{ marginBottom: 'var(--ns-sp-4)' }} className="ns-rise ns-rise-1">
        <div className="ns-inline" style={{ justifyContent: 'space-between', flexWrap: 'wrap', gap: 'var(--ns-sp-3)' }}>
          <Segmented
            value={filter}
            onChange={(v) => setFilter(v as FilterKey)}
            options={FILTERS.map((f) => ({
              label: (
                <span>
                  {f.label}
                  {counts[f.key] > 0 ? (
                    <span className="ns-num" style={{ marginLeft: 6, opacity: 0.7 }}>
                      {counts[f.key]}
                    </span>
                  ) : null}
                </span>
              ),
              value: f.key,
            }))}
          />
          <div className="ns-inline" style={{ gap: 'var(--ns-sp-2)' }}>
            <Input
              size="small"
              allowClear
              prefix={<SearchOutlined style={{ color: 'var(--ns-text-tertiary)' }} />}
              placeholder="搜索文件名、设备名或任务 ID"
              value={keyword}
              onChange={(e) => setKeyword(e.target.value)}
              style={{ width: 260, maxWidth: '100%' }}
            />
            <Button
              size="small"
              type={onlyMine ? 'primary' : 'default'}
              onClick={() => setOnlyMine((v) => !v)}
            >
              仅我参与
            </Button>
          </div>
        </div>
      </Card>

      <Card className="ns-rise ns-rise-2">
        {error ? (
          <EmptyHint
            title="无法获取任务列表"
            description={error}
            action={
              <Button size="small" onClick={() => void load()}>
                重试
              </Button>
            }
          />
        ) : (
          <TaskList
            tasks={filtered}
            emptyTitle={
              keyword
                ? '没有匹配的任务'
                : filter === 'all'
                  ? '还没有任何传输任务'
                  : '这个分类下暂时没有任务'
            }
            emptyDescription={
              keyword ? '换个关键词试试。' : '在「发送与接收」页选择文件与设备即可发起传输。'
            }
            emptyAction={
              keyword ? (
                <Button size="small" onClick={() => setKeyword('')}>
                  清除搜索
                </Button>
              ) : (
                <Button size="small" type="primary" onClick={() => navigate('/')}>
                  去发送文件
                </Button>
              )
            }
          />
        )}
      </Card>

      <div className="ns-meta" style={{ marginTop: 'var(--ns-sp-3)' }}>
        说明：「任务」与「记录」是同一批数据的不同视图：任务页面向进行中的工作，
        记录页面向已完成的历史，两者在终态下内容一致。
      </div>
    </>
  )
}
