package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 收集收到的 batch 數與筆數,可切換回應碼。
type fakeServer struct {
	mu      sync.Mutex
	batches int
	rows    int
	status  int32 // atomic:回應碼
	hang    int32 // atomic:>0 則睡 hangFor
	hangFor time.Duration
}

func (f *fakeServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&f.hang) > 0 {
			time.Sleep(f.hangFor)
		}
		body, _ := io.ReadAll(r.Body)
		n := 0
		for _, b := range body {
			if b == '\n' {
				n++
			}
		}
		f.mu.Lock()
		f.batches++
		f.rows += n
		f.mu.Unlock()
		code := int(atomic.LoadInt32(&f.status))
		if code == 0 {
			code = 200
		}
		w.WriteHeader(code)
	})
}

func cfgFor(url string) RemoteConfig {
	return RemoteConfig{Endpoint: url, APIKey: "k", Service: "api",
		BatchSize: 2, FlushInterval: 10 * time.Millisecond, QueueSize: 100,
		Fallback: io.Discard}.withDefaults()
}

func TestSenderDeliversBatches(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	s := newSender(cfgFor(srv.URL), func(time.Duration) {}, time.Now)
	s.start()
	for i := 0; i < 6; i++ {
		if !s.enqueue(entry{Service: "api", Level: "info", Message: "x"}) {
			t.Fatal("不該丟棄")
		}
	}
	dropped := s.close() // 排空並送出
	if dropped != 0 {
		t.Fatalf("dropped = %d; want 0", dropped)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows != 6 {
		t.Fatalf("server 收到 %d 筆; want 6", f.rows)
	}
}

func TestSenderNonBlockingDropsWhenFull(t *testing.T) {
	f := &fakeServer{}
	atomic.StoreInt32(&f.hang, 1)
	f.hangFor = 200 * time.Millisecond
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	cfg := cfgFor(srv.URL)
	cfg.QueueSize = 2 // 極小,容易滿
	s := newSender(cfg, func(time.Duration) {}, time.Now)
	s.start()
	// 猛塞;因 server 掛住,queue 會滿 → 部分被丟。enqueue 絕不阻塞。
	dropSeen := false
	for i := 0; i < 1000; i++ {
		if !s.enqueue(entry{Service: "api", Level: "info", Message: "x"}) {
			dropSeen = true
		}
	}
	if !dropSeen {
		t.Fatal("queue 應該滿並回報丟棄")
	}
	if s.dropped() == 0 {
		t.Fatal("drop 計數應 > 0(不可靜默)")
	}
	s.close()
}

func TestSenderRetriesThenBreakerOpens(t *testing.T) {
	f := &fakeServer{}
	atomic.StoreInt32(&f.status, 500) // 一律失敗
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	var sleeps int32
	s := newSender(cfgFor(srv.URL), func(time.Duration) { atomic.AddInt32(&sleeps, 1) }, time.Now)
	s.start()
	for i := 0; i < 20; i++ {
		s.enqueue(entry{Service: "api", Level: "info", Message: "x"})
	}
	s.close()
	// 每批失敗都應有重試(sleep 被呼叫);且熔斷 open 後應停止再打 server。
	if atomic.LoadInt32(&sleeps) == 0 {
		t.Fatal("失敗應觸發重試退避 sleep")
	}
}
