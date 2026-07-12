package client

import (
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestWithDefaults(t *testing.T) {
	c := RemoteConfig{Endpoint: "http://x/ingest", Service: "api"}.withDefaults()
	if c.BatchSize != 100 || c.QueueSize != 10000 || c.FlushInterval != time.Second {
		t.Fatalf("batch/queue/flush 預設錯: %+v", c)
	}
	if c.Fallback != os.Stdout {
		t.Fatal("Fallback 應預設 os.Stdout")
	}
	if c.HTTPClient == nil || c.HTTPClient.Timeout != 2*time.Second {
		t.Fatal("HTTPClient 應預設 timeout 2s")
	}
	if c.Level == nil || c.Level.Level() != slog.LevelInfo {
		t.Fatal("Level 應預設 Info")
	}
}

func TestWithDefaultsKeepsProvided(t *testing.T) {
	c := RemoteConfig{BatchSize: 50, FlushInterval: 5 * time.Second}.withDefaults()
	if c.BatchSize != 50 || c.FlushInterval != 5*time.Second {
		t.Fatalf("已提供的值不該被覆蓋: %+v", c)
	}
}
