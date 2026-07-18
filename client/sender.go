package client

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxRetries      = 2
	breakerFailOpen = 5
	breakerOpenFor  = 30 * time.Second
	baseBackoff     = 100 * time.Millisecond

	closeDrainTimeout = 5 * time.Second
)

type sender struct {
	cfg     RemoteConfig
	queue   chan entry
	brk     *breaker
	sleep   func(time.Duration)
	dropCnt int64

	quit      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func newSender(cfg RemoteConfig, sleep func(time.Duration), now func() time.Time) *sender {
	return &sender{
		cfg:   cfg,
		queue: make(chan entry, cfg.QueueSize),
		brk:   newBreaker(breakerFailOpen, breakerOpenFor, now),
		sleep: sleep,
		quit:  make(chan struct{}),
	}
}

func (s *sender) start() {
	s.wg.Add(1)
	go s.run()
}

// enqueue 非阻塞。queue 滿 → 丟棄 + 計數,回傳 false。
func (s *sender) enqueue(e entry) bool {
	select {
	case s.queue <- e:
		return true
	default:
		atomic.AddInt64(&s.dropCnt, 1)
		return false
	}
}

func (s *sender) dropped() int64 { return atomic.LoadInt64(&s.dropCnt) }

func (s *sender) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()
	batch := make([]entry, 0, s.cfg.BatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.send(batch)
		batch = batch[:0]
	}

	for {
		select {
		case <-s.quit:
			// 排空 queue 後最後一次 flush
			for {
				select {
				case e := <-s.queue:
					batch = append(batch, e)
					if len(batch) >= s.cfg.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case e := <-s.queue:
			batch = append(batch, e)
			if len(batch) >= s.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// send 送一批,含熔斷檢查 + 有限重試指數退避。失敗只影響 HTTP;
// 因 handler 端已雙寫 stdout,這裡不再 fallback。
func (s *sender) send(batch []entry) {
	if !s.brk.allow() {
		return // 熔斷 open,跳過 HTTP(靠 stdout)
	}
	var body bytes.Buffer
	if err := encodeNDJSON(&body, batch); err != nil {
		return
	}
	payload := body.Bytes()
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			s.sleep(baseBackoff << (attempt - 1)) // 100ms, 200ms
		}
		if s.post(payload) {
			s.brk.onSuccess()
			return
		}
	}
	s.brk.onFailure()
}

func (s *sender) post(payload []byte) bool {
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, s.cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return false
	}
	if s.cfg.Username != "" {
		req.SetBasicAuth(s.cfg.Username, s.cfg.Password)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (s *sender) close() int64 {
	s.closeOnce.Do(func() { close(s.quit) })
	// 上限 5s 的排空:server 死掉且熔斷仍關閉時,排空 backlog 可能耗時
	// (retries × HTTP timeout),不應無限拖住 defer handler.Close()。
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(closeDrainTimeout):
	}
	return s.dropped()
}
