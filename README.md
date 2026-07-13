# docklog

AI-first log 觀測:app 用 Go slog transport 送 log 進 **VictoriaLogs**（儲存 / retention /
LogsQL / 內建 web UI），我們的 **stdio MCP server**（`cmd/docklog-mcp`）把 VL 包成一組
防幻覺的工具給 Claude Code 用。

```
apps ──slog RemoteHandler──▶ VictoriaLogs ◀──LogsQL── docklog-mcp ◀──stdio── Claude Code
       (client/)              (storage/UI)             (MCP tools)
```

設計背景見 [`docs/superpowers/specs/2026-07-13-victorialogs-mcp-pivot-design.md`](docs/superpowers/specs/2026-07-13-victorialogs-mcp-pivot-design.md)。

## 元件

- **`client/`** — 純 stdlib 的 Go transport（一個 `slog.Handler`）。app 引入它把 log 以
  NDJSON POST 進 VL。指向 VL 的設定見 [`client/README.md`](client/README.md)。
- **VictoriaLogs** — 外部 OSS 單一執行檔，負責儲存、retention、LogsQL 查詢與 vmui。
- **`cmd/docklog-mcp`** — stdio MCP server，把 VL 包成 4 個 AI-first 工具給 Claude Code。

## 快速上手

需要 Go 1.24（系統 `go` 即可；`GOTOOLCHAIN=go1.24.0` 只是可選的重現性 pin，非必要）。

### 1. 取得並啟動 VictoriaLogs

下載 OSS 單一執行檔（**不是** `-enterprise` 版）:
[VictoriaLogs releases](https://github.com/VictoriaMetrics/VictoriaLogs/releases/latest) →
`victoria-logs-linux-amd64-v*.tar.gz` → 解出 `victoria-logs-prod`。

用 helper 腳本啟動（綁 localhost、retention 30 天）:

```bash
VL_BINARY=/path/to/victoria-logs-prod scripts/run-dev.sh vl
```

或直接跑:

```bash
victoria-logs-prod -storageDataPath=./data/victoria-logs \
  -retentionPeriod=30d -httpListenAddr=127.0.0.1:9428
```

內建 web UI（vmui）: <http://127.0.0.1:9428/select/vmui>

### 2. 讓 app 送 log 進 VL

在 app 裡把 slog transport 的 `RemoteHandler` `Endpoint` 指向 VL 的 jsonline ingest:

```
http://<vl-host>:9428/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service
```

wire 格式與欄位映射的細節見 [`client/README.md`](client/README.md)。

### 3. Build 並在 Claude Code 註冊 MCP server

```bash
go build ./cmd/docklog-mcp
```

`docklog-mcp` 是 stdio server，由 Claude Code 啟動（不用手動跑）。註冊:

```bash
claude mcp add docklog --env VL_URL=http://127.0.0.1:9428 -- /abs/path/to/docklog-mcp
```

或等價的 JSON 設定:

```json
{
  "mcpServers": {
    "docklog": {
      "command": "/abs/path/to/docklog-mcp",
      "env": { "VL_URL": "http://127.0.0.1:9428" }
    }
  }
}
```

`scripts/run-dev.sh mcp` 會 build 並印出上述指令（含絕對路徑）。

## 設定

MCP server 目前只讀一個環境變數:

| 變數 | 預設 | 作用 |
| --- | --- | --- |
| `VL_URL` | `http://127.0.0.1:9428` | 要連的 VictoriaLogs 位址 |

啟動時會 Ping VL,不可達則 fail fast。查詢 timeout 目前硬編 30s。

## MCP 工具

四個工具都回傳統一的 envelope，套用**防幻覺契約**:回應 token-bounded（≤ ~4000
tokens）；超量時**明確降級**（full → 抽樣 → count-only）並標 `downgraded` / `reason` /
`hint`；`truncated` / `total_matched` 誠實回報；每個回應帶 `schema_hint`；**絕不靜默丟資料**。

| 工具 | 參數 | 用途 |
| --- | --- | --- |
| `summarize_errors` | `service?`, `time_range` | AI 主入口:把 error 以 fingerprint 分群成 pattern + 次數 |
| `get_trace` | `trace_id` | 拉出某 trace 的完整 log |
| `search_logs` | `query`, `time_range`, `limit?` | 結構化 LogsQL 查詢,含誠實的 truncation 標示 |
| `compare_periods` | `t1`, `t2`, `service?` | 比較兩時段,找新出現 / 暴增的 error pattern |

可查欄位: `_time`, `service`, `level`, `trace_id`, `_msg`（transport 送的 `attrs` 會被 VL
攤平成 `attrs.<key>`）。時間範圍如 `1h` / `30m` / `2h`。

## 已知限制（v1）

- **兩個 process**: VL + MCP server,兩者都是低運維的單一執行檔。
- **無 auth**: 靠受信任的網路 —— 把 VL 綁在 localhost（或內網），不要對外曝露。
- **MCP 只有 stdio transport**: 由 Claude Code 本機啟動,無遠端 / HTTP MCP。

## 開發

```bash
go build ./... && go vet ./...
cd client && go test -short ./...            # 單元測試（e2e 會 skip）
VL_BINARY=/path/to/victoria-logs-prod go test ./...   # 含整合 / e2e
```
