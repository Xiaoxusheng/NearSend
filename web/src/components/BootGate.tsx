/**
 * 启动门禁。
 *
 * 负责「打开页面就能知道服务是否可用」：服务不可用或会话建立失败时，
 * 展示具体原因与可执行的排查建议，而不是留一个空白页面或转圈动画。
 */

import { Button, Result, Spin } from 'antd'
import { Outlet } from 'react-router-dom'

import { useSession } from '../store/session'

export function BootGate() {
  const status = useSession((s) => s.status)
  const reason = useSession((s) => s.blockedReason)
  const retryBoot = useSession((s) => s.retryBoot)

  if (status === 'booting') {
    return (
      <div
        style={{
          minHeight: '100vh',
          display: 'grid',
          placeItems: 'center',
          background: 'var(--ns-bg-app)',
        }}
      >
        <div style={{ textAlign: 'center' }}>
          <Spin size="large" />
          <div style={{ marginTop: 16, color: 'var(--ns-text-secondary)', fontSize: 13 }}>
            正在连接局域网快传服务…
          </div>
        </div>
      </div>
    )
  }

  if (status === 'blocked') {
    return (
      <div
        style={{
          minHeight: '100vh',
          display: 'grid',
          placeItems: 'center',
          background: 'var(--ns-bg-app)',
          padding: 16,
        }}
      >
        <div className="ns-card" style={{ maxWidth: 560, width: '100%' }}>
          <Result
            status="warning"
            title="无法建立连接"
            subTitle={reason}
            extra={[
              <Button key="retry" type="primary" onClick={() => void retryBoot()}>
                重新连接
              </Button>,
              <Button key="reload" onClick={() => window.location.reload()}>
                刷新页面
              </Button>,
            ]}
          >
            <div
              style={{
                fontSize: 13,
                color: 'var(--ns-text-secondary)',
                lineHeight: 1.9,
                textAlign: 'left',
              }}
            >
              <div style={{ fontWeight: 600, color: 'var(--ns-text)', marginBottom: 4 }}>可以这样排查：</div>
              <ul style={{ margin: 0, paddingLeft: 18 }}>
                <li>确认服务程序仍在运行（命令行窗口没有关闭、没有报错）。</li>
                <li>确认访问地址里的 IP 与端口和启动时打印的一致。</li>
                <li>确认当前设备与服务在同一局域网，且没有开启访客网络隔离。</li>
                <li>若刚修改过监听地址或端口，需要重启服务后才会生效。</li>
                <li>若被防火墙拦截，请在系统防火墙中允许本程序的专用网络访问。</li>
              </ul>
            </div>
          </Result>
        </div>
      </div>
    )
  }

  // ready：交还给内部路由（含 AppLayout）。
  return <Outlet />
}
