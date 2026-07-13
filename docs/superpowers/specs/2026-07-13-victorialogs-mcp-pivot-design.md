# 轉向設計:VictoriaLogs backend + 薄 MCP 工具層

## 為什麼轉向(背景)

原始 `tech.md` v4 賭「DuckLake v1.0(2026-04)production-ready」。Phase 0–1 實作 + scale/soak 驗證後發現:**可用工具鏈最新只到 DuckDB 1.4.1 + pre-1.0 DuckLake**,有多個粗糙邊緣:

- **空間回收壞掉**:連續 retention 下 `cleanup_old_files` 只把檔排進刪除佇列、從不實體刪除 → 磁碟隨歷來寫入量無限爆長(#1 死法)。需自寫檔案 GC 才能繞過(Codex 獨立驗證)。
- **時間範圍查詢無剪枝**:`WHERE ts > now()-1h`(日誌最常見查詢)在 150M 筆要全掃 ts 欄位 ~1.2s,partition(標準解)的 CHECKPOINT 直接 crash。
- SQLite catalog CHECKPOINT 壞、`flush_inlined_data` 壞等。

**關鍵洞察**:AI-first 的價值在 **MCP 工具層(backend-agnostic)**,不在儲存基底。而規格自建的唯一理由「LogsQL 不是 SQL → AI 幻覺」**不成立** —— AI 走**我們的 MCP tools**,tools 由我們用 backend 語言實作,AI 看不到 LogsQL。

**VictoriaLogs spike 驗證(20 萬筆實測)**:VL 原生給了全部 AI 原語且快 —— 時間範圍瞬回(~0ms vs DuckLake 1.2s)、`get_trace` 欄位過濾、全文快、而 **`collapse_nums | stats by (_msg)` 一條 query 就是 `summarize_errors` fingerprint 分群**(規格說「核心、要自己實作 Drain」的那塊,VL 內建)。retention/compaction/scale 全內建成熟。VL 也是單一 Go binary、低運維。

**決定:VL 扛儲存,我們只建 MCP 工具層。**

## 範圍

- **建**:MCP server(4 tools + 防幻覺契約)、transport 重指 VL、運行腳本 + 文件。
- **不建(VL 給)**:儲存 / retention / compaction / ingest API / LogsQL 查詢 / web UI(vmui)/ scale。
- **丟**:自建 DuckLake 儲存層(`internal/store`、metrics、checkpoint、diskguard、ingest handler、health、server config、cmd/docklog)—— 沉沒成本,但那是水管。
- **v1 明確不含(YAGNI)**:Node transport(later)、SSE MCP(v1 只 stdio)、custom web UI(用 vmui)、auth(信任網路)、alert(用 VL 的 vmalert 生態)。

## 架構

```
apps ──(現有 Go slog transport,重指到 VL 的 /insert/jsonline + 欄位映射)──▶
         VictoriaLogs  [ingest / 存 / 30d retention / compaction / LogsQL / vmui]   (綁 localhost/內網,v1 無 auth)
              ▲ localhost LogsQL HTTP (127.0.0.1:9428)
         我們的 MCP server (Go, mcp-go, stdio) ◀── Claude Code
              tools: summarize_errors · get_trace · search_logs · compare_periods
              + token-bounding / 明確降級 / schema_hint / 絕不靜默截斷
```

## 元件

### 1. VictoriaLogs(外部,我們只設定/運行)

- 單 binary(OSS `victoria-logs-prod`,~23MB)。旗標:`-storageDataPath`、`-retentionPeriod=30d`、`-httpListenAddr=127.0.0.1:9428`。
- Ingest:`POST /insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service`
- Query:`GET/POST /select/logsql/query`(回 NDJSON)、`/select/logsql/stats_query`。
- Web UI:內建 vmui(`/select/vmui`)。

### 2. Transport(重用 `client/`,小改)

- 現有 Go slog `RemoteHandler` 只需把 `Endpoint` 指到 VL 的 jsonline endpoint(含 `_time_field=ts&_msg_field=message&_stream_fields=service` query 參數)。**wire 格式不變** `{ts, service, level, trace_id, message, attrs}`。
- 失敗模式 #6 韌性(非阻塞 / 2s timeout / 重試 / 熔斷 / stdout fallback)照舊有效、trace ID middleware 照舊。
- **待確認細節(plan spike)**:`attrs` 是巢狀 JSON object;VL jsonline 對巢狀欄位的處理(攤平 / 存為 JSON 字串)—— v1 `attrs` 多為 `{}`,影響小,但要在 plan 第一步用真 VL 確認一筆帶 attrs 的 log 能正確 ingest + 查回。

### 3. MCP server(新建;Go、`github.com/mark3labs/mcp-go`、stdio transport)

核心 = **薄 LogsQL wrapper + 防幻覺契約**。透過 HTTP 打 VL 的 LogsQL API(localhost)。

**Tools**

| Tool | LogsQL(已 spike 驗證的形態) | 用途 |
|---|---|---|
| `summarize_errors(service?, time_range, limit=20)` | `{svc} level:=error _time:{R} \| collapse_nums \| stats by (_msg) count() as n \| sort by (n desc) \| limit {N}` | **AI 主入口**:fingerprint pattern + 次數 |
| `get_trace(trace_id)` | `trace_id:={id} \| sort by (_time) \| limit {cap}` | 完整 trace |
| `search_logs(query, time_range, limit)` | 結構化 LogsQL filter + `_time:{R}` + limit;另跑 `\| stats count()` 取 total | 結構化查詢 + truncation 標示 |
| `compare_periods(t1, t2, service?)` | 兩窗各跑 fingerprint stats,diff 出「新出現 / 暴增」的 pattern | 找異常 spike |

**防幻覺契約(規格最強調、真正屬於我們的價值)**

- 每個 tool 回傳 **≤ ~4000 tokens**(估算,超過就降級)。
- **降級鏈**:`raw logs → sampled logs → fingerprint summary → count only`;降級時**明確**回 `{"downgraded": true, "reason": "...", "returned": "fingerprint_summary", "hint": "..."}`,**絕不靜默**。
- **truncation 永遠標示**:`{"total_matched": N, "returned": M, "truncated": true, "next_cursor": ...}`。
- **每次回傳附 `schema_hint`**(可用欄位 + LogsQL 存取提示),減少 AI 猜。
- 錯誤明確:`{"status":"error","error_code":"QUERY_TIMEOUT","message":"...","hint":"..."}`。

**查詢安全(LogsQL 資源上限)**

- 強制 time range(tool 參數必含或補預設窗)。
- 強制 server 端 limit 上限。
- Query timeout(context / VL `-search.maxQueryDuration`)。
- VL 本身有並發與資源保護,不必自建。

## 失敗模式(轉向後哪些還是我們的)

- **儲存類失敗(crash / 磁碟 / checkpoint / retention)→ VL 的責任**(成熟,不用我們管)。
- **仍是我們的**:
  - Transport 韌性(已建於 `client/`)。
  - MCP token-bounding / 防幻覺(核心)。
  - **VL 掛掉時**:MCP tool 回明確 error(不靜默腦補);transport 有 stdout fallback,log 不消失。
  - Rate limit:v1 靠 VL 自身 ingest 保護;需要再加 vmauth。

## 測試要求

- **MCP tools 整合測試**(對真 VL):啟 VL(spike 已可跑)→ ingest 樣本 → 每個 tool 回正確結構;`summarize_errors` 回 fingerprint patterns;`get_trace` 回該 trace 全部。
- **防幻覺契約單元測試**:塞超量結果 → 驗 `downgraded:true` + reason;truncation 標 `truncated:true` + total/returned;絕不靜默。
- **Transport 重指 VL 端到端**:repoint → slog 送幾筆 → VL 查得到(欄位映射正確、trace_id 可 get_trace)。
- **VL down**:MCP tool 回明確 error_code,不腦補空結果。

## 已知取捨(接受)

- **2 個 process**(VL + 我們的 MCP),不再是單 binary —— 但兩者都是低運維單檔。
- **v1 無 auth**(信任網路 / VL 綁內網)。需要時加 vmauth。
- `collapse_nums` 偶有 over-collapse 小瑕疵(有選項可調 / 可後處理)。
- **依賴 VL roadmap** —— 但成熟、活躍、廣用(這正是轉向的重點:用成熟度換掉 pre-1.0 DuckLake 的維護負擔)。
- 少了「完整 SQL 逃生門」—— 但 LogsQL 夠表達(stats / filter / pipe);需要時可加一個受限的 raw-LogsQL tool。

## 沉沒成本處置

- **保留**:`client/` transport 模組(repoint 即可重用)、AI 工具層的**設計**(summarize/get_trace/token-bounding 概念照搬)。
- **移除**:`internal/*`(store, metrics, auth, ratelimit, diskguard, checkpoint, ingest, health, config)、`cmd/docklog`。plan 會有一個獨立 task 乾淨移除,git 歷史保留(需要時 revert 得回來)。
