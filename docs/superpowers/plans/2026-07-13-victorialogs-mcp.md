# VictoriaLogs + MCP 工具層 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 一個薄 Go MCP server,把 VictoriaLogs 的 LogsQL 包成 AI-first 工具(summarize_errors / get_trace / search_logs / compare_periods),外加規格最強調的「防幻覺契約」(token-bounding、明確降級、絕不靜默截斷)。

**Architecture:** apps → 現有 Go slog transport(重指 VL)→ VictoriaLogs(存/retention/查/UI 全包)← 我們的 MCP server(Go, mcp-go, stdio)透過 localhost LogsQL HTTP 查 VL ← Claude Code。VL 扛儲存,我們只建工具層。

**Tech Stack:** Go、`github.com/mark3labs/mcp-go`(MCP stdio server)、stdlib `net/http`(VL LogsQL client)。**無 CGO、無 go-duckdb。** VictoriaLogs OSS binary `victoria-logs-prod`(外部)。

**Scope:** 只建 MCP 工具層 + repoint transport + 移除自建 DuckLake 儲存層。依據:`docs/superpowers/specs/2026-07-13-victorialogs-mcp-pivot-design.md`。v1 不含 Node transport、SSE MCP、custom UI、auth。

## Global Constraints

- **VL ingest 映射**:transport 送我們的 wire JSON `{ts, service, level, trace_id, message, attrs}` 到 VL 的 `POST /insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service`。wire 格式**不變**。
- **VL query**:`GET /select/logsql/query`(回 NDJSON,一行一筆)、`/select/logsql/stats_query`。query 用 `query` 參數(URL-encoded LogsQL)。
- **已 spike 驗證的 LogsQL 形態**(用這些):
  - summarize:`{svc} level:=error _time:{R} | collapse_nums | stats by (_msg) count() as n | sort by (n desc) | limit {N}`
  - get_trace:`trace_id:={id} | sort by (_time) | limit {cap}`
  - count:`{filter} | stats count() as n`
- **防幻覺契約(硬需求)**:每 tool 回傳 ≤ ~4000 tokens(估算:`len(jsonBytes)/4`);超過就沿 `raw → sampled → fingerprint → count` 降級,且回應**明確**帶 `downgraded:true` + `reason` + `hint`;truncation 帶 `total_matched`/`returned`/`truncated`;每次附 `schema_hint`;錯誤帶 `error_code`/`message`/`hint`。**絕不靜默丟棄或截斷。**
- **查詢安全**:tool 一律強制 time range(缺就補預設或拒絕)、強制 server 端 limit 上限、query 有 context timeout。
- **MCP transport**:v1 只 stdio。
- **模組**:MCP server 在 root `ducklog` 模組(移除 go-duckdb 後為純 Go);`client/` transport 為既有獨立模組。
- **Build**:root 模組移除 go-duckdb 後用普通 `go`(確認 `go.mod` 的 go 版本降到 mcp-go 要求的下限即可,不需 GOTOOLCHAIN override,除非 mcp-go 要求更高)。

---

## File Structure

```
go.mod / go.sum              root 模組 ducklog:移除 go-duckdb,加入 mcp-go
cmd/ducklog-mcp/main.go      MCP server 入口(stdio)
internal/vl/client.go        VL LogsQL HTTP client:Query / Count / Ping
internal/vl/logsql.go        LogsQL 組裝:time range、field-value 轉義、collapse_nums summarize builder
internal/vl/client_test.go
internal/vl/testutil.go      (test-only)locate/啟動 VL binary,回 URL + cleanup(供整合測試共用)
internal/bound/bound.go      token 估算 + 降級鏈 + 回應信封(防幻覺契約)
internal/bound/bound_test.go
internal/tools/tools.go      Tool 介面 + 共用:time_range 解析、schema_hint 常數
internal/tools/summarize.go  summarize_errors
internal/tools/gettrace.go   get_trace
internal/tools/search.go     search_logs
internal/tools/compare.go    compare_periods
internal/tools/tools_test.go 各 tool 對真 VL 的整合測試
internal/mcpserver/server.go 把 tools 註冊進 mcp-go、ServeStdio
cmd/ducklog-mcp/main_test.go MCP 協定 smoke(stdio handshake + 呼叫一個 tool)
client/…                     既有 transport,repoint(Task 1)
config.example.yaml          MCP server 設定(VL URL、預設窗、limit 上限)
scripts/run-dev.sh           啟 VL + MCP 的開發腳本
README.md                    如何跑
── 移除 ──
internal/store, internal/metrics, internal/auth, internal/ratelimit,
internal/diskguard, internal/checkpoint, internal/ingest, internal/health,
internal/config, cmd/ducklog   (Task 8)
```

---

## Task 1: Repoint transport 到 VL + 確認 attrs ingest(spike + 小改)

**Files:**
- Modify: `client/e2e_test.go`(改成打真 VL,而非已移除的 ducklog server)
- Modify: `client/config.go`(doc 註解:Endpoint 指 VL jsonline;無程式行為變更)
- Create: `client/README.md`(如何指向 VL)

**Interfaces:**
- Produces: 一個確認「現有 transport 送我們的 wire 格式 → VL jsonline → 查得回、trace_id 可過濾、attrs 正確」的整合測試。

- [ ] **Step 1: 確認 VL binary 可用**

VL OSS binary 已於 spike 下載到 `<scratch>/vl/victoria-logs-prod`。若不在,從 `https://github.com/VictoriaMetrics/VictoriaLogs/releases/latest` 抓 `victoria-logs-linux-amd64-v*.tar.gz`(**非** enterprise)。整合測試會用它。

- [ ] **Step 2: 寫失敗的 repoint e2e 測試**

改寫 `client/e2e_test.go` 的 `TestEndToEndWireContract`:
- 啟真 VL(暫存 data dir、ephemeral port,見下方 helper 概念),等 `/health`。
- 用現有 `RemoteHandler`,`Endpoint` 設為 `http://127.0.0.1:<port>/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service`,log 3 筆(含 1 筆有 `attrs={"host":"10.0.1.5"}`、canonical trace_id),`Close()`。
- 等 VL flush,查 `/select/logsql/query?query=*`,斷言:3 筆都在;`_msg` = 我們的 message;`trace_id:=<id>` 過濾回得到該筆;帶 attrs 的那筆 `attrs` 欄位查得回(**確認巢狀處理**:VL 可能把 `attrs` 存成 JSON 字串或攤平成 `attrs.host` —— 測試印出實際欄位形態並斷言「host 值查得到」,不硬綁形態)。
- `-short` 時 skip(需 VL binary + 較重)。

> **helper**:e2e 測試需要「找到並啟動 VL、回 URL、defer kill」。可在 `client/e2e_test.go` 內就地寫一個 `startVL(t)`(找 binary path from env `VL_BINARY` 或 scratch 預設 → `exec.Command` 起在 `127.0.0.1:0`... 注意 VL 的 `-httpListenAddr` 不吃 `:0`,要 bind-probe 取空 port:`net.Listen("tcp","127.0.0.1:0")` 取 addr、close、用該 port)。

- [ ] **Step 3: 執行確認失敗**

Run: `cd client && VL_BINARY=<path> go test -run EndToEnd ./...`
Expected: FAIL(舊測試 build `../cmd/ducklog` 已不適用 / 尚未改寫)。

- [ ] **Step 4: 實作 repoint 測試**

依 Step 2 完成 `startVL` + 測試主體。transport 程式碼**不需改**(只是設不同 Endpoint);在 `client/config.go` 的 `Endpoint` 欄位加一行 doc 註解:指向 VL 時用 `/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service`。`client/README.md` 記錄用法。

- [ ] **Step 5: 執行確認通過**

Run: `cd client && VL_BINARY=<path> go test -run EndToEnd ./... -v`；`cd client && go test -short ./...`(應 skip e2e、其餘綠)。
Expected: PASS。**若 attrs 的巢狀處理不如預期**(VL 沒攤平、host 查不回),在 report 記錄實際形態 + 是否需要 transport 把 attrs 攤平成 `attrs_<key>` —— 但**不要在此 task 改 transport 行為**,只回報,由後續決定。

- [ ] **Step 6: Commit**

```bash
cd /home/dva/workspace/ducklog
git add client/e2e_test.go client/config.go client/README.md
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "feat(client): repoint transport e2e to VictoriaLogs ingest"
```

---

## Task 2: VL LogsQL client(internal/vl)

**Files:**
- Create: `internal/vl/client.go`, `internal/vl/logsql.go`, `internal/vl/testutil.go`, `internal/vl/client_test.go`
- Modify: `go.mod`(此 task 起 root 模組加入 internal/vl;go-duckdb 仍在,Task 8 才移除)

**Interfaces:**
- Produces:
  - `type Client struct{...}`；`func New(baseURL string, timeout time.Duration) *Client`
  - `func (*Client) Query(ctx, logsql string, limit int) ([]map[string]any, error)` — 打 `/select/logsql/query`,解析 NDJSON;limit>0 時附 `&limit=`
  - `func (*Client) Count(ctx, logsql string) (int64, error)` — 附 `| stats count() as n`,回 n
  - `func (*Client) Ping(ctx) error` — `/health`
  - logsql.go:`func Escape(v string) string`(field value)、`func TimeFilter(r string) string`(如 `"1h"`→`_time:1h`)、`func SummarizeErrorsQuery(service, timeRange string, limit int) string`
- Consumes(test):`testutil.StartVL(t) (baseURL string)`

- [ ] **Step 1: 寫 testutil(啟動 VL)**

`internal/vl/testutil.go`(注意:非 `_test.go` 也可,但只在測試用;為避免正式 build 拉入,放 `//go:build test_vl` 或就命名 `testutil_test.go` 供同套件測試用)。實作 `StartVL(t testing.TB) string`:
- 找 VL binary:env `VL_BINARY`,否則 `<SCRATCH or /tmp>/vl/victoria-logs-prod`;找不到就 `t.Skip("VL binary 不存在,設 VL_BINARY")`。
- bind-probe 取空 port;`exec.Command(bin, "-storageDataPath="+t.TempDir(), "-httpListenAddr=127.0.0.1:"+port, "-retentionPeriod=30d")`;`Start()`;`t.Cleanup` kill。
- poll `/health` 直到 200(逾時 t.Fatal)。回 `http://127.0.0.1:<port>`。

- [ ] **Step 2: 寫失敗測試**

`internal/vl/client_test.go`:
```go
package vl

import (
	"context"
	"strings"
	"testing"
	"time"
)

func ingest(t *testing.T, base string, lines string) { /* POST lines 到 /insert/jsonline?... ,等 flush(poll count) */ }

func TestQueryAndCount(t *testing.T) {
	if testing.Short() { t.Skip("需 VL") }
	base := StartVL(t)
	c := New(base, 10*time.Second)
	ingest(t, base, strings.Join([]string{
		`{"ts":"`+nowRFC(t)+`","message":"boom","service":"api","level":"error","trace_id":"0af76519-16cd-43dd-8448-eb211c80319c"}`,
		`{"ts":"`+nowRFC(t)+`","message":"ok","service":"api","level":"info"}`,
	}, "\n"))
	ctx := context.Background()
	n, err := c.Count(ctx, `service:=api`)
	if err != nil || n != 2 { t.Fatalf("Count=%d err=%v; want 2", n, err) }
	rows, err := c.Query(ctx, `level:=error`, 10)
	if err != nil || len(rows) != 1 || rows[0]["_msg"] != "boom" {
		t.Fatalf("Query error rows=%v err=%v", rows, err)
	}
}
```
(`nowRFC`/`ingest` 為測試小工具,實作者補;`ingest` 要 poll 直到 `Count`==期望,因 VL flush 非即時。)

- [ ] **Step 3: 執行確認失敗** — Run: `cd /home/dva/workspace/ducklog && GOTOOLCHAIN=go1.24.0 go test ./internal/vl/`(此時 root 仍有 go-duckdb,故仍需 GOTOOLCHAIN);Expected: FAIL(`New` 未定義)。

- [ ] **Step 4: 實作 client.go + logsql.go**

`internal/vl/client.go`:
```go
// Package vl 是 VictoriaLogs LogsQL 的薄 HTTP client(唯讀查詢)。
package vl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	base string
	hc   *http.Client
}

func New(baseURL string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: &http.Client{Timeout: timeout}}
}

func (c *Client) Ping(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("VL health %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) Query(ctx context.Context, logsql string, limit int) ([]map[string]any, error) {
	v := url.Values{"query": {logsql}}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/select/logsql/query?"+v.Encode(), nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("VL query %d: %s", resp.StatusCode, readErr(resp))
	}
	var out []map[string]any
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("parse VL row: %w", err)
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

func (c *Client) Count(ctx context.Context, logsql string) (int64, error) {
	rows, err := c.Query(ctx, logsql+" | stats count() as n", 0)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	// VL 回字串數字
	switch v := rows[0]["n"].(type) {
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n, nil
	case float64:
		return int64(v), nil
	}
	return 0, nil
}

func readErr(resp *http.Response) string {
	b := make([]byte, 512)
	n, _ := resp.Body.Read(b)
	return string(b[:n])
}
```

`internal/vl/logsql.go`:
```go
package vl

import (
	"fmt"
	"strings"
)

// Escape 轉義 LogsQL 的 field-value(用引號包住並跳脫引號)。
func Escape(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}

// TimeFilter 回 LogsQL 的時間過濾片段,如 "1h" → "_time:1h"。空字串回空(呼叫端負責強制)。
func TimeFilter(r string) string {
	if r == "" {
		return ""
	}
	return "_time:" + r
}

// SummarizeErrorsQuery 組出 fingerprint 分群查詢(已 spike 驗證)。
func SummarizeErrorsQuery(service, timeRange string, limit int) string {
	var b strings.Builder
	if service != "" {
		b.WriteString("service:=" + Escape(service) + " ")
	}
	b.WriteString("level:=error ")
	b.WriteString(TimeFilter(timeRange))
	fmt.Fprintf(&b, " | collapse_nums | stats by (_msg) count() as n | sort by (n desc) | limit %d", limit)
	return b.String()
}
```

- [ ] **Step 5: 執行確認通過** — Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/vl/ -v`;Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/vl/ go.mod go.sum
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "feat(vl): LogsQL HTTP client + query builders"
```

---

## Task 3: 防幻覺契約(internal/bound)

**Files:**
- Create: `internal/bound/bound.go`, `internal/bound/bound_test.go`

**Interfaces:**
- Produces:
  - `const MaxTokens = 4000`；`func EstimateTokens(v any) int`(marshal→`len/4`)
  - `type Envelope struct{ Status string; Downgraded bool; Reason,Returned,Hint,SchemaHint,ErrorCode,Message string; TotalMatched int64; ReturnedN int; Truncated bool; Data any }`(對應 JSON tag,omitempty)
  - `func OK(data any, schemaHint string) Envelope`
  - `func Err(code, msg, hint string) Envelope`
  - `func (Envelope) FitsBudget() bool` / `func EstimateTokens...`
  - `func BoundRaw(rows []map[string]any, schemaHint string) Envelope` — raw logs 太大時降級:full → 抽樣(每 N 筆)→ 若仍爆,回 `returned:"count"` 只給筆數;每次降級標 downgraded+reason+hint

- [ ] **Step 1: 寫失敗測試**

`internal/bound/bound_test.go`:
```go
package bound

import "testing"

func TestOKFits(t *testing.T) {
	e := OK([]map[string]any{{"a": 1}}, "hint")
	if e.Status != "ok" || e.Downgraded || e.SchemaHint != "hint" {
		t.Fatalf("%+v", e)
	}
}

func TestBoundRawDowngradesLargeResult(t *testing.T) {
	// 造超過 MaxTokens 的大量 rows
	rows := make([]map[string]any, 20000)
	for i := range rows {
		rows[i] = map[string]any{"_msg": "a fairly long log message number that repeats", "service": "api", "i": i}
	}
	e := BoundRaw(rows, "hint")
	if !e.Downgraded {
		t.Fatal("超量結果必須標 downgraded")
	}
	if e.Reason == "" || e.Hint == "" {
		t.Fatal("降級必須有 reason + hint(絕不靜默)")
	}
	if EstimateTokens(e.Data) > MaxTokens && e.Returned != "count" {
		t.Fatal("降級後仍須在預算內,或最終退到 count")
	}
}

func TestErr(t *testing.T) {
	e := Err("QUERY_TIMEOUT", "too slow", "narrow range")
	if e.Status != "error" || e.ErrorCode != "QUERY_TIMEOUT" || e.Hint == "" {
		t.Fatalf("%+v", e)
	}
}
```

- [ ] **Step 2: 執行確認失敗** — Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/bound/`;Expected: FAIL。

- [ ] **Step 3: 實作 bound.go**

`internal/bound/bound.go`:
```go
// Package bound 實作 MCP 的防幻覺契約:token-bounding、明確降級、絕不靜默。
package bound

import "encoding/json"

const MaxTokens = 4000

// EstimateTokens 以 ~4 chars/token 粗估(足夠當降級門檻)。
func EstimateTokens(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 1 << 30
	}
	return len(b) / 4
}

type Envelope struct {
	Status       string `json:"status"`
	Downgraded   bool   `json:"downgraded,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Returned     string `json:"returned_kind,omitempty"` // 降級種類(sampled_logs/count),與筆數 returned 區分
	TotalMatched int64  `json:"total_matched,omitempty"`
	ReturnedN    int    `json:"returned"`
	Truncated    bool   `json:"truncated,omitempty"`
	Hint         string `json:"hint,omitempty"`
	SchemaHint   string `json:"schema_hint"`
	ErrorCode    string `json:"error_code,omitempty"`
	Message      string `json:"message,omitempty"`
	Data         any    `json:"data,omitempty"`
}

func OK(data any, schemaHint string) Envelope {
	n := 0
	if s, ok := data.([]map[string]any); ok {
		n = len(s)
	}
	return Envelope{Status: "ok", ReturnedN: n, SchemaHint: schemaHint, Data: data}
}

func Err(code, msg, hint string) Envelope {
	return Envelope{Status: "error", ErrorCode: code, Message: msg, Hint: hint}
}

// BoundRaw:raw logs 若超過 token 預算,依序降級 full → 抽樣 → count-only,並明確標示。
func BoundRaw(rows []map[string]any, schemaHint string) Envelope {
	total := int64(len(rows))
	if EstimateTokens(rows) <= MaxTokens {
		e := OK(rows, schemaHint)
		e.TotalMatched = total
		return e
	}
	// 抽樣:回傳「放得進 token 預算的最大均勻抽樣」。floor 估算會高估,放不下就減半重試。
	keep := MaxTokens / max(1, EstimateTokens(rows)/max(1, len(rows)))
	for keep >= 1 {
		if keep >= len(rows) {
			keep = len(rows) - 1
		}
		step := len(rows) / keep
		sampled := make([]map[string]any, 0, keep)
		for i := 0; i < len(rows); i += step {
			sampled = append(sampled, rows[i])
		}
		if EstimateTokens(sampled) <= MaxTokens {
			return Envelope{
				Status: "ok", Downgraded: true, Returned: "sampled_logs",
				Reason:       "原始結果超過 token 上限,已均勻抽樣",
				TotalMatched: total, ReturnedN: len(sampled), Truncated: true,
				Hint:       "縮小時間範圍或加 service 過濾可取得原始 log",
				SchemaHint: schemaHint, Data: sampled,
			}
		}
		keep /= 2
	}
	// 最終退到 count-only
	return Envelope{
		Status: "ok", Downgraded: true, Returned: "count",
		Reason:       "原始結果過大,連抽樣都超過 token 上限",
		TotalMatched: total, ReturnedN: 0, Truncated: true,
		Hint:       "請大幅縮小時間範圍或加更嚴格過濾",
		SchemaHint: schemaHint, Data: map[string]any{"count": total},
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
```

- [ ] **Step 4: 執行確認通過** — Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/bound/ -v`;Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/bound/
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "feat(bound): anti-confabulation contract (token-bound, explicit downgrade)"
```

---

## Task 4: Tools —— summarize_errors / get_trace / search_logs / compare_periods

**Files:**
- Create: `internal/tools/tools.go`, `internal/tools/summarize.go`, `internal/tools/gettrace.go`, `internal/tools/search.go`, `internal/tools/compare.go`, `internal/tools/tools_test.go`

**Interfaces:**
- Consumes: `vl.Client`, `bound.Envelope`, `vl.SummarizeErrorsQuery`
- Produces(每個 tool 都是純函式,回 `bound.Envelope`,方便測試):
  - `const SchemaHint = "可用欄位:_time, service, level, trace_id, _msg。時間範圍如 '1h' / '30m'。"`
  - `func SummarizeErrors(ctx, c *vl.Client, service, timeRange string, limit int) bound.Envelope`
  - `func GetTrace(ctx, c *vl.Client, traceID string) bound.Envelope`
  - `func SearchLogs(ctx, c *vl.Client, query, timeRange string, limit int) bound.Envelope`
  - `func ComparePeriods(ctx, c *vl.Client, service, t1, t2 string) bound.Envelope`
  - 共用:`func requireRange(timeRange string) (string, *bound.Envelope)`(空 range → 回 error envelope「強制時間範圍」);`defaultLimitCap = 1000`

- [ ] **Step 1: 寫失敗整合測試(對真 VL)**

`internal/tools/tools_test.go`(用 `internal/vl` 的 `StartVL` + ingest helper;`-short` skip):
```go
package tools

import (
	"context"
	"testing"
	"time"

	"ducklog/internal/vl"
)

func TestSummarizeErrorsFingerprints(t *testing.T) {
	if testing.Short() { t.Skip("需 VL") }
	base := vl.StartVL(t)
	c := vl.New(base, 10*time.Second)
	// ingest:兩種 error 模板各數十筆(數字不同 → 應收斂成 2 個 pattern)+ 一些 info
	ingestErrors(t, base) // helper:寫 "timeout after {N}s" ×30、"panic at handler {N}" ×20、info ×50
	e := SummarizeErrors(context.Background(), c, "api", "1h", 20)
	if e.Status != "ok" { t.Fatalf("%+v", e) }
	pats, _ := e.Data.([]map[string]any)
	if len(pats) != 2 { t.Fatalf("應收斂成 2 個 fingerprint pattern,得 %d: %v", len(pats), pats) }
}

func TestGetTraceReturnsWholeTrace(t *testing.T) {
	if testing.Short() { t.Skip("需 VL") }
	base := vl.StartVL(t); c := vl.New(base, 10*time.Second)
	tid := "0af76519-16cd-43dd-8448-eb211c80319c"
	ingestTrace(t, base, tid, 3) // 同 trace_id 3 筆
	e := GetTrace(context.Background(), c, tid)
	rows, _ := e.Data.([]map[string]any)
	if e.Status != "ok" || len(rows) != 3 { t.Fatalf("get_trace = %d 筆; want 3", len(rows)) }
}

func TestSearchLogsMarksTruncation(t *testing.T) {
	if testing.Short() { t.Skip("需 VL") }
	base := vl.StartVL(t); c := vl.New(base, 10*time.Second)
	ingestN(t, base, 500) // 500 筆 info
	e := SearchLogs(context.Background(), c, "level:=info", "1h", 100)
	if e.TotalMatched < 500 || !e.Truncated { t.Fatalf("truncation 應標示:%+v", e) }
	if e.ReturnedN > 100 { t.Fatalf("returned 應 ≤ limit") }
}

func TestSearchRequiresTimeRange(t *testing.T) {
	base := vl.StartVL(t); c := vl.New(base, 10*time.Second)
	e := SearchLogs(context.Background(), c, "level:=info", "", 100)
	if e.Status != "error" || e.ErrorCode != "MISSING_TIME_RANGE" { t.Fatalf("空 range 應拒:%+v", e) }
}
```
(`ingestErrors`/`ingestTrace`/`ingestN` 為測試小工具,實作者補;都 POST 到 `/insert/jsonline?...` 並 poll 直到可查。)

- [ ] **Step 2: 執行確認失敗** — Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/tools/`;Expected: FAIL。

- [ ] **Step 3: 實作 tools**

`internal/tools/tools.go`:
```go
// Package tools 把 VL LogsQL 包成 AI-first 工具,套用防幻覺契約。
package tools

import "ducklog/internal/bound"

const SchemaHint = "可用欄位:_time, service, level, trace_id, _msg。時間範圍如 '1h' / '30m' / '2h'。"

const defaultLimitCap = 1000

// requireRange 強制時間範圍;空則回 error envelope(第二回傳非 nil 代表該直接回它)。
func requireRange(timeRange string) *bound.Envelope {
	if timeRange == "" {
		e := bound.Err("MISSING_TIME_RANGE", "查詢必須指定時間範圍", "例如 time_range='1h'")
		return &e
	}
	return nil
}
```

`internal/tools/summarize.go`:
```go
package tools

import (
	"context"

	"ducklog/internal/bound"
	"ducklog/internal/vl"
)

func SummarizeErrors(ctx context.Context, c *vl.Client, service, timeRange string, limit int) bound.Envelope {
	if e := requireRange(timeRange); e != nil {
		return *e
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := c.Query(ctx, vl.SummarizeErrorsQuery(service, timeRange, limit), 0)
	if err != nil {
		return bound.Err("QUERY_FAILED", err.Error(), "確認 VL 可達或縮小範圍")
	}
	// summarize 已是聚合結果(小),直接 OK;仍套 token 檢查(理論上不會爆)
	return boundOrOK(rows)
}

func boundOrOK(rows []map[string]any) bound.Envelope {
	if bound.EstimateTokens(rows) <= bound.MaxTokens {
		return bound.OK(rows, SchemaHint)
	}
	return bound.BoundRaw(rows, SchemaHint)
}
```

`internal/tools/gettrace.go`:
```go
package tools

import (
	"context"
	"fmt"

	"ducklog/internal/bound"
	"ducklog/internal/vl"
)

func GetTrace(ctx context.Context, c *vl.Client, traceID string) bound.Envelope {
	if traceID == "" {
		return bound.Err("MISSING_TRACE_ID", "需要 trace_id", "")
	}
	q := fmt.Sprintf("trace_id:=%s | sort by (_time) | limit %d", vl.Escape(traceID), defaultLimitCap)
	rows, err := c.Query(ctx, q, 0)
	if err != nil {
		return bound.Err("QUERY_FAILED", err.Error(), "")
	}
	return bound.BoundRaw(rows, SchemaHint)
}
```

`internal/tools/search.go`:
```go
package tools

import (
	"context"
	"fmt"

	"ducklog/internal/bound"
	"ducklog/internal/vl"
)

func SearchLogs(ctx context.Context, c *vl.Client, query, timeRange string, limit int) bound.Envelope {
	if e := requireRange(timeRange); e != nil {
		return *e
	}
	if limit <= 0 || limit > defaultLimitCap {
		limit = 100
	}
	full := fmt.Sprintf("%s %s", query, vl.TimeFilter(timeRange))
	total, err := c.Count(ctx, full)
	if err != nil {
		return bound.Err("QUERY_FAILED", err.Error(), "確認查詢語法或縮小範圍")
	}
	rows, err := c.Query(ctx, full+" | sort by (_time) desc", limit)
	if err != nil {
		return bound.Err("QUERY_FAILED", err.Error(), "")
	}
	e := bound.BoundRaw(rows, SchemaHint)
	e.TotalMatched = total
	if total > int64(len(rows)) {
		e.Truncated = true
	}
	return e
}
```

`internal/tools/compare.go`:
```go
package tools

import (
	"context"

	"ducklog/internal/bound"
	"ducklog/internal/vl"
)

// ComparePeriods 對兩個時間窗各跑 fingerprint stats,回「新出現 / 暴增」的 pattern。
func ComparePeriods(ctx context.Context, c *vl.Client, service, t1, t2 string) bound.Envelope {
	if t1 == "" || t2 == "" {
		return bound.Err("MISSING_TIME_RANGE", "需要兩個時間範圍 t1, t2", "如 t1='2h', t2='1h'")
	}
	p1, err := c.Query(ctx, vl.SummarizeErrorsQuery(service, t1, 50), 0)
	if err != nil {
		return bound.Err("QUERY_FAILED", err.Error(), "")
	}
	p2, err := c.Query(ctx, vl.SummarizeErrorsQuery(service, t2, 50), 0)
	if err != nil {
		return bound.Err("QUERY_FAILED", err.Error(), "")
	}
	// 以 _msg 為 key,算 t2 相對 t1 的變化(新出現 / 次數暴增)
	base := map[string]int64{}
	for _, r := range p1 {
		base[str(r["_msg"])] = toInt(r["n"])
	}
	var diffs []map[string]any
	for _, r := range p2 {
		msg := str(r["_msg"])
		now, was := toInt(r["n"]), base[msg]
		if was == 0 || now > was*2 {
			diffs = append(diffs, map[string]any{"pattern": msg, "t1_count": was, "t2_count": now, "new": was == 0})
		}
	}
	return bound.OK(diffs, SchemaHint)
}
```
(小工具 `str`/`toInt` 放 tools.go;`toInt` 處理 VL 回的字串數字。)

- [ ] **Step 4: 執行確認通過** — Run: `GOTOOLCHAIN=go1.24.0 go test ./internal/tools/ -v`;Expected: PASS(整合測試綠)。

- [ ] **Step 5: Commit**

```bash
git add internal/tools/
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "feat(tools): summarize_errors, get_trace, search_logs, compare_periods over LogsQL"
```

---

## Task 5: MCP server(mcp-go stdio wiring)

**Files:**
- Create: `internal/mcpserver/server.go`, `cmd/ducklog-mcp/main.go`, `cmd/ducklog-mcp/main_test.go`, `config.example.yaml`
- Modify: `go.mod`(`go get github.com/mark3labs/mcp-go`)

**Interfaces:**
- Consumes: 全部 tools、`vl.Client`
- Produces:
  - `func Serve(ctx, vlClient *vl.Client) error` — 建 mcp-go server、註冊 4 tools、ServeStdio
  - `main`:讀 config(VL URL、timeout)→ 建 vl.Client → `Serve`

- [ ] **Step 1: 驗證 mcp-go API(先探,像先前驗 go-duckdb)**

```bash
GOTOOLCHAIN=go1.24.0 go get github.com/mark3labs/mcp-go@latest
```
寫一個最小 spike(可丟在 `cmd/ducklog-mcp/spike_test.go` 或直接讀 mcp-go 的 `server` package 文件/godoc)確認實際 API:server 建立、`AddTool`/`RegisterTool` 名稱、tool 定義(`mcp.NewTool` + `mcp.WithString(...)`)、handler 簽名(`func(ctx, mcp.CallToolRequest) (*mcp.CallToolResult, error)`)、參數取值(`req.Params.Arguments` / helper)、回傳文字結果(`mcp.NewToolResultText(jsonString)`)、`server.ServeStdio(s)`。**以實際安裝版本為準**,下面 code 若與 API 不符,依實際調整(這是唯一未在本專案驗證過的相依)。

- [ ] **Step 2: 寫 smoke 測試**

`cmd/ducklog-mcp/main_test.go`:對 `internal/mcpserver` 做「不需真 VL」的最小驗證 —— 用一個假的 tool 回應路徑或直接測 tool 註冊數/名稱。若 mcp-go 難以在測試中驅動 stdio,改為:測 `internal/mcpserver` 的 tool schema 組裝(名稱、參數)正確,並在 report 說明 stdio handshake 用手動 `ducklog-mcp` + `mcp` client 驗證。

- [ ] **Step 3: 實作 server.go + main.go**

`internal/mcpserver/server.go`(**依 Step 1 實測的 mcp-go API 調整**;以下為結構示意):
```go
// Package mcpserver 把 ducklog tools 註冊成 MCP(stdio)。
package mcpserver

import (
	"context"
	"encoding/json"

	"ducklog/internal/bound"
	"ducklog/internal/tools"
	"ducklog/internal/vl"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func Serve(ctx context.Context, c *vl.Client) error {
	s := server.NewMCPServer("ducklog", "0.1.0")

	s.AddTool(mcp.NewTool("summarize_errors",
		mcp.WithDescription("AI 主入口:把 error 用 fingerprint 分群回 pattern+次數"),
		mcp.WithString("service", mcp.Description("服務名,可省")),
		mcp.WithString("time_range", mcp.Required(), mcp.Description("如 1h / 30m")),
	), wrap(func(ctx context.Context, a map[string]any) bound.Envelope {
		return tools.SummarizeErrors(ctx, c, str(a["service"]), str(a["time_range"]), 20)
	}))

	s.AddTool(mcp.NewTool("get_trace",
		mcp.WithDescription("拉出某 trace_id 的完整 log"),
		mcp.WithString("trace_id", mcp.Required()),
	), wrap(func(ctx context.Context, a map[string]any) bound.Envelope {
		return tools.GetTrace(ctx, c, str(a["trace_id"]))
	}))

	s.AddTool(mcp.NewTool("search_logs",
		mcp.WithDescription("結構化 LogsQL 查詢(含 truncation 標示)"),
		mcp.WithString("query", mcp.Required(), mcp.Description("LogsQL filter,如 service:=api level:=error")),
		mcp.WithString("time_range", mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("上限,預設 100")),
	), wrap(func(ctx context.Context, a map[string]any) bound.Envelope {
		return tools.SearchLogs(ctx, c, str(a["query"]), str(a["time_range"]), toInt(a["limit"]))
	}))

	s.AddTool(mcp.NewTool("compare_periods",
		mcp.WithDescription("比較兩時段的 error pattern,找暴增/新出現"),
		mcp.WithString("service"), mcp.WithString("t1", mcp.Required()), mcp.WithString("t2", mcp.Required()),
	), wrap(func(ctx context.Context, a map[string]any) bound.Envelope {
		return tools.ComparePeriods(ctx, c, str(a["service"]), str(a["t1"]), str(a["t2"]))
	}))

	return server.ServeStdio(s)
}

// wrap 把「回 Envelope 的函式」轉成 mcp-go handler:JSON 序列化 envelope 成 tool 文字結果。
func wrap(fn func(context.Context, map[string]any) bound.Envelope) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		env := fn(ctx, req.Params.Arguments) // 依實際 API 取 arguments
		b, _ := json.Marshal(env)
		return mcp.NewToolResultText(string(b)), nil
	}
}
```

`cmd/ducklog-mcp/main.go`:
```go
// ducklog-mcp:把 VictoriaLogs 包成 AI-first MCP 工具(stdio)。
package main

import (
	"context"
	"log"
	"os"
	"time"

	"ducklog/internal/mcpserver"
	"ducklog/internal/vl"
)

func main() {
	vlURL := os.Getenv("VL_URL")
	if vlURL == "" {
		vlURL = "http://127.0.0.1:9428"
	}
	c := vl.New(vlURL, 30*time.Second)
	if err := c.Ping(context.Background()); err != nil {
		log.Fatalf("VictoriaLogs 不可達(%s): %v", vlURL, err)
	}
	if err := mcpserver.Serve(context.Background(), c); err != nil {
		log.Fatalf("MCP server: %v", err)
	}
}
```

`config.example.yaml`:
```yaml
vl_url: "http://127.0.0.1:9428"
query_timeout: "30s"
```

- [ ] **Step 4: 執行 + build** — Run: `GOTOOLCHAIN=go1.24.0 go build ./cmd/ducklog-mcp && go test ./internal/mcpserver/ ./cmd/ducklog-mcp/`;Expected: build OK,測試 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/ cmd/ducklog-mcp/ config.example.yaml go.mod go.sum
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "feat(mcp): stdio MCP server exposing 4 LogsQL-backed tools"
```

---

## Task 6: 移除自建 DuckLake 儲存層

**Files:**
- Delete: `internal/store`, `internal/metrics`, `internal/auth`, `internal/ratelimit`, `internal/diskguard`, `internal/checkpoint`, `internal/ingest`, `internal/health`, `internal/config`, `cmd/ducklog`
- Modify: `go.mod`/`go.sum`(`go mod tidy` 移除 go-duckdb 及其相依)

**Interfaces:** 無新增。目標:root 模組只剩 MCP server(vl/bound/tools/mcpserver + cmd/ducklog-mcp),不再依賴 go-duckdb。

- [ ] **Step 1: 刪除 superseded 套件**

```bash
cd /home/dva/workspace/ducklog
git rm -r internal/store internal/metrics internal/auth internal/ratelimit \
         internal/diskguard internal/checkpoint internal/ingest internal/health \
         internal/config cmd/ducklog
```

- [ ] **Step 2: tidy(移除 go-duckdb)**

```bash
GOTOOLCHAIN=go1.24.0 go mod tidy
```
Expected: `go.mod` 不再有 `go-duckdb`;若 `rogpeppe/go-internal`(先前擋 tidy 的 test-dep)再度阻擋,改用 `go mod tidy -e` 或手動編輯 require 區塊移除 go-duckdb 系列 + 執行 `go build ./...` 確認。移除 go-duckdb 後理論上可不再需 `GOTOOLCHAIN=go1.24.0`(除非 mcp-go 要求)——實作者確認後更新 README。

- [ ] **Step 3: 全 build + vet + test**

Run:
```bash
go build ./... && go vet ./...
go test -short ./...          # 不需 VL 的都要綠
VL_BINARY=<path> go test ./... # 含整合測試
```
Expected: 全綠。無 dangling import。

- [ ] **Step 4: Commit**

```bash
git add -A
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "refactor: remove superseded DuckLake storage layer (superseded by VictoriaLogs)"
```

---

## Task 7: Run 腳本 + README

**Files:**
- Create: `scripts/run-dev.sh`, `README.md`

- [ ] **Step 1: run-dev.sh**

`scripts/run-dev.sh`:啟 VictoriaLogs(找 binary from `VL_BINARY` 或提示下載)於 `127.0.0.1:9428`、印出 vmui URL、然後 `exec ./ducklog-mcp`(stdio,供 Claude Code 掛)。或分兩步:`run-dev.sh vl`(起 VL)、`run-dev.sh mcp`(起 MCP)。

- [ ] **Step 2: README**

`README.md`:轉向說明(VL backend + MCP 工具層)、如何跑(下載 VL OSS binary、起 VL、build `ducklog-mcp`、在 Claude Code 註冊此 MCP server 的 stdio 指令)、4 個 tool 的用途、transport 如何指向 VL、已知限制(v1 無 auth、2 process)。連結設計 spec。

- [ ] **Step 3: Commit**

```bash
git add scripts/ README.md
git -c user.name='ducklog' -c user.email='dev@ducklog.local' commit -m "docs: run scripts + README for VictoriaLogs + MCP setup"
```

---

## 收尾:全套件

- [ ] Run: `go build ./... && go vet ./...`(root)+ `cd client && go test -short ./...`
- [ ] Run(整合):`VL_BINARY=<path> go test ./...`(root)+ `cd client && VL_BINARY=<path> go test ./...`
- Expected: 全綠。

---

## Self-Review(對照 pivot spec)

- **MCP 4 tools** → Task 4 ✅;**mcp-go stdio 註冊** → Task 5 ✅
- **防幻覺契約**(token-bound / 明確降級 / truncation / schema_hint / 絕不靜默)→ Task 3(bound)+ 各 tool 套用 ✅
- **VL LogsQL client** → Task 2 ✅
- **transport 重指 VL**(wire 不變 + attrs 確認)→ Task 1 ✅
- **移除 DuckLake 儲存層** → Task 6 ✅
- **run/docs** → Task 7 ✅
- **查詢安全**(強制 range / limit 上限 / timeout)→ Task 4 `requireRange` + limit cap + client timeout ✅

**已知風險 / 待實作者確認:**
1. **mcp-go 的實際 API**(Task 5)是本專案唯一未驗證的相依 —— Task 5 Step 1 先探,code 依實測調整。
2. **VL 對巢狀 `attrs` 的處理**(Task 1)—— 確認 host 值查得回;若 VL 不攤平,回報後再決定是否要 transport 攤平(不在本計畫改)。
3. `collapse_nums` over-collapse 小瑕疵 —— 接受;fingerprint 分群品質已 spike 驗證足夠。
4. `go mod tidy` 移除 go-duckdb 後,先前擋 tidy 的 `rogpeppe/go-internal` 可能再現 —— Task 6 Step 2 有 fallback。
