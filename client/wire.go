// Package client 是 docklog 的純 stdlib Go 傳輸層:一個 slog.Handler + trace ID 傳遞。
package client

import (
	"encoding/json"
	"io"
	"log/slog"
)

// entry 是送往 server /ingest 的一行 wire JSON。
type entry struct {
	TS      string         `json:"ts"`
	Service string         `json:"service"`
	Level   string         `json:"level"`
	TraceID string         `json:"trace_id,omitempty"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// levelString 把 slog.Level 對應到 docklog 的小寫等級字串。
func levelString(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}

// encodeNDJSON 把 entries 寫成每行一筆的 NDJSON。
func encodeNDJSON(w io.Writer, entries []entry) error {
	enc := json.NewEncoder(w) // Encode 會自動補換行
	for i := range entries {
		if err := enc.Encode(&entries[i]); err != nil {
			return err
		}
	}
	return nil
}
