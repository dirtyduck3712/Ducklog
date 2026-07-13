package tools

import (
	"context"
	"fmt"
	"testing"
	"time"

	"docklog/internal/vl"
	"docklog/internal/vltest"
)

// ingestErrors 寫入兩種 error 模板(數字不同,應收斂成 2 個 pattern)+ 一些 info。
func ingestErrors(t *testing.T, base string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"ts":"%s","message":"timeout after %ds","service":"api","level":"error"}`, now, i))
	}
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"ts":"%s","message":"panic at handler %d","service":"api","level":"error"}`, now, i))
	}
	for i := 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"ts":"%s","message":"request served %d","service":"api","level":"info"}`, now, i))
	}
	vltest.Ingest(t, base, lines...)
}

// ingestTrace 寫入同 trace_id 的 n 筆 log。
func ingestTrace(t *testing.T, base, traceID string, n int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var lines []string
	for i := 0; i < n; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"ts":"%s","message":"step %d","service":"api","level":"info","trace_id":"%s"}`, now, i, traceID))
	}
	vltest.Ingest(t, base, lines...)
}

// ingestN 寫入 n 筆 info log。
func ingestN(t *testing.T, base string, n int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"ts":"%s","message":"line %d","service":"api","level":"info"}`, now, i))
	}
	vltest.Ingest(t, base, lines...)
}

func TestSummarizeErrorsFingerprints(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVL(t)
	c := vl.New(base, 10*time.Second)
	ingestErrors(t, base)
	e := SummarizeErrors(context.Background(), c, "api", "1h", 20)
	if e.Status != "ok" {
		t.Fatalf("%+v", e)
	}
	pats, _ := e.Data.([]map[string]any)
	if len(pats) != 2 {
		t.Fatalf("應收斂成 2 個 fingerprint pattern,得 %d: %v", len(pats), pats)
	}
}

func TestGetTraceReturnsWholeTrace(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVL(t)
	c := vl.New(base, 10*time.Second)
	tid := "0af76519-16cd-43dd-8448-eb211c80319c"
	ingestTrace(t, base, tid, 3)
	e := GetTrace(context.Background(), c, tid)
	rows, _ := e.Data.([]map[string]any)
	if e.Status != "ok" || len(rows) != 3 {
		t.Fatalf("get_trace = %d 筆; want 3", len(rows))
	}
}

func TestSearchLogsMarksTruncation(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVL(t)
	c := vl.New(base, 10*time.Second)
	ingestN(t, base, 500)
	e := SearchLogs(context.Background(), c, "level:=info", "1h", 100)
	if e.TotalMatched < 500 || !e.Truncated {
		t.Fatalf("truncation 應標示:%+v", e)
	}
	if e.ReturnedN > 100 {
		t.Fatalf("returned 應 ≤ limit")
	}
}

func TestSearchRequiresTimeRange(t *testing.T) {
	base := vltest.StartVL(t)
	c := vl.New(base, 10*time.Second)
	e := SearchLogs(context.Background(), c, "level:=info", "", 100)
	if e.Status != "error" || e.ErrorCode != "MISSING_TIME_RANGE" {
		t.Fatalf("空 range 應拒:%+v", e)
	}
}
