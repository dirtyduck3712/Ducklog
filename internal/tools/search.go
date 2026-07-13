package tools

import (
	"context"
	"fmt"
	"strings"

	"docklog/internal/bound"
	"docklog/internal/vl"
)

func SearchLogs(ctx context.Context, c *vl.Client, query, timeRange string, limit int) bound.Envelope {
	if e := requireRange(timeRange); e != nil {
		return *e
	}
	if strings.Contains(query, "|") {
		return bound.Err("MALFORMED_QUERY", "query 不可含 pipe (|);只接受 LogsQL filter 運算式", "把 pipe 邏輯交給對應的專用工具,或只傳 filter 條件")
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
