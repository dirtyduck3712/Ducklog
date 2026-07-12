package client

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// RemoteConfig 設定 RemoteHandler。只有 Endpoint/APIKey/Service 通常必填,其餘有預設。
type RemoteConfig struct {
	Endpoint      string        // 例:http://logd:8080/ingest
	APIKey        string        // Bearer(ingest scope)
	Service       string        // 本服務名稱
	Level         slog.Leveler  // 最低等級,預設 Info
	BatchSize     int           // 預設 100
	QueueSize     int           // channel 容量,預設 10000
	FlushInterval time.Duration // 預設 1s
	// Fallback 是雙寫目的地,預設 os.Stdout。必須非阻塞/快速:
	// Handle 會在呼叫端 goroutine 上同步寫入,阻塞的 Fallback(如卡住的 pipe)
	// 會連帶阻塞 logging。os.Stdout 沒問題。
	Fallback   io.Writer
	HTTPClient *http.Client // 預設 timeout 2s
}

func (c RemoteConfig) withDefaults() RemoteConfig {
	if c.BatchSize == 0 {
		c.BatchSize = 100
	}
	if c.QueueSize == 0 {
		c.QueueSize = 10000
	}
	if c.FlushInterval == 0 {
		c.FlushInterval = time.Second
	}
	if c.Fallback == nil {
		c.Fallback = os.Stdout
	}
	if c.Level == nil {
		c.Level = slog.LevelInfo
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 2 * time.Second}
	}
	return c
}
