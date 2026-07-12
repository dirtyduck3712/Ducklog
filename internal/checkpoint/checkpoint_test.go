package checkpoint

import (
	"context"
	"testing"
	"time"

	"docklog/internal/store"
)

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
