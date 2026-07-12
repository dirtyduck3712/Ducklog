package client

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestLevelString(t *testing.T) {
	cases := map[slog.Level]string{
		slog.LevelDebug: "debug", slog.LevelInfo: "info",
		slog.LevelWarn: "warn", slog.LevelError: "error",
		slog.LevelInfo + 1:  "info",  // Info<x<Warn → info
		slog.LevelError + 4: "error", // 超過 Error 仍 error
	}
	for lvl, want := range cases {
		if got := levelString(lvl); got != want {
			t.Fatalf("levelString(%v) = %q; want %q", lvl, got, want)
		}
	}
}

func TestEncodeNDJSON(t *testing.T) {
	var buf bytes.Buffer
	entries := []entry{
		{TS: "2026-07-13T10:00:00Z", Service: "api", Level: "error",
			TraceID: "0af76519-16cd-43dd-8448-eb211c80319c",
			Message: "boom", Attrs: map[string]any{"a": 1}},
		{TS: "2026-07-13T10:00:01Z", Service: "api", Level: "info", Message: "ok"},
	}
	if err := encodeNDJSON(&buf, entries); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), buf.String())
	}
	// 第一筆含 trace_id,第二筆(空 trace_id)應 omit
	if !strings.Contains(lines[0], `"trace_id":"0af76519`) {
		t.Fatalf("line0 缺 trace_id: %s", lines[0])
	}
	if strings.Contains(lines[1], "trace_id") {
		t.Fatalf("line1 空 trace_id 應 omitempty: %s", lines[1])
	}
}
