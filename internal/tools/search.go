package tools

import (
	"context"
	"fmt"

	"docklog/internal/bound"
	"docklog/internal/vl"
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
