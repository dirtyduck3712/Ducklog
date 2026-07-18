# ducklog 接入指南（給要對接的工程師）

把一個服務的 log 送進 ducklog（→ VictoriaLogs），並讓 AI 透過 MCP 查詢。
架構:`你的服務 ──日誌傳輸──▶ VictoriaLogs ◀──LogsQL── ducklog-mcp ◀── Claude Code`。

前提:VictoriaLogs 已在跑(見 [README](../README.md) 的「取得並啟動 VictoriaLogs」)。以下假設它在 `http://127.0.0.1:9428`。

---

## 0. 先讓你的專案抓得到 ducklog（模組解析)

ducklog 的 transport 模組已發佈在公開 repo,直接 `go get`:
```
go get github.com/dirtyduck3712/ducklog/client@latest
```
用 zap 的服務再加:
```
go get github.com/dirtyduck3712/ducklog/zapsink@latest
```
import 用完整路徑(見下方各節)。

> Go 版本:`client` 需 Go ≥ 1.22、`zapsink` 需 Go ≥ 1.24。

> 本機開發 ducklog 自身時,zapsink 以 `replace => ../client` 連本地 client;這只影響 repo 內開發,不影響 `go get` 的消費者。

---

## 1. 服務用 stdlib `log/slog`(最單純)

ducklog 的 transport 本身就是一個 `slog.Handler`。在服務啟動處:

```go
import "github.com/dirtyduck3712/ducklog/client"

if vlURL := os.Getenv("DUCKLOG_VL_URL"); vlURL != "" {
    h := client.NewRemoteHandler(client.RemoteConfig{
        Endpoint: vlURL + "/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service",
        Service:  "my-service",   // 這個名字就是 LogsQL 裡的 service 欄位
        Fallback: os.Stderr,      // VL 不可達時的安全網(永遠雙寫,格式為 NDJSON)
    })
    defer h.Close()               // 關閉時排空 queue(上限 5s)—— 一定要
    slog.SetDefault(slog.New(h))
}
```
之後所有 `slog.Info/Error(...)` 自動進 VL,現有程式碼一行不用改。

---

## 2. 服務用 `go.uber.org/zap`

zap 與 slog 是不同介面,用 `github.com/dirtyduck3712/ducklog/zapsink` 這個轉接橋。若你用 `zap.Config.Build`:

```go
import (
    "github.com/dirtyduck3712/ducklog/client"
    "github.com/dirtyduck3712/ducklog/zapsink"
    "go.uber.org/zap"
    "go.uber.org/zap/zapcore"
)

func buildLogger() (*zap.Logger, func()) {
    cfg := zap.NewProductionConfig()
    var opts []zap.Option
    cleanup := func() {}
    if vlURL := os.Getenv("DUCKLOG_VL_URL"); vlURL != "" {
        opts = append(opts, zap.WrapCore(func(base zapcore.Core) zapcore.Core {
            core, stop := zapsink.Tee(base, client.RemoteConfig{
                Endpoint: vlURL + "/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service",
                Service:  "my-service",
                Fallback: io.Discard, // zap 的 console 已是本地安全網,避免 stdout 雙寫
            })
            cleanup = stop
            return core
        }))
    }
    logger, err := cfg.Build(opts...)
    if err != nil { panic(err) }
    return logger, cleanup   // 呼叫端要 defer cleanup()
}
```
zap 的既有 console 輸出保留,ducklog 是**額外**的 sink。`zapsink.NewCore(handler, minLevel)` 是更底層的通用版(自己建 core 時用)。

> zap 的 `logger.Fatal` 會 `os.Exit`,**跳過 defer** → cleanup 不執行、最後那筆可能沒排空。多數 log 靠 1 秒的定時 flush 已送出;若很在意最後一筆,shutdown 走 graceful(別用 Fatal)。

---

## 3. 建議:用 `DUCKLOG_VL_URL` 當開關

上面兩種都以 env `DUCKLOG_VL_URL` 為開關 —— **不設就完全維持原本 logging 行為**。好處:程式碼可以先合進去,VL 還沒佈署也不影響;要開就設環境變數。

---

## 4. 驗證有沒有進去

**vmui(瀏覽器)**:開 `http://127.0.0.1:9428/select/vmui`,查詢 `service:=my-service`。
> ⚠️ **最常見的「看不到」原因:vmui 右上角時間範圍預設只有 5 分鐘**。log 稍舊就被濾掉。把範圍拉大(Last 15 min / 1 hour)。

**LogsQL(curl)**:
```bash
curl -s "http://127.0.0.1:9428/select/logsql/query" \
  --data-urlencode 'query=service:=my-service | sort by (_time) desc | limit 10'
```

**手動灌一筆測試 log**(注意 content-type):
```bash
printf '{"ts":"%s","service":"my-service","level":"error","message":"boom"}\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
| curl -s -H "Content-Type: application/x-ndjson" \
  -X POST "http://127.0.0.1:9428/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service" --data-binary @-
```
> ⚠️ 手動 curl **一定要帶 `-H "Content-Type: application/x-ndjson"`**,否則 VL 回 200 卻靜默丟棄(transport 本身有帶,不受影響)。

---

## 5. 讓 AI 查(MCP)

```bash
cd ducklog && go build -o ~/bin/ducklog-mcp ./cmd/ducklog-mcp
claude mcp add ducklog --env VL_URL=http://127.0.0.1:9428 -- ~/bin/ducklog-mcp
```
之後在 Claude Code 問「summarize errors in my-service in the last hour」即可。四個工具:`summarize_errors`(主入口,fingerprint 分群)、`get_trace`、`search_logs`、`compare_periods`。

---

## 6. 常見坑 / FAQ

| 症狀 | 原因 / 解法 |
|---|---|
| vmui 看不到 log | 時間範圍預設 5 分鐘 → 拉大(見 §4) |
| 手動 curl 回 200 但查不到 | 少了 `Content-Type: application/x-ndjson`(見 §4) |
| 別人 build 失敗 `cannot find module github.com/dirtyduck3712/ducklog/client` | 用 `go get github.com/dirtyduck3712/ducklog/client@latest`(見 §0),別用本機 replace |
| 巢狀 `attrs` 查不到 | VL 會攤平成 dot 欄位:`attrs={"host":"x"}` → 查 `attrs.host:="x"` |
| log 有函式/連線物件等欄位 | transport 已自動把不可序列化的 attr 值轉字串,不會丟批(v1 已修) |
| zap Fatal 後最後一筆沒進 | Fatal=os.Exit 跳過 defer;shutdown 改走 graceful |
| 要 auth | v1 無 auth,把 VL 綁 `127.0.0.1`/內網;需要再加 vmauth |

## 欄位對照(wire → VL)

| wire 欄位 | VL 欄位 | 說明 |
|---|---|---|
| `ts`(RFC3339Nano) | `_time` | `_time_field=ts` |
| `message` | `_msg` | `_msg_field=message` |
| `service` | `service`(stream) | `_stream_fields=service` |
| `level` | `level` | 同名,可 `level:=error` 過濾 |
| `trace_id` | `trace_id` | 可 `trace_id:="<uuid>"` |
| `attrs.<k>` | `attrs.<k>` | 巢狀攤平 |
