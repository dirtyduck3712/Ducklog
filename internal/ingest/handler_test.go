package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"docklog/internal/auth"
	"docklog/internal/diskguard"
	"docklog/internal/metrics"
	"docklog/internal/model"
	"docklog/internal/ratelimit"
	"docklog/internal/store"
)

func newTestHandler(t *testing.T, usage float64) (*Handler, *store.Store, *metrics.MetricsStore) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := metrics.Open(filepath.Join(dir, "metrics.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{
		Store:   st,
		Metrics: ms,
		Keys:    auth.New([]auth.Key{{Key: "k", Name: "t", Scopes: []string{"ingest"}}}),
		Limiter: ratelimit.New(1000, nil),
		Guard:   diskguard.New(dir, func(string) (float64, error) { return usage, nil }),
		Now:     func() time.Time { return time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC) },
	})
	return h, st, ms
}

func post(h *Handler, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestIngestOK(t *testing.T) {
	h, st, _ := newTestHandler(t, 0.10)
	body := `{"ts":"2026-07-13T10:00:00Z","service":"api","level":"error","message":"boom","attrs":{"a":1}}
{"ts":"2026-07-13T10:00:01Z","service":"api","level":"info","message":"ok"}`
	rr := post(h, "k", body)
	if rr.Code != 200 {
		t.Fatalf("code = %d, body=%s", rr.Code, rr.Body)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["accepted"].(float64) != 2 {
		t.Fatalf("accepted = %v; want 2", resp["accepted"])
	}
	n, _ := st.Count(context.Background())
	if n != 2 {
		t.Fatalf("stored = %d; want 2", n)
	}
}

func TestRejectNoAuth(t *testing.T) {
	h, _, _ := newTestHandler(t, 0.10)
	if rr := post(h, "", `{}`); rr.Code != 401 {
		t.Fatalf("no auth code = %d; want 401", rr.Code)
	}
	if rr := post(h, "wrong", `{}`); rr.Code != 401 {
		t.Fatalf("bad key code = %d; want 401", rr.Code)
	}
}

func TestDiskFullRejects(t *testing.T) {
	h, _, _ := newTestHandler(t, 0.92) // > RejectThreshold
	rr := post(h, "k", `{"ts":"2026-07-13T10:00:00Z","service":"api","level":"info","message":"x"}`)
	if rr.Code != 503 {
		t.Fatalf("disk full code = %d; want 503", rr.Code)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["error_code"] != "DISK_FULL" {
		t.Fatalf("error_code = %v; want DISK_FULL", resp["error_code"])
	}
}

func TestClockSkewOverride(t *testing.T) {
	h, st, _ := newTestHandler(t, 0.10)
	// ts 比 server now(10:00)早 2 小時 → 超過 ±1h,應覆蓋為 server 時間 + 標記
	post(h, "k", `{"ts":"2026-07-13T08:00:00Z","service":"api","level":"info","message":"skew"}`)
	var attrs string
	if err := st.DB().QueryRow("SELECT attrs::VARCHAR FROM lake.logs LIMIT 1").Scan(&attrs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(attrs, "_clock_skew") {
		t.Fatalf("attrs 應含 _clock_skew,得到 %s", attrs)
	}
}

// Fix I-2:未知 level 降級為 Info 但明確標記 _level_unknown,不靜默。
func TestUnknownLevelMarked(t *testing.T) {
	h, st, _ := newTestHandler(t, 0.10)
	rr := post(h, "k", `{"ts":"2026-07-13T10:00:00Z","service":"api","level":"fatal","message":"x"}`)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	var level uint8
	var attrs string
	if err := st.DB().QueryRow("SELECT level, attrs::VARCHAR FROM lake.logs LIMIT 1").Scan(&level, &attrs); err != nil {
		t.Fatal(err)
	}
	if level != uint8(model.Info) {
		t.Fatalf("level=%d; want Info(%d)", level, model.Info)
	}
	if !strings.Contains(attrs, "_level_unknown") {
		t.Fatalf("attrs 應含 _level_unknown,得到 %s", attrs)
	}
}

// Fix I-1:非 UUID 的 trace_id 不能毒死整個 batch;該列寫 NULL 並標記,其餘照常。
func TestInvalidTraceIDDoesNotFailBatch(t *testing.T) {
	h, st, _ := newTestHandler(t, 0.10)
	body := `{"ts":"2026-07-13T10:00:00Z","service":"api","level":"info","message":"bad","trace_id":"not-a-uuid"}
{"ts":"2026-07-13T10:00:01Z","service":"api","level":"info","message":"good","trace_id":"550e8400-e29b-41d4-a716-446655440000"}`
	rr := post(h, "k", body)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["accepted"].(float64) != 2 {
		t.Fatalf("accepted=%v; want 2", resp["accepted"])
	}
	n, _ := st.Count(context.Background())
	if n != 2 {
		t.Fatalf("stored=%d; want 2", n)
	}
	var attrs string
	var traceNull bool
	if err := st.DB().QueryRow(
		"SELECT attrs::VARCHAR, trace_id IS NULL FROM lake.logs WHERE message='bad'").Scan(&attrs, &traceNull); err != nil {
		t.Fatal(err)
	}
	if !traceNull {
		t.Fatal("invalid trace_id 應存 NULL")
	}
	if !strings.Contains(attrs, "_trace_id_invalid") {
		t.Fatalf("attrs 應含 _trace_id_invalid,得到 %s", attrs)
	}
}

// Fix I-1 (canonicalization):有效但非 canonical 的 trace_id(如 brace-wrapped)也不能
// 毒死 batch,必須正規化成 8-4-4-4-12 後才進 ?::UUID。
func TestNonCanonicalTraceIDCanonicalized(t *testing.T) {
	h, st, _ := newTestHandler(t, 0.10)
	rr := post(h, "k", `{"ts":"2026-07-13T10:00:00Z","service":"api","level":"info","message":"brace","trace_id":"{0af76519-16cd-43dd-8448-eb211c80319c}"}`)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	var traceID string
	if err := st.DB().QueryRow(
		"SELECT trace_id::VARCHAR FROM lake.logs WHERE message='brace'").Scan(&traceID); err != nil {
		t.Fatal(err)
	}
	if traceID != "0af76519-16cd-43dd-8448-eb211c80319c" {
		t.Fatalf("trace_id=%q; want canonical 0af76519-16cd-43dd-8448-eb211c80319c", traceID)
	}
}

// Fix I-3:壞行要計數並回報 malformed,不靜默丟棄。
func TestMalformedLinesCounted(t *testing.T) {
	h, st, _ := newTestHandler(t, 0.10)
	body := `{"ts":"2026-07-13T10:00:00Z","service":"api","level":"info","message":"ok"}
{not json}
this is garbage
{"ts":"2026-07-13T10:00:01Z","service":"api","level":"info","message":"ok2"}`
	rr := post(h, "k", body)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["malformed"].(float64) != 2 {
		t.Fatalf("malformed=%v; want 2", resp["malformed"])
	}
	if resp["accepted"].(float64) != 2 {
		t.Fatalf("accepted=%v; want 2", resp["accepted"])
	}
	n, _ := st.Count(context.Background())
	if n != 2 {
		t.Fatalf("stored=%d; want 2", n)
	}
}

// Fix I-3:單行超過 8MB scanner cap 會截斷請求,必須回 400 而非靜默 200。
func TestOversizeLineReturns400(t *testing.T) {
	h, _, _ := newTestHandler(t, 0.10)
	huge := `{"ts":"2026-07-13T10:00:00Z","service":"api","level":"info","message":"` +
		strings.Repeat("x", 9*1024*1024) + `"}`
	rr := post(h, "k", huge)
	if rr.Code != 400 {
		t.Fatalf("oversize code=%d; want 400", rr.Code)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["error_code"] != "BAD_REQUEST" {
		t.Fatalf("error_code=%v; want BAD_REQUEST", resp["error_code"])
	}
}

func TestRateLimitRecordsDropped(t *testing.T) {
	h, _, ms := newTestHandler(t, 0.10)
	// 超過 default 1000/s:送 1200 筆同 service,應 accept 1000 drop 200,且 dropped 表有記錄
	var sb strings.Builder
	for i := 0; i < 1200; i++ {
		sb.WriteString(`{"ts":"2026-07-13T10:00:00Z","service":"flood","level":"info","message":"x"}` + "\n")
	}
	rr := post(h, "k", sb.String())
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["rejected"].(float64) != 200 {
		t.Fatalf("rejected = %v; want 200", resp["rejected"])
	}
	got, _ := ms.DroppedSince(context.Background(), time.Time{})
	if got != 200 {
		t.Fatalf("metrics dropped = %d; want 200", got)
	}
}
