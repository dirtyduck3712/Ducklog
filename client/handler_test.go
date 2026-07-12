package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func collectServer() (*httptest.Server, *bytes.Buffer, *sync.Mutex) {
	var mu sync.Mutex
	buf := &bytes.Buffer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		io.Copy(buf, r.Body)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	return srv, buf, &mu
}

func TestHandlerDualWritesAndTrace(t *testing.T) {
	srv, srvBuf, mu := collectServer()
	defer srv.Close()
	var fallback bytes.Buffer
	h := NewRemoteHandler(RemoteConfig{
		Endpoint: srv.URL, APIKey: "k", Service: "api",
		BatchSize: 1, FlushInterval: 5 * time.Millisecond, Fallback: &fallback,
	})
	log := slog.New(h)
	ctx := ContextWithTraceID(context.Background(), "0af76519-16cd-43dd-8448-eb211c80319c")
	log.ErrorContext(ctx, "boom", "user", 42)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// fallback(stdout 雙寫)應有這筆
	if !strings.Contains(fallback.String(), "boom") {
		t.Fatalf("fallback 缺 log: %s", fallback.String())
	}
	// server 應收到,且含 trace_id + level=error + attrs.user
	mu.Lock()
	got := srvBuf.String()
	mu.Unlock()
	for _, want := range []string{`"level":"error"`, `"trace_id":"0af76519`, `"message":"boom"`, `"user":42`} {
		if !strings.Contains(got, want) {
			t.Fatalf("server payload 缺 %q: %s", want, got)
		}
	}
}

func TestHandlerEnabled(t *testing.T) {
	h := NewRemoteHandler(RemoteConfig{Endpoint: "http://x", Service: "api",
		Level: slog.LevelWarn, Fallback: io.Discard})
	defer h.Close()
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("Info < Warn 應 disabled")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("Error >= Warn 應 enabled")
	}
}

func TestHandlerWithAttrs(t *testing.T) {
	srv, srvBuf, mu := collectServer()
	defer srv.Close()
	h := NewRemoteHandler(RemoteConfig{Endpoint: srv.URL, Service: "api",
		BatchSize: 1, FlushInterval: 5 * time.Millisecond, Fallback: io.Discard})
	log := slog.New(h).With("region", "tw")
	log.Info("hi")
	h.Close()
	mu.Lock()
	got := srvBuf.String()
	mu.Unlock()
	if !strings.Contains(got, `"region":"tw"`) {
		t.Fatalf("WithAttrs 的 region 沒帶上: %s", got)
	}
}

type redactedValue struct{ raw string }

func (redactedValue) LogValue() slog.Value { return slog.StringValue("REDACTED") }

func TestHandlerResolvesLogValuer(t *testing.T) {
	srv, srvBuf, mu := collectServer()
	defer srv.Close()
	h := NewRemoteHandler(RemoteConfig{Endpoint: srv.URL, Service: "api",
		BatchSize: 1, FlushInterval: 5 * time.Millisecond, Fallback: io.Discard})
	log := slog.New(h)
	log.Info("secret", "password", redactedValue{raw: "hunter2"})
	h.Close()
	mu.Lock()
	got := srvBuf.String()
	mu.Unlock()
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("LogValuer 沒被 resolve: %s", got)
	}
	if strings.Contains(got, "hunter2") {
		t.Fatalf("原值洩漏了: %s", got)
	}
}

func TestHandlerFlattensInlineGroup(t *testing.T) {
	srv, srvBuf, mu := collectServer()
	defer srv.Close()
	h := NewRemoteHandler(RemoteConfig{Endpoint: srv.URL, Service: "api",
		BatchSize: 1, FlushInterval: 5 * time.Millisecond, Fallback: io.Discard})
	log := slog.New(h)
	log.Info("req", slog.Group("req", slog.String("method", "GET"), slog.Int("status", 200)))
	h.Close()
	mu.Lock()
	got := srvBuf.String()
	mu.Unlock()
	for _, want := range []string{`"req.method":"GET"`, `"req.status":200`} {
		if !strings.Contains(got, want) {
			t.Fatalf("inline group 沒攤平,缺 %q: %s", want, got)
		}
	}
}

type blockingRoundTripper struct{ release chan struct{} }

func (rt blockingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// 一直卡住直到測試放行或 request context 取消,模擬掛死的 server。
	select {
	case <-rt.release:
	case <-req.Context().Done():
	}
	return nil, errors.New("blocked")
}

func TestCloseBoundedWhenServerHangs(t *testing.T) {
	// endpoint 掛死(POST 永遠卡住),排空 backlog 本應遠超過 5s 上限;
	// Close 必須被 closeDrainTimeout 約束而在 ~5s 內返回。
	release := make(chan struct{})
	defer close(release) // 測試結束放行背景 goroutine,避免洩漏
	h := NewRemoteHandler(RemoteConfig{Endpoint: "http://logd.invalid", Service: "api",
		BatchSize: 1, FlushInterval: 5 * time.Millisecond, Fallback: io.Discard,
		HTTPClient: &http.Client{Transport: blockingRoundTripper{release: release}}})
	log := slog.New(h)
	for i := 0; i < 10; i++ {
		log.Info("backlog")
	}
	start := time.Now()
	h.Close()
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Fatalf("Close 沒被上限約束,耗時 %v", elapsed)
	}
}

func TestDoubleCloseIsSafe(t *testing.T) {
	h := NewRemoteHandler(RemoteConfig{Endpoint: "http://x", Service: "api",
		Fallback: io.Discard})
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	// 第二次 Close 不應 panic(close of closed channel)。
	if err := h.Close(); err != nil {
		t.Fatalf("second Close 回傳 err: %v", err)
	}
}

func TestHandlerNeverBlocks(t *testing.T) {
	// endpoint 掛住 + queue 極小;大量 log 不應阻塞呼叫端。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()
	h := NewRemoteHandler(RemoteConfig{Endpoint: srv.URL, Service: "api",
		QueueSize: 1, BatchSize: 1, Fallback: io.Discard})
	log := slog.New(h)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			log.Info("flood")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("logging 阻塞了呼叫端")
	}
	h.Close()
}
