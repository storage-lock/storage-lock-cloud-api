package main

import (
	"sync"
	"time"
)

// 令牌桶限流。
//
// 原实现有两个问题：clients map 只增不减（每个新 IP 永久占一份内存，被人扫一遍 IP 段就 OOM），
// 且按固定一分钟窗口计数，边界处能翻倍放行。这里改成：
//   - 每客户端固定容量令牌桶，按速率恢复；
//   - 桶长期不用会被惰性淘汰（lastSeen 超时就整条删掉）；
//   - 后台定期扫描，避免只有冷数据时不触发淘汰。
type bucket struct {
	tokens   float64
	last     time.Time
	lastSeen time.Time
}

type tokenBucketLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity float64
	rate     float64 // 每秒恢复的令牌数
	idle     time.Duration
	now      func() time.Time
}

func newTokenBucketLimiter(capacity, refillPerSecond float64, idle time.Duration) *tokenBucketLimiter {
	return &tokenBucketLimiter{
		buckets:  make(map[string]*bucket),
		capacity: capacity,
		rate:     refillPerSecond,
		idle:     idle,
		now:      time.Now,
	}
}

// allow 消耗一个令牌；返回是否放行，以及需要等待多久才有下一个令牌。
func (l *tokenBucketLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.capacity, last: now}
		l.buckets[key] = b
	}
	b.lastSeen = now

	// 按经过的时间补充令牌，上限不超过桶容量。
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.capacity {
			b.tokens = l.capacity
		}
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	need := (1 - b.tokens) / l.rate
	return false, time.Duration(need * float64(time.Second))
}

// sweep 清掉长时间不活跃的桶。
func (l *tokenBucketLimiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > l.idle {
			delete(l.buckets, k)
		}
	}
}

// runSweeper 后台定期清理，直到 stop 关闭。
func (l *tokenBucketLimiter) runSweeper(stop <-chan struct{}, interval time.Duration) {
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			l.sweep()
		}
	}
}

// 限流策略：默认每个 IP 每分钟 120 次、每个租户每分钟 600 次。
//
// 桶容量取"一分钟的额度"，而不是几十个：NAT / 公司出口后面会有很多用户共享同一个 IP，
// 容量太小的话他们会在几秒内被互相挤掉。突发到一分钟的量是可接受的，
// 平均速率仍然受每分钟基线约束。
type limiters struct {
	perIP   *tokenBucketLimiter
	perUser *tokenBucketLimiter
}

func newLimiters(perIPPerMin, perTenantPerMin float64) *limiters {
	perIPPerMin = max(perIPPerMin, 1)
	perTenantPerMin = max(perTenantPerMin, 1)
	return &limiters{
		perIP:   newTokenBucketLimiter(perIPPerMin, perIPPerMin/60, 10*time.Minute),
		perUser: newTokenBucketLimiter(perTenantPerMin, perTenantPerMin/60, 30*time.Minute),
	}
}
