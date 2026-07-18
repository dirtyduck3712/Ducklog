// Package zapsink 把 zap 的 log 轉發到 ducklog 以 slog 為介面的傳輸層
// (最終進 VictoriaLogs),讓 zap-based 服務也能沿用 ducklog 的批次/重試/
// 熔斷/fallback 韌性。通用型 NewCore 只依賴 zap 與 stdlib;Tee 便利函式
// 額外接上 ducklog client,一行完成接入。
package zapsink

import (
	"context"
	"log/slog"

	"github.com/dirtyduck3712/ducklog/client"

	"go.uber.org/zap/zapcore"
)

// Core 是一個 zapcore.Core,把每筆 zap entry 翻成 slog.Record 交給底層 slog.Handler。
type Core struct {
	handler slog.Handler
	min     slog.Level
	fields  []zapcore.Field // 經由 With 累積
}

// NewCore 回傳一個轉發到 h 的 zapcore.Core,只轉發等級 >= min 的 entry。
// 通用:不綁 ducklog,測試可用任何 slog.Handler。
func NewCore(h slog.Handler, min slog.Level) *Core {
	return &Core{handler: h, min: min}
}

func (c *Core) Enabled(l zapcore.Level) bool {
	return zapToSlogLevel(l) >= c.min
}

func (c *Core) With(fields []zapcore.Field) zapcore.Core {
	merged := make([]zapcore.Field, 0, len(c.fields)+len(fields))
	merged = append(merged, c.fields...)
	merged = append(merged, fields...)
	return &Core{handler: c.handler, min: c.min, fields: merged}
}

func (c *Core) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *Core) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range c.fields {
		f.AddTo(enc)
	}
	for _, f := range fields {
		f.AddTo(enc)
	}
	rec := slog.NewRecord(ent.Time, zapToSlogLevel(ent.Level), ent.Message, 0)
	for k, v := range enc.Fields {
		rec.AddAttrs(slog.Any(k, v))
	}
	return c.handler.Handle(context.Background(), rec)
}

// Sync 無需動作:底層 slog.Handler 自有批次/排空(關閉時 Close 排空)。
func (c *Core) Sync() error { return nil }

// Tee 把 base 與一個由 cfg 建立的 ducklog sink 併成一個 core,並回傳排空用的
// cleanup func(關閉時呼叫以排空 transport queue)。ducklog core 依 cfg.Level 過濾
// (未設則預設 Info)。注意 zapcore.NewTee 對每個 sub-core 獨立評估 Enabled,base
// logger 的 atomic level 不會 gate 併排的 ducklog core,故此處必須自帶等級過濾。
//
//	logger := zap.New(...)  // 或用 zapCfg.Build(zap.WrapCore(...))
//	core, stop := zapsink.Tee(baseCore, client.RemoteConfig{
//	    Endpoint: vlURL + "/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service",
//	    Service:  "my-service",
//	    Level:    slog.LevelInfo,
//	})
//	defer stop()
func Tee(base zapcore.Core, cfg client.RemoteConfig) (zapcore.Core, func()) {
	h := client.NewRemoteHandler(cfg)
	return zapcore.NewTee(base, NewCore(h, sinkLevel(cfg))), func() { _ = h.Close() }
}

// sinkLevel 取 cfg 設定的最低等級,未設則預設 Info(與 client.RemoteHandler 一致)。
func sinkLevel(cfg client.RemoteConfig) slog.Level {
	if cfg.Level != nil {
		return cfg.Level.Level()
	}
	return slog.LevelInfo
}

// zapToSlogLevel 把 zap 等級對映到 slog 等級(zap 間距 1、slog 間距 4,故 ×4)。
func zapToSlogLevel(l zapcore.Level) slog.Level {
	return slog.Level(int(l) * 4)
}
