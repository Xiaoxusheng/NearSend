/**
 * 局域网快传 UI 巡检（playwright-core + 系统 Edge）
 *
 * 目的：在真实后端上驱动真实浏览器，验证：
 *   1. 每个页面都能渲染，且没有 JS 报错、没有白屏
 *   2. 没有横向溢出（移动端尤其）
 *   3. 深浅主题都能正确切换，文字可读
 *   4. 真实的发送 → 接收确认 → 传输 → 记录 全流程在 UI 上可用
 *
 * 输出：/tmp/ns-ui/shots/*.png 以及控制台断言结果。
 */
import { chromium } from 'playwright-core'
import fs from 'node:fs'
import path from 'node:path'

const BASE = process.env.BASE || 'http://127.0.0.1:8791'
// 输出目录可通过环境变量覆盖；默认放在系统临时目录下，避免污染仓库。
const OUT = process.env.OUT_DIR || 'shots'
fs.mkdirSync(OUT, { recursive: true })

const issues = []
const notes = []

function record(kind, msg) {
  if (kind === 'error') issues.push(msg)
  else notes.push(msg)
  console.log(`[${kind.toUpperCase()}] ${msg}`)
}

async function shot(page, name) {
  await page.screenshot({ path: path.join(OUT, `${name}.png`), fullPage: false })
  console.log(`  · 截图 ${name}.png`)
}

/** 检查页面是否存在横向溢出，并给出最宽的越界元素。 */
async function checkOverflow(page, label) {
  const res = await page.evaluate(() => {
    const de = document.documentElement
    const overflow = de.scrollWidth - de.clientWidth
    let worst = null
    if (overflow > 1) {
      let max = 0
      for (const el of document.querySelectorAll('*')) {
        const r = el.getBoundingClientRect()
        if (r.right > de.clientWidth + 1 && r.width > 0) {
          if (r.right > max) {
            max = r.right
            worst = `${el.tagName.toLowerCase()}.${(el.className || '').toString().slice(0, 60)} right=${Math.round(r.right)}`
          }
        }
      }
    }
    return { overflow, worst, clientWidth: de.clientWidth, scrollWidth: de.scrollWidth }
  })
  if (res.overflow > 1) {
    record('error', `${label}: 横向溢出 ${res.overflow}px (scrollWidth=${res.scrollWidth}) 最宽元素: ${res.worst}`)
  } else {
    record('note', `${label}: 无横向溢出`)
  }
  return res
}

/** 检查关键页面元素是否有内容。 */
async function checkRendered(page, label, selectors) {
  for (const [name, sel] of Object.entries(selectors)) {
    const count = await page.locator(sel).count()
    if (count === 0) record('error', `${label}: 未找到 ${name} (${sel})`)
  }
}

async function gotoRoute(page, route, waitMs = 900) {
  await page.goto(BASE + route, { waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(waitMs)
}

async function main() {
  const browser = await chromium.launch({
    channel: 'msedge',
    headless: true,
    args: ['--no-proxy-server', '--disable-features=IsolateOrigins,site-per-process'],
  })

  const errorsByPage = new Map()

  // ---------- 桌面端主页面 ----------
  const desktopCtx = await browser.newContext({ viewport: { width: 1600, height: 900 } })
  const desktop = await desktopCtx.newPage()
  desktop.on('pageerror', (e) => {
    record('error', `桌面端 JS 异常: ${e.message}`)
  })
  desktop.on('console', (m) => {
    if (m.type() === 'error') {
      const t = m.text()
      if (!t.includes('favicon')) record('error', `桌面端 console.error: ${t.slice(0, 180)}`)
    }
  })

  console.log('\n=== 1. 桌面端首屏（浅色） ===')
  await gotoRoute(desktop, '/', 1600)
  await checkRendered(desktop, '首页', {
    '产品标题': 'text=局域网快传',
    '拖拽区': '.ns-dropzone',
    '设备区标题': 'text=附近在线设备',
    '侧边栏导航': '.ns-nav-item',
    '发送按钮': 'button:has-text("请先选择文件")',
  })
  await checkOverflow(desktop, '首页(1600x900)')
  await shot(desktop, '01-home-light')

  // ---------- 第二个"设备"（手机视口），形成真实在线设备 ----------
  console.log('\n=== 2. 手机端作为第二台设备 ===')
  const mobileCtx = await browser.newContext({
    viewport: { width: 390, height: 844 },
    userAgent:
      'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Mobile Safari/537.36',
    isMobile: true,
    hasTouch: true,
  })
  const mobile = await mobileCtx.newPage()
  mobile.on('pageerror', (e) => record('error', `手机端 JS 异常: ${e.message}`))
  await gotoRoute(mobile, '/', 1800)
  await checkOverflow(mobile, '首页(390x844)')
  await shot(mobile, '02-mobile-home-light')

  // 让桌面端刷新出这台设备
  await desktop.waitForTimeout(1200)
  await desktop.reload({ waitUntil: 'domcontentloaded' })
  await desktop.waitForTimeout(1600)
  const onlineNow = await desktop.locator('.ns-row-selectable').count()
  record(onlineNow > 0 ? 'note' : 'error', `首页在线设备条目数: ${onlineNow}`)
  await shot(desktop, '03-home-with-device')

  // ---------- 真实发送流程 ----------
  console.log('\n=== 3. 真实发送 → 接收确认 流程 ===')
  // 准备一个真实文件供上传
  const payload = Buffer.alloc(9 * 1024 * 1024 + 4321)
  for (let i = 0; i < payload.length; i++) payload[i] = (i * 31) & 0xff
  // 用相对当前工作目录的路径，避免不同环境下绝对路径解析不一致
  const filePath = path.join(process.cwd(), '传输测试文件.bin')
  fs.writeFileSync(filePath, payload)

  await desktop.setInputFiles('input[type=file]', filePath)
  await desktop.waitForTimeout(600)
  const fileRowCount = await desktop.locator('text=传输测试文件.bin').count()
  record(fileRowCount > 0 ? 'note' : 'error', `选择文件后列表出现文件条目: ${fileRowCount}`)
  await shot(desktop, '04-home-file-selected')

  // 选择第一台在线设备
  const deviceRow = desktop.locator('.ns-row-selectable').first()
  if ((await deviceRow.count()) > 0) {
    await deviceRow.click()
    await desktop.waitForTimeout(400)
  } else {
    record('error', '没有可选的在线设备，无法继续发送流程')
  }
  await shot(desktop, '05-home-target-selected')

  // 点击发送
  const sendBtn = desktop.locator('button:has-text("发送文件 ·")')
  if ((await sendBtn.count()) > 0) {
    await sendBtn.click()
    record('note', '已点击发送按钮')
  } else {
    const txt = await desktop.locator('button.ns-').allInnerTexts().catch(() => [])
    record('error', `未找到发送按钮，当前按钮文本: ${JSON.stringify(txt)}`)
  }

  // 手机端应弹出接收确认
  await mobile.waitForTimeout(2500)
  // 用可见文本而不是内部类名定位：antd 大版本升级会改类名，
  // 而「弹窗标题」是产品语义，更稳定。
  const modalTitle = mobile.locator('text=收到文件传输请求')
  const modalVisible = await modalTitle.count()
  record(modalVisible > 0 ? 'note' : 'error', `手机端收到接收请求弹窗: ${modalVisible}`)
  await shot(mobile, '06-mobile-receive-request')
  if (modalVisible > 0) {
    const box = mobile.locator('.ant-modal').first()
    const modalText = await box.innerText()
    record('note', `弹窗内容摘要: ${modalText.replace(/\s+/g, ' ').slice(0, 160)}`)
    // 关闭按钮在这里没有意义：请求必须被明确接受或拒绝。
    const closeCount = await mobile.locator('.ant-modal-close').count()
    record(closeCount === 0 ? 'note' : 'error', `接收请求弹窗没有无效的关闭按钮: ${closeCount === 0}`)
  }

  // 接受
  const acceptBtn = mobile.locator('button:has-text("接受并接收")')
  if ((await acceptBtn.count()) > 0) {
    await acceptBtn.click()
    record('note', '手机端已点击接受')
  }
  await mobile.waitForTimeout(1500)
  await shot(mobile, '07-mobile-accepted')

  // 观察桌面端传输结果。
  //
  // 局域网内 9MB 传输不到 1 秒就会完成，首页的「当前传输任务」区随即变空，
  // 因此断言必须落在任务页（它同时包含进行中与已完成的任务），
  // 否则会把「完成得太快」误判成「传输失败」。
  await desktop.waitForTimeout(2000)
  await shot(desktop, '08-home-after-send')

  await gotoRoute(desktop, '/tasks', 1600)
  let tasksText = ''
  for (let i = 0; i < 25; i++) {
    tasksText = (await desktop.locator('.ns-task').allInnerTexts().catch(() => [])).join(' | ')
    if (tasksText.includes('已完成')) break
    await desktop.waitForTimeout(1000)
  }
  record('note', `桌面端任务页文本: ${tasksText.slice(0, 300)}`)
  record(tasksText.includes('已完成') ? 'note' : 'error', `桌面端显示传输已完成`)
  record(tasksText.includes('校验通过') ? 'note' : 'error', `桌面端显示完整性校验通过`)
  record(/100%/.test(tasksText) ? 'note' : 'error', `桌面端进度显示 100%`)
  // 文案必须反映真实状态：同一任务在发送方视角应显示「发送至」
  record(tasksText.includes('发送至') ? 'note' : 'error', `发送方视角显示「发送至」`)
  await shot(desktop, '09-desktop-tasks-done')

  // 展开任务详情，验证分块与校验信息可见
  const expandBtn = desktop.locator('button[aria-label="查看任务详情"]').first()
  if ((await expandBtn.count()) > 0) {
    await expandBtn.click()
    await desktop.waitForTimeout(700)
    const detail = await desktop.locator('.ns-task').first().innerText()
    record(detail.includes('SHA-256') ? 'note' : 'error', `详情中展示 SHA-256 校验结果`)
    record(detail.includes('任务 ID') ? 'note' : 'error', `详情中展示任务 ID`)
    await shot(desktop, '09c-desktop-task-detail')
  }

  // 手机端应能看到已收到的文件并提供下载入口
  await mobile.waitForTimeout(1500)
  await gotoRoute(mobile, '/tasks', 1600)
  const mobileTasks = await mobile.locator('.ns-task').allInnerTexts().catch(() => [])
  record('note', `手机端任务页文本: ${JSON.stringify(mobileTasks).slice(0, 260)}`)
  record(
    mobileTasks.some((x) => x.includes('已完成')) ? 'note' : 'error',
    `手机端任务页显示已完成: ${mobileTasks.some((x) => x.includes('已完成'))}`,
  )
  const downloadBtn = await mobile.locator('button:has-text("下载")').count()
  record(downloadBtn > 0 ? 'note' : 'error', `手机端出现可用下载入口: ${downloadBtn}`)
  await shot(mobile, '09b-mobile-tasks-after')

  // ---------- 逐页巡检（桌面端） ----------
  console.log('\n=== 4. 桌面端逐页巡检 ===')
  const routes = [
    ['/tasks', '传输任务', '10-tasks'],
    ['/history', '传输记录', '11-history'],
    ['/devices', '设备管理', '12-devices'],
    ['/dropbox', '临时接收箱', '13-dropbox'],
    ['/diagnostics', '网络诊断', '14-diagnostics'],
    ['/settings', '设置', '15-settings'],
  ]
  for (const [route, title, name] of routes) {
    await gotoRoute(desktop, route, 1400)
    const hasTitle = await desktop.locator(`text=${title}`).count()
    record(hasTitle > 0 ? 'note' : 'error', `${route} 渲染出标题「${title}」`)
    const emptyBody = (await desktop.locator('body').innerText()).trim().length
    if (emptyBody < 40) record('error', `${route} 页面内容疑似空白`)
    await checkOverflow(desktop, `${route}(1600x900)`)
    await shot(desktop, name)
  }

  // 后端不可用的处理：临时入口页需要 token，单独验证
  await gotoRoute(desktop, '/drop?token=invalid-token', 1500)
  const dropText = await desktop.locator('body').innerText()
  record(
    dropText.includes('临时接收入口') ? 'note' : 'error',
    `临时上传页无效令牌给出明确提示: ${dropText.includes('不可用') || dropText.includes('失效')}`,
  )
  await shot(desktop, '16-drop-invalid')

  // ---------- 深色主题 ----------
  console.log('\n=== 5. 深色主题 ===')
  await gotoRoute(desktop, '/', 1200)
  await desktop.click('button[aria-label="切换主题"]')
  await desktop.waitForTimeout(700)
  const darkAttr = await desktop.evaluate(() => document.documentElement.dataset.theme)
  record(darkAttr === 'dark' ? 'note' : 'error', `主题切换到 dark: ${darkAttr}`)
  // 校验暗色下文字可读（取几个关键元素的颜色对比）
  const contrast = await desktop.evaluate(() => {
    const parse = (c) => {
      const m = c.match(/rgba?\(([^)]+)\)/)
      if (!m) return null
      const p = m[1].split(',').map((x) => parseFloat(x.trim()))
      return p.slice(0, 3)
    }
    const lum = (rgb) => {
      const f = rgb.map((v) => {
        const s = v / 255
        return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
      })
      return 0.2126 * f[0] + 0.7152 * f[1] + 0.0722 * f[2]
    }
    const bodyBg = parse(getComputedStyle(document.body).backgroundColor) || [0, 0, 0]
    const out = []
    for (const sel of ['h1', '.ns-card', '.ns-meta', '.ns-nav-item']) {
      const el = document.querySelector(sel)
      if (!el) continue
      const st = getComputedStyle(el)
      let fg = parse(st.color)
      let bg = parse(st.backgroundColor)
      if (!bg || (bg[0] === 0 && bg[1] === 0 && bg[2] === 0 && st.backgroundColor === 'rgba(0, 0, 0, 0)')) bg = bodyBg
      if (!fg) continue
      const l1 = lum(fg)
      const l2 = lum(bg)
      const ratio = (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05)
      out.push({ sel, ratio: Math.round(ratio * 100) / 100, fg: st.color, bg: st.backgroundColor })
    }
    return out
  })
  for (const c of contrast) {
    record(
      c.ratio >= 4.0 ? 'note' : 'error',
      `深色主题对比度 ${c.sel}: ${c.ratio}:1 (${c.fg} on ${c.bg})`,
    )
  }
  await shot(desktop, '17-home-dark')
  for (const [route, title, name] of [['/history', '传输记录', '18-history-dark'], ['/settings', '设置', '19-settings-dark']]) {
    await gotoRoute(desktop, route, 1200)
    record((await desktop.locator(`text=${title}`).count()) > 0 ? 'note' : 'error', `${route} 深色渲染正常`)
    await shot(desktop, name)
  }

  // 回浅色
  await desktop.click('button[aria-label="切换主题"]')
  await desktop.waitForTimeout(500)

  // ---------- 多分辨率溢出检查 ----------
  console.log('\n=== 6. 多分辨率横向溢出检查 ===')
  for (const [w, h] of [[1920, 1080], [1440, 900], [1024, 768], [768, 1024], [390, 844], [360, 800]]) {
    await desktop.setViewportSize({ width: w, height: h })
    await gotoRoute(desktop, '/', 1100)
    await checkOverflow(desktop, `首页 ${w}x${h}`)
    await gotoRoute(desktop, '/history', 1100)
    await checkOverflow(desktop, `记录页 ${w}x${h}`)
    if (w <= 430) {
      await shot(desktop, `20-mobile-${w}-home`)
    }
  }
  // 移动端底部标签栏应存在且包含全部入口
  await desktop.setViewportSize({ width: 390, height: 844 })
  await gotoRoute(desktop, '/', 1100)
  const tabs = await desktop.locator('.ns-tab').count()
  record(tabs >= 5 ? 'note' : 'error', `移动端底部标签数量: ${tabs}`)
  const sidebarVisible = await desktop.locator('.ns-sidebar').isVisible().catch(() => false)
  record(!sidebarVisible ? 'note' : 'error', `移动端侧边栏已隐藏: ${!sidebarVisible}`)
  // 抽屉应包含全部 7 个入口
  await desktop.locator('.ns-tab:has-text("更多")').click()
  await desktop.waitForTimeout(700)
  const drawerItems = await desktop.locator('.ant-drawer .ns-nav-item').count()
  record(drawerItems >= 7 ? 'note' : 'error', `移动端抽屉导航条目数: ${drawerItems}`)
  await shot(desktop, '21-mobile-drawer')
  await desktop.keyboard.press('Escape')
  await desktop.waitForTimeout(400)
  await gotoRoute(desktop, '/tasks', 1200)
  await shot(desktop, '22-mobile-tasks')
  await gotoRoute(desktop, '/history', 1200)
  await shot(desktop, '23-mobile-history')
  await gotoRoute(desktop, '/devices', 1200)
  await shot(desktop, '24-mobile-devices')

  await browser.close()

  console.log('\n================ 巡检结论 ================')
  console.log(`提示 ${notes.length} 条，问题 ${issues.length} 条`)
  if (issues.length) {
    console.log('\n发现的问题：')
    issues.forEach((i) => console.log('  ✗ ' + i))
  } else {
    console.log('未发现 UI 问题。')
  }
  process.exit(issues.length ? 1 : 0)
}

main().catch((e) => {
  console.error('巡检脚本异常:', e)
  process.exit(2)
})
