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
