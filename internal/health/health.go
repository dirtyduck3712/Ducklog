// Package health 提供 GET /health,暴露新架構的關鍵指標(catalog 大小 / 最後 checkpoint)。
package health

import (
	"encoding/json"
	"net/http"
	"time"

	"docklog/internal/diskguard"
	"docklog/internal/metrics"
	"docklog/internal/store"
)

type Deps struct {
	Store          *store.Store
	Metrics        *metrics.MetricsStore
	Guard          *diskguard.Guard
	LastCheckpoint func() time.Time
}

type Handler struct{ d Deps }

func New(d Deps) *Handler { return &Handler{d: d} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, usage, _ := h.d.Guard.State()
	catBytes, _ := h.d.Store.CatalogSizeBytes()
	dropped, _ := h.d.Metrics.DroppedSince(r.Context(), time.Now().Add(-time.Hour))
	last := h.d.LastCheckpoint()
	var lastStr any
	if !last.IsZero() {
		lastStr = last.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":            "ok",
		"disk_usage":        usage,
		"catalog_size_mb":   float64(catBytes) / 1e6,
		"last_checkpoint":   lastStr,
		"dropped_last_hour": dropped,
	})
}
