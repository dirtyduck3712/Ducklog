package client

import (
	"sync"
	"time"
)

// breaker 是簡單的連續失敗熔斷器。open 期間 allow() 回 false;
// 過了 openFor 後允許一次 half-open 探測,由 onSuccess/onFailure 決定 close 或續 open。
type breaker struct {
	mu            sync.Mutex
	failThreshold int
	openFor       time.Duration
	now           func() time.Time
	consecutive   int
	openedAt      time.Time // 零值代表未 open
}

func newBreaker(failThreshold int, openFor time.Duration, now func() time.Time) *breaker {
	return &breaker{failThreshold: failThreshold, openFor: openFor, now: now}
}

func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openedAt.IsZero() {
		return true // closed
	}
	// open:過了 openFor 才允許一次探測
	return b.now().Sub(b.openedAt) >= b.openFor
}

func (b *breaker) onSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
	b.openedAt = time.Time{}
}

func (b *breaker) onFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive++
	if b.consecutive >= b.failThreshold {
		b.openedAt = b.now() // (重新)標記 open 起點,half-open 探測失敗會刷新
	}
}
