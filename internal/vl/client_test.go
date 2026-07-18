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

func TestBasicAuthGuardsQueries(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVLWithAuth(t, "reader", "secret")

	// 無憑證 → Query 應失敗(401)。VL 的 /health 不受 -httpAuth 保護,
	// 故用受保護的 query endpoint 驗證護欄(與測試名一致)。
	noauth := New(base, 5*time.Second)
	if _, err := noauth.Query(context.Background(), "* _time:1h", 10); err == nil {
		t.Fatal("無憑證的 Query 應失敗")
	}

	// 對的憑證 → Ping 成功、Query 不報錯(空結果可接受)。
	ok := NewWithAuth(base, 5*time.Second, "reader", "secret")
	if err := ok.Ping(context.Background()); err != nil {
		t.Fatalf("帶對憑證的 Ping 應成功: %v", err)
	}
	if _, err := ok.Query(context.Background(), "* _time:1h", 10); err != nil {
		t.Fatalf("帶對憑證的 Query 應成功: %v", err)
	}
}
