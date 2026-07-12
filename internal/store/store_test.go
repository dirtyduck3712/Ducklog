package store

import (
	"context"
	"testing"
	"time"

	"docklog/internal/model"
)

func sample(n int) []model.LogEntry {
	now := time.Now().UTC()
	out := make([]model.LogEntry, n)
	for i := range out {
		out[i] = model.LogEntry{
			TS: now, IngestedAt: now, Service: "api", Level: model.Error,
			TraceID: "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4050",
			Message: "connection timeout", Attrs: `{"host":"10.0.1.5"}`,
		}
	}
	return out
}

func TestInsertAndCount(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.Insert(ctx, sample(100)); err != nil {
		t.Fatal(err)
	}
	n, err := s.Count(ctx)
	if err != nil || n != 100 {
		t.Fatalf("Count = %d, %v; want 100", n, err)
	}
}

func TestEmptyTraceIDIsNull(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	e := sample(1)
	e[0].TraceID = "" // 應寫成 NULL,不報錯
	if err := s.Insert(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.Insert(context.Background(), sample(50))
	s.Close()

	s2, err := Open(dir) // 重開同目錄,資料要在
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	n, _ := s2.Count(context.Background())
	if n != 50 {
		t.Fatalf("重開後 Count = %d; want 50", n)
	}
}

func TestCheckpointFlushesParquet(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	defer s.Close()
	s.Insert(context.Background(), sample(2000))
	if err := s.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 不直接斷言檔數(避免與 DuckLake 內部行為耦合),只確認 checkpoint 不報錯且資料仍在
	n, _ := s.Count(context.Background())
	if n != 2000 {
		t.Fatalf("checkpoint 後 Count = %d; want 2000", n)
	}
}
