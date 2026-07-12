package checkpoint

import (
	"context"
	"sync"
	"testing"
	"time"

	"docklog/internal/store"
)

type flakyCheckpointer struct {
	mu    sync.Mutex
	calls int
}

func (f *flakyCheckpointer) Checkpoint(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls == 1 {
		panic("boom") // first tick panics
	}
	return nil
}

func TestPanicRecoveryKeepsLooping(t *testing.T) {
	l := NewLoop(&flakyCheckpointer{}, time.Hour)

	// 第一次 Once panic,recovery 攔下,不 re-panic,LastCheckpoint 維持零值。
	if err := l.Once(context.Background()); err != nil {
		t.Fatalf("panic 應被 recovery 攔下,不回傳 error,got %v", err)
	}
	if !l.LastCheckpoint().IsZero() {
		t.Fatal("panic 後 LastCheckpoint 不應被更新")
	}

	// loop 仍可用:第二次 Once 正常完成並更新 LastCheckpoint。
	if err := l.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.LastCheckpoint().IsZero() {
		t.Fatal("recovery 後第二次 Once 應更新 LastCheckpoint")
	}
}

func TestOnceUpdatesTimestamp(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	l := NewLoop(st, time.Hour)
	if !l.LastCheckpoint().IsZero() {
		t.Fatal("初始 LastCheckpoint 應為零值")
	}
	if err := l.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.LastCheckpoint().IsZero() {
		t.Fatal("Once 後 LastCheckpoint 應被更新")
	}
}

func TestRunStopsOnContext(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	l := NewLoop(st, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run 未在 ctx 取消後結束")
	}
	if l.LastCheckpoint().IsZero() {
		t.Fatal("Run 期間應至少 checkpoint 一次")
	}
}
