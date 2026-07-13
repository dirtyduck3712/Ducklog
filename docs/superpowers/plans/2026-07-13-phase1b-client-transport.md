# Phase 1b — Go Client Transport Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 一個純 stdlib 的 Go `slog.Handler`,讓任何 Go 服務用標準 `log/slog` 把 log 可靠送進 ducklog server —— 日誌系統掛掉絕不拖垮應用 —— 外加 trace ID 傳遞。

**Architecture:** 獨立模組 `client/`(零外部相依)。`RemoteHandler` 實作 `slog.Handler`:`Handle` 非阻塞入 queue 並同時雙寫 stdout;背景 `sender` goroutine 批次 POST 到 `/ingest`,含 2s timeout、有限重試、熔斷。trace ID 用 `context.Context` 傳遞,HTTP middleware 讀/生成、outbound helper 跨服務延續。

**Tech Stack:** Go 1.22,純標準庫(`net/http`, `log/slog`, `context`, `crypto/rand`, `encoding/json`, `sync`, `sync/atomic`, `time`, `bufio`, `regexp`)。**無 CGO、無外部相依、不需 GOTOOLCHAIN override。**

**Scope:** 只做 Go transport + Go trace ID(`tech.md` items 5–6 的 Go 半邊)。Node pino transport = Phase 1c。設計依據:`docs/superpowers/specs/2026-07-13-phase1b-client-transport-design.md`。

## Global Constraints

- **獨立模組**:`client/` 有自己的 `go.mod`,module path `ducklog/client`,`go 1.22`。**零外部相依**(只用 stdlib)。不 import 任何 `ducklog/internal/...`。
- **Build/test**:在 `client/` 目錄用**普通 `go test ./...`**(本機 go 1.22.2),**不需** `GOTOOLCHAIN=go1.24.0`(沒有 go-duckdb)。唯一例外:選配的端到端測試(Task 7)要 build server binary,那時才需 `GOTOOLCHAIN=go1.24.0`。
- **Wire 契約**(對齊 server `/ingest`,NDJSON 每行一筆):`{"ts","service","level","trace_id","message","attrs"}`。`ts`=RFC3339 UTC;`level`=小寫 `debug|info|warn|error`;`trace_id`=canonical UUID(或省略);`attrs`=JSON object。
- **失敗模式 #6 五要件**(硬需求):(1) 非阻塞 —— queue 滿就丟、絕不阻塞呼叫端;(2) HTTP timeout 2s;(3) 有限重試 —— 單批最多 2 次、指數退避;(4) 熔斷 —— 連續 5 次失敗→open,30s→half-open 探測;(5) fallback —— **第一版 stdout + HTTP 雙寫**。
- **絕不靜默**:丟棄的筆數要計數,`Close` 時(或有丟棄時)回報,不可靜默。
- **level 對應**:`slog.Level` < Info → `debug`;< Warn → `info`;< Error → `warn`;≥ Error → `error`。
- **trace_id 生成 canonical UUIDv4**(`crypto/rand`),因 server 是 UUID 欄位。
- **graceful shutdown**:`Close()` 排空 queue、送出最後一批(有上限 timeout)、回報 drop 數。
- **時間/退避可注入**:退避 sleep 與熔斷計時透過注入的 `now func() time.Time` / `sleep func(time.Duration)`,測試 deterministic,不靠 wall-clock。
- **DRY / YAGNI**:不做 Node、OTel、可關雙寫、壓縮、動態 config。

---

## File Structure

```
client/go.mod                     module ducklog/client, go 1.22
client/wire.go                    entry struct、ndjson 編碼、slog.Level→level 字串、attrs 攤平
client/wire_test.go
client/config.go                  RemoteConfig + applyDefaults
client/config_test.go
client/trace.go                   NewTraceID(canonical UUIDv4)、context 存取、isCanonicalUUID
client/trace_test.go
client/breaker.go                 circuit breaker(注入 clock)
client/breaker_test.go
client/sender.go                  背景批次送出:enqueue(非阻塞)+ run(批次/重試/熔斷)+ close(排空)+ drop 計數
client/sender_test.go
client/handler.go                 RemoteHandler 實作 slog.Handler + 雙寫 + Close
client/handler_test.go
client/middleware.go              HTTP middleware + InjectTraceID(outbound)
client/middleware_test.go
client/e2e_test.go                (選配)對真 server binary 的端到端測試,-short 可略
```

決策邊界:`sender` 擁有 queue、非阻塞 enqueue、drop 計數、背景送出;`handler` 只管 slog plumbing + 雙寫,委派給 sender。`trace`/`breaker` 各自獨立可測。

---

## Task 0: 模組骨架 + wire 型別

**Files:**
- Create: `client/go.mod`, `client/wire.go`, `client/wire_test.go`

**Interfaces:**
- Produces:
  - `type entry struct{ TS, Service, Level, TraceID, Message string; Attrs map[string]any }`(internal;JSON tag 對齊 wire;`trace_id` 空字串時 omitempty)
  - `func levelString(l slog.Level) string`
  - `func encodeNDJSON(w io.Writer, entries []entry) error`(每筆一行 JSON + `\n`)

- [ ] **Step 1: 建模組**

```bash
cd /home/dva/workspace/ducklog/client
go mod init ducklog/client   # 產生 go.mod;若已存在則略過
```
確認 `client/go.mod` 內容為:
```
module ducklog/client

go 1.22
```
(純 stdlib,無 require 區塊。)

- [ ] **Step 2: 寫失敗測試**

`client/wire_test.go`:
```go
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
		slog.LevelInfo + 1: "info",  // Info<x<Warn → info
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
```

- [ ] **Step 3: 執行確認失敗**

Run: `cd client && go test ./...`
Expected: FAIL(`entry` / `levelString` / `encodeNDJSON` 未定義)。

- [ ] **Step 4: 實作 wire.go**

`client/wire.go`:
```go
// Package client 是 ducklog 的純 stdlib Go 傳輸層:一個 slog.Handler + trace ID 傳遞。
package client

import (
	"encoding/json"
	"io"
	"log/slog"
)

// entry 是送往 server /ingest 的一行 wire JSON。
type entry struct {
	TS      string         `json:"ts"`
	Service string         `json:"service"`
	Level   string         `json:"level"`
	TraceID string         `json:"trace_id,omitempty"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// levelString 把 slog.Level 對應到 ducklog 的小寫等級字串。
func levelString(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}

// encodeNDJSON 把 entries 寫成每行一筆的 NDJSON。
func encodeNDJSON(w io.Writer, entries []entry) error {
	enc := json.NewEncoder(w) // Encode 會自動補換行
	for i := range entries {
		if err := enc.Encode(&entries[i]); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 5: 執行確認通過**

Run: `cd client && go test ./...`
Expected: PASS。

- [ ] **Step 6: Commit**

```bash
cd /home/dva/workspace/ducklog
git add client/go.mod client/wire.go client/wire_test.go
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "feat(client): module scaffold + wire types"
```

---

## Task 1: Trace ID 生成與 context 存取

**Files:**
- Create: `client/trace.go`, `client/trace_test.go`

**Interfaces:**
- Produces:
  - `func NewTraceID() string` — canonical UUIDv4
  - `func isCanonicalUUID(s string) bool`
  - `func ContextWithTraceID(ctx context.Context, id string) context.Context`
  - `func TraceIDFromContext(ctx context.Context) (string, bool)`

- [ ] **Step 1: 寫失敗測試**

`client/trace_test.go`:
```go
package client

import (
	"context"
	"testing"
)

func TestNewTraceIDIsCanonical(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := NewTraceID()
		if !isCanonicalUUID(id) {
			t.Fatalf("NewTraceID 產生非 canonical UUID: %q", id)
		}
	}
	// 兩次不應相同
	if NewTraceID() == NewTraceID() {
		t.Fatal("NewTraceID 不該重複")
	}
}

func TestIsCanonicalUUID(t *testing.T) {
	good := "0af76519-16cd-43dd-8448-eb211c80319c"
	bad := []string{"", "not-a-uuid", "0af7651916cd43dd8448eb211c80319c",
		"{0af76519-16cd-43dd-8448-eb211c80319c}", "0af76519-16cd-43dd-8448-eb211c80319"}
	if !isCanonicalUUID(good) {
		t.Fatalf("%q 應為 canonical", good)
	}
	for _, b := range bad {
		if isCanonicalUUID(b) {
			t.Fatalf("%q 不該被當 canonical", b)
		}
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := ContextWithTraceID(context.Background(), "abc")
	if id, ok := TraceIDFromContext(ctx); !ok || id != "abc" {
		t.Fatalf("round trip = %q,%v; want abc,true", id, ok)
	}
	if _, ok := TraceIDFromContext(context.Background()); ok {
		t.Fatal("空 context 不應有 trace id")
	}
}
```

- [ ] **Step 2: 執行確認失敗**

Run: `cd client && go test -run 'Trace|UUID' ./...`
Expected: FAIL。

- [ ] **Step 3: 實作 trace.go**

`client/trace.go`:
```go
package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

// NewTraceID 用 crypto/rand 產生 canonical 的 UUIDv4 字串。
func NewTraceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

var canonicalUUIDRe = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isCanonicalUUID 檢查是否為 8-4-4-4-12 十六進位格式。
func isCanonicalUUID(s string) bool { return canonicalUUIDRe.MatchString(s) }

type traceIDKey struct{}

func ContextWithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, id)
}

func TraceIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(traceIDKey{}).(string)
	return id, ok && id != ""
}
```

- [ ] **Step 4: 執行確認通過**

Run: `cd client && go test -run 'Trace|UUID' ./...`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
cd /home/dva/workspace/ducklog
git add client/trace.go client/trace_test.go
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "feat(client): trace id generation + context propagation"
```

---

## Task 2: Circuit breaker(注入 clock)

**Files:**
- Create: `client/breaker.go`, `client/breaker_test.go`

**Interfaces:**
- Produces:
  - `type breaker struct{...}`
  - `func newBreaker(failThreshold int, openFor time.Duration, now func() time.Time) *breaker`
  - `func (*breaker) allow() bool` — open 且未到 half-open 時間 → false
  - `func (*breaker) onSuccess()` / `func (*breaker) onFailure()`

- [ ] **Step 1: 寫失敗測試**

`client/breaker_test.go`:
```go
package client

import (
	"testing"
	"time"
)

func TestBreakerOpensAfterThreshold(t *testing.T) {
	now := time.Unix(0, 0)
	b := newBreaker(5, 30*time.Second, func() time.Time { return now })
	for i := 0; i < 4; i++ {
		b.onFailure()
		if !b.allow() {
			t.Fatalf("第 %d 次失敗後不該 open", i+1)
		}
	}
	b.onFailure() // 第 5 次
	if b.allow() {
		t.Fatal("連續 5 次失敗後應 open")
	}
}

func TestBreakerHalfOpenAfterTimeout(t *testing.T) {
	now := time.Unix(0, 0)
	b := newBreaker(5, 30*time.Second, func() time.Time { return now })
	for i := 0; i < 5; i++ {
		b.onFailure()
	}
	if b.allow() {
		t.Fatal("剛 open 應擋")
	}
	now = now.Add(31 * time.Second) // 過了 open 期
	if !b.allow() {
		t.Fatal("30s 後應允許 half-open 探測")
	}
	b.onSuccess() // 探測成功 → close
	now = now.Add(time.Second)
	if !b.allow() {
		t.Fatal("成功後應 close(恆允許)")
	}
}
```

- [ ] **Step 2: 執行確認失敗**

Run: `cd client && go test -run Breaker ./...`
Expected: FAIL。

- [ ] **Step 3: 實作 breaker.go**

`client/breaker.go`:
```go
package client

import (
	"sync"
	"time"
)

// breaker 是簡單的連續失敗熔斷器。open 期間 allow() 回 false;
// 過了 openFor 後允許一次 half-open 探測,由 onSuccess/onFailure 決定 close 或續 open。
type breaker struct {
	mu            sync.Mutex
	failThreshold int
	openFor       time.Duration
	now           func() time.Time
	consecutive   int
	openedAt      time.Time // 零值代表未 open
}

func newBreaker(failThreshold int, openFor time.Duration, now func() time.Time) *breaker {
	return &breaker{failThreshold: failThreshold, openFor: openFor, now: now}
}

func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openedAt.IsZero() {
		return true // closed
	}
	// open:過了 openFor 才允許一次探測
	return b.now().Sub(b.openedAt) >= b.openFor
}

func (b *breaker) onSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
	b.openedAt = time.Time{}
}

func (b *breaker) onFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive++
	if b.consecutive >= b.failThreshold {
		b.openedAt = b.now() // (重新)標記 open 起點,half-open 探測失敗會刷新
	}
}
```

- [ ] **Step 4: 執行確認通過**

Run: `cd client && go test -run Breaker ./...`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
cd /home/dva/workspace/ducklog
git add client/breaker.go client/breaker_test.go
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "feat(client): consecutive-failure circuit breaker"
```

---

## Task 3: RemoteConfig + 預設值

**Files:**
- Create: `client/config.go`, `client/config_test.go`

**Interfaces:**
- Produces:
  - `type RemoteConfig struct{ Endpoint, APIKey, Service string; Level slog.Leveler; BatchSize, QueueSize int; FlushInterval time.Duration; Fallback io.Writer; HTTPClient *http.Client }`
  - `func (RemoteConfig) withDefaults() RemoteConfig` — 補上 BatchSize=100, QueueSize=10000, FlushInterval=1s, Fallback=os.Stdout, Level=Info, HTTPClient timeout=2s

- [ ] **Step 1: 寫失敗測試**

`client/config_test.go`:
```go
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
```

- [ ] **Step 2: 執行確認失敗**

Run: `cd client && go test -run Defaults ./...`
Expected: FAIL。

- [ ] **Step 3: 實作 config.go**

`client/config.go`:
```go
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
	Fallback      io.Writer     // 雙寫目的地,預設 os.Stdout
	HTTPClient    *http.Client  // 預設 timeout 2s
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
```

- [ ] **Step 4: 執行確認通過**

Run: `cd client && go test -run Defaults ./...`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
cd /home/dva/workspace/ducklog
git add client/config.go client/config_test.go
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "feat(client): RemoteConfig with defaults"
```

---

## Task 4: Sender —— 非阻塞 enqueue + 背景批次送出 + 重試 + 熔斷 + drop 計數

**Files:**
- Create: `client/sender.go`, `client/sender_test.go`

**Interfaces:**
- Consumes: `entry`, `RemoteConfig`, `breaker`, `encodeNDJSON`
- Produces:
  - `func newSender(cfg RemoteConfig, sleep func(time.Duration), now func() time.Time) *sender`
  - `func (*sender) start()` — 啟動背景 goroutine
  - `func (*sender) enqueue(e entry) bool` — 非阻塞;queue 滿回 false 並累計 drop
  - `func (*sender) close()` — 停止收新、排空、送出、回傳 `dropped int64`
  - `func (*sender) dropped() int64`
  - 內部:單批最多 2 次重試、指數退避(用注入的 `sleep`);每批送出走 `breaker.allow()`;送出結果回饋 `onSuccess/onFailure`。

- [ ] **Step 1: 寫失敗測試**

`client/sender_test.go`:
```go
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
	mu       sync.Mutex
	batches  int
	rows     int
	status   int32 // atomic:回應碼
	hang     int32 // atomic:>0 則睡 hangFor
	hangFor  time.Duration
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
```

- [ ] **Step 2: 執行確認失敗**

Run: `cd client && go test -run Sender ./...`
Expected: FAIL(`newSender` 未定義)。

- [ ] **Step 3: 實作 sender.go**

`client/sender.go`:
```go
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
)

type sender struct {
	cfg     RemoteConfig
	queue   chan entry
	brk     *breaker
	sleep   func(time.Duration)
	dropCnt int64

	quit chan struct{}
	wg   sync.WaitGroup
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
	req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (s *sender) close() int64 {
	close(s.quit)
	s.wg.Wait()
	return s.dropped()
}
```

- [ ] **Step 4: 執行確認通過**

Run: `cd client && go test -run Sender ./...`
Expected: PASS(3 個 sender 測試)。

- [ ] **Step 5: Commit**

```bash
cd /home/dva/workspace/ducklog
git add client/sender.go client/sender_test.go
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "feat(client): background batch sender with retry, breaker, non-blocking drop"
```

---

## Task 5: RemoteHandler —— slog.Handler + 雙寫 + Close

**Files:**
- Create: `client/handler.go`, `client/handler_test.go`

**Interfaces:**
- Consumes: `sender`, `RemoteConfig`, `entry`, `levelString`, `TraceIDFromContext`
- Produces:
  - `func NewRemoteHandler(cfg RemoteConfig) *RemoteHandler`
  - `RemoteHandler` 實作 `slog.Handler`(`Enabled`, `Handle`, `WithAttrs`, `WithGroup`)
  - `func (*RemoteHandler) Close() error` — 排空 sender、若有 drop 寫一行摘要到 Fallback
  - 行為:`Handle` 建 entry(ts=record.Time.UTC RFC3339、level 字串、attrs 攤平含 WithAttrs/group 前綴)、trace_id 從 ctx、**雙寫 Fallback + 非阻塞 enqueue**,永不阻塞。

- [ ] **Step 1: 寫失敗測試**

`client/handler_test.go`:
```go
package client

import (
	"bytes"
	"context"
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
```

- [ ] **Step 2: 執行確認失敗**

Run: `cd client && go test -run Handler ./...`
Expected: FAIL。

- [ ] **Step 3: 實作 handler.go**

`client/handler.go`:
```go
package client

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// RemoteHandler 是把 slog record 送往 ducklog 的 slog.Handler。
// Handle 永不阻塞:同時雙寫 Fallback(stdout)與非阻塞入 sender queue。
type RemoteHandler struct {
	cfg      RemoteConfig
	snd      *sender
	fbMu     *fallbackWriter
	attrs    []slog.Attr // WithAttrs 累積
	groups   []string    // WithGroup 累積
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
			`{"_ducklog_client":"shutdown","dropped":%d}`+"\n", dropped)
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
```

`client/handler.go`(續 — fallback writer,序列化雙寫避免交錯):
```go
import "sync"

type fallbackWriter struct {
	mu sync.Mutex
	w  interface{ Write([]byte) (int, error) }
}

func (f *fallbackWriter) writeLine(e entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = encodeNDJSON(f.w, []entry{e})
}
```
> 註:把上面兩段合進單一 `handler.go`,`import` 區塊合併(`context`, `fmt`, `log/slog`, `sync`, `time`)。`fallbackWriter.w` 用 `io.Writer` 亦可,這裡用內嵌介面等同 `io.Writer`;實作時直接用 `io.Writer` 型別並 `import "io"`。

- [ ] **Step 4: 執行確認通過**

Run: `cd client && go test -run Handler ./...`
Expected: PASS(4 個 handler 測試)。

- [ ] **Step 5: Commit**

```bash
cd /home/dva/workspace/ducklog
git add client/handler.go client/handler_test.go
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "feat(client): slog.Handler with dual-write, trace, non-blocking, Close"
```

---

## Task 6: Trace ID HTTP middleware + outbound 傳遞

**Files:**
- Create: `client/middleware.go`, `client/middleware_test.go`

**Interfaces:**
- Consumes: `NewTraceID`, `isCanonicalUUID`, `ContextWithTraceID`, `TraceIDFromContext`
- Produces:
  - `const TraceHeader = "X-Trace-Id"`
  - `func Middleware(next http.Handler) http.Handler` — 讀入站 header(不合法就生新的)、塞 context、回應也帶 header
  - `func InjectTraceID(req *http.Request, ctx context.Context)` — 把 ctx 的 trace_id 設到 outbound 請求 header

- [ ] **Step 1: 寫失敗測試**

`client/middleware_test.go`:
```go
package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareUsesInboundValidHeader(t *testing.T) {
	var seen string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := TraceIDFromContext(r.Context())
		seen = id
	}))
	req := httptest.NewRequest("GET", "/", nil)
	valid := "0af76519-16cd-43dd-8448-eb211c80319c"
	req.Header.Set(TraceHeader, valid)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if seen != valid {
		t.Fatalf("應沿用入站 trace id %q, got %q", valid, seen)
	}
	if rr.Header().Get(TraceHeader) != valid {
		t.Fatal("回應也應帶 trace id")
	}
}

func TestMiddlewareGeneratesWhenMissingOrInvalid(t *testing.T) {
	for _, in := range []string{"", "garbage"} {
		var seen string
		h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen, _ = TraceIDFromContext(r.Context())
		}))
		req := httptest.NewRequest("GET", "/", nil)
		if in != "" {
			req.Header.Set(TraceHeader, in)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if !isCanonicalUUID(seen) {
			t.Fatalf("入站 %q 時應生成 canonical trace id, got %q", in, seen)
		}
	}
}

func TestInjectTraceID(t *testing.T) {
	ctx := ContextWithTraceID(context.Background(), "abc-123")
	req, _ := http.NewRequest("GET", "http://x", nil)
	InjectTraceID(req, ctx)
	if req.Header.Get(TraceHeader) != "abc-123" {
		t.Fatalf("outbound header = %q; want abc-123", req.Header.Get(TraceHeader))
	}
	// ctx 無 trace id 時不設 header
	req2, _ := http.NewRequest("GET", "http://x", nil)
	InjectTraceID(req2, context.Background())
	if req2.Header.Get(TraceHeader) != "" {
		t.Fatal("無 trace id 不該設 header")
	}
}
```

- [ ] **Step 2: 執行確認失敗**

Run: `cd client && go test -run 'Middleware|Inject' ./...`
Expected: FAIL。

- [ ] **Step 3: 實作 middleware.go**

`client/middleware.go`:
```go
package client

import (
	"context"
	"net/http"
)

const TraceHeader = "X-Trace-Id"

// Middleware 讀入站 X-Trace-Id(不合法或缺少就生成 canonical UUID),
// 塞進 request context,並在回應帶上同一個 id 方便除錯。
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(TraceHeader)
		if !isCanonicalUUID(id) {
			id = NewTraceID()
		}
		w.Header().Set(TraceHeader, id)
		next.ServeHTTP(w, r.WithContext(ContextWithTraceID(r.Context(), id)))
	})
}

// InjectTraceID 把 ctx 裡的 trace id 設到 outbound 請求,讓 trace 跨服務延續。
func InjectTraceID(req *http.Request, ctx context.Context) {
	if id, ok := TraceIDFromContext(ctx); ok {
		req.Header.Set(TraceHeader, id)
	}
}
```

- [ ] **Step 4: 執行確認通過**

Run: `cd client && go test -run 'Middleware|Inject' ./...`
Expected: PASS。

- [ ] **Step 5: 全套件 + race**

Run: `cd client && go test ./... -race`
Expected: 全部 PASS,無 race。

- [ ] **Step 6: Commit**

```bash
cd /home/dva/workspace/ducklog
git add client/middleware.go client/middleware_test.go
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "feat(client): trace id HTTP middleware + outbound propagation"
```

---

## Task 7:(選配)端到端測試 —— 打真 ducklog server

**Files:**
- Create: `client/e2e_test.go`

**Interfaces:**
- 目的:啟真 server binary(Phase 1a),用 `slog` + `RemoteHandler` 送幾筆,查 server `/query`... 但 Phase 1a 尚無 query API(Phase 2)。故改查 server 的 `/health`(`dropped_last_hour`=0)+ 直接讀 server 的 DuckLake 不可行(跨 process)。**替代:驗證 ingest 回應**——本測試改為「啟 server、透過 handler 送、斷言 server 進程的 stdout 或 /health 顯示已接收」。

**因 Phase 1a 沒有 query API,真正的端到端驗證能力有限。建議本 task 只做「輕量煙霧」:**
- `-short` 直接 skip。
- 用 `GOTOOLCHAIN=go1.24.0 go build -o <tmp>/ducklogd ../cmd/ducklog`(跨模組,build server)。
- 寫一個 server config(隨機 port、暫存 data dir、一個 ingest key)。
- 啟 server,等 `/health` 回 200。
- 用 `NewRemoteHandler` 送 5 筆 error、`Close()`。
- 輪詢 `/health`,斷言 `disk_usage` 欄位存在且 server 未 crash(回 200)。因無 query API,**不逐筆驗內容**,只驗「端到端管線接得起來、server 收 ingest 不出錯」。
- 明確在測試註解與 report 標注:完整內容驗證要等 Phase 2 query API。

- [ ] **Step 1: 寫測試(含 -short skip 與跨模組 build)**

`client/e2e_test.go`(重點骨架,實作者補完 server config 與輪詢):
```go
package client

import (
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestEndToEndAgainstRealServer(t *testing.T) {
	if testing.Short() {
		t.Skip("端到端測試較重")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "ducklogd")
	build := exec.Command("go", "build", "-o", bin, "../cmd/ducklog")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=go1.24.0")
	build.Dir = "." // client 目錄;../cmd/ducklog 指到 server module
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, out)
	}
	// 寫 config(隨機 port、tmp data dir、ingest key),啟 server,等 /health,
	// 送 5 筆,Close,輪詢 /health 斷言 200 + 有 disk_usage 欄位。
	// (實作者補完;若 ../cmd/ducklog 因跨模組 build 失敗,改用 go run 並在 report 說明。)
	_ = bin
	_ = http.MethodGet
	_ = slog.LevelError
	_ = time.Second
}
```

> **實作者注意**:此 task 較 fiddly(跨模組 build、server 生命週期、port 管理)。若跨模組 `go build ../cmd/ducklog` 因兩個獨立 module 而無法從 client 目錄直接建,fallback:在 repo 根用 `GOTOOLCHAIN=go1.24.0 go build -o <tmp>/ducklogd ./cmd/ducklog`(server module),再由測試以絕對路徑啟動。若整體過於脆弱,標 `DONE_WITH_CONCERNS` 並在 report 說明改用手動驗證步驟,不要硬湊一個 flaky 測試。

- [ ] **Step 2: 執行**

Run: `cd client && go test -run EndToEnd ./...`(非 short)
Expected: PASS,或 `-short` 時 skip。

- [ ] **Step 3: Commit**

```bash
cd /home/dva/workspace/ducklog
git add client/e2e_test.go
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "test(client): end-to-end smoke against real ducklog server"
```

---

## 收尾:全套件

- [ ] **client 全測試 + race + vet**

Run: `cd client && go test ./... -race && go vet ./...`
Expected: 全 PASS,無 race,vet 乾淨。（`-short` 版應 skip e2e。）

- [ ] **確認零外部相依**

Run: `cd client && go mod tidy && git diff --exit-code go.mod`
Expected: `go.mod` 無變化(仍無 require 區塊)。若 tidy 想加東西 → 表示不小心引入了外部相依,要移除。

---

## Self-Review(對照 spec 與 tech.md items 5–6 的 Go 半邊)

- **slog 自訂 handler**(spec 的 Go 範例)→ Task 5 ✅
- **RemoteConfig(Endpoint/APIKey/Service/BatchSize/FlushInterval/Fallback)** → Task 3 ✅
- **失敗模式 #6 五要件**:非阻塞(Task 4 enqueue + Task 5 Handle)、timeout(Task 3 HTTPClient 2s)、有限重試(Task 4 send)、熔斷(Task 2 + Task 4)、stdout fallback 雙寫(Task 5)→ ✅
- **絕不靜默丟棄**:drop 計數 + Close 回報(Task 4 + Task 5)→ ✅
- **Trace ID 傳遞**:生成(Task 1)、context(Task 1)、HTTP middleware(Task 6)、outbound(Task 6)、handler 自動帶上(Task 5)→ ✅
- **graceful Close 排空** → Task 4 close + Task 5 Close ✅
- **零外部相依 / go 1.22 / 無 CGO** → Task 0 go.mod + 收尾 tidy 檢查 ✅

**明確不做(YAGNI,已在 spec 排除)**:Node pino(Phase 1c)、OTel、可關雙寫、壓縮。

**已知取捨**:
1. 端到端測試(Task 7)因 Phase 1a 尚無 query API,只能驗「管線接得起來」,不逐筆驗內容 —— 完整驗證待 Phase 2。
2. 雙寫固定開(第一版);之後穩定可加關閉選項。
3. drop 回報目前只在 Close(或有 drop 時);若要即時可觀測性,之後可加週期性 stderr 摘要。
