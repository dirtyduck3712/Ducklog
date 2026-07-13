package tools

import (
	"context"
	"fmt"

	"docklog/internal/bound"
	"docklog/internal/vl"
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
