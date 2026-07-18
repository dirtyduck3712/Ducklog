# run_logsql 逃生 tool 設計

日期：2026-07-18
狀態：已通過 brainstorming，待實作

## 目的

在 4 個罐頭 MCP tool（`summarize_errors` / `search_logs` / `get_trace` / `compare_periods`）都不合用時，提供一個**受控的 raw LogsQL 通道**，讓 AI 送任意 LogsQL pipeline（含 pipe）。

定位是**逃生口 / 最後手段**，不是主入口——description 需主動引導 AI 優先走專用 tool，以保住專用 tool 的護欄價值（fingerprint 分群等）。

## 背景

- `vl.Client.Query(logsql, limit)` 底層打 VL 唯讀端點 `/select/logsql/query`，本來就能跑任意 LogsQL（含 pipe）。**無寫入/刪除風險**，唯一風險是「無界掃描」的資源成本。
- `search_logs` 目前**明確擋掉 pipe**（`"把 pipe 邏輯交給對應的專用工具"`），逃生 tool 正是補這個缺口。
- 全專案 tool 共用兩條護欄：`requireRange`（強制時間範圍）+ `bound.BoundRaw`（token 上限降級鏈）。逃生 tool 沿用兩者，維持一致契約。

## 元件

### 1. `internal/tools/runlogsql.go`

```
RunLogsQL(ctx context.Context, c *vl.Client, query, timeRange string, limit int) bound.Envelope
```

流程：
1. `requireRange(timeRange)` — 空時間範圍回 `MISSING_TIME_RANGE`（沿用既有）。
2. 空 query → `bound.Err("MALFORMED_QUERY", ...)`。
3. `limit <= 0 || limit > 1000` → 預設 100（沿用 `defaultLimitCap`）。
4. **注入時間過濾**（見「關鍵細節」）。
5. `c.Query(ctx, injected, limit)` → 失敗回 `bound.Err("QUERY_FAILED", err.Error(), ...)`。
6. `bound.BoundRaw(rows, SchemaHint)` 包裝。
7. 若 `len(rows) == limit` → `Truncated = true`（命中 limit 即視為截斷）。

### 2. `internal/mcpserver/server.go`

註冊第 5 個 tool `run_logsql`：
- `query`（required）— 任意 LogsQL pipeline。
- `time_range`（required）。
- `limit`（optional，預設 100）。
- description 明講：「逃生口：僅在 summarize_errors / search_logs / get_trace / compare_periods 都不合用時才用。」

### 3. `internal/tools/runlogsql_test.go`

沿用 `vltest`（起真 VL + ingest）。涵蓋：
- pipe 查詢可正常執行（如 `... | stats by (service) count()`）。
- 時間範圍強制（空 `time_range` 被拒）。
- 空 query 被拒。
- 命中 limit 標 `Truncated`。
- 大輸出走 `BoundRaw` 降級（sampled / count-only）。

## 關鍵細節：時間過濾注入

`_time:` 在 LogsQL 是 **filter**，必須放在**第一個 `|` 之前**。`search_logs` 因擋掉 pipe，可直接 `query + " " + _time:1h`；但逃生 tool 的 query 可含 pipe，append 到 pipeline 尾端會壞掉。

做法：以**第一個 `|`** 切分，把 `_time:X` 注入 head 段：

```
"error | stats by (service) count()"  →  "error _time:1h | stats by (service) count()"
"level:=error"（無 pipe，head 即全部）  →  "level:=error _time:1h"
```

若 AI 在 query 裡已自帶 `_time:`，兩者並存；LogsQL 對多個 `_time:` 取交集，語意安全（brainstorming 決定 A）。

## 刻意不做（YAGNI）

- **不算獨立 Count**：`search_logs` 會先 `Count` 再 `Query`，但對任意 pipeline，`| stats count()` 疊在使用者 pipeline 後語意含糊（輸出列數 vs matched log 數）。改用「命中 limit 即標 `Truncated`」的誠實訊號，不硬湊意義不明的 `total_matched`。
- **不做輸出形態偵測**：`BoundRaw` 的抽樣是為 raw logs 設計，對「上千 group 的 stats」抽樣語意略怪，但降級鏈會誠實標 `downgraded/sampled/truncated`，AI 不會被誤導。偵測聚合輸出改走別的降級是推測性複雜度，不做（brainstorming 決定 A）。
- **不做輸入白名單/黑名單**：端點唯讀，無破壞性操作可擋；逃生口刻意不限制 LogsQL 語法。

## 影響範圍

三處改動（新增 1 檔、改 1 檔、新增 1 測試檔），無新依賴，延續既有 `bound` / `vl` 契約。
