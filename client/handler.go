package client

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// RemoteHandler 是把 slog record 送往 docklog 的 slog.Handler。
// Handle 永不阻塞:同時雙寫 Fallback(stdout)與非阻塞入 sender queue。
type RemoteHandler struct {
	cfg    RemoteConfig
	snd    *sender
	fbMu   *fallbackWriter
	attrs  []slog.Attr // WithAttrs 累積
	groups []string    // WithGroup 累積
}

// NewRemoteHandler 建立 handler 並啟動背景 sender。用完要 Close。
func NewRemoteHandler(cfg RemoteConfig) *RemoteHandler {
	cfg = cfg.withDefaults()
	s := newSender(cfg, time.Sleep, time.Now)
	s.start()
	return &RemoteHandler{cfg: cfg, snd: s, fbMu: &fallbackWriter{w: cfg.Fallback}}
}

func (h *RemoteHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.cfg.Level.Level()
}

func (h *RemoteHandler) Handle(ctx context.Context, r slog.Record) error {
	e := entry{
		TS:      r.Time.UTC().Format(time.RFC3339Nano),
		Service: h.cfg.Service,
		Level:   levelString(r.Level),
		Message: r.Message,
		Attrs:   map[string]any{},
	}
	if id, ok := TraceIDFromContext(ctx); ok {
		e.TraceID = id
	}
	// 先放 WithAttrs 的,再放本次 record 的(record 覆蓋)
	prefix := ""
	if len(h.groups) > 0 {
		prefix = joinGroups(h.groups) + "."
	}
	for _, a := range h.attrs {
		e.Attrs[prefix+a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		e.Attrs[prefix+a.Key] = a.Value.Any()
		return true
	})

	// 雙寫:先 stdout(永遠成功的安全網),再非阻塞入 queue。
	h.fbMu.writeLine(e)
	h.snd.enqueue(e)
	return nil
}

func (h *RemoteHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &nh
}

func (h *RemoteHandler) WithGroup(name string) slog.Handler {
	nh := *h
	nh.groups = append(append([]string{}, h.groups...), name)
	return &nh
}

// Close 排空 sender 並回報丟棄數(不靜默)。
func (h *RemoteHandler) Close() error {
	dropped := h.snd.close()
	if dropped > 0 {
		fmt.Fprintf(h.cfg.Fallback,
			`{"_docklog_client":"shutdown","dropped":%d}`+"\n", dropped)
	}
	return nil
}

func joinGroups(g []string) string {
	out := g[0]
	for _, s := range g[1:] {
		out += "." + s
	}
	return out
}

// fallbackWriter 序列化雙寫,避免並行 goroutine 交錯輸出。
type fallbackWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (f *fallbackWriter) writeLine(e entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = encodeNDJSON(f.w, []entry{e})
}
