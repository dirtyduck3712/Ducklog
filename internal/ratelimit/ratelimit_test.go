package ratelimit

import (
	"testing"
	"time"
)

func TestPerServiceOverrideAndDrop(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(1000, map[string]float64{"batch-worker": 5000})
	l.now = func() time.Time { return now }

	// api:default 1000/s。一次要 1500 → 准 1000 丟 500(bucket 起始滿)。
	allowed, dropped := l.Allow("api", 1500)
	if allowed != 1000 || dropped != 500 {
		t.Fatalf("api Allow(1500) = %d,%d; want 1000,500", allowed, dropped)
	}
	// batch-worker override 5000/s:要 3000 全准。
	allowed, dropped = l.Allow("batch-worker", 3000)
	if allowed != 3000 || dropped != 0 {
		t.Fatalf("batch Allow(3000) = %d,%d; want 3000,0", allowed, dropped)
	}
}

func TestRefillOverTime(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(1000, nil)
	l.now = func() time.Time { return now }
	l.Allow("api", 1000)        // 用光
	_, d := l.Allow("api", 100) // 立刻再要 → 全丟
	if d != 100 {
		t.Fatalf("耗盡後 dropped = %d; want 100", d)
	}
	now = now.Add(time.Second) // 過 1 秒補滿
	a, _ := l.Allow("api", 500)
	if a != 500 {
		t.Fatalf("補充後 allowed = %d; want 500", a)
	}
}
