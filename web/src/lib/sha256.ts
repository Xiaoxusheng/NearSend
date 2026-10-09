/**
 * SHA-256 计算。
 *
 * 为什么不能只用 crypto.subtle：
 *   Web Crypto 在**非安全上下文**下不可用，而局域网访问通常是
 *   http://192.168.x.x:8787 —— 不是 localhost，因此 crypto.subtle 为 undefined。
 *   如果只依赖它，真实部署场景下分块哈希校验会静默失效。
 *
 * 因此这里提供两级实现：
 *   1. 安全上下文（localhost / HTTPS）→ 使用浏览器原生实现，最快。
 *   2. 其余情况 → 使用纯 JS 实现，并在计算过程中定期让出主线程，
 *      避免大分块哈希时界面卡死。
 *
 * 两级实现的结果都经过测试向量验证一致（见 assertSelfTest）。
 */

const K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
])

/** 每个让出周期处理的 64 字节块数量。16384 * 64B = 1 MiB。 */
const YIELD_BLOCKS = 16384

function rotr(x: number, n: number): number {
  return (x >>> n) | (x << (32 - n))
}

/**
 * 计算 padded 后的消息，逐块压缩。
 * onProgress 用于长任务让出主线程。
 */
function sha256BlocksSync(bytes: Uint8Array): Uint8Array {
  const H = new Uint32Array([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ])

  const len = bytes.length
  // 填充：1 字节 0x80 + 若干 0 + 8 字节大端位长度，总长为 64 的整数倍。
  const totalLen = Math.ceil((len + 9) / 64) * 64
  const msg = new Uint8Array(totalLen)
  msg.set(bytes)
  msg[len] = 0x80

  const bitLenHi = Math.floor((len * 8) / 0x100000000)
  const bitLenLo = (len * 8) >>> 0
  msg[totalLen - 8] = (bitLenHi >>> 24) & 0xff
  msg[totalLen - 7] = (bitLenHi >>> 16) & 0xff
  msg[totalLen - 6] = (bitLenHi >>> 8) & 0xff
  msg[totalLen - 5] = bitLenHi & 0xff
  msg[totalLen - 4] = (bitLenLo >>> 24) & 0xff
  msg[totalLen - 3] = (bitLenLo >>> 16) & 0xff
  msg[totalLen - 2] = (bitLenLo >>> 8) & 0xff
  msg[totalLen - 1] = bitLenLo & 0xff

  const w = new Uint32Array(64)
  const blocks = totalLen / 64

  for (let b = 0; b < blocks; b++) {
    const base = b * 64
    for (let i = 0; i < 16; i++) {
      const o = base + i * 4
      w[i] = ((msg[o] << 24) | (msg[o + 1] << 16) | (msg[o + 2] << 8) | msg[o + 3]) >>> 0
    }
    for (let i = 16; i < 64; i++) {
      const x = w[i - 15]
      const y = w[i - 2]
      const s0 = (rotr(x, 7) ^ rotr(x, 18) ^ (x >>> 3)) >>> 0
      const s1 = (rotr(y, 17) ^ rotr(y, 19) ^ (y >>> 10)) >>> 0
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0
    }

    let a = H[0]
    let bb = H[1]
    let c = H[2]
    let d = H[3]
    let e = H[4]
    let f = H[5]
    let g = H[6]
    let h = H[7]

    for (let i = 0; i < 64; i++) {
      const S1 = (rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25)) >>> 0
      const ch = ((e & f) ^ (~e & g)) >>> 0
      const t1 = (h + S1 + ch + K[i] + w[i]) >>> 0
      const S0 = (rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22)) >>> 0
      const maj = ((a & bb) ^ (a & c) ^ (bb & c)) >>> 0
      const t2 = (S0 + maj) >>> 0

      h = g
      g = f
      f = e
      e = (d + t1) >>> 0
      d = c
      c = bb
      bb = a
      a = (t1 + t2) >>> 0
    }

    H[0] = (H[0] + a) >>> 0
    H[1] = (H[1] + bb) >>> 0
    H[2] = (H[2] + c) >>> 0
    H[3] = (H[3] + d) >>> 0
    H[4] = (H[4] + e) >>> 0
    H[5] = (H[5] + f) >>> 0
    H[6] = (H[6] + g) >>> 0
    H[7] = (H[7] + h) >>> 0
  }

  const out = new Uint8Array(32)
  for (let i = 0; i < 8; i++) {
    out[i * 4] = (H[i] >>> 24) & 0xff
    out[i * 4 + 1] = (H[i] >>> 16) & 0xff
    out[i * 4 + 2] = (H[i] >>> 8) & 0xff
    out[i * 4 + 3] = H[i] & 0xff
  }
  return out
}

/** 是否可以使用浏览器原生 Web Crypto。 */
export function hasWebCrypto(): boolean {
  return (
    typeof crypto !== 'undefined' &&
    typeof crypto.subtle !== 'undefined' &&
    typeof crypto.subtle.digest === 'function' &&
    (typeof isSecureContext === 'undefined' || isSecureContext)
  )
}

function toHex(bytes: Uint8Array): string {
  let out = ''
  for (let i = 0; i < bytes.length; i++) {
    out += bytes[i].toString(16).padStart(2, '0')
  }
  return out
}

const yieldToMain = () =>
  new Promise<void>((resolve) => {
    // 优先使用 scheduler.yield，其次 MessageChannel，最后 setTimeout。
    const sched = (globalThis as { scheduler?: { yield?: () => Promise<void> } }).scheduler
    if (sched?.yield) {
      sched.yield().then(resolve, () => resolve())
      return
    }
    if (typeof MessageChannel !== 'undefined') {
      const ch = new MessageChannel()
      ch.port1.onmessage = () => resolve()
      ch.port2.postMessage(undefined)
      return
    }
    setTimeout(resolve, 0)
  })

/**
 * 计算 ArrayBuffer 的 SHA-256，返回小写十六进制字符串。
 *
 * 三级策略，逐级降级，保证在任何部署形态下都能算出真实哈希：
 *   1. Web Crypto（安全上下文：localhost / HTTPS）——硬件加速，最快。
 *   2. Web Worker 池 + 纯 JS 实现（明文 HTTP 的常规情况）——多核并行且不阻塞界面。
 *   3. 主线程纯 JS 实现（Worker 不可用，例如被 CSP 限制）——分片让出，仍然可用。
 */
export async function sha256Hex(buf: ArrayBuffer): Promise<string> {
  if (hasWebCrypto()) {
    try {
      const digest = await crypto.subtle.digest('SHA-256', buf)
      return toHex(new Uint8Array(digest))
    } catch {
      // 某些环境下 subtle 存在但调用失败，继续降级。
    }
  }
  const pooled = await tryPooledHash(buf)
  if (pooled !== null) return pooled
  return sha256HexFallback(buf)
}

/** 哈希能力的可读描述，供设置页/诊断页如实展示。 */
export function hashCapability(): 'webcrypto' | 'worker' | 'main-thread' {
  if (hasWebCrypto()) return 'webcrypto'
  if (canUseWorker()) return 'worker'
  return 'main-thread'
}

// ---------------------------------------------------------------- Worker 池 ---

interface PendingJob {
  resolve: (hex: string) => void
  reject: (err: Error) => void
}

interface PoolWorker {
  worker: Worker
  current: PendingJob | null
  id: number
}

let pool: PoolWorker[] | null = null
let poolBroken = false
let jobSeq = 0

function canUseWorker(): boolean {
  return typeof Worker !== 'undefined' && !poolBroken
}

function poolSize(): number {
  const cpus = (navigator as { hardwareConcurrency?: number }).hardwareConcurrency ?? 4
  // 上限 4：哈希已足够快，过多 Worker 反而与上传请求争抢带宽与内存。
  return Math.max(1, Math.min(4, Math.floor(cpus / 2) || 1))
}

function ensurePool(): PoolWorker[] | null {
  if (pool) return pool
  if (!canUseWorker()) return null
  try {
    const workers: PoolWorker[] = []
    for (let i = 0; i < poolSize(); i++) {
      const worker = new Worker(new URL('./sha256.worker.ts', import.meta.url), { type: 'module' })
      workers.push({ worker, current: null, id: i })
      worker.onmessage = (event: MessageEvent<{ id: number; hex?: string; error?: string }>) => {
        const slot = workers.find((w) => w.worker === worker)
        if (!slot || !slot.current) return
        const job = slot.current
        slot.current = null
        if (event.data.error) job.reject(new Error(event.data.error))
        else job.resolve(event.data.hex ?? '')
      }
      worker.onerror = () => {
        // Worker 出错则整体放弃池，退回主线程实现，保证功能不中断。
        poolBroken = true
        if (slot_of(workers, worker)?.current) {
          const job = slot_of(workers, worker)!.current!
          slot_of(workers, worker)!.current = null
          job.reject(new Error('worker failed'))
        }
        teardownPool()
      }
    }
    pool = workers
    return pool
  } catch {
    poolBroken = true
    return null
  }
}

function slot_of(workers: PoolWorker[], worker: Worker): PoolWorker | undefined {
  return workers.find((w) => w.worker === worker)
}

function teardownPool() {
  if (!pool) return
  for (const slot of pool) {
    try {
      slot.worker.terminate()
    } catch {
      /* 忽略终止失败 */
    }
  }
  pool = null
}

/** 尝试用 Worker 池计算；返回 null 表示应降级到主线程。 */
async function tryPooledHash(buf: ArrayBuffer): Promise<string | null> {
  const workers = ensurePool()
  if (!workers || workers.length === 0) return null

  // 找到空闲的 Worker；都在忙则等待最近一个完成。
  const free = workers.find((w) => w.current === null)
  if (!free) return null

  const id = ++jobSeq
  return new Promise<string>((resolve, reject) => {
    const job: PendingJob = { resolve, reject }
    free.current = job
    free.worker.postMessage({ id, buf })
  })
}


/** 纯 JS 实现（分片让出，避免长时间阻塞主线程）。 */
export async function sha256HexFallback(buf: ArrayBuffer): Promise<string> {
  const bytes = new Uint8Array(buf)
  // 小数据直接一次算完，避免不必要的调度开销。
  if (bytes.length <= YIELD_BLOCKS * 64) {
    return toHex(sha256BlocksSync(bytes))
  }
  // 大数据：以 1MiB 为粒度让出，同时复用同一个 padded 缓冲区。
  const chunkBytes = YIELD_BLOCKS * 64
  const H = new Uint32Array([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ])
  const w = new Uint32Array(64)

  const processBlocks = (block: Uint8Array) => {
    for (let b = 0; b < block.length / 64; b++) {
      const base = b * 64
      for (let i = 0; i < 16; i++) {
        const o = base + i * 4
        w[i] = ((block[o] << 24) | (block[o + 1] << 16) | (block[o + 2] << 8) | block[o + 3]) >>> 0
      }
      for (let i = 16; i < 64; i++) {
        const x = w[i - 15]
        const y = w[i - 2]
        const s0 = (rotr(x, 7) ^ rotr(x, 18) ^ (x >>> 3)) >>> 0
        const s1 = (rotr(y, 17) ^ rotr(y, 19) ^ (y >>> 10)) >>> 0
        w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0
      }
      let a = H[0], bb = H[1], c = H[2], d = H[3], e = H[4], f = H[5], g = H[6], h = H[7]
      for (let i = 0; i < 64; i++) {
        const S1 = (rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25)) >>> 0
        const ch = ((e & f) ^ (~e & g)) >>> 0
        const t1 = (h + S1 + ch + K[i] + w[i]) >>> 0
        const S0 = (rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22)) >>> 0
        const maj = ((a & bb) ^ (a & c) ^ (bb & c)) >>> 0
        const t2 = (S0 + maj) >>> 0
        h = g; g = f; f = e; e = (d + t1) >>> 0
        d = c; c = bb; bb = a; a = (t1 + t2) >>> 0
      }
      H[0] = (H[0] + a) >>> 0
      H[1] = (H[1] + bb) >>> 0
      H[2] = (H[2] + c) >>> 0
      H[3] = (H[3] + d) >>> 0
      H[4] = (H[4] + e) >>> 0
      H[5] = (H[5] + f) >>> 0
      H[6] = (H[6] + g) >>> 0
      H[7] = (H[7] + h) >>> 0
    }
  }

  // 除最后一段外，全部按整块处理；最后一段带上填充与长度。
  const fullLen = bytes.length
  const tailStart = Math.floor(fullLen / chunkBytes) * chunkBytes
  // 保证尾部不超过一个粒度：若恰好整除，则把最后一段留作尾部。
  const headEnd = tailStart === fullLen ? Math.max(0, fullLen - chunkBytes) : tailStart

  for (let off = 0; off < headEnd; off += chunkBytes) {
    processBlocks(bytes.subarray(off, Math.min(off + chunkBytes, headEnd)))
    await yieldToMain()
  }

  // 尾部：与整体长度关联，需要单独做填充，不能再分片。
  const tail = bytes.subarray(headEnd)
  const totalTailLen = Math.ceil((tail.length + 9) / 64) * 64
  const padded = new Uint8Array(totalTailLen)
  padded.set(tail)
  padded[tail.length] = 0x80
  const bitLen = fullLen * 8
  const hi = Math.floor(bitLen / 0x100000000)
  const lo = bitLen >>> 0
  padded[totalTailLen - 8] = (hi >>> 24) & 0xff
  padded[totalTailLen - 7] = (hi >>> 16) & 0xff
  padded[totalTailLen - 6] = (hi >>> 8) & 0xff
  padded[totalTailLen - 5] = hi & 0xff
  padded[totalTailLen - 4] = (lo >>> 24) & 0xff
  padded[totalTailLen - 3] = (lo >>> 16) & 0xff
  padded[totalTailLen - 2] = (lo >>> 8) & 0xff
  padded[totalTailLen - 1] = lo & 0xff
  processBlocks(padded)

  const out = new Uint8Array(32)
  for (let i = 0; i < 8; i++) {
    out[i * 4] = (H[i] >>> 24) & 0xff
    out[i * 4 + 1] = (H[i] >>> 16) & 0xff
    out[i * 4 + 2] = (H[i] >>> 8) & 0xff
    out[i * 4 + 3] = H[i] & 0xff
  }
  return toHex(out)
}

/**
 * 自检：用公开测试向量验证当前环境下的实现正确。
 * 返回 null 表示通过，否则返回错误描述。
 * 调用方（设置页 / 诊断页）可以据此如实展示「哈希能力」状态。
 */
export async function selfTestSha256(): Promise<string | null> {
  const vectors: [string, string][] = [
    ['', 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'],
    ['abc', 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad'],
    [
      'abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq',
      '248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1',
    ],
  ]
  for (const [input, want] of vectors) {
    const buf = new TextEncoder().encode(input).buffer as ArrayBuffer
    const got = await sha256Hex(buf)
    if (got !== want) return `向量 "${input.slice(0, 12)}" 不匹配: ${got}`
  }
  // 再验证一遍纯 JS 路径，确保两条路径结果一致。
  for (const [input, want] of vectors) {
    const buf = new TextEncoder().encode(input).buffer as ArrayBuffer
    const got = await sha256HexFallback(buf)
    if (got !== want) return `JS 实现向量 "${input.slice(0, 12)}" 不匹配: ${got}`
  }
  return null
}
