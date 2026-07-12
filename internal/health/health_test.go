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
}
