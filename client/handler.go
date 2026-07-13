package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// RemoteHandler 是把 slog record 送往 ducklog 的 slog.Handler。
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
	// 先放 WithAttrs 的,再放本次 record 的(record 覆蓋)。
	// 已知簡化:WithGroup 前加入的 attr 仍會帶上 group 前綴(攤平 wire 模型),
	// 且 WithGroup("") 不被視為 slog 的 no-op。
	prefix := ""
	if len(h.groups) > 0 {
		prefix = joinGroups(h.groups) + "."
	}
	for _, a := range h.attrs {
		flattenAttr(e.Attrs, prefix, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		flattenAttr(e.Attrs, prefix, a)
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
		h.fbMu.writeRaw([]byte(fmt.Sprintf(
			`{"_ducklog_client":"shutdown","dropped":%d}`+"\n", dropped)))
	}
	return nil
}

// flattenAttr 解析 slog.LogValuer(避免洩漏被 redact 的原值)並把 inline group
// 攤平成 "group.key" 形式的鍵。空鍵的非 group attr 依 slog 慣例略過。
func flattenAttr(dst map[string]any, prefix string, a slog.Attr) {
	v := a.Value.Resolve() // resolves LogValuer
	if v.Kind() == slog.KindGroup {
		g := v.Group()
		if a.Key != "" {
			prefix = prefix + a.Key + "."
		}
		for _, ga := range g {
			flattenAttr(dst, prefix, ga)
		}
		return
	}
	if a.Key == "" { // slog: empty-key non-group attr is elided
		return
	}
	dst[prefix+a.Key] = jsonSafe(v)
}

// jsonSafe 確保 attr 值可被 JSON 序列化。可序列化的原樣保留;不可序列化的
// (函式、channel、內含此類欄位的結構 —— 例如 pgconn.Config)轉成字串形式。
// 否則單一毒值會讓 encodeNDJSON 失敗、整批 log 被靜默丟棄。
func jsonSafe(v slog.Value) any {
	a := v.Any()
	if v.Kind() != slog.KindAny {
		return a // 基本型別(字串/數字/bool/時間/Duration)一定可序列化
	}
	if _, err := json.Marshal(a); err != nil {
		return fmt.Sprintf("%+v", a)
	}
	return a
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

func (f *fallbackWriter) writeRaw(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = f.w.Write(b)
}
