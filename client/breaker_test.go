package client

import (
	"testing"
	"time"
)

func TestBreakerOpensAfterThreshold(t *testing.T) {
	now := time.Unix(0, 0)
	b := newBreaker(5, 30*time.Second, func() time.Time { return now })
	for i := 0; i < 4; i++ {
		b.onFailure()
		if !b.allow() {
			t.Fatalf("第 %d 次失敗後不該 open", i+1)
		}
	}
	b.onFailure() // 第 5 次
	if b.allow() {
		t.Fatal("連續 5 次失敗後應 open")
	}
}

func TestBreakerHalfOpenAfterTimeout(t *testing.T) {
	now := time.Unix(0, 0)
	b := newBreaker(5, 30*time.Second, func() time.Time { return now })
	for i := 0; i < 5; i++ {
		b.onFailure()
	}
	if b.allow() {
		t.Fatal("剛 open 應擋")
	}
	now = now.Add(31 * time.Second) // 過了 open 期
	if !b.allow() {
		t.Fatal("30s 後應允許 half-open 探測")
	}
	b.onSuccess() // 探測成功 → close
	now = now.Add(time.Second)
	if !b.allow() {
		t.Fatal("成功後應 close(恆允許)")
	}
}
