// Package ingest 是 POST /ingest 的 HTTP handler,把 auth / clock-skew / rate-limit /
// disk-guard / 儲存串起來。順序:先擋(auth→disk),再解析,再限流,最後寫。
package ingest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"docklog/internal/auth"
	"docklog/internal/diskguard"
	"docklog/internal/metrics"
	"docklog/internal/model"
	"docklog/internal/ratelimit"
	"docklog/internal/store"
)

type Deps struct {
	Store   *store.Store
	Metrics *metrics.MetricsStore
	Keys    *auth.KeyStore
	Limiter *ratelimit.Limiter
	Guard   *diskguard.Guard
	Now     func() time.Time
}

type Handler struct{ d Deps }

func New(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Handler{d: d}
}

// 進來的一行 JSON。
type rawLog struct {
	TS      time.Time       `json:"ts"`
	Service string          `json:"service"`
	Level   string          `json:"level"`
	TraceID string          `json:"trace_id"`
	Message string          `json:"message"`
	Attrs   json.RawMessage `json:"attrs"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "error", "error_code": "METHOD"})
		return
	}
	// 1. auth(需 ingest scope)
	key, ok := h.d.Keys.Authenticate(r.Header.Get("Authorization"))
	if !ok || !key.HasScope("ingest") {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"status": "error", "error_code": "UNAUTHORIZED"})
		return
	}
	// 2. disk guard:Reject/Purge 都拒新寫入(拒絕好過寫壞檔案)
	state, usage, err := h.d.Guard.State()
	if err == nil && state >= diskguard.Reject {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "error", "error_code": "DISK_FULL",
			"message": fmt.Sprintf("磁碟使用率 %.0f%%,拒絕寫入", usage*100),
		})
		return
	}
	// 3. 解析 ndjson
	now := h.d.Now().UTC()
	var entries []model.LogEntry
	perService := map[string]int{}
	sc := bufio.NewScanner(r.Body)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw rawLog
		if err := json.Unmarshal(line, &raw); err != nil {
			continue // 壞行跳過(第一版寬鬆);TODO 之後計數
		}
		lvl, ok := model.ParseLevel(raw.Level)
		if !ok {
			lvl = model.Info
		}
		e := model.LogEntry{
			TS: raw.TS, IngestedAt: now, Service: raw.Service, Level: lvl,
			TraceID: raw.TraceID, Message: raw.Message, Attrs: string(raw.Attrs),
		}
		applyClockSkew(&e, now)
		entries = append(entries, e)
		perService[raw.Service]++
	}

	// 4. rate limit(per service),被丟棄的記進 metrics.dropped
	accepted := entries[:0]
	var rejected int
	allowedByService := map[string]int{}
	for svc, n := range perService {
		a, dropped := h.d.Limiter.Allow(svc, n)
		allowedByService[svc] = a
		if dropped > 0 {
			rejected += dropped
			_ = h.d.Metrics.AddDropped(r.Context(), now, svc, int64(dropped))
		}
	}
	for _, e := range entries {
		if allowedByService[e.Service] > 0 {
			allowedByService[e.Service]--
			accepted = append(accepted, e)
		}
	}

	// 5. 寫入 store + metrics 聚合
	if err := h.d.Store.Insert(r.Context(), accepted); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"status": "error", "error_code": "STORE", "message": err.Error()})
		return
	}
	_ = h.d.Metrics.Add(r.Context(), buckets(accepted))

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "accepted": len(accepted), "rejected": rejected})
}

// applyClockSkew:偏差 > ±1h 用 server 時間覆蓋 ts 並標記 attrs._clock_skew。
func applyClockSkew(e *model.LogEntry, now time.Time) {
	if e.TS.IsZero() {
		e.TS = now
		return
	}
	diff := e.TS.Sub(now)
	if diff < -time.Hour || diff > time.Hour {
		e.TS = now
		e.ClockSkewed = true
		e.Attrs = mergeSkewFlag(e.Attrs)
	}
}

// mergeSkewFlag 在 JSON 物件裡塞入 "_clock_skew":true。空/壞值一律換成標記物件。
func mergeSkewFlag(attrs string) string {
	m := map[string]any{}
	if attrs != "" {
		_ = json.Unmarshal([]byte(attrs), &m)
	}
	m["_clock_skew"] = true
	b, _ := json.Marshal(m)
	return string(b)
}

func buckets(entries []model.LogEntry) []metrics.Bucket {
	agg := map[metrics.Bucket]int64{}
	for _, e := range entries {
		k := metrics.Bucket{TS: e.IngestedAt.Truncate(time.Minute), Service: e.Service, Level: e.Level}
		agg[k]++
	}
	out := make([]metrics.Bucket, 0, len(agg))
	for k, c := range agg {
		k.Count = c
		out = append(out, k)
	}
	return out
}
