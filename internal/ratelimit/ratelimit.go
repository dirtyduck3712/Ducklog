// Package ratelimit 是 per-service token bucket。超量丟棄,回報丟棄數(不可靜默)。
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

type Limiter struct {
	mu        sync.Mutex
	defRate   float64
	overrides map[string]float64
	buckets   map[string]*bucket
	now       func() time.Time
}

func New(defaultPerSec float64, overrides map[string]float64) *Limiter {
	if overrides == nil {
		overrides = map[string]float64{}
	}
	return &Limiter{
		defRate:   defaultPerSec,
		overrides: overrides,
		buckets:   map[string]*bucket{},
		now:       time.Now,
	}
}

func (l *Limiter) rate(service string) float64 {
	if r, ok := l.overrides[service]; ok {
		return r
	}
	return l.defRate
}

// Allow 針對一次寫 n 筆的請求,回傳准許數與丟棄數。burst 容量 = 1 秒的 rate。
func (l *Limiter) Allow(service string, n int) (allowed, dropped int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rate := l.rate(service)
	now := l.now()
	b := l.buckets[service]
	if b == nil {
		b = &bucket{tokens: rate, last: now} // 起始滿
		l.buckets[service] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		b.tokens = min(rate, b.tokens+elapsed*rate)
		b.last = now
	}
	allowed = n
	if float64(allowed) > b.tokens {
		allowed = int(b.tokens)
	}
	b.tokens -= float64(allowed)
	return allowed, n - allowed
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
