# ducklog

AI-first log 觀測:app 用 Go slog transport 送 log 進 **VictoriaLogs**（儲存 / retention /
LogsQL / 內建 web UI），我們的 **stdio MCP server**（`cmd/ducklog-mcp`）把 VL 包成一組
防幻覺的工具給 Claude Code 用。

```
apps ──slog RemoteHandler──▶ VictoriaLogs ◀──LogsQL── ducklog-mcp ◀──stdio── Claude Code
       (client/)              (storage/UI)             (MCP tools)
```

設計背景見 [`docs/superpowers/specs/2026-07-13-victorialogs-mcp-pivot-design.md`](docs/superpowers/specs/2026-07-13-victorialogs-mcp-pivot-design.md)。

## 元件

- **`client/`** — 純 stdlib 的 Go transport（一個 `slog.Handler`）。app 引入它把 log 以
  NDJSON POST 進 VL。指向 VL 的設定見 [`client/README.md`](client/README.md)。
- **VictoriaLogs** — 外部 OSS 單一執行檔，負責儲存、retention、LogsQL 查詢與 vmui。
- **`cmd/ducklog-mcp`** — stdio MCP server，把 VL 包成 4 個 AI-first 工具給 Claude Code。

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

開 Basic Auth（選用）:

    victoria-logs-prod -storageDataPath=./data/victoria-logs \
      -retentionPeriod=30d -httpListenAddr=127.0.0.1:9428 \
      -httpAuth.username=admin -httpAuth.password=secret

內建 web UI（vmui）: <http://127.0.0.1:9428/select/vmui>

### 2. 讓 app 送 log 進 VL

在 app 裡把 slog transport 的 `RemoteHandler` `Endpoint` 指向 VL 的 jsonline ingest:

```
http://<vl-host>:9428/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service
```

wire 格式與欄位映射的細節見 [`client/README.md`](client/README.md)。

### 3. Build 並在 Claude Code 註冊 MCP server

```bash
go build ./cmd/ducklog-mcp
```

`ducklog-mcp` 是 stdio server，由 Claude Code 啟動（不用手動跑）。註冊:

```bash
claude mcp add ducklog --env VL_URL=http://127.0.0.1:9428 -- /abs/path/to/ducklog-mcp
```

或等價的 JSON 設定:

```json
{
  "mcpServers": {
    "ducklog": {
      "command": "/abs/path/to/ducklog-mcp",
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
| `VL_USERNAME` | （空） | VL 開 Basic Auth 時的使用者;空則不帶 auth |
| `VL_PASSWORD` | （空） | VL Basic Auth 密碼 |

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

## 告警(alert,選用)

告警走獨立 stack,預設不啟,與手動 binary 流程解耦。

1. `cp .env.example .env`,填入 `DUCKLOG_ALERT_WEBHOOK_URL`、`DUCKLOG_TELEGRAM_BOT_TOKEN`、`DUCKLOG_TELEGRAM_CHAT_ID`;VL 若開 Basic Auth 另填 `DUCKLOG_VL_USERNAME/PASSWORD` 並取消 `docker-compose.alert.yml` 內 vmalert 的 basicAuth 兩行註解。若要在本 stack 自帶的 `victorialogs` service 上實際開 auth,還需在 `docker-compose.alert.yml` 的 `victorialogs` service command 加上 `-httpAuth.username=... -httpAuth.password=...`(值對應 `.env` 的 `DUCKLOG_VL_USERNAME/PASSWORD`)。
2. 渲染 Alertmanager config:`scripts/render-alertmanager-config.sh .env`
3. 起 stack:`docker compose --env-file .env -f docker-compose.alert.yml up -d`
4. rule 在 `alert-rules/`(門檻/關鍵字/interval 改這裡即可,不需重建 image)。

**Rule 集**:`HighErrorRate`(每 service 1m error 數 > 50,固定門檻非突增)、`FatalPattern`(panic/OOM/SIGSEGV 出現即報)。

**端到端自測**:`scripts/test-alert-e2e.sh`(驗 ingest → firing → webhook)。

**Telegram 手動驗收清單**(不進自動測試):
- [ ] Telegram 真的收到告警訊息
- [ ] 告警解除時 `send_resolved` 通知會來
- [ ] VL 開 Basic Auth 時 vmalert 仍能查 VL(`/api/v1/rules` 有 group)

### service 靜默偵測

`alert-rules/silence.yml` 預設空(不啟)。要監控某 service 靜默,取消範例註解、移除 `rules: []` 的 `[]`,每個 service 加一條:

```yaml
- alert: ServiceSilent
  expr: 'service:=<你的service> _time:10m | stats count() as n | filter n:0'
  for: 0s
  labels: { severity: critical, service: <你的service> }
  annotations: { summary: 'service <你的service> 已靜默 ≥10m' }
```

- `_time:` 是靜默判定窗(預設 10m,依 service 產 log 頻率各自調;低頻/批次型調大或不納入)。
- ⚠️ service 名必須「存在過」(曾產過 log)。打錯名字或監控從未存在的 service → 不分組 stats 恆回 0 → **恆 firing**。
- ⚠️ 冷啟/重啟瞬時誤報:vmalert 剛啟動、VL 剛清空、或某 service 剛部署還沒產第一批 log 時,`_time:10m | filter n:0` 會因窗內 0 筆而短暫 firing——屬預期行為(service 確實還沒 log)。若不想要重啟後的瞬時雜訊,可把 rule 的 `for:` 調大(例如 `for: 2m`)讓它持續靜默一段時間才告警,或靠 Alertmanager 的 `group_wait`/`repeat_interval` 收斂。
- 自測機制:`scripts/test-silence-e2e.sh`(短窗驗 dead firing + alive 對照)。

## 已知限制（v1）

- **兩個 process**: VL + MCP server,兩者都是低運維的單一執行檔。
- **Basic Auth（選用）**: VL 以 `-httpAuth.username/-httpAuth.password` 開一組共享 Basic Auth（涵蓋 ingest 與 query）。寫入端設 `RemoteConfig.Username/Password`,MCP 端設 `VL_USERNAME/VL_PASSWORD`。三端留空則維持無 auth。
  未開 auth 時仍請把 VL 綁 localhost / 內網,勿對外曝露。
- **caveat**: Basic Auth 在純 HTTP 上是 base64、非加密;請在受信任網段使用,需端到端加密再開 VL `-tls`。
- **MCP 只有 stdio transport**: 由 Claude Code 本機啟動,無遠端 / HTTP MCP。

## 開發

```bash
go build ./... && go vet ./...
cd client && go test -short ./...            # 單元測試（e2e 會 skip）
VL_BINARY=/path/to/victoria-logs-prod go test ./...   # 含整合 / e2e
```
