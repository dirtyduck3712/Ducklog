package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"docklog/internal/diskguard"
	"docklog/internal/metrics"
	"docklog/internal/store"
)

func TestHealth(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(dir)
	defer st.Close()
	ms, _ := metrics.Open(filepath.Join(dir, "metrics.duckdb"))
	defer ms.Close()
	h := New(Deps{
		Store:          st,
		Metrics:        ms,
		Guard:          diskguard.New(dir, func(string) (float64, error) { return 0.42, nil }),
		LastCheckpoint: func() time.Time { return time.Time{} },
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != 200 {
		t.Fatalf("code = %d", rr.Code)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["status"] != "ok" || resp["disk_usage"].(float64) != 0.42 {
		t.Fatalf("resp = %v", resp)
	}
	if resp["last_checkpoint"] != nil {
		t.Fatalf("last_checkpoint = %v, want null", resp["last_checkpoint"])
	}
	if _, ok := resp["catalog_size_mb"].(float64); !ok {
		t.Fatalf("catalog_size_mb missing or not a number: %v", resp["catalog_size_mb"])
	}
	if resp["dropped_last_hour"] != float64(0) {
		t.Fatalf("dropped_last_hour = %v, want 0", resp["dropped_last_hour"])
	}
}
