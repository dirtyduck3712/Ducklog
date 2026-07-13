# Phase 1b — Go Client Transport 設計

**範圍**:`tech.md` 的 Client Transport(Go 部分)+ Trace ID 傳遞(items 5–6 的 Go 半邊)。
**不含**:Node pino transport(→ Phase 1c)、完整 OpenTelemetry、Prometheus exporter。

## 目標與非目標

**目標**
- 一個 `slog.Handler`,讓 Go 服務用標準 `log/slog` 就能把 log 送進 ducklog server。
- 日誌系統掛掉**絕不拖垮應用**(失敗模式 #6):非阻塞、timeout、有限重試、熔斷、stdout fallback。
- Trace ID 第一版就有:middleware 讀/生成、跨服務傳遞、handler 自動帶上。

**非目標**
- 高吞吐最佳化(應用端 log 量遠低於 server 承載)。
- 保證不丟(rate limit / channel 滿本來就會丟,但要有 stdout 兜底)。
- 與 server 共用 Go 型別(用 wire JSON 契約解耦)。

## 模組與相依

- **獨立模組** `client/`,自己的 `go.mod`,module path `ducklog/client`。
- **零外部相依**:只用 stdlib(`net/http`、`log/slog`、`context`、`crypto/rand`、`encoding/json`、`sync`、`time`、`bufio`）。
- **不 import server 的 `ducklog/...`**。client 自定義 wire struct;共享的是 JSON 線上契約,不是 Go 型別。
- 下游服務 import 它**不會**揹上 go-duckdb / CGO / go1.24 toolchain 要求。

## Wire 契約(與 server /ingest 對齊)

每行一筆 JSON(`application/x-ndjson`):
```json
{"ts":"2026-07-13T10:23:45Z","service":"api","level":"error","trace_id":"<canonical-uuid>","message":"...","attrs":{}}
```
- `ts`:RFC3339 UTC(client 產生時間)。
- `level`:小寫字串 `debug|info|warn|error`(server 現在大小寫不敏感,但送 canonical 小寫)。
- `trace_id`:**canonical UUID**(server 是 UUID 欄位);無 trace 時省略/空。
- `attrs`:slog 的 attributes 攤平成 JSON object。

## 元件

| 檔案 | 職責 | 對外 interface |
|---|---|---|
| `wire.go` | wire struct + ndjson 編碼 + slog.Level→level 字串 | `entry` struct(internal) |
| `config.go` | `RemoteConfig` + 預設值 | `RemoteConfig` |
| `handler.go` | 實作 `slog.Handler`,非阻塞入 queue + stdout 雙寫 | `NewRemoteHandler(RemoteConfig) *RemoteHandler`；`(*RemoteHandler) Close() error` |
| `sender.go` | 背景 goroutine:批次聚合 + HTTP POST + 重試 + fallback | internal |
| `breaker.go` | 熔斷器(注入 clock) | internal `breaker` |
| `trace.go` | trace id 生成(canonical UUIDv4)+ context 存取 | `ContextWithTraceID`、`TraceIDFromContext`、`NewTraceID` |
| `middleware.go` | HTTP server middleware + outbound 傳遞 helper | `Middleware(next http.Handler) http.Handler`、`InjectTraceID(req, ctx)` |

### RemoteConfig

```go
type RemoteConfig struct {
    Endpoint      string        // e.g. http://logd:8080/ingest
    APIKey        string        // Bearer(ingest scope)
    Service       string        // 這個服務的名字
    Level         slog.Leveler  // 最低等級,預設 Info
    BatchSize     int           // 預設 100
    FlushInterval time.Duration // 預設 1s
    QueueSize     int           // channel 容量,預設 10000
    Fallback      io.Writer     // 預設 os.Stdout(必填語意:雙寫)
    HTTPClient    *http.Client  // 預設 timeout 2s
    // 熔斷 / 重試預設值寫死在 sender/breaker,不對外(YAGNI)
}
```

### slog.Handler 行為

- `Enabled(ctx, level)`:比 `Level` 低就 false。
- `Handle(ctx, record)`:
  1. 轉成 wire `entry`(ts=record.Time.UTC、level 字串、message、attrs 攤平)。
  2. `trace_id` 從 `TraceIDFromContext(ctx)` 取(slog 會把 log 呼叫的 ctx 傳進來)。
  3. **雙寫**:寫一份到 `Fallback`(stdout);同時**非阻塞**送進 queue(`select{ case q<-entry: default: atomic drop++ }`)。
  4. 絕不回傳阻塞 —— `Handle` 永遠快速返回。
- `WithAttrs` / `WithGroup`:標準 slog handler 累積 attrs / group prefix(共享底層 sender)。

### 失敗模式 #6 五要件(sender.go + breaker.go)

1. **非阻塞**:queue 滿 → 丟棄 + 累計 drop 數(`Close` 時或定期 log 一次到 stderr,不靜默)。
2. **timeout**:`HTTPClient` 預設 `Timeout: 2s`。
3. **有限重試**:單批最多 2 次重試,指數退避(注入 sleep/clock 以利測試)。
4. **熔斷**:連續 5 次送出失敗 → open;open 期間直接跳過 HTTP(只靠 stdout);30s 後 half-open 送一批探測,成功則 close、失敗則續 open。
5. **fallback**:因為第一版雙寫,stdout 永遠有;HTTP 是「盡力而為」。

### Close / Flush(graceful shutdown)

- `Close()`:停止收新 entry、把 queue 排空、送出最後一批(有 timeout 上限,如 5s)、回報總 drop 數。
- 應用在 shutdown 時 `defer handler.Close()`,避免掉最後一批。

### Trace ID(trace.go + middleware.go)

- `NewTraceID() string`:`crypto/rand` 生 canonical UUIDv4。
- `ContextWithTraceID(ctx, id) ctx` / `TraceIDFromContext(ctx) (string, bool)`:context key(未匯出型別)。
- `Middleware`:HTTP server 中介層 —— 讀入站 `X-Trace-Id`,有就用(**驗證是 canonical UUID,不合法就生新的**,避免污染),沒有就 `NewTraceID`,塞進 request context。回應也帶上 `X-Trace-Id` 方便除錯。
- `InjectTraceID(req, ctx)`:對外送 HTTP 前,把 ctx 裡的 trace_id 設到 `X-Trace-Id` header,讓 trace 跨服務延續。

## 測試策略

- **httptest.Server** 當假 endpoint,可切換:成功 / 500 / hang(睡超過 timeout)/ 計數收到幾批。
- **注入 clock**:退避 sleep 與熔斷計時用可控 time,測試 deterministic、不靠 wall-clock。
- **非阻塞**:把 QueueSize 設 1、塞爆,斷言 `Handle` 不阻塞且 drop 被計數。
- **雙寫 fallback**:斷言 `Fallback` buffer 收到每一筆(即使 HTTP 全失敗)。
- **重試 + 熔斷**:endpoint 連續失敗,斷言重試次數、5 次後 open、30s 後 half-open 探測。
- **timeout**:endpoint hang,斷言 2s 後放棄、不阻塞 sender。
- **Close 排空**:入隊數筆後 `Close`,斷言假 endpoint 收到全部(在 timeout 內)。
- **trace**:middleware 讀入站 header / 生成 / 驗證壞值換新;handler 從 ctx 帶出 trace_id 到 wire。
- **端到端(選配,一個)**:啟真 ducklog server binary(來自 Phase 1a),slog 打幾筆,查 server 存到了 —— 跨模組,標 `-short` 可略。

## 明確排除(YAGNI)
- Node pino(Phase 1c)、OpenTelemetry、動態 config 熱更新、可關的雙寫(第一版固定雙寫)、gzip 壓縮、TLS 客製。
