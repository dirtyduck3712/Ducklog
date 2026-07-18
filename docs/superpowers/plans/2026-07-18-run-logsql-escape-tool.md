# run_logsql 逃生 tool Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新增第 5 個 MCP tool `run_logsql`——4 個罐頭 tool 不合用時的受控 raw LogsQL 逃生口。

**Architecture:** 沿用既有 `tools → vl → bound` 分層。新增 `RunLogsQL` 接任意 LogsQL pipeline，強制時間範圍（注入第一個 pipe 之前）、走 `BoundRaw` token 護欄，再於 `mcpserver` 註冊。

**Tech Stack:** Go（system go 1.24，純 stdlib + `github.com/mark3labs/mcp-go` v0.48.0）、VictoriaLogs LogsQL、`vltest` 整合測試。

## Global Constraints

- 純 stdlib，不新增依賴。
- 唯讀：只透過 `vl.Client.Query` 打 VL `/select/logsql/query`。
- 延續防幻覺契約：強制時間範圍、`BoundRaw` 降級、絕不靜默。
- 整合測試需 VL binary（`VL_BINARY` env），`-short` 會 skip。
- 設計來源：`docs/superpowers/specs/2026-07-18-run-logsql-escape-tool-design.md`。

---

### Task 1: `RunLogsQL` 核心 + 時間注入

**Files:**
- Create: `internal/tools/runlogsql.go`
- Test: `internal/tools/runlogsql_test.go`

**Interfaces:**
- Consumes: `bound.Err`、`bound.BoundRaw`（`internal/bound/bound.go`）；`vl.Client.Query`、`vl.TimeFilter`（`internal/vl`）；`requireRange`、`defaultLimitCap`、`SchemaHint`（`internal/tools/tools.go`）。
- Produces: `func RunLogsQL(ctx context.Context, c *vl.Client, query, timeRange string, limit int) bound.Envelope`——供 Task 2 註冊。

- [ ] **Step 1: 寫失敗測試**

寫入 `internal/tools/runlogsql_test.go`（沿用 `tools_test.go` 內既有的 `ingestErrors` / `ingestN` helper，同 package 可直接呼叫）：

```go
package tools

import (
	"context"
	"testing"
	"time"

	"ducklog/internal/vl"
	"ducklog/internal/vltest"
)

// pipe 查詢能跑，且證明 _time: 被注入到第一個 pipe「之前」。
// 若錯誤地 append 到 pipeline 尾端(... count() as n _time:1h),VL 會語法錯 → QUERY_FAILED。
func TestRunLogsQLRunsPipedQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVL(t)
	c := vl.New(base, 10*time.Second)
	ingestErrors(t, base) // 50 筆 service=api 的 error
	e := RunLogsQL(context.Background(), c, "level:=error | stats by (service) count() as n", "1h", 100)
	if e.Status != "ok" {
		t.Fatalf("pipe 查詢應成功: %+v", e)
	}
	rows, _ := e.Data.([]map[string]any)
	if len(rows) != 1 || str(rows[0]["service"]) != "api" || str(rows[0]["n"]) != "50" {
		t.Fatalf("聚合結果不符,want 1 列 service=api n=50,得: %v", rows)
	}
}

func TestRunLogsQLRequiresTimeRange(t *testing.T) {
	c := vl.New("http://127.0.0.1:1", time.Second)
	e := RunLogsQL(context.Background(), c, "level:=info", "", 100)
	if e.Status != "error" || e.ErrorCode != "MISSING_TIME_RANGE" {
		t.Fatalf("空 range 應拒: %+v", e)
	}
}

func TestRunLogsQLRejectsEmptyQuery(t *testing.T) {
	c := vl.New("http://127.0.0.1:1", time.Second)
	e := RunLogsQL(context.Background(), c, "   ", "1h", 100)
	if e.Status != "error" || e.ErrorCode != "MALFORMED_QUERY" {
		t.Fatalf("空 query 應拒: %+v", e)
	}
}

func TestRunLogsQLMarksTruncation(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVL(t)
	c := vl.New(base, 10*time.Second)
	ingestN(t, base, 500) // 500 筆 info(無 pipe 查詢路徑)
	e := RunLogsQL(context.Background(), c, "level:=info", "1h", 100)
	if !e.Truncated {
		t.Fatalf("命中 limit 應標 Truncated: %+v", e)
	}
	if e.ReturnedN > 100 {
		t.Fatalf("returned 應 ≤ limit,得 %d", e.ReturnedN)
	}
}
```

- [ ] **Step 2: 跑測試確認失敗**

Run: `go test ./internal/tools/ -run TestRunLogsQL`
Expected: FAIL，`undefined: RunLogsQL`（編譯錯）。

- [ ] **Step 3: 寫最小實作**

寫入 `internal/tools/runlogsql.go`：

```go
package tools

import (
	"context"
	"strings"

	"ducklog/internal/bound"
	"ducklog/internal/vl"
)

// RunLogsQL 是逃生口:執行任意 LogsQL pipeline(含 pipe),套用時間護欄與 BoundRaw。
// 僅在 4 個罐頭 tool 都不合用時使用。
func RunLogsQL(ctx context.Context, c *vl.Client, query, timeRange string, limit int) bound.Envelope {
	if e := requireRange(timeRange); e != nil {
		return *e
	}
	if strings.TrimSpace(query) == "" {
		return bound.Err("MALFORMED_QUERY", "query 不可為空",
			"傳一段 LogsQL,如 'level:=error | stats by (service) count()'")
	}
	if limit <= 0 || limit > defaultLimitCap {
		limit = 100
	}
	rows, err := c.Query(ctx, injectTimeFilter(query, timeRange), limit)
	if err != nil {
		return bound.Err("QUERY_FAILED", err.Error(), "確認 LogsQL 語法或縮小範圍")
	}
	e := bound.BoundRaw(rows, SchemaHint)
	if len(rows) == limit { // 命中 limit → 誠實標截斷(不硬湊語意含糊的 Count)
		e.Truncated = true
	}
	return e
}

// injectTimeFilter 把 _time: filter 注入第一個 pipe「之前」。
// _time: 是 filter,不能放在 pipeline 尾端;故以第一個 '|' 切分,注入 head 段。
func injectTimeFilter(query, timeRange string) string {
	head, rest, hasPipe := strings.Cut(query, "|")
	head = strings.TrimSpace(head) + " " + vl.TimeFilter(timeRange)
	if hasPipe {
		return head + " | " + strings.TrimSpace(rest)
	}
	return head
}
```

- [ ] **Step 4: 跑測試確認通過**

Run: `go test ./internal/tools/ -run TestRunLogsQL`
Expected: PASS（有 VL binary 時全過；無 VL 時 `RunsPipedQuery` / `MarksTruncation` skip，`RequiresTimeRange` / `RejectsEmptyQuery` 仍過）。

- [ ] **Step 5: Commit**

```bash
git add internal/tools/runlogsql.go internal/tools/runlogsql_test.go
git commit -m "feat(tools): run_logsql 逃生口 — 任意 LogsQL pipeline

時間過濾注入第一個 pipe 之前;命中 limit 即標 Truncated,不硬湊 Count。

Claude-Session: https://claude.ai/code/session_018usf3iQoLwJuvsjzPQmXDN"
```

---

### Task 2: 註冊 `run_logsql` MCP tool

**Files:**
- Modify: `internal/mcpserver/server.go:53`（`compare_periods` 註冊區塊之後、`return s` 之前插入）
- Test: `internal/mcpserver/server_test.go`（若不存在則建立；沿用 `cmd/ducklog-mcp/main_test.go` 或既有 server test 的驅動方式）

**Interfaces:**
- Consumes: `tools.RunLogsQL`（Task 1）；既有的 `wrap`、`str`、`toInt`（同檔）。

- [ ] **Step 1: 寫失敗測試**

先確認既有測試如何驅動 server（決定要不要建 `server_test.go`）：

Run: `ls internal/mcpserver/*_test.go; sed -n '1,60p' cmd/ducklog-mcp/main_test.go`

依既有慣例，在 server 層測「`run_logsql` 已註冊」。若 `internal/mcpserver` 無測試檔，建立 `internal/mcpserver/server_test.go`：

```go
package mcpserver

import (
	"testing"
	"time"

	"ducklog/internal/vl"
)

func TestRunLogsQLToolRegistered(t *testing.T) {
	s := NewServer(vl.New("http://127.0.0.1:1", time.Second))
	if s == nil {
		t.Fatal("NewServer 回 nil")
	}
	// mcp-go v0.48.0 無公開列舉 API;以 NewServer 不 panic 且下方 sanity 註冊為準。
	// 真正的行為驗證在 internal/tools 的 RunLogsQL 測試。
}
```

> 註：`mcp-go` v0.48.0 未提供公開的「列出已註冊 tool」API。tool 的**行為**已由 Task 1 的 `internal/tools` 整合測試覆蓋；此處僅確保註冊程式碼可編譯、`NewServer` 不 panic。若探查後發現 v0.48.0 有可列舉 API，改成斷言 `run_logsql` 存在。

- [ ] **Step 2: 跑測試確認失敗（或紅→綠前的基準）**

Run: `go test ./internal/mcpserver/ -run TestRunLogsQLToolRegistered`
Expected: 若新增了對 `run_logsql` 的斷言則 FAIL；若採上方 sanity 版則此步為基準綠燈，實際保護來自編譯。

- [ ] **Step 3: 加上註冊區塊**

在 `internal/mcpserver/server.go` 的 `compare_periods` 區塊之後、`return s` 之前插入：

```go
	s.AddTool(mcp.NewTool("run_logsql",
		mcp.WithDescription("逃生口:執行任意 LogsQL pipeline。僅在 summarize_errors / search_logs / get_trace / compare_periods 都不合用時才用"),
		mcp.WithString("query", mcp.Required(), mcp.Description("任意 LogsQL pipeline,如 level:=error | stats by (service) count()")),
		mcp.WithString("time_range", mcp.Required(), mcp.Description("如 1h / 30m")),
		mcp.WithNumber("limit", mcp.Description("上限,預設 100")),
	), wrap(func(ctx context.Context, a map[string]any) bound.Envelope {
		return tools.RunLogsQL(ctx, c, str(a["query"]), str(a["time_range"]), toInt(a["limit"]))
	}))
```

- [ ] **Step 4: 跑測試 + 全量 build 確認通過**

Run: `go build ./... && go test ./internal/... -short`
Expected: build 成功、測試 PASS（`-short` 下需 VL 的整合測試 skip）。

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/server.go internal/mcpserver/server_test.go
git commit -m "feat(mcpserver): 註冊 run_logsql tool

第 5 個 tool,description 引導 AI 優先用專用 tool。

Claude-Session: https://claude.ai/code/session_018usf3iQoLwJuvsjzPQmXDN"
```

---

## Self-Review

**Spec coverage：**
- 元件 1（`RunLogsQL`）→ Task 1 ✓
- 元件 2（註冊）→ Task 2 ✓
- 元件 3（測試：pipe 可跑、時間強制、空 query 拒、命中 limit 標 Truncated、大輸出走 BoundRaw）→ Task 1 的 4 個測試 ✓（大輸出降級由 `BoundRaw` 既有測試覆蓋；此處 500 筆 info 會觸發 truncation 路徑）
- 關鍵細節（時間注入第一個 pipe 之前）→ `injectTimeFilter` + `TestRunLogsQLRunsPipedQuery`（pipe 查詢成功即證明注入位置正確）✓
- YAGNI（不算 Count、不偵測輸出形態、不做輸入白名單）→ 實作與計畫一致 ✓

**Placeholder scan：** 無 TBD／TODO；所有步驟含完整程式碼與確切指令。Task 2 Step 1 的 `mcp-go` 列舉 API 註記為刻意的探查分支，非佔位。

**Type consistency：** `RunLogsQL` 簽章在 Task 1 Produces 與 Task 2 Consumes 一致；`injectTimeFilter`、`str`、`toInt`、`defaultLimitCap`、`SchemaHint` 均對應既有定義。
