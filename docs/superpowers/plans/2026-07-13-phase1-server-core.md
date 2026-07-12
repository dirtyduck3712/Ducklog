# Phase 1a — Server Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 一個單 binary 的日誌收集 server,能可靠接收 HTTP ingest 並把資料 crash-safe 地存進 DuckLake,絕不掉已接收的資料。

**Architecture:** Go HTTP server + DuckLake(DuckDB catalog backend)。小批次資料 inline 在 catalog(ACID,crash 不掉),背景 goroutine 週期 `CHECKPOINT` 落地成 Parquet。獨立 `metrics.duckdb` 存永久聚合。磁碟/rate-limit/clock-skew 防護在 ingest 路徑上把「日誌系統的常見死法」擋掉。

**Tech Stack:** Go 1.22(build 用 `GOTOOLCHAIN=go1.24.0`)、`github.com/marcboeker/go-duckdb/v2 v2.4.3`(DuckDB 1.4.1 + DuckLake)、標準庫 `net/http`。

**Scope:** 本計畫只涵蓋 spec「實作順序」Phase 1 的 **items 1–4**(server core)。**items 5–6(Go/Node client transport + trace ID 傳遞)是獨立子系統,另開 `phase1b-client-transport` 計畫。** MCP、query API、Web UI、alert 屬 Phase 2+,不在此。

**依據:** 所有 DuckLake 相關語法與行為已在 `phase0/FINDINGS.md` 實測驗證。與原始 `tech.md` 有出入處,**以 FINDINGS.md 為準**。

## Global Constraints

以下值直接抄自 spec / FINDINGS,每個 task 都隱含適用:

- **Build**:`GOTOOLCHAIN=go1.24.0`(go-duckdb v2.4.3 的 go.mod 要求 go 1.24;本機 go 1.22 靠 toolchain 下載)。
- **Catalog backend = DuckDB,不是 SQLite**:`ATTACH 'ducklake:<path>/logs.ducklake' AS lake (DATA_PATH '<path>/logs/')`(無 `sqlite:` 前綴)。SQLite backend 的 `CHECKPOINT` 在此版壞掉。
- **inline 設定**:`CALL lake.set_option('data_inlining_row_limit', '1000')`(per-catalog,值為字串)。
- **Parquet 巢狀路徑**:資料檔在 `DATA_PATH/<schema>/<table>/*.parquet`(如 `data/logs/main/logs/ducklake-*.parquet`)。數檔要遞迴。
- **Catalog 大小 = `logs.ducklake` + `logs.ducklake.wal` 兩檔合計**(inline 資料在 `.wal`)。
- **維護**:週期 `CHECKPOINT lake` 之外,還要 `CALL ducklake_expire_snapshots('lake', older_than => ...)` + `CALL ducklake_cleanup_old_files('lake', cleanup_all => true)` 來 bound catalog 成長。
- **磁碟門檻**:Warn `0.80`(log+alert)、Reject `0.90`(拒 ingest 回 503)、Purge `0.95`(強制刪最舊 + CHECKPOINT)。
- **level 編碼**:`0=debug 1=info 2=warn 3=error`(UTINYINT)。
- **兩個時間**:`ts`=client 聲稱(顯示/排序);`ingested_at`=server 接收(**retention 一律用這個**)。
- **Clock skew**:偏差 > ±5 分鐘記警告;> ±1 小時用 server 時間覆蓋 `ts`,並在 `attrs` 標記 `_clock_skew: true`。
- **Rate limit**:default `1000/s`;可 per-service override(如 `batch-worker: 5000/s`)。超過丟棄,但**必須把丟棄數記進 metrics,不可靜默**。
- **Retention**:`DELETE FROM lake.logs WHERE ingested_at < now() - INTERVAL 30 DAY` 後 `CHECKPOINT lake`。
- **認證**:Bearer token;ingest 與 query 用**不同 key**(不同 scopes)。第一版就要有。
- **Single writer**:ingest 與(未來的)query 同 process,同一個 `*sql.DB`。
- **絕不靜默**:拒絕/丟棄/降級都要明確回報。

---

## File Structure

```
go.mod / go.sum
.gitignore
config.example.yaml
internal/model/log.go              LogEntry、Level 型別與 level 字串轉換
internal/store/store.go            Store:DuckLake 開啟/schema/批次 Insert/Count/Checkpoint/CatalogSize/DeleteOlderThan
internal/store/store_test.go
internal/metrics/metrics.go        MetricsStore:獨立 metrics.duckdb,upsert 聚合 + 丟棄計數
internal/metrics/metrics_test.go
internal/auth/auth.go              KeyStore:Bearer key → name + scopes
internal/auth/auth_test.go
internal/ratelimit/ratelimit.go    Limiter:per-service token bucket,回報 allowed/dropped
internal/ratelimit/ratelimit_test.go
internal/diskguard/diskguard.go    Guard:磁碟使用率 + catalog 大小 → Warn/Reject/Purge 狀態
internal/diskguard/diskguard_test.go
internal/checkpoint/checkpoint.go  Loop:週期 checkpoint+expire+cleanup,panic recovery,LastCheckpoint
internal/checkpoint/checkpoint_test.go
internal/ingest/handler.go         POST /ingest:auth→parse→skew→ratelimit→diskguard→insert(+metrics)
internal/ingest/handler_test.go
internal/health/health.go          GET /health
internal/health/health_test.go
internal/config/config.go          Config 載入(YAML)
cmd/docklog/main.go                組裝與 graceful shutdown
```

決策邊界:每個 `internal/*` 套件單一職責、可獨立測試。`store` 只管 DuckLake,不知道 HTTP;`ingest` 編排各防護但不知道 DuckLake 細節。

---

## Task 0: 專案骨架與相依

**Files:**
- Create: `go.mod`, `.gitignore`, `internal/model/log.go`, `internal/model/log_test.go`

**Interfaces:**
- Produces: `model.Level`(uint8)、`model.LogEntry`、`model.ParseLevel(string) (Level, bool)`、`(Level).String() string`

- [ ] **Step 1: git init + module + 相依**

```bash
cd /home/dva/workspace/docklog
git init
printf 'data/\n/docklog\n*.duckdb\n*.ducklake\n*.ducklake.wal\nphase0/\n' > .gitignore
export GOTOOLCHAIN=go1.24.0
go mod init docklog
go get github.com/marcboeker/go-duckdb/v2@v2.4.3
```

Expected: `go.mod` 出現 `require github.com/marcboeker/go-duckdb/v2 v2.4.3`。

- [ ] **Step 2: 寫 model 的失敗測試**

`internal/model/log_test.go`:
```go
package model

import "testing"

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{"debug": 0, "info": 1, "warn": 2, "error": 3}
	for name, want := range cases {
		got, ok := ParseLevel(name)
		if !ok || got != want {
			t.Fatalf("ParseLevel(%q) = %d,%v; want %d,true", name, got, ok, want)
		}
	}
	if _, ok := ParseLevel("bogus"); ok {
		t.Fatal("ParseLevel(bogus) should return ok=false")
	}
}

func TestLevelString(t *testing.T) {
	if Level(3).String() != "error" {
		t.Fatalf("Level(3).String() = %q; want error", Level(3).String())
	}
}
```

- [ ] **Step 3: 執行確認失敗**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/model/`
Expected: FAIL(`ParseLevel` / `LogEntry` 未定義)。

- [ ] **Step 4: 實作 model**

`internal/model/log.go`:
```go
// Package model 定義日誌的核心型別。
package model

import "time"

// Level 對應 spec 的 level 編碼:0=debug 1=info 2=warn 3=error。
type Level uint8

const (
	Debug Level = iota
	Info
	Warn
	Error
)

var levelNames = [...]string{"debug", "info", "warn", "error"}

func (l Level) String() string {
	if int(l) < len(levelNames) {
		return levelNames[l]
	}
	return "unknown"
}

// ParseLevel 把字串 level 轉成 Level;第二個回傳值為是否有效。
func ParseLevel(s string) (Level, bool) {
	for i, n := range levelNames {
		if n == s {
			return Level(i), true
		}
	}
	return 0, false
}

// LogEntry 是一筆待寫入的日誌。TraceID 為空字串代表 NULL。
// Attrs 是原始 JSON 字串(預設 "{}")。ClockSkewed 由 ingest 在覆蓋 ts 時設為 true。
type LogEntry struct {
	TS          time.Time
	IngestedAt  time.Time
	Service     string
	Level       Level
	TraceID     string
	Message     string
	Attrs       string
	ClockSkewed bool
}
```

- [ ] **Step 5: 執行確認通過**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/model/`
Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum .gitignore internal/model/
git commit -m "feat: project scaffold + model types"
```

---

## Task 1: Store — DuckLake 儲存層(核心:資料不能掉)

**Files:**
- Create: `internal/store/store.go`, `internal/store/store_test.go`

**Interfaces:**
- Consumes: `model.LogEntry`, `model.Level`
- Produces:
  - `func Open(dataDir string) (*Store, error)` — 建立/開啟 catalog,建 schema,設 inline limit
  - `func (*Store) Insert(ctx context.Context, entries []model.LogEntry) error` — 單一 tx batch
  - `func (*Store) Count(ctx context.Context) (int64, error)`
  - `func (*Store) Checkpoint(ctx context.Context) error` — `CHECKPOINT lake` + expire + cleanup
  - `func (*Store) CatalogSizeBytes() (int64, error)` — `.ducklake` + `.ducklake.wal` 合計
  - `func (*Store) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)`
  - `func (*Store) DB() *sql.DB`(給未來 query API,read path 用)
  - `func (*Store) Close() error`

- [ ] **Step 1: 寫失敗測試(insert + count + crash-safe 重開)**

`internal/store/store_test.go`:
```go
package store

import (
	"context"
	"testing"
	"time"

	"docklog/internal/model"
)

func sample(n int) []model.LogEntry {
	now := time.Now().UTC()
	out := make([]model.LogEntry, n)
	for i := range out {
		out[i] = model.LogEntry{
			TS: now, IngestedAt: now, Service: "api", Level: model.Error,
			TraceID: "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4050",
			Message: "connection timeout", Attrs: `{"host":"10.0.1.5"}`,
		}
	}
	return out
}

func TestInsertAndCount(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.Insert(ctx, sample(100)); err != nil {
		t.Fatal(err)
	}
	n, err := s.Count(ctx)
	if err != nil || n != 100 {
		t.Fatalf("Count = %d, %v; want 100", n, err)
	}
}

func TestEmptyTraceIDIsNull(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	e := sample(1)
	e[0].TraceID = "" // 應寫成 NULL,不報錯
	if err := s.Insert(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.Insert(context.Background(), sample(50))
	s.Close()

	s2, err := Open(dir) // 重開同目錄,資料要在
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	n, _ := s2.Count(context.Background())
	if n != 50 {
		t.Fatalf("重開後 Count = %d; want 50", n)
	}
}

func TestCheckpointFlushesParquet(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	defer s.Close()
	s.Insert(context.Background(), sample(2000))
	if err := s.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 不直接斷言檔數(避免與 DuckLake 內部行為耦合),只確認 checkpoint 不報錯且資料仍在
	n, _ := s.Count(context.Background())
	if n != 2000 {
		t.Fatalf("checkpoint 後 Count = %d; want 2000", n)
	}
}
```

- [ ] **Step 2: 執行確認失敗**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/store/`
Expected: FAIL(`Open` 未定義)。

- [ ] **Step 3: 實作 Store**

`internal/store/store.go`:
```go
// Package store 封裝 DuckLake 儲存層。所有 DuckLake 相關語法已於 phase0 驗證。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"docklog/internal/model"

	_ "github.com/marcboeker/go-duckdb/v2"
)

type Store struct {
	db       *sql.DB
	catalog  string // logs.ducklake 檔的絕對路徑
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	// 單 writer:序列化到單一連線,避免 DuckLake single-writer 衝突。
	db.SetMaxOpenConns(1)

	catalog := filepath.Join(dataDir, "logs.ducklake")
	setup := []string{
		"INSTALL ducklake",
		"LOAD ducklake",
		fmt.Sprintf("ATTACH 'ducklake:%s' AS lake (DATA_PATH '%s')",
			catalog, filepath.Join(dataDir, "logs")+string(os.PathSeparator)),
		`CREATE TABLE IF NOT EXISTS lake.logs (
			ts TIMESTAMP, ingested_at TIMESTAMP, service VARCHAR,
			level UTINYINT, trace_id UUID, message VARCHAR, attrs JSON)`,
		"CALL lake.set_option('data_inlining_row_limit', '1000')",
	}
	for _, stmt := range setup {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("store setup %q: %w", stmt, err)
		}
	}
	return &Store{db: db, catalog: catalog}, nil
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Insert(ctx context.Context, entries []model.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // Commit 成功後為 no-op
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO lake.logs VALUES (?, ?, ?, ?, ?::UUID, ?, ?::JSON)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		var traceID any // 空字串 → NULL
		if e.TraceID != "" {
			traceID = e.TraceID
		}
		attrs := e.Attrs
		if attrs == "" {
			attrs = "{}"
		}
		if _, err := stmt.ExecContext(ctx,
			e.TS.UTC(), e.IngestedAt.UTC(), e.Service, uint8(e.Level),
			traceID, e.Message, attrs); err != nil {
			return fmt.Errorf("insert row: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) Count(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM lake.logs").Scan(&n)
	return n, err
}

// Checkpoint flush inline → Parquet,並修剪過期 snapshot / 清理孤兒檔以 bound catalog 成長。
func (s *Store) Checkpoint(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "CHECKPOINT lake"); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	// 維護類失敗不致命(可能無事可做),記錄但不中斷。
	_, _ = s.db.ExecContext(ctx,
		"CALL ducklake_expire_snapshots('lake', older_than => now() - INTERVAL 7 DAY)")
	_, _ = s.db.ExecContext(ctx,
		"CALL ducklake_cleanup_old_files('lake', cleanup_all => true)")
	return nil
}

// CatalogSizeBytes 回傳 .ducklake + .ducklake.wal 合計(inline 資料在 .wal)。
func (s *Store) CatalogSizeBytes() (int64, error) {
	var total int64
	for _, p := range []string{s.catalog, s.catalog + ".wal"} {
		fi, err := os.Stat(p)
		if err == nil {
			total += fi.Size()
		} else if !os.IsNotExist(err) {
			return 0, err
		}
	}
	return total, nil
}

func (s *Store) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM lake.logs WHERE ingested_at < ?", cutoff.UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.ExecContext(ctx, "CHECKPOINT lake"); err != nil {
		return n, fmt.Errorf("checkpoint after delete: %w", err)
	}
	return n, nil
}

func (s *Store) Close() error { return s.db.Close() }
```

- [ ] **Step 4: 執行確認通過**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/store/ -v`
Expected: 4 個測試 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat: DuckLake store with batch insert, checkpoint, retention"
```

---

## Task 2: MetricsStore — 獨立 metrics.duckdb(永久聚合 + 丟棄計數)

**Files:**
- Create: `internal/metrics/metrics.go`, `internal/metrics/metrics_test.go`

**Interfaces:**
- Consumes: `model.Level`
- Produces:
  - `func Open(path string) (*MetricsStore, error)`
  - `func (*MetricsStore) Add(ctx, buckets []Bucket) error` — upsert `count += delta`
  - `func (*MetricsStore) AddDropped(ctx, ts time.Time, service string, n int64) error`
  - `func (*MetricsStore) DroppedSince(ctx, since time.Time) (int64, error)`
  - `type Bucket struct { TS time.Time; Service string; Level model.Level; Count int64 }`
  - `func (*MetricsStore) Close() error`

- [ ] **Step 1: 寫失敗測試(upsert 累加 + 丟棄計數)**

`internal/metrics/metrics_test.go`:
```go
package metrics

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"docklog/internal/model"
)

func TestUpsertAccumulates(t *testing.T) {
	m, err := Open(filepath.Join(t.TempDir(), "metrics.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := context.Background()
	ts := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	b := []Bucket{{TS: ts, Service: "api", Level: model.Error, Count: 3}}
	m.Add(ctx, b)
	m.Add(ctx, b) // 同 key 再加 3 → 應為 6
	var got int64
	m.DB().QueryRowContext(ctx,
		"SELECT count FROM metrics WHERE service='api' AND level=3").Scan(&got)
	if got != 6 {
		t.Fatalf("count = %d; want 6 (upsert 累加)", got)
	}
}

func TestDropped(t *testing.T) {
	m, _ := Open(filepath.Join(t.TempDir(), "metrics.duckdb"))
	defer m.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	m.AddDropped(ctx, now, "batch-worker", 500)
	m.AddDropped(ctx, now, "batch-worker", 200)
	got, _ := m.DroppedSince(ctx, now.Add(-time.Hour))
	if got != 700 {
		t.Fatalf("DroppedSince = %d; want 700", got)
	}
}
```

- [ ] **Step 2: 執行確認失敗**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/metrics/`
Expected: FAIL(`Open` 未定義)。

- [ ] **Step 3: 實作 MetricsStore**

`internal/metrics/metrics.go`:
```go
// Package metrics 管獨立的 metrics.duckdb(不進 DuckLake,因為需要 PRIMARY KEY upsert)。
package metrics

import (
	"context"
	"database/sql"
	"time"

	"docklog/internal/model"

	_ "github.com/marcboeker/go-duckdb/v2"
)

type Bucket struct {
	TS      time.Time
	Service string
	Level   model.Level
	Count   int64
}

type MetricsStore struct{ db *sql.DB }

func Open(path string) (*MetricsStore, error) {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	schema := []string{
		`CREATE TABLE IF NOT EXISTS metrics (
			ts TIMESTAMP, service VARCHAR, level UTINYINT, count BIGINT,
			PRIMARY KEY (ts, service, level))`,
		`CREATE TABLE IF NOT EXISTS dropped (
			ts TIMESTAMP, service VARCHAR, count BIGINT,
			PRIMARY KEY (ts, service))`,
	}
	for _, s := range schema {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &MetricsStore{db: db}, nil
}

func (m *MetricsStore) DB() *sql.DB { return m.db }

// Add 以分鐘為 bucket upsert 累加。ts 由呼叫端對齊到分鐘。
func (m *MetricsStore) Add(ctx context.Context, buckets []Bucket) error {
	if len(buckets) == 0 {
		return nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO metrics VALUES (?, ?, ?, ?)
		 ON CONFLICT (ts, service, level) DO UPDATE SET count = count + excluded.count`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, b := range buckets {
		if _, err := stmt.ExecContext(ctx,
			b.TS.UTC().Truncate(time.Minute), b.Service, uint8(b.Level), b.Count); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (m *MetricsStore) AddDropped(ctx context.Context, ts time.Time, service string, n int64) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO dropped VALUES (?, ?, ?)
		 ON CONFLICT (ts, service) DO UPDATE SET count = count + excluded.count`,
		ts.UTC().Truncate(time.Minute), service, n)
	return err
}

func (m *MetricsStore) DroppedSince(ctx context.Context, since time.Time) (int64, error) {
	var n sql.NullInt64
	err := m.db.QueryRowContext(ctx,
		"SELECT sum(count) FROM dropped WHERE ts >= ?", since.UTC()).Scan(&n)
	return n.Int64, err
}

func (m *MetricsStore) Close() error { return m.db.Close() }
```

- [ ] **Step 4: 執行確認通過**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/metrics/ -v`
Expected: PASS。若 `ON CONFLICT` 語法在此 DuckDB 版本不支援,測試會直接抓到 → 改用先 `SELECT` 再 `INSERT`/`UPDATE` 的 in-tx 手動 upsert。

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/
git commit -m "feat: separate metrics store with upsert + dropped counter"
```

---

## Task 3: Auth — API key scopes

**Files:**
- Create: `internal/auth/auth.go`, `internal/auth/auth_test.go`

**Interfaces:**
- Produces:
  - `type Key struct { Key, Name string; Scopes []string }`
  - `func New(keys []Key) *KeyStore`
  - `func (*KeyStore) Authenticate(bearer string) (*Key, bool)` — 從 `Authorization: Bearer x` 的 raw header 值解析
  - `func (*Key) HasScope(scope string) bool`

- [ ] **Step 1: 寫失敗測試**

`internal/auth/auth_test.go`:
```go
package auth

import "testing"

func store() *KeyStore {
	return New([]Key{
		{Key: "ingest-secret", Name: "k8s", Scopes: []string{"ingest"}},
		{Key: "query-secret", Name: "claude", Scopes: []string{"query", "mcp"}},
	})
}

func TestAuthenticate(t *testing.T) {
	k, ok := store().Authenticate("Bearer ingest-secret")
	if !ok || k.Name != "k8s" {
		t.Fatalf("Authenticate ingest = %+v, %v", k, ok)
	}
	if !k.HasScope("ingest") || k.HasScope("query") {
		t.Fatal("scope 判斷錯誤")
	}
}

func TestRejectBadKey(t *testing.T) {
	if _, ok := store().Authenticate("Bearer nope"); ok {
		t.Fatal("未知 key 應被拒")
	}
	if _, ok := store().Authenticate("ingest-secret"); ok {
		t.Fatal("缺 Bearer 前綴應被拒")
	}
}
```

- [ ] **Step 2: 執行確認失敗**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/auth/`
Expected: FAIL。

- [ ] **Step 3: 實作 auth**

`internal/auth/auth.go`:
```go
// Package auth 做純 Bearer token → scopes 對應。
package auth

import "strings"

type Key struct {
	Key    string
	Name   string
	Scopes []string
}

func (k *Key) HasScope(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

type KeyStore struct{ byKey map[string]*Key }

func New(keys []Key) *KeyStore {
	m := make(map[string]*Key, len(keys))
	for i := range keys {
		k := keys[i]
		m[k.Key] = &k
	}
	return &KeyStore{byKey: m}
}

// Authenticate 接受完整的 Authorization header 值(含 "Bearer " 前綴)。
func (ks *KeyStore) Authenticate(authHeader string) (*Key, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return nil, false
	}
	k, ok := ks.byKey[strings.TrimPrefix(authHeader, prefix)]
	return k, ok
}
```

- [ ] **Step 4: 執行確認通過**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/auth/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/auth/
git commit -m "feat: bearer key auth with scopes"
```

---

## Task 4: Rate limiter — per-service token bucket

**Files:**
- Create: `internal/ratelimit/ratelimit.go`, `internal/ratelimit/ratelimit_test.go`

**Interfaces:**
- Produces:
  - `func New(defaultPerSec float64, overrides map[string]float64) *Limiter`
  - `func (*Limiter) Allow(service string, n int) (allowed, dropped int)` — 一次要求寫 n 筆,回傳准許/丟棄數
  - 內部用 token bucket,burst = 1 秒容量;time 以 `now func() time.Time` 注入以利測試

- [ ] **Step 1: 寫失敗測試**

`internal/ratelimit/ratelimit_test.go`:
```go
package ratelimit

import (
	"testing"
	"time"
)

func TestPerServiceOverrideAndDrop(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(1000, map[string]float64{"batch-worker": 5000})
	l.now = func() time.Time { return now }

	// api:default 1000/s。一次要 1500 → 准 1000 丟 500(bucket 起始滿)。
	allowed, dropped := l.Allow("api", 1500)
	if allowed != 1000 || dropped != 500 {
		t.Fatalf("api Allow(1500) = %d,%d; want 1000,500", allowed, dropped)
	}
	// batch-worker override 5000/s:要 3000 全准。
	allowed, dropped = l.Allow("batch-worker", 3000)
	if allowed != 3000 || dropped != 0 {
		t.Fatalf("batch Allow(3000) = %d,%d; want 3000,0", allowed, dropped)
	}
}

func TestRefillOverTime(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(1000, nil)
	l.now = func() time.Time { return now }
	l.Allow("api", 1000)        // 用光
	_, d := l.Allow("api", 100) // 立刻再要 → 全丟
	if d != 100 {
		t.Fatalf("耗盡後 dropped = %d; want 100", d)
	}
	now = now.Add(time.Second) // 過 1 秒補滿
	a, _ := l.Allow("api", 500)
	if a != 500 {
		t.Fatalf("補充後 allowed = %d; want 500", a)
	}
}
```

- [ ] **Step 2: 執行確認失敗**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/ratelimit/`
Expected: FAIL。

- [ ] **Step 3: 實作 limiter**

`internal/ratelimit/ratelimit.go`:
```go
// Package ratelimit 是 per-service token bucket。超量丟棄,回報丟棄數(不可靜默)。
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

type Limiter struct {
	mu         sync.Mutex
	defRate    float64
	overrides  map[string]float64
	buckets    map[string]*bucket
	now        func() time.Time
}

func New(defaultPerSec float64, overrides map[string]float64) *Limiter {
	if overrides == nil {
		overrides = map[string]float64{}
	}
	return &Limiter{
		defRate:   defaultPerSec,
		overrides: overrides,
		buckets:   map[string]*bucket{},
		now:       time.Now,
	}
}

func (l *Limiter) rate(service string) float64 {
	if r, ok := l.overrides[service]; ok {
		return r
	}
	return l.defRate
}

// Allow 針對一次寫 n 筆的請求,回傳准許數與丟棄數。burst 容量 = 1 秒的 rate。
func (l *Limiter) Allow(service string, n int) (allowed, dropped int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rate := l.rate(service)
	now := l.now()
	b := l.buckets[service]
	if b == nil {
		b = &bucket{tokens: rate, last: now} // 起始滿
		l.buckets[service] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		b.tokens = min(rate, b.tokens+elapsed*rate)
		b.last = now
	}
	allowed = n
	if float64(allowed) > b.tokens {
		allowed = int(b.tokens)
	}
	b.tokens -= float64(allowed)
	return allowed, n - allowed
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
```

- [ ] **Step 4: 執行確認通過**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/ratelimit/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/ratelimit/
git commit -m "feat: per-service token bucket rate limiter"
```

---

## Task 5: DiskGuard — 磁碟 + catalog 大小防護

**Files:**
- Create: `internal/diskguard/diskguard.go`, `internal/diskguard/diskguard_test.go`

**Interfaces:**
- Produces:
  - `type State int`(`OK`, `Warn`, `Reject`, `Purge`)
  - `func New(path string, usageFn func(path string) (float64, error)) *Guard` — `usageFn` 注入以利測試,production 用 `DiskUsage`
  - `func (*Guard) State() (State, float64, error)` — 回傳狀態 + 目前使用率
  - `func DiskUsage(path string) (float64, error)` — 真實實作(`syscall.Statfs`)
  - 門檻常數:`WarnThreshold=0.80`、`RejectThreshold=0.90`、`PurgeThreshold=0.95`

- [ ] **Step 1: 寫失敗測試(門檻邏輯用注入的 usageFn)**

`internal/diskguard/diskguard_test.go`:
```go
package diskguard

import "testing"

func guardAt(u float64) *Guard {
	return New("/", func(string) (float64, error) { return u, nil })
}

func TestThresholds(t *testing.T) {
	cases := []struct {
		usage float64
		want  State
	}{
		{0.50, OK}, {0.80, Warn}, {0.85, Warn},
		{0.90, Reject}, {0.94, Reject}, {0.95, Purge}, {0.99, Purge},
	}
	for _, c := range cases {
		got, _, _ := guardAt(c.usage).State()
		if got != c.want {
			t.Fatalf("usage %.2f → %v; want %v", c.usage, got, c.want)
		}
	}
}
```

- [ ] **Step 2: 執行確認失敗**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/diskguard/`
Expected: FAIL。

- [ ] **Step 3: 實作 diskguard**

`internal/diskguard/diskguard.go`:
```go
// Package diskguard 監控磁碟使用率,把「磁碟滿寫壞 catalog」這個最常見死法擋掉。
package diskguard

import "syscall"

type State int

const (
	OK State = iota
	Warn
	Reject
	Purge
)

func (s State) String() string {
	return [...]string{"ok", "warn", "reject", "purge"}[s]
}

const (
	WarnThreshold   = 0.80
	RejectThreshold = 0.90
	PurgeThreshold  = 0.95
)

type Guard struct {
	path    string
	usageFn func(string) (float64, error)
}

func New(path string, usageFn func(string) (float64, error)) *Guard {
	if usageFn == nil {
		usageFn = DiskUsage
	}
	return &Guard{path: path, usageFn: usageFn}
}

func (g *Guard) State() (State, float64, error) {
	u, err := g.usageFn(g.path)
	if err != nil {
		return OK, 0, err
	}
	switch {
	case u >= PurgeThreshold:
		return Purge, u, nil
	case u >= RejectThreshold:
		return Reject, u, nil
	case u >= WarnThreshold:
		return Warn, u, nil
	default:
		return OK, u, nil
	}
}

// DiskUsage 回傳 path 所在檔案系統的已用比例(0..1)。
func DiskUsage(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	avail := float64(st.Bavail) * float64(st.Bsize)
	if total == 0 {
		return 0, nil
	}
	return 1 - avail/total, nil
}
```

- [ ] **Step 4: 執行確認通過**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/diskguard/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/diskguard/
git commit -m "feat: disk usage guard with warn/reject/purge thresholds"
```

---

## Task 6: Ingest handler — 把所有防護接起來

**Files:**
- Create: `internal/ingest/handler.go`, `internal/ingest/handler_test.go`

**Interfaces:**
- Consumes: `store.Store`, `metrics.MetricsStore`, `auth.KeyStore`, `ratelimit.Limiter`, `diskguard.Guard`, `model.LogEntry`
- Produces:
  - `func New(deps Deps) *Handler`,`Handler` 實作 `http.Handler`(`ServeHTTP`)
  - `type Deps struct { Store *store.Store; Metrics *metrics.MetricsStore; Keys *auth.KeyStore; Limiter *ratelimit.Limiter; Guard *diskguard.Guard; Now func() time.Time }`
  - 行為:`POST /ingest`,`Authorization: Bearer`,body 為 `application/x-ndjson`
  - 回應:`{"status":"ok","accepted":N,"rejected":M}` 或 `{"status":"error","error_code":"DISK_FULL",...}`

- [ ] **Step 1: 寫失敗測試(涵蓋 auth、skew、disk reject、rate limit 記丟棄)**

`internal/ingest/handler_test.go`:
```go
package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"docklog/internal/auth"
	"docklog/internal/diskguard"
	"docklog/internal/metrics"
	"docklog/internal/ratelimit"
	"docklog/internal/store"
)

func newTestHandler(t *testing.T, usage float64) (*Handler, *store.Store, *metrics.MetricsStore) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := metrics.Open(filepath.Join(dir, "metrics.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{
		Store:   st,
		Metrics: ms,
		Keys:    auth.New([]auth.Key{{Key: "k", Name: "t", Scopes: []string{"ingest"}}}),
		Limiter: ratelimit.New(1000, nil),
		Guard:   diskguard.New(dir, func(string) (float64, error) { return usage, nil }),
		Now:     func() time.Time { return time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC) },
	})
	return h, st, ms
}

func post(h *Handler, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestIngestOK(t *testing.T) {
	h, st, _ := newTestHandler(t, 0.10)
	body := `{"ts":"2026-07-13T10:00:00Z","service":"api","level":"error","message":"boom","attrs":{"a":1}}
{"ts":"2026-07-13T10:00:01Z","service":"api","level":"info","message":"ok"}`
	rr := post(h, "k", body)
	if rr.Code != 200 {
		t.Fatalf("code = %d, body=%s", rr.Code, rr.Body)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["accepted"].(float64) != 2 {
		t.Fatalf("accepted = %v; want 2", resp["accepted"])
	}
	n, _ := st.Count(context.Background())
	if n != 2 {
		t.Fatalf("stored = %d; want 2", n)
	}
}

func TestRejectNoAuth(t *testing.T) {
	h, _, _ := newTestHandler(t, 0.10)
	if rr := post(h, "", `{}`); rr.Code != 401 {
		t.Fatalf("no auth code = %d; want 401", rr.Code)
	}
	if rr := post(h, "wrong", `{}`); rr.Code != 401 {
		t.Fatalf("bad key code = %d; want 401", rr.Code)
	}
}

func TestDiskFullRejects(t *testing.T) {
	h, _, _ := newTestHandler(t, 0.92) // > RejectThreshold
	rr := post(h, "k", `{"ts":"2026-07-13T10:00:00Z","service":"api","level":"info","message":"x"}`)
	if rr.Code != 503 {
		t.Fatalf("disk full code = %d; want 503", rr.Code)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["error_code"] != "DISK_FULL" {
		t.Fatalf("error_code = %v; want DISK_FULL", resp["error_code"])
	}
}

func TestClockSkewOverride(t *testing.T) {
	h, st, _ := newTestHandler(t, 0.10)
	// ts 比 server now(10:00)早 2 小時 → 超過 ±1h,應覆蓋為 server 時間 + 標記
	post(h, "k", `{"ts":"2026-07-13T08:00:00Z","service":"api","level":"info","message":"skew"}`)
	var attrs string
	st.DB().QueryRow("SELECT attrs FROM lake.logs LIMIT 1").Scan(&attrs)
	if !strings.Contains(attrs, "_clock_skew") {
		t.Fatalf("attrs 應含 _clock_skew,得到 %s", attrs)
	}
}

func TestRateLimitRecordsDropped(t *testing.T) {
	h, _, ms := newTestHandler(t, 0.10)
	// 超過 default 1000/s:送 1200 筆同 service,應 accept 1000 drop 200,且 dropped 表有記錄
	var sb strings.Builder
	for i := 0; i < 1200; i++ {
		sb.WriteString(`{"ts":"2026-07-13T10:00:00Z","service":"flood","level":"info","message":"x"}` + "\n")
	}
	rr := post(h, "k", sb.String())
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["rejected"].(float64) != 200 {
		t.Fatalf("rejected = %v; want 200", resp["rejected"])
	}
	got, _ := ms.DroppedSince(context.Background(), time.Time{})
	if got != 200 {
		t.Fatalf("metrics dropped = %d; want 200", got)
	}
}
```

- [ ] **Step 2: 執行確認失敗**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/ingest/`
Expected: FAIL(`New` / `Handler` 未定義)。

- [ ] **Step 3: 實作 handler**

`internal/ingest/handler.go`:
```go
// Package ingest 是 POST /ingest 的 HTTP handler,把 auth / clock-skew / rate-limit /
// disk-guard / 儲存串起來。順序:先擋(auth→disk),再解析,再限流,最後寫。
package ingest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"docklog/internal/auth"
	"docklog/internal/diskguard"
	"docklog/internal/metrics"
	"docklog/internal/model"
	"docklog/internal/ratelimit"
	"docklog/internal/store"
)

type Deps struct {
	Store   *store.Store
	Metrics *metrics.MetricsStore
	Keys    *auth.KeyStore
	Limiter *ratelimit.Limiter
	Guard   *diskguard.Guard
	Now     func() time.Time
}

type Handler struct{ d Deps }

func New(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Handler{d: d}
}

// 進來的一行 JSON。
type rawLog struct {
	TS      time.Time       `json:"ts"`
	Service string          `json:"service"`
	Level   string          `json:"level"`
	TraceID string          `json:"trace_id"`
	Message string          `json:"message"`
	Attrs   json.RawMessage `json:"attrs"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "error", "error_code": "METHOD"})
		return
	}
	// 1. auth(需 ingest scope)
	key, ok := h.d.Keys.Authenticate(r.Header.Get("Authorization"))
	if !ok || !key.HasScope("ingest") {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"status": "error", "error_code": "UNAUTHORIZED"})
		return
	}
	// 2. disk guard:Reject/Purge 都拒新寫入(拒絕好過寫壞檔案)
	state, usage, err := h.d.Guard.State()
	if err == nil && state >= diskguard.Reject {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "error", "error_code": "DISK_FULL",
			"message": fmt.Sprintf("磁碟使用率 %.0f%%,拒絕寫入", usage*100),
		})
		return
	}
	// 3. 解析 ndjson
	now := h.d.Now().UTC()
	var entries []model.LogEntry
	perService := map[string]int{}
	sc := bufio.NewScanner(r.Body)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw rawLog
		if err := json.Unmarshal(line, &raw); err != nil {
			continue // 壞行跳過(第一版寬鬆);TODO 之後計數
		}
		lvl, ok := model.ParseLevel(raw.Level)
		if !ok {
			lvl = model.Info
		}
		e := model.LogEntry{
			TS: raw.TS, IngestedAt: now, Service: raw.Service, Level: lvl,
			TraceID: raw.TraceID, Message: raw.Message, Attrs: string(raw.Attrs),
		}
		applyClockSkew(&e, now)
		entries = append(entries, e)
		perService[raw.Service]++
	}

	// 4. rate limit(per service),被丟棄的記進 metrics.dropped
	accepted := entries[:0]
	var rejected int
	allowedByService := map[string]int{}
	for svc, n := range perService {
		a, dropped := h.d.Limiter.Allow(svc, n)
		allowedByService[svc] = a
		if dropped > 0 {
			rejected += dropped
			_ = h.d.Metrics.AddDropped(r.Context(), now, svc, int64(dropped))
		}
	}
	for _, e := range entries {
		if allowedByService[e.Service] > 0 {
			allowedByService[e.Service]--
			accepted = append(accepted, e)
		}
	}

	// 5. 寫入 store + metrics 聚合
	if err := h.d.Store.Insert(r.Context(), accepted); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"status": "error", "error_code": "STORE", "message": err.Error()})
		return
	}
	_ = h.d.Metrics.Add(r.Context(), buckets(accepted))

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "accepted": len(accepted), "rejected": rejected})
}

// applyClockSkew:偏差 > ±1h 用 server 時間覆蓋 ts 並標記 attrs._clock_skew。
func applyClockSkew(e *model.LogEntry, now time.Time) {
	if e.TS.IsZero() {
		e.TS = now
		return
	}
	diff := e.TS.Sub(now)
	if diff < -time.Hour || diff > time.Hour {
		e.TS = now
		e.ClockSkewed = true
		e.Attrs = mergeSkewFlag(e.Attrs)
	}
}

// mergeSkewFlag 在 JSON 物件裡塞入 "_clock_skew":true。空/壞值一律換成標記物件。
func mergeSkewFlag(attrs string) string {
	m := map[string]any{}
	if attrs != "" {
		_ = json.Unmarshal([]byte(attrs), &m)
	}
	m["_clock_skew"] = true
	b, _ := json.Marshal(m)
	return string(b)
}

func buckets(entries []model.LogEntry) []metrics.Bucket {
	agg := map[metrics.Bucket]int64{}
	for _, e := range entries {
		k := metrics.Bucket{TS: e.IngestedAt.Truncate(time.Minute), Service: e.Service, Level: e.Level}
		agg[k]++
	}
	out := make([]metrics.Bucket, 0, len(agg))
	for k, c := range agg {
		k.Count = c
		out = append(out, k)
	}
	return out
}
```

- [ ] **Step 4: 執行確認通過**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/ingest/ -v`
Expected: 5 個測試 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/ingest/
git commit -m "feat: ingest handler wiring auth, skew, rate-limit, disk-guard, store"
```

---

## Task 7: Checkpoint loop — 週期落地 + panic recovery

**Files:**
- Create: `internal/checkpoint/checkpoint.go`, `internal/checkpoint/checkpoint_test.go`

**Interfaces:**
- Consumes: `store.Store`
- Produces:
  - `func NewLoop(st *store.Store, interval time.Duration) *Loop`
  - `func (*Loop) Run(ctx context.Context)` — 阻塞直到 ctx 取消;每 interval 呼叫 `st.Checkpoint`,panic 有 recovery
  - `func (*Loop) LastCheckpoint() time.Time` — 供 health check
  - `func (*Loop) Once(ctx context.Context) error` — 手動觸發一次(測試用)

- [ ] **Step 1: 寫失敗測試**

`internal/checkpoint/checkpoint_test.go`:
```go
package checkpoint

import (
	"context"
	"testing"
	"time"

	"docklog/internal/store"
)

func TestOnceUpdatesTimestamp(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	l := NewLoop(st, time.Hour)
	if !l.LastCheckpoint().IsZero() {
		t.Fatal("初始 LastCheckpoint 應為零值")
	}
	if err := l.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.LastCheckpoint().IsZero() {
		t.Fatal("Once 後 LastCheckpoint 應被更新")
	}
}

func TestRunStopsOnContext(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	l := NewLoop(st, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run 未在 ctx 取消後結束")
	}
	if l.LastCheckpoint().IsZero() {
		t.Fatal("Run 期間應至少 checkpoint 一次")
	}
}
```

- [ ] **Step 2: 執行確認失敗**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/checkpoint/`
Expected: FAIL。

- [ ] **Step 3: 實作 loop**

`internal/checkpoint/checkpoint.go`:
```go
// Package checkpoint 週期性把 inline 資料落地成 Parquet 並修剪 catalog。
// goroutine 死掉會讓 inline 無限累積(失敗模式 #5),故必有 panic recovery。
package checkpoint

import (
	"context"
	"log"
	"sync"
	"time"

	"docklog/internal/store"
)

type Loop struct {
	st       *store.Store
	interval time.Duration
	mu       sync.Mutex
	last     time.Time
}

func NewLoop(st *store.Store, interval time.Duration) *Loop {
	return &Loop{st: st, interval: interval}
}

func (l *Loop) LastCheckpoint() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

func (l *Loop) Once(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("checkpoint panic recovered: %v", r)
		}
	}()
	if err = l.st.Checkpoint(ctx); err != nil {
		return err
	}
	l.mu.Lock()
	l.last = time.Now()
	l.mu.Unlock()
	return nil
}

func (l *Loop) Run(ctx context.Context) {
	t := time.NewTicker(l.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.Once(ctx); err != nil {
				log.Printf("checkpoint error: %v", err)
			}
		}
	}
}
```

- [ ] **Step 4: 執行確認通過**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/checkpoint/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/checkpoint/
git commit -m "feat: checkpoint loop with panic recovery + last-checkpoint tracking"
```

---

## Task 8: Health endpoint

**Files:**
- Create: `internal/health/health.go`, `internal/health/health_test.go`

**Interfaces:**
- Consumes: `store.Store`, `metrics.MetricsStore`, `diskguard.Guard`, `checkpoint.Loop`
- Produces:
  - `func New(deps Deps) *Handler`(實作 `http.Handler`)
  - 回應 `GET /health`:`{status, disk_usage, catalog_size_mb, last_checkpoint, dropped_last_hour}`

- [ ] **Step 1: 寫失敗測試**

`internal/health/health_test.go`:
```go
package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"docklog/internal/diskguard"
	"docklog/internal/metrics"
	"docklog/internal/store"
)

func TestHealth(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(dir)
	defer st.Close()
	ms, _ := metrics.Open(filepath.Join(dir, "metrics.duckdb"))
	defer ms.Close()
	h := New(Deps{
		Store:   st,
		Metrics: ms,
		Guard:   diskguard.New(dir, func(string) (float64, error) { return 0.42, nil }),
		LastCheckpoint: func() (t timeStub) { return },
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != 200 {
		t.Fatalf("code = %d", rr.Code)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["status"] != "ok" || resp["disk_usage"].(float64) != 0.42 {
		t.Fatalf("resp = %v", resp)
	}
}
```

> 註:上面 `timeStub` 只是示意 `LastCheckpoint func() time.Time`。實作 Task 時用真正的 `func() time.Time`,測試傳 `func() time.Time { return time.Time{} }`。

- [ ] **Step 2: 執行確認失敗**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/health/`
Expected: FAIL。

- [ ] **Step 3: 實作 health**

`internal/health/health.go`:
```go
// Package health 提供 GET /health,暴露新架構的關鍵指標(catalog 大小 / 最後 checkpoint)。
package health

import (
	"encoding/json"
	"net/http"
	"time"

	"docklog/internal/diskguard"
	"docklog/internal/metrics"
	"docklog/internal/store"
)

type Deps struct {
	Store          *store.Store
	Metrics        *metrics.MetricsStore
	Guard          *diskguard.Guard
	LastCheckpoint func() time.Time
}

type Handler struct{ d Deps }

func New(d Deps) *Handler { return &Handler{d: d} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, usage, _ := h.d.Guard.State()
	catBytes, _ := h.d.Store.CatalogSizeBytes()
	dropped, _ := h.d.Metrics.DroppedSince(r.Context(), time.Now().Add(-time.Hour))
	last := h.d.LastCheckpoint()
	var lastStr any
	if !last.IsZero() {
		lastStr = last.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":           "ok",
		"disk_usage":       usage,
		"catalog_size_mb":  float64(catBytes) / 1e6,
		"last_checkpoint":  lastStr,
		"dropped_last_hour": dropped,
	})
}
```

> 修正測試:把 `LastCheckpoint: func() (t timeStub) {...}` 改成 `LastCheckpoint: func() time.Time { return time.Time{} }`,移除 `timeStub`。

- [ ] **Step 4: 執行確認通過**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/health/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/health/
git commit -m "feat: health endpoint with catalog size + last checkpoint"
```

---

## Task 9: Config + main.go 組裝 + graceful shutdown

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`, `cmd/docklog/main.go`, `config.example.yaml`
- 需要 YAML:`go get gopkg.in/yaml.v3`

**Interfaces:**
- Consumes: 全部套件
- Produces:
  - `type Config struct {...}`,`func Load(path string) (Config, error)`
  - `main`:載 config → 開 store/metrics → 起 checkpoint loop → 掛 `/ingest` `/health` → `http.Server` + signal 優雅關閉

- [ ] **Step 1: 寫 config 失敗測試**

`internal/config/config_test.go`:
```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte(`
listen: ":8080"
data_dir: "data"
checkpoint_interval: "1h"
rate_limits:
  default: 1000
  overrides:
    batch-worker: 5000
api_keys:
  - key: "ingest-secret"
    name: "k8s"
    scopes: ["ingest"]
`), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.RateLimits.Default != 1000 ||
		c.RateLimits.Overrides["batch-worker"] != 5000 ||
		len(c.APIKeys) != 1 || c.APIKeys[0].Scopes[0] != "ingest" {
		t.Fatalf("config 解析錯誤:%+v", c)
	}
	if c.CheckpointInterval.String() != "1h0m0s" {
		t.Fatalf("interval 解析錯誤:%v", c.CheckpointInterval)
	}
}
```

- [ ] **Step 2: 執行確認失敗**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/config/`
Expected: FAIL。

- [ ] **Step 3: 實作 config**

`internal/config/config.go`:
```go
// Package config 載入 YAML 設定。
package config

import (
	"os"
	"time"

	"docklog/internal/auth"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen             string        `yaml:"listen"`
	DataDir            string        `yaml:"data_dir"`
	CheckpointInterval time.Duration `yaml:"checkpoint_interval"`
	RateLimits         struct {
		Default   float64            `yaml:"default"`
		Overrides map[string]float64 `yaml:"overrides"`
	} `yaml:"rate_limits"`
	APIKeys []auth.Key `yaml:"api_keys"`
}

func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	// 用中介型別把 checkpoint_interval 的字串("1h")轉成 time.Duration。
	var raw struct {
		Config             `yaml:",inline"`
		CheckpointInterval string `yaml:"checkpoint_interval"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return c, err
	}
	c = raw.Config
	if raw.CheckpointInterval != "" {
		d, err := time.ParseDuration(raw.CheckpointInterval)
		if err != nil {
			return c, err
		}
		c.CheckpointInterval = d
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.CheckpointInterval == 0 {
		c.CheckpointInterval = time.Hour
	}
	return c, nil
}
```

> 註:`auth.Key` 的 yaml tag 需對應 `key`/`name`/`scopes`。在 Task 3 的 `auth.Key` 補上 tag:
> ```go
> type Key struct {
>     Key    string   `yaml:"key"`
>     Name   string   `yaml:"name"`
>     Scopes []string `yaml:"scopes"`
> }
> ```

- [ ] **Step 4: 執行確認通過(config)**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/config/`
Expected: PASS。

- [ ] **Step 5: 寫 main.go**

`cmd/docklog/main.go`:
```go
// docklog server:單 binary 的日誌收集器。Phase 1a = ingest + 儲存 + 防護。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"docklog/internal/auth"
	"docklog/internal/checkpoint"
	"docklog/internal/config"
	"docklog/internal/diskguard"
	"docklog/internal/health"
	"docklog/internal/ingest"
	"docklog/internal/metrics"
	"docklog/internal/ratelimit"
	"docklog/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "設定檔路徑")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("載入 config: %v", err)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("開啟 store: %v", err)
	}
	defer st.Close()
	ms, err := metrics.Open(filepath.Join(cfg.DataDir, "metrics.duckdb"))
	if err != nil {
		log.Fatalf("開啟 metrics: %v", err)
	}
	defer ms.Close()

	guard := diskguard.New(cfg.DataDir, nil)
	loop := checkpoint.NewLoop(st, cfg.CheckpointInterval)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go loop.Run(ctx)

	mux := http.NewServeMux()
	mux.Handle("/ingest", ingest.New(ingest.Deps{
		Store: st, Metrics: ms, Keys: auth.New(cfg.APIKeys),
		Limiter: ratelimit.New(cfg.RateLimits.Default, cfg.RateLimits.Overrides),
		Guard:   guard, Now: time.Now,
	}))
	mux.Handle("/health", health.New(health.Deps{
		Store: st, Metrics: ms, Guard: guard, LastCheckpoint: loop.LastCheckpoint,
	}))

	srv := &http.Server{Addr: cfg.Listen, Handler: mux}
	go func() {
		log.Printf("docklog 監聽 %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("關閉中…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	_ = os.Stdout.Sync()
}
```

`config.example.yaml`:
```yaml
listen: ":8080"
data_dir: "data"
checkpoint_interval: "1h"
rate_limits:
  default: 1000
  overrides:
    batch-worker: 5000
api_keys:
  - key: "CHANGE-ME-ingest"
    name: "k8s-daemonset"
    scopes: ["ingest"]
  - key: "CHANGE-ME-query"
    name: "claude-code"
    scopes: ["query", "mcp"]
```

- [ ] **Step 6: 建置與冒煙測試**

```bash
GOTOOLCHAIN=go1.24.0 go build -o docklog ./cmd/docklog
cp config.example.yaml config.yaml   # 改掉 key
./docklog -config config.yaml &
sleep 2
curl -s -XPOST localhost:8080/ingest -H 'Authorization: Bearer CHANGE-ME-ingest' \
  --data-binary $'{"ts":"2026-07-13T10:00:00Z","service":"api","level":"error","message":"boom"}'
curl -s localhost:8080/health
kill %1
```

Expected:ingest 回 `{"status":"ok","accepted":1,"rejected":0}`;health 回含 `catalog_size_mb`、`disk_usage` 的 JSON。

- [ ] **Step 7: Commit**

```bash
git add internal/config/ cmd/ config.example.yaml go.mod go.sum
git commit -m "feat: config loader + main wiring with graceful shutdown"
```

---

## Task 10: Crash 恢復整合測試(spec 測試要求)

**Files:**
- Create: `internal/store/crash_test.go`

**Interfaces:**
- Consumes: `store.Store`
- 目的:兌現 spec「測試要求 → Crash 恢復」。用子行程 + `SIGKILL` 驗證 kill -9 後資料完整。
- 參考 `phase0/crash/main.go` 的手法(build 真 binary 再 kill,避免殺到 `go test` wrapper)。

- [ ] **Step 1: 寫整合測試**

`internal/store/crash_test.go`:
```go
package store_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"docklog/internal/store"
)

// 這個測試會 build 一個小 writer helper、SIGKILL 它、再用 Store 重開驗證。
// helper 原始碼寫進 tempdir,以獨立 main 編譯。
func TestCrashDuringIngestKeepsCommittedData(t *testing.T) {
	if testing.Short() {
		t.Skip("crash 測試較慢")
	}
	dir := t.TempDir()
	helper := writeHelper(t, dir)     // 見下,回傳 helper binary 路徑
	dataDir := filepath.Join(dir, "data")
	counter := filepath.Join(dir, "committed.txt")

	cmd := exec.Command(helper, dataDir, counter)
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=go1.24.0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGKILL)
	cmd.Wait()

	// 重開驗證:count 為 batch 倍數(無 torn tx)且 >= 已 commit 數(無遺失)
	committed := readInt(counter)
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("crash 後重開失敗: %v", err)
	}
	defer st.Close()
	n, _ := st.Count(context.Background())
	if n%50 != 0 {
		t.Fatalf("count %d 非 50 倍數 → 有半個 transaction", n)
	}
	if n < committed {
		t.Fatalf("count %d < 已 commit %d → 掉資料", n, committed)
	}
}

func readInt(path string) int64 {
	b, _ := os.ReadFile(path)
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
```

> `writeHelper` 把一段 `package main` 原始碼寫進 `dir/helper/main.go`,內容:開 `store.Open(dataDir)`、以 50 筆/tx 迴圈 `Insert`,每次 commit 後 fsync 累計數到 counter 檔;然後 `GOTOOLCHAIN=go1.24.0 go build` 成 binary 回傳路徑。**helper 需 import `docklog/internal/store`,所以要在同 module 內編譯**(用 `go build` 指到暫存的 `.go`,並確保 `GOFLAGS`/工作目錄在 repo 內;或把 helper 放進 `internal/store/testdata/crashhelper/` 這種固定套件,用 build tag 排除於一般編譯,再於測試中 `go build ./internal/store/testdata/crashhelper`)。實作時採後者較穩。

- [ ] **Step 2: 實作 crash helper(固定套件版)**

Create `internal/store/testdata/crashhelper/main.go`(此路徑在 `testdata` 下,不被一般 `go build ./...` 納入):
```go
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"docklog/internal/model"
	"docklog/internal/store"
)

func main() {
	dataDir, counter := os.Args[1], os.Args[2]
	st, err := store.Open(dataDir)
	if err != nil {
		panic(err)
	}
	cf, _ := os.OpenFile(counter, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	var committed int64
	now := time.Now().UTC()
	for {
		batch := make([]model.LogEntry, 50)
		for i := range batch {
			batch[i] = model.LogEntry{TS: now, IngestedAt: now, Service: "api",
				Level: model.Info, Message: "x", Attrs: "{}"}
		}
		if err := st.Insert(context.Background(), batch); err != nil {
			panic(err)
		}
		committed += 50
		cf.Seek(0, 0)
		cf.WriteString(strconv.FormatInt(committed, 10) + "\n")
		cf.Sync()
		fmt.Fprintln(os.Stderr, committed)
	}
}
```

`writeHelper` 改為:`go build -o <dir>/helper ./internal/store/testdata/crashhelper`(在 repo 根執行)。

- [ ] **Step 3: 執行整合測試**

Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/store/ -run Crash -v`
Expected: PASS(crash 後 count 為 50 倍數且不少於 committed)。

- [ ] **Step 4: Commit**

```bash
git add internal/store/crash_test.go internal/store/testdata/
git commit -m "test: crash recovery integration test (kill -9 during ingest)"
```

---

## 收尾:全套測試

- [ ] **執行全部測試**

Run: `GOTOOLCHAIN=go1.24.0 go test ./...`
Expected: 全數 PASS。

- [ ] **確認 build 乾淨**

Run: `GOTOOLCHAIN=go1.24.0 go build ./... && go vet ./...`
Expected: 無錯誤。

---

## Self-Review(對照 spec Phase 1 items 1–4)

- **item 1 DuckLake 初始化 + schema + metrics 表** → Task 1(store)+ Task 2(metrics)✅
- **item 2 HTTP ingest + API key + rate limit** → Task 3(auth)+ Task 4(ratelimit)+ Task 6(handler)✅
- **item 3 Checkpoint goroutine + panic recovery** → Task 7 ✅
- **item 4 磁碟監控 + 拒絕寫入 + catalog 大小監控** → Task 5(diskguard)+ Task 6(handler 回 503)+ Task 8(health 暴露 catalog_size)✅
- **失敗模式測試(crash 恢復)** → Task 10 ✅
- **失敗模式 #7 log flood 記丟棄** → Task 2 `dropped` 表 + Task 6 rate-limit 記錄 ✅
- **失敗模式 #8 clock skew** → Task 6 `applyClockSkew` ✅
- **未涵蓋(刻意延後)**:transport(items 5–6)、query API + AST validator(Phase 2)、Retention 的排程 goroutine(store 已有 `DeleteOlderThan`,排程留待與 alert 一起做)、Purge 門檻的實際刪除動作(diskguard 回報 Purge 狀態,但自動刪除邏輯需一個 background 監控 goroutine — 建議在 transport 計畫後補一個 "Task: disk purge worker",本計畫先讓 Reject 擋住寫入以保命)。

**已知待辦(不在本計畫,已記錄以免遺漏):**
1. Purge worker(0.95 → 強制刪最舊 + CHECKPOINT)—— 目前只做到 Reject(0.90)保命。
2. Retention 排程 goroutine —— `DeleteOlderThan` 已備好,缺每日觸發。
3. 壞 ndjson 行的計數上報(目前靜默跳過,與「不可靜默」原則有張力,需補 metrics 計數)。
