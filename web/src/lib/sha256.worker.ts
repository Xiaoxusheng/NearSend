/// <reference lib="webworker" />

/**
 * SHA-256 计算 Worker。
 *
 * 为什么需要它：
 *   在局域网明文 HTTP 下 crypto.subtle 不可用，只能用纯 JS 实现；
 *   主线程单核计算会成为千兆局域网的瓶颈。放到 Worker 池后可以
 *   并行利用多核，同时完全不阻塞界面渲染。
 */

import { sha256HexFallback } from './sha256'

interface HashRequest {
  id: number
  buf: ArrayBuffer
}

interface HashResponse {
  id: number
  hex?: string
  error?: string
}

self.onmessage = async (event: MessageEvent<HashRequest>) => {
  const { id, buf } = event.data
  const reply: HashResponse = { id }
  try {
    reply.hex = await sha256HexFallback(buf)
  } catch (err) {
    reply.error = err instanceof Error ? err.message : String(err)
  }
  ;(self as unknown as Worker).postMessage(reply)
}
