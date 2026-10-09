/**
 * 设置页。
 *
 * 左侧分类、右侧内容。每一项显示名称、解释、当前值与保存动作。
 * 所有写入都在服务端做完整校验：任一字段非法则整体不生效，并把原因返回给用户。
 * 危险操作（清理临时文件、清除历史）需要二次确认，并在文案中说明「不会删除什么」。
 */

import {
  DeleteOutlined,
  LockOutlined,
  ReloadOutlined,
  SafetyCertificateOutlined,
  SaveOutlined,
  SettingOutlined,
  SwapOutlined,
  UnlockOutlined,
} from '@ant-design/icons'
import { Alert, Button, Divider, Input, InputNumber, Modal, Popconfirm, Select, Switch } from 'antd'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'

import { Card, EmptyHint, KeyValues, PageHeader, Section, Tag } from '../components/common'
import { ApiError, api } from '../lib/api'
import { formatBytes } from '../lib/format'
import type { ConflictPolicy, Settings, StorageStats, StorageUsage } from '../lib/types'
import { useConfigStore } from '../store/config'
import { isPrivileged } from '../store/identity'
import { explainErrorCode, toast } from '../store/notify'
import { useSession } from '../store/session'

type CategoryKey = 'transfer' | 'device' | 'security' | 'storage' | 'service'

const CATEGORIES: { key: CategoryKey; label: string; icon: React.ReactNode }[] = [
  { key: 'transfer', label: '传输设置', icon: <SwapOutlined /> },
  { key: 'device', label: '设备设置', icon: <SettingOutlined /> },
  { key: 'security', label: '安全设置', icon: <SafetyCertificateOutlined /> },
  { key: 'storage', label: '存储设置', icon: <DeleteOutlined /> },
  { key: 'service', label: '服务设置', icon: <ReloadOutlined /> },
]

const CHUNK_PRESETS = [
  { value: 256 * 1024, label: '256 KB' },
  { value: 512 * 1024, label: '512 KB' },
  { value: 1024 * 1024, label: '1 MB' },
  { value: 4 * 1024 * 1024, label: '4 MB' },
  { value: 8 * 1024 * 1024, label: '8 MB（默认）' },
  { value: 16 * 1024 * 1024, label: '16 MB' },
  { value: 32 * 1024 * 1024, label: '32 MB' },
]

/** 单个设置项的通用容器：名称 + 解释 + 控件。 */
function Item({
  name,
  help,
  children,
  onSave,
  dirty,
  saving,
}: {
  name: string
  help?: React.ReactNode
  children: React.ReactNode
  onSave?: () => void
  dirty?: boolean
  saving?: boolean
}) {
  return (
    <div className="ns-setting-item">
      <div className="ns-setting-label">
        <div className="ns-setting-name">{name}</div>
        {help ? <div className="ns-setting-help">{help}</div> : null}
      </div>
      <div className="ns-setting-control">
        <div className="ns-inline" style={{ flexWrap: 'wrap' }}>
          {children}
          {onSave ? (
            <Button
              size="small"
              type={dirty ? 'primary' : 'default'}
              icon={<SaveOutlined />}
              loading={saving}
              disabled={!dirty}
              onClick={onSave}
            >
              保存
            </Button>
          ) : null}
        </div>
      </div>
    </div>
  )
}

export function SettingsPage() {
  const settings = useConfigStore((s) => s.settings)
  const canEdit = useConfigStore((s) => s.canEdit)
  const loadSettings = useConfigStore((s) => s.loadSettings)
  const patchSettings = useConfigStore((s) => s.patchSettings)
  const saving = useConfigStore((s) => s.saving)

  const deviceName = useSession((s) => s.deviceName)
  const rename = useSession((s) => s.rename)
  const access = useSession((s) => s.access)
  const deviceId = useSession((s) => s.deviceId)

  const navigate = useNavigate()
  const [category, setCategory] = useState<CategoryKey>('transfer')
  const [draft, setDraft] = useState<Partial<Settings>>({})
  const [storage, setStorage] = useState<{ usage: StorageUsage; stats: StorageStats } | null>(null)
  const [localName, setLocalName] = useState(deviceName)
  const [claimOpen, setClaimOpen] = useState(false)
  const [claimToken, setClaimToken] = useState('')
  const [claiming, setClaiming] = useState(false)
  const [cleaning, setCleaning] = useState(false)

  const admin = canEdit || isPrivileged()

  useEffect(() => {
    void loadSettings()
  }, [loadSettings])

  useEffect(() => {
    setLocalName(deviceName)
  }, [deviceName])

  const refreshStorage = useCallback(async () => {
    try {
      setStorage(await api.storage())
    } catch {
      /* 存储信息非关键路径，失败时保持旧值 */
    }
  }, [])

  useEffect(() => {
    if (admin) void refreshStorage()
  }, [admin, refreshStorage])

  const merged: Partial<Settings> | null = useMemo(
    () => (settings ? { ...settings, ...draft } : null),
    [settings, draft],
  )

  const set = <K extends keyof Settings>(key: K, value: Settings[K]) => {
    setDraft((prev) => ({ ...prev, [key]: value }))
  }

  const dirty = (key: keyof Settings): boolean =>
    Boolean(settings && draft[key] !== undefined && draft[key] !== settings[key])

  const save = async (keys: (keyof Settings)[]) => {
    const patch: Record<string, unknown> = {}
    for (const key of keys) {
      if (draft[key] !== undefined) patch[key] = draft[key]
    }
    if (Object.keys(patch).length === 0) return
    const ok = await patchSettings(patch)
    if (ok) {
      // 只有服务端确认保存成功后才清空草稿，避免「看起来保存了其实没有」。
      setDraft((prev) => {
        const next = { ...prev }
        for (const key of keys) delete next[key]
        return next
      })
      void refreshStorage()
    }
  }

  const doClaim = async () => {
    setClaiming(true)
    try {
      const ok = await useSession.getState().claimAdmin(claimToken.trim())
      if (ok) {
        setClaimOpen(false)
        setClaimToken('')
        await loadSettings()
      }
    } finally {
      setClaiming(false)
    }
  }

  const doCleanup = async () => {
    setCleaning(true)
    try {
      const res = await api.cleanupStorage()
      toast.success(`${res.message}（回收 ${res.removedDirs} 个目录，释放 ${formatBytes(res.freedBytes)}）`)
      void refreshStorage()
    } catch (err) {
      const message = err instanceof ApiError ? explainErrorCode(err.code, err.message) : '清理失败'
      toast.error(message)
    } finally {
      setCleaning(false)
    }
  }

  if (!merged) {
    return (
      <>
        <PageHeader title="设置" />
        <Card>
          <EmptyHint
            title="正在加载设置…"
            description="如果长时间没有响应，请检查与服务的连接。"
            action={
              <Button size="small" onClick={() => void loadSettings()}>
                重新加载
              </Button>
            }
          />
        </Card>
      </>
    )
  }

  const s = merged as Settings

  return (
    <>
      <PageHeader
        title="设置"
        description="配置会立即作用到新的传输任务。监听地址与端口需要重启服务后生效。"
        extra={
          admin ? (
            <Tag tone="success">
              <UnlockOutlined /> 可编辑
            </Tag>
          ) : (
            <Tag tone="warning">
              <LockOutlined /> 只读
            </Tag>
          )
        }
      />

      {!admin ? (
        <Alert
          style={{ marginBottom: 'var(--ns-sp-4)' }}
          type="info"
          showIcon
          message="当前设备只能查看设置"
          description={
            <div className="ns-stack-sm">
              <span>
                出于安全考虑，只有服务所在机器上打开的页面、或已受信任的设备才能修改设置。
                你可以输入管理令牌来取得权限；令牌在服务首次启动时打印在控制台，并保存在数据目录的
                admin-token.txt 中。
              </span>
              <div>
                <Button size="small" type="primary" onClick={() => setClaimOpen(true)}>
                  输入管理令牌
                </Button>
              </div>
            </div>
          }
        />
      ) : null}

      <div className="ns-settings-cols">
        {/* 左侧分类导航 */}
        <div className="ns-settings-nav ns-rise ns-rise-1">
          {CATEGORIES.map((c) => (
            <button
              key={c.key}
              type="button"
              className={`ns-nav-item ${category === c.key ? 'is-active' : ''}`}
              onClick={() => setCategory(c.key)}
            >
              {c.icon}
              <span>{c.label}</span>
            </button>
          ))}
        </div>

        <div className="ns-stack ns-rise ns-rise-2">
          {/* ---------------------------------------------------- 传输设置 --- */}
          {category === 'transfer' ? (
            <Card>
              <Section title="传输设置" />
              <Item
                name="默认接收目录"
                help="所有接收到的文件都会保存在这里，按「日期 / 发送方」自动分目录。必须是绝对路径且可写。"
              >
                <Input
                  style={{ width: 360, maxWidth: '100%' }}
                  value={s.receiveDir}
                  disabled={!admin}
                  onChange={(e) => set('receiveDir', e.target.value)}
                  placeholder={admin ? '例如 D:\\NearSend\\received' : '需要管理权限才能查看'}
                />
              </Item>
              <Item
                name="最大并发传输数"
                help="同时处于「传输中」状态的任务上限，超出的任务会自动排队。过大会增加磁盘与网络压力。"
                dirty={dirty('maxConcurrentTransfers')}
                saving={saving}
                onSave={() => void save(['maxConcurrentTransfers'])}
              >
                <InputNumber
                  min={1}
                  max={32}
                  value={s.maxConcurrentTransfers}
                  disabled={!admin}
                  onChange={(v) => set('maxConcurrentTransfers', Number(v ?? 1))}
                />
              </Item>
              <Item
                name="默认分块大小"
                help="分块越大，单次请求开销越小、但内存占用越高；断点续传的粒度也由它决定。"
                dirty={dirty('chunkSize')}
                saving={saving}
                onSave={() => void save(['chunkSize'])}
              >
                <Select
                  style={{ width: 190 }}
                  value={s.chunkSize}
                  disabled={!admin}
                  onChange={(v) => set('chunkSize', v)}
                  options={CHUNK_PRESETS}
                />
              </Item>
              <Item
                name="单任务大小限制"
                help="一次传输（所有文件合计）的上限。发起时会校验，超限直接拒绝而不是传到一半才失败。"
                dirty={dirty('maxTaskSize')}
                saving={saving}
                onSave={() => void save(['maxTaskSize'])}
              >
                <InputNumber
                  min={1024 * 1024}
                  step={1024 * 1024 * 1024}
                  style={{ width: 190 }}
                  value={s.maxTaskSize}
                  disabled={!admin}
                  onChange={(v) => set('maxTaskSize', Number(v ?? 0))}
                  formatter={(v) => formatBytes(Number(v ?? 0))}
                  parser={(v) => Number(String(v ?? '').replace(/[^\d]/g, ''))}
                />
              </Item>
              <Item
                name="单文件大小限制"
                help="单个文件的上限。"
                dirty={dirty('maxFileSize')}
                saving={saving}
                onSave={() => void save(['maxFileSize'])}
              >
                <InputNumber
                  min={1024 * 1024}
                  step={1024 * 1024 * 1024}
                  style={{ width: 190 }}
                  value={s.maxFileSize}
                  disabled={!admin}
                  onChange={(v) => set('maxFileSize', Number(v ?? 0))}
                  formatter={(v) => formatBytes(Number(v ?? 0))}
                  parser={(v) => Number(String(v ?? '').replace(/[^\d]/g, ''))}
                />
              </Item>
              <Item
                name="失败自动重试次数"
                help="分块上传遇到网络抖动等可重试错误时的自动重试次数（0 表示不自动重试）。"
                dirty={dirty('autoRetryCount')}
                saving={saving}
                onSave={() => void save(['autoRetryCount'])}
              >
                <InputNumber
                  min={0}
                  max={10}
                  value={s.autoRetryCount}
                  disabled={!admin}
                  onChange={(v) => set('autoRetryCount', Number(v ?? 0))}
                />
              </Item>
              <Item
                name="带宽限制"
                help="服务端整体传输限速。0 表示不限制（默认）。修改后立即生效。"
                dirty={dirty('bandwidthLimit')}
                saving={saving}
                onSave={() => void save(['bandwidthLimit'])}
              >
                <InputNumber
                  min={0}
                  step={1024 * 1024}
                  style={{ width: 190 }}
                  value={s.bandwidthLimit}
                  disabled={!admin}
                  onChange={(v) => set('bandwidthLimit', Number(v ?? 0))}
                  formatter={(v) => (Number(v ?? 0) === 0 ? '不限制' : `${formatBytes(Number(v ?? 0))}/s`)}
                  parser={(v) => Number(String(v ?? '').replace(/[^\d]/g, ''))}
                />
              </Item>
              <Item
                name="默认同名文件策略"
                help="新任务的默认重名处理方式。接收方在确认时仍可临时更改。"
                dirty={dirty('defaultConflictPolicy')}
                saving={saving}
                onSave={() => void save(['defaultConflictPolicy'])}
              >
                <Select
                  style={{ width: 190 }}
                  value={s.defaultConflictPolicy}
                  disabled={!admin}
                  onChange={(v) => set('defaultConflictPolicy', v as ConflictPolicy)}
                  options={[
                    { value: 'rename', label: '自动重命名（推荐）' },
                    { value: 'overwrite', label: '覆盖已有文件' },
                    { value: 'skip', label: '跳过同名文件' },
                  ]}
                />
              </Item>
            </Card>
          ) : null}

          {/* ---------------------------------------------------- 设备设置 --- */}
          {category === 'device' ? (
            <>
              <Card>
                <Section title="当前设备" />
                <Item
                  name="设备名称"
                  help="展示给局域网内其它设备的名称，用于区分不同客户端。修改后立即生效。"
                >
                  <Input
                    style={{ width: 260 }}
                    maxLength={48}
                    value={localName}
                    onChange={(e) => setLocalName(e.target.value)}
                  />
                  <Button
                    size="small"
                    type={localName !== deviceName ? 'primary' : 'default'}
                    icon={<SaveOutlined />}
                    disabled={!localName.trim() || localName === deviceName}
                    onClick={() => void rename(localName.trim())}
                  >
                    保存
                  </Button>
                </Item>
                <Item name="当前连接地址" help="其它设备可通过这些地址访问本服务。">
                  <div className="ns-stack-sm" style={{ gap: 4 }}>
                    {(access?.addresses ?? []).map((a) => (
                      <div key={a.url} className="ns-inline">
                        <span className="ns-mono">{a.url}</span>
                        <span className="ns-meta">{a.iface} · {a.family}</span>
                        {a.recommended ? <Tag tone="primary">推荐</Tag> : null}
                        {a.loopback ? <Tag tone="neutral">仅本机</Tag> : null}
                      </div>
                    ))}
                    {access && access.addresses.length === 0 ? (
                      <span className="ns-meta">未检测到可用的局域网地址。</span>
                    ) : null}
                  </div>
                </Item>
                <Item name="本设备标识" help="会话标识由服务端随机生成，同名设备也拥有不同标识，不能凭名称判定身份。">
                  <span className="ns-mono">{deviceId || '—'}</span>
                </Item>
              </Card>

              <Card>
                <Section title="信任与管理" />
                <div className="ns-meta" style={{ lineHeight: 1.9 }}>
                  已信任设备的添加、撤销与断开连接都在「设备管理」页完成。
                  撤销信任会立即断开该设备的实时连接。
                </div>
                <div style={{ marginTop: 'var(--ns-sp-3)' }}>
                  <Button size="small" onClick={() => navigate('/devices')}>
                    前往设备管理
                  </Button>
                </div>
              </Card>
            </>
          ) : null}

          {/* ---------------------------------------------------- 安全设置 --- */}
          {category === 'security' ? (
            <Card>
              <Section title="安全设置" />
              <Item
                name="允许可信设备简化接收确认"
                help="开启后，已受信任的设备发来的文件会自动接受，不再弹窗确认。默认关闭——关闭时永远需要你手动确认。"
                dirty={dirty('allowTrustedAutoAccept')}
                saving={saving}
                onSave={() => void save(['allowTrustedAutoAccept'])}
              >
                <Switch
                  checked={s.allowTrustedAutoAccept}
                  disabled={!admin}
                  onChange={(v) => set('allowTrustedAutoAccept', v)}
                />
                <span className="ns-meta">{s.allowTrustedAutoAccept ? '已开启（仅对可信设备生效）' : '已关闭（每个请求都需确认）'}</span>
              </Item>
              <Item
                name="会话超时时间"
                help="设备断开后，多久视为会话不再活跃。用于设备列表的在线判定与诊断展示。"
                dirty={dirty('sessionTimeoutSec')}
                saving={saving}
                onSave={() => void save(['sessionTimeoutSec'])}
              >
                <InputNumber
                  min={30}
                  max={86400}
                  addonAfter="秒"
                  value={s.sessionTimeoutSec}
                  disabled={!admin}
                  onChange={(v) => set('sessionTimeoutSec', Number(v ?? 120))}
                />
              </Item>
              <Item
                name="信任有效期"
                help="受信任设备的授权最长时间。到期后需要重新授权。"
                dirty={dirty('trustedTokenTtlHours')}
                saving={saving}
                onSave={() => void save(['trustedTokenTtlHours'])}
              >
                <InputNumber
                  min={1}
                  max={8760}
                  addonAfter="小时"
                  value={s.trustedTokenTtlHours}
                  disabled={!admin}
                  onChange={(v) => set('trustedTokenTtlHours', Number(v ?? 720))}
                />
              </Item>
              <Item
                name="临时入口默认有效期"
                help="开启临时接收入口时的默认时限，最大 24 小时。"
                dirty={dirty('dropboxTtlMinutes')}
                saving={saving}
                onSave={() => void save(['dropboxTtlMinutes'])}
              >
                <InputNumber
                  min={1}
                  max={1440}
                  addonAfter="分钟"
                  value={s.dropboxTtlMinutes}
                  disabled={!admin}
                  onChange={(v) => set('dropboxTtlMinutes', Number(v ?? 30))}
                />
              </Item>

              <Divider style={{ margin: 'var(--ns-sp-3) 0' }} />
              <Alert
                type="warning"
                showIcon
                message="关于加密"
                description={
                  <span style={{ fontSize: 13 }}>
                    本服务使用明文 HTTP 提供局域网访问，未启用 TLS，<strong>不具备端到端加密能力</strong>。
                    请仅在可信的局域网中使用。若必须在不可信网络传输敏感文件，请自行在服务前置 TLS 反向代理。
                  </span>
                }
              />
            </Card>
          ) : null}

          {/* ---------------------------------------------------- 存储设置 --- */}
          {category === 'storage' ? (
            <>
              <Card>
                <Section title="存储设置" />
                <Item
                  name="临时文件目录"
                  help="分块在传输期间先写到这里，校验通过后才原子提交到接收目录。与接收目录不能是同一个目录。"
                >
                  <Input
                    style={{ width: 360, maxWidth: '100%' }}
                    value={s.tempDir}
                    disabled={!admin}
                    onChange={(e) => set('tempDir', e.target.value)}
                  />
                </Item>
                <Item
                  name="历史记录保留时间"
                  help="超过该天数的历史记录会自动隐藏（不会删除磁盘文件）。0 表示永久保留。"
                  dirty={dirty('historyRetentionDays')}
                  saving={saving}
                  onSave={() => void save(['historyRetentionDays'])}
                >
                  <InputNumber
                    min={0}
                    max={3650}
                    addonAfter="天"
                    value={s.historyRetentionDays}
                    disabled={!admin}
                    onChange={(v) => set('historyRetentionDays', Number(v ?? 30))}
                  />
                </Item>
                <Item
                  name="临时文件保留时间"
                  help="失败、取消的任务其分块数据会保留这么久，便于重试续传；到期后自动清理。"
                  dirty={dirty('tempRetentionHours')}
                  saving={saving}
                  onSave={() => void save(['tempRetentionHours'])}
                >
                  <InputNumber
                    min={1}
                    max={8760}
                    addonAfter="小时"
                    value={s.tempRetentionHours}
                    disabled={!admin}
                    onChange={(v) => set('tempRetentionHours', Number(v ?? 24))}
                  />
                </Item>
                <Item
                  name="自动清理临时文件"
                  help="开启后服务会定期清理过期的临时数据。清理逻辑绝不会删除接收目录中的文件，也不会触碰正在传输的任务。"
                  dirty={dirty('autoCleanTemp')}
                  saving={saving}
                  onSave={() => void save(['autoCleanTemp'])}
                >
                  <Switch
                    checked={s.autoCleanTemp}
                    disabled={!admin}
                    onChange={(v) => set('autoCleanTemp', v)}
                  />
                </Item>
              </Card>

              <Card>
                <Section title="存储占用" extra={
                  <Button size="small" icon={<ReloadOutlined />} onClick={() => void refreshStorage()}>
                    刷新
                  </Button>
                } />
                {storage ? (
                  <KeyValues
                    items={[
                      { key: '接收目录', value: <span className="ns-mono">{storage.usage.receiveDir}</span> },
                      { key: '接收占用', value: `${storage.usage.receiveFiles} 个文件 / ${formatBytes(storage.usage.receiveBytes)}` },
                      { key: '临时目录', value: <span className="ns-mono">{storage.usage.tempDir}</span> },
                      { key: '临时占用', value: `${storage.usage.tempFiles} 个文件 / ${formatBytes(storage.usage.tempBytes)}` },
                      { key: '磁盘可用', value: `${formatBytes(storage.usage.diskFreeBytes)} / ${formatBytes(storage.usage.diskTotalBytes)}` },
                      { key: '任务总数', value: `${storage.stats.tasks} 个（进行中 ${storage.stats.active}，失败 ${storage.stats.failed}）` },
                      { key: '已落盘总量', value: formatBytes(storage.stats.storedBytes) },
                      { key: '设备', value: `${storage.stats.devices} 台已知 / ${storage.stats.trusted} 台受信任` },
                    ]}
                  />
                ) : (
                  <div className="ns-meta">存储信息加载中或暂不可用。</div>
                )}
              </Card>

              <Card>
                <Section title="危险操作" />
                <div className="ns-setting-item">
                  <div className="ns-setting-label">
                    <div className="ns-setting-name">清理过期临时文件</div>
                    <div className="ns-setting-help">
                      删除临时目录中已过期或已完成任务的残留分块。
                      <strong>不会删除接收目录中的任何文件</strong>，也不会影响正在传输的任务。
                    </div>
                  </div>
                  <div className="ns-setting-control">
                    <Popconfirm
                      title="确认清理临时文件？"
                      description="只会清理临时目录。接收目录中的文件不受影响。"
                      okText="确认清理"
                      okButtonProps={{ danger: true, loading: cleaning }}
                      cancelText="取消"
                      onConfirm={doCleanup}
                    >
                      <Button danger disabled={!admin}>
                        立即清理临时文件
                      </Button>
                    </Popconfirm>
                  </div>
                </div>
              </Card>
            </>
          ) : null}

          {/* ---------------------------------------------------- 服务设置 --- */}
          {category === 'service' ? (
            <Card>
              <Section title="服务设置" />
              <Alert
                type="info"
                showIcon
                style={{ marginBottom: 'var(--ns-sp-3)' }}
                message="修改以下两项后需要重启服务才会生效"
                description="保存后当前进程仍使用旧的监听参数；重启后新配置生效。服务端会在保存时明确告知是否需要重启。"
              />
              <Item
                name="监听地址"
                help="0.0.0.0 表示监听所有网卡（局域网可访问）；127.0.0.1 表示仅本机可访问。"
                dirty={dirty('listenAddr')}
                saving={saving}
                onSave={() => void save(['listenAddr'])}
              >
                <Select
                  style={{ width: 190 }}
                  value={s.listenAddr}
                  disabled={!admin}
                  onChange={(v) => set('listenAddr', v)}
                  options={[
                    { value: '0.0.0.0', label: '0.0.0.0（所有网卡）' },
                    { value: '127.0.0.1', label: '127.0.0.1（仅本机）' },
                  ]}
                />
              </Item>
              <Item
                name="服务端口"
                help="访问地址中的端口。若提示被占用，请换一个端口后重启。"
                dirty={dirty('port')}
                saving={saving}
                onSave={() => void save(['port'])}
              >
                <InputNumber
                  min={1}
                  max={65535}
                  value={s.port}
                  disabled={!admin}
                  onChange={(v) => set('port', Number(v ?? 8787))}
                />
              </Item>
              <Divider style={{ margin: 'var(--ns-sp-3) 0' }} />
              <KeyValues
                items={[
                  { key: '服务版本', value: s.version ?? '—' },
                  { key: '数据目录', value: <span className="ns-mono">{s.dataDir || '—'}</span> },
                  {
                    key: '重启需求',
                    value: s.needsRestart ? <Tag tone="warning">有变更待重启生效</Tag> : <Tag tone="neutral">无待生效变更</Tag>,
                  },
                ]}
              />
              <div className="ns-meta" style={{ marginTop: 'var(--ns-sp-3)' }}>
                重启服务请在运行服务的终端中停止进程后重新启动；本页面无法替你重启进程。
              </div>
            </Card>
          ) : null}
        </div>
      </div>

      <Modal
        title="输入管理令牌"
        open={claimOpen}
        onCancel={() => setClaimOpen(false)}
        onOk={() => void doClaim()}
        okText="取得权限"
        confirmLoading={claiming}
        okButtonProps={{ disabled: !claimToken.trim() }}
      >
        <div className="ns-stack-sm">
          <div className="ns-meta" style={{ lineHeight: 1.8 }}>
            管理令牌在服务首次启动时打印在控制台，并保存在数据目录的
            <span className="ns-mono"> admin-token.txt </span>
            文件中。它只用于让「非主机上的浏览器」取得管理权限。
          </div>
          <Input.Password
            value={claimToken}
            onChange={(e) => setClaimToken(e.target.value)}
            placeholder="粘贴管理令牌"
            onPressEnter={() => void doClaim()}
          />
        </div>
      </Modal>

    </>
  )
}
