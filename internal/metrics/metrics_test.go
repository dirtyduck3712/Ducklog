package metrics

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"docklog/internal/model"
)

func TestUpsertAccumulates(t *testing.T) {
	m, err := Open(filepath.Join(t.TempDir(), "metrics.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := context.Background()
	ts := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	b := []Bucket{{TS: ts, Service: "api", Level: model.Error, Count: 3}}
	m.Add(ctx, b)
	m.Add(ctx, b) // 同 key 再加 3 → 應為 6
	var got int64
	m.DB().QueryRowContext(ctx,
		"SELECT count FROM metrics WHERE service='api' AND level=3").Scan(&got)
	if got != 6 {
		t.Fatalf("count = %d; want 6 (upsert 累加)", got)
	}
}

func TestDropped(t *testing.T) {
	m, _ := Open(filepath.Join(t.TempDir(), "metrics.duckdb"))
	defer m.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	m.AddDropped(ctx, now, "batch-worker", 500)
	m.AddDropped(ctx, now, "batch-worker", 200)
	got, _ := m.DroppedSince(ctx, now.Add(-time.Hour))
	if got != 700 {
		t.Fatalf("DroppedSince = %d; want 700", got)
	}
}
