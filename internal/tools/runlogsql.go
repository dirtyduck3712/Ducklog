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
