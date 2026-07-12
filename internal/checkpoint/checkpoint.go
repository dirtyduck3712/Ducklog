// Package checkpoint 週期性把 inline 資料落地成 Parquet 並修剪 catalog。
// goroutine 死掉會讓 inline 無限累積(失敗模式 #5),故必有 panic recovery。
package checkpoint

import (
	"context"
	"log"
	"sync"
	"time"
)

type Checkpointer interface {
	Checkpoint(ctx context.Context) error
}

type Loop struct {
	st       Checkpointer
	interval time.Duration
	mu       sync.Mutex
	last     time.Time
}

func NewLoop(st Checkpointer, interval time.Duration) *Loop {
	return &Loop{st: st, interval: interval}
}

func (l *Loop) LastCheckpoint() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

func (l *Loop) Once(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("checkpoint panic recovered: %v", r)
		}
	}()
	if err = l.st.Checkpoint(ctx); err != nil {
		return err
	}
	l.mu.Lock()
	l.last = time.Now()
	l.mu.Unlock()
	return nil
}

func (l *Loop) Run(ctx context.Context) {
	t := time.NewTicker(l.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.Once(ctx); err != nil {
				log.Printf("checkpoint error: %v", err)
			}
		}
	}
}
