package zapsink

import (
	"context"
	"log/slog"
	"testing"

	"github.com/dirtyduck3712/ducklog/client"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// capturingHandler 是最小的 slog.Handler,記下收到的 record 供斷言。
type capturingHandler struct {
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func recordAttrs(r slog.Record) map[string]any {
	m := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	return m
}

func TestCoreForwardsZapEntryToSlogHandler(t *testing.T) {
	cap := &capturingHandler{}
	logger := zap.New(NewCore(cap, slog.LevelDebug))

	logger.Error("db timeout", zap.String("host", "10.0.1.5"), zap.Int("attempt", 3))

	if len(cap.records) != 1 {
		t.Fatalf("want 1 record, got %d", len(cap.records))
	}
	r := cap.records[0]
	if r.Message != "db timeout" {
		t.Errorf("message = %q, want %q", r.Message, "db timeout")
	}
	if r.Level != slog.LevelError {
		t.Errorf("level = %v, want Error", r.Level)
	}
	if r.Time.IsZero() {
		t.Error("record time should be set from the zap entry")
	}
	attrs := recordAttrs(r)
	if attrs["host"] != "10.0.1.5" {
		t.Errorf("host attr = %v, want 10.0.1.5", attrs["host"])
	}
	if attrs["attempt"] != int64(3) {
		t.Errorf("attempt attr = %v (%T), want int64(3)", attrs["attempt"], attrs["attempt"])
	}
}

func TestCoreLevelMapping(t *testing.T) {
	cases := []struct {
		z zapcore.Level
		s slog.Level
	}{
		{zapcore.DebugLevel, slog.LevelDebug},
		{zapcore.InfoLevel, slog.LevelInfo},
		{zapcore.WarnLevel, slog.LevelWarn},
		{zapcore.ErrorLevel, slog.LevelError},
	}
	for _, c := range cases {
		if got := zapToSlogLevel(c.z); got != c.s {
			t.Errorf("zap %v -> %v, want %v", c.z, got, c.s)
		}
	}
}

func TestCoreWithFieldsAccumulate(t *testing.T) {
	cap := &capturingHandler{}
	logger := zap.New(NewCore(cap, slog.LevelDebug)).With(zap.String("svc", "ledger"))

	logger.Info("hi", zap.Int("n", 1))

	if len(cap.records) != 1 {
		t.Fatalf("want 1 record, got %d", len(cap.records))
	}
	attrs := recordAttrs(cap.records[0])
	if attrs["svc"] != "ledger" {
		t.Errorf("accumulated field svc = %v, want ledger", attrs["svc"])
	}
	if attrs["n"] != int64(1) {
		t.Errorf("call-site field n = %v, want int64(1)", attrs["n"])
	}
}

func TestCoreRespectsMinLevel(t *testing.T) {
	cap := &capturingHandler{}
	logger := zap.New(NewCore(cap, slog.LevelWarn))

	logger.Info("below threshold")
	logger.Warn("at threshold")

	if len(cap.records) != 1 {
		t.Fatalf("want only the Warn record, got %d", len(cap.records))
	}
	if cap.records[0].Message != "at threshold" {
		t.Errorf("forwarded wrong record: %q", cap.records[0].Message)
	}
}

// sinkLevel 決定 ducklog sink core 的最低等級:未設 -> Info,有設 -> 跟隨。
// 迴歸守衛:歷史上 Tee 寫死 Debug,導致 LOG_LEVEL=info 仍把 debug 灌進 VL。
func TestSinkLevel(t *testing.T) {
	if got := sinkLevel(client.RemoteConfig{}); got != slog.LevelInfo {
		t.Errorf("未設 Level 應預設 Info,got %v", got)
	}
	if got := sinkLevel(client.RemoteConfig{Level: slog.LevelWarn}); got != slog.LevelWarn {
		t.Errorf("有設 Level 應跟隨,got %v want Warn", got)
	}
}

// Tee 應回一個可用的 core + 非 nil 的 cleanup(建構不需要 VL 連線)。
func TestTeeReturnsCoreAndCleanup(t *testing.T) {
	base := zapcore.NewNopCore()
	core, stop := Tee(base, client.RemoteConfig{
		Endpoint: "http://127.0.0.1:1/insert/jsonline",
		Service:  "test",
	})
	if core == nil {
		t.Fatal("Tee 應回非 nil core")
	}
	if stop == nil {
		t.Fatal("Tee 應回非 nil cleanup")
	}
	// 用 tee 的 logger 記一筆不應 panic;stop 排空(VL 不可達 -> transport 靜默,靠 fallback)
	zap.New(core).Info("smoke")
	stop()
}
