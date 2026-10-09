/**
 * 主题（浅色 / 深色）。
 *
 * 规则：
 *  - 首次运行跟随系统；用户手动切换后写入 localStorage 并长期保留。
 *  - 只改写 <html data-theme>，颜色全部由 CSS 变量驱动，因此切换不会闪烁或跳版。
 */

import { create } from 'zustand'

export type ThemeMode = 'light' | 'dark'
export type ThemePreference = ThemeMode | 'system'

const STORAGE_KEY = 'nearsend.theme'

function systemPrefersDark(): boolean {
  if (typeof window === 'undefined' || !window.matchMedia) return false
  return window.matchMedia('(prefers-color-scheme: dark)').matches
}

function readPreference(): ThemePreference {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw === 'light' || raw === 'dark') return raw
  } catch {
    /* localStorage 不可用时退回跟随系统 */
  }
  return 'system'
}

const mediaQuery =
  typeof window !== 'undefined' && window.matchMedia
    ? window.matchMedia('(prefers-color-scheme: dark)')
    : null

/** 在首帧渲染前同步 <html> 属性，避免出现一帧浅色再跳暗色。 */
export function applyInitialTheme() {
  const pref = readPreference()
  const mode: ThemeMode = pref === 'system' ? (systemPrefersDark() ? 'dark' : 'light') : pref
  document.documentElement.dataset.theme = mode
  const meta = document.querySelector('meta[name="theme-color"]')
  if (meta) meta.setAttribute('content', mode === 'dark' ? '#14161a' : '#f4f5f7')
}

interface ThemeState {
  /** 用户偏好，可能是 system。 */
  preference: ThemePreference
  /** 实际生效的主题。 */
  mode: ThemeMode
  setPreference: (pref: ThemePreference) => void
  toggle: () => void
}

function resolve(pref: ThemePreference): ThemeMode {
  if (pref === 'system') return systemPrefersDark() ? 'dark' : 'light'
  return pref
}

function writeDom(mode: ThemeMode) {
  document.documentElement.dataset.theme = mode
  const meta = document.querySelector('meta[name="theme-color"]')
  if (meta) meta.setAttribute('content', mode === 'dark' ? '#14161a' : '#f4f5f7')
}

export const useTheme = create<ThemeState>((set, get) => ({
  preference: readPreference(),
  mode: resolve(readPreference()),
  setPreference: (pref) => {
    try {
      if (pref === 'system') localStorage.removeItem(STORAGE_KEY)
      else localStorage.setItem(STORAGE_KEY, pref)
    } catch {
      /* 无法持久化时仅本次会话生效 */
    }
    const mode = resolve(pref)
    writeDom(mode)
    set({ preference: pref, mode })
  },
  toggle: () => {
    const next: ThemeMode = get().mode === 'dark' ? 'light' : 'dark'
    get().setPreference(next)
  },
}))

// 跟随系统时，系统主题变化立即生效。
mediaQuery?.addEventListener('change', () => {
  const state = useTheme.getState()
  if (state.preference !== 'system') return
  const mode = resolve('system')
  writeDom(mode)
  useTheme.setState({ mode })
})
