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
	filter := fmt.Sprintf("trace_id:=%s", vl.Escape(traceID))
	trueCount, err := c.Count(ctx, filter)
	if err != nil {
		return bound.Err("QUERY_FAILED", err.Error(), "")
	}
	q := fmt.Sprintf("%s | sort by (_time) | limit %d", filter, defaultLimitCap)
	rows, err := c.Query(ctx, q, 0)
	if err != nil {
		return bound.Err("QUERY_FAILED", err.Error(), "")
	}
	e := bound.BoundRaw(rows, SchemaHint)
	e.TotalMatched = trueCount
	if trueCount > int64(len(rows)) {
		e.Truncated = true
	}
	return e
}
