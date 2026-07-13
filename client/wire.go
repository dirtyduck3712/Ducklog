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
// 對單一無法序列化的 entry 有韌性:json.Encoder.Encode 先 marshal 到內部 buffer,
// marshal 失敗時不會寫出部分資料,故可安全改送精簡版 —— 保留 ts/service/level/
// message,只把 attrs 換成 _encode_error 標記。一顆毒值不會丟整批,也不丟該行。
func encodeNDJSON(w io.Writer, entries []entry) error {
	enc := json.NewEncoder(w) // Encode 會自動補換行
	for i := range entries {
		if err := enc.Encode(&entries[i]); err != nil {
			safe := entries[i]
			safe.Attrs = map[string]any{"_encode_error": err.Error()}
			if err := enc.Encode(&safe); err != nil {
				return err // 連精簡版都失敗(理論上不會發生)
			}
		}
	}
	return nil
}
