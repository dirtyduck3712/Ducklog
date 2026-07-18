package tools

import (
	"context"
	"testing"
	"time"

	"ducklog/internal/vl"
	"ducklog/internal/vltest"
)

// pipe 查詢能跑，且證明 _time: 被注入到第一個 pipe「之前」。
// 若錯誤地 append 到 pipeline 尾端(... count() as n _time:1h),VL 會語法錯 → QUERY_FAILED。
func TestRunLogsQLRunsPipedQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVL(t)
	c := vl.New(base, 10*time.Second)
	ingestErrors(t, base) // 50 筆 service=api 的 error
	e := RunLogsQL(context.Background(), c, "level:=error | stats by (service) count() as n", "1h", 100)
	if e.Status != "ok" {
		t.Fatalf("pipe 查詢應成功: %+v", e)
	}
	rows, _ := e.Data.([]map[string]any)
	if len(rows) != 1 || str(rows[0]["service"]) != "api" || str(rows[0]["n"]) != "50" {
		t.Fatalf("聚合結果不符,want 1 列 service=api n=50,得: %v", rows)
	}
}

func TestRunLogsQLRequiresTimeRange(t *testing.T) {
	c := vl.New("http://127.0.0.1:1", time.Second)
	e := RunLogsQL(context.Background(), c, "level:=info", "", 100)
	if e.Status != "error" || e.ErrorCode != "MISSING_TIME_RANGE" {
		t.Fatalf("空 range 應拒: %+v", e)
	}
}

func TestRunLogsQLRejectsEmptyQuery(t *testing.T) {
	c := vl.New("http://127.0.0.1:1", time.Second)
	e := RunLogsQL(context.Background(), c, "   ", "1h", 100)
	if e.Status != "error" || e.ErrorCode != "MALFORMED_QUERY" {
		t.Fatalf("空 query 應拒: %+v", e)
	}
}

func TestRunLogsQLMarksTruncation(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVL(t)
	c := vl.New(base, 10*time.Second)
	ingestN(t, base, 500) // 500 筆 info(無 pipe 查詢路徑)
	e := RunLogsQL(context.Background(), c, "level:=info", "1h", 100)
	if !e.Truncated {
		t.Fatalf("命中 limit 應標 Truncated: %+v", e)
	}
	if e.ReturnedN > 100 {
		t.Fatalf("returned 應 ≤ limit,得 %d", e.ReturnedN)
	}
}
