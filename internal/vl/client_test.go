package vl

import (
	"context"
	"testing"
	"time"

	"ducklog/internal/vltest"
)

func TestQueryAndCount(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVL(t)
	c := New(base, 10*time.Second)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	vltest.Ingest(t, base,
		`{"ts":"`+now+`","message":"boom","service":"api","level":"error","trace_id":"0af76519-16cd-43dd-8448-eb211c80319c"}`,
		`{"ts":"`+now+`","message":"ok","service":"api","level":"info"}`,
	)
	ctx := context.Background()
	n, err := c.Count(ctx, `service:=api`)
	if err != nil || n != 2 {
		t.Fatalf("Count=%d err=%v; want 2", n, err)
	}
	rows, err := c.Query(ctx, `level:=error`, 10)
	if err != nil || len(rows) != 1 || rows[0]["_msg"] != "boom" {
		t.Fatalf("Query error rows=%v err=%v", rows, err)
	}
}
