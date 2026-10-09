import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { AppRoot } from './App'
import { applyInitialTheme } from './store/theme'

import './styles/tokens.css'
import './styles/base.css'
import './styles/animations.css'
import './styles/layout.css'

// 在 React 挂载前先确定主题，避免首帧闪一下浅色再跳到暗色。
applyInitialTheme()

const root = document.getElementById('root')
if (!root) {
  throw new Error('找不到 #root 挂载点')
}

createRoot(root).render(
  <StrictMode>
    <AppRoot />
  </StrictMode>,
)
