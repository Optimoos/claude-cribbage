package server

import (
	"sync"
	"time"
)

// limiter is a set of token buckets by key: each key may spend up to burst
// tokens at once, refilled at one token per every.
type limiter struct {
	burst float64
	every time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(burst int, every time.Duration) *limiter {
	return &limiter{burst: float64(burst), every: every, buckets: map[string]*bucket{}}
}

// maxBuckets is how many keys a limiter tracks before it forgets the full
// ones, so an attacker cycling keys cannot grow it without bound.
const maxBuckets = 100_000

// fill returns key's bucket brought up to date. Callers hold mu.
func (l *limiter) fill(key string, now time.Time) *bucket {
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxBuckets && now.Sub(l.swept) > time.Second {
			l.sweep(now)
		}
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
		return b
	}
	b.tokens = min(l.burst, b.tokens+float64(now.Sub(b.at))/float64(l.every))
	b.at = now
	return b
}

// sweep drops buckets that have refilled, which behave like new ones.
func (l *limiter) sweep(now time.Time) {
	l.swept = now
	full := time.Duration(l.burst * float64(l.every))
	for k, b := range l.buckets {
		if now.Sub(b.at) >= full {
			delete(l.buckets, k)
		}
	}
}

// allow reports whether key has a token left, without spending it.
func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fill(key, now).tokens >= 1
}

// take spends one of key's tokens and reports whether there was one.
func (l *limiter) take(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.fill(key, now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
