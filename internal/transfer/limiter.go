package transfer

import (
	"context"
	"io"
	"sync"
	"time"
)

// Limiter 是面向字节的令牌桶限速器。limit <= 0 表示不限速。
//
// 只在实际拷贝路径上调用 Wait，因此不会为「未启用的功能」付出额外开销。
// 实现刻意保持简单：单锁 + 浮点令牌，精度足够用于带宽控制，
// 不引入额外依赖。
type Limiter struct {
	mu     sync.Mutex
	limit  float64 // 每秒允许的字节数
	tokens float64
	last   time.Time
	burst  float64
}

// NewLimiter 创建限速器。limit 为每秒字节数，0 或负数表示不限速。
func NewLimiter(limit int64) *Limiter {
	l := &Limiter{}
	l.SetLimit(limit)
	return l
}

// SetLimit 更新限速值，允许运行期调整（修改设置后立即生效）。
func (l *Limiter) SetLimit(limit int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 {
		l.limit = 0
		l.tokens = 0
		l.burst = 0
		return
	}
	l.limit = float64(limit)
	// 桶容量取 1 秒的额度，保证短时间突发可用，同时长传输严格受限。
	l.burst = float64(limit)
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	if l.last.IsZero() {
		l.tokens = l.burst
		l.last = time.Now()
	}
}

// Limit 返回当前限速值。
func (l *Limiter) Limit() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(l.limit)
}

// Wait 阻塞直到可以消费 n 个字节，或 ctx 结束。
func (l *Limiter) Wait(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	for {
		l.mu.Lock()
		if l.limit <= 0 {
			l.mu.Unlock()
			return nil
		}
		now := time.Now()
		elapsed := now.Sub(l.last).Seconds()
		l.last = now
		l.tokens += elapsed * l.limit
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		if l.tokens >= float64(n) {
			l.tokens -= float64(n)
			l.mu.Unlock()
			return nil
		}
		deficit := float64(n) - l.tokens
		waitSec := deficit / l.limit
		l.mu.Unlock()

		// 单次等待上限 200ms，便于设置变更后快速生效。
		d := time.Duration(waitSec * float64(time.Second))
		if d > 200*time.Millisecond {
			d = 200 * time.Millisecond
		}
		if d < time.Millisecond {
			d = time.Millisecond
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// limitedReader 包装 io.Reader，在每次读取前申请配额。
type limitedReader struct {
	r   io.Reader
	l   *Limiter
	ctx context.Context
}

func (lr *limitedReader) Read(p []byte) (int, error) {
	// 单次申请不超过 256KiB，避免一次占满整个桶导致长时间阻塞。
	if len(p) > 256<<10 {
		p = p[:256<<10]
	}
	if err := lr.l.Wait(lr.ctx, len(p)); err != nil {
		return 0, err
	}
	return lr.r.Read(p)
}
