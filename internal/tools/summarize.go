package tools

import (
	"context"

	"docklog/internal/bound"
	"docklog/internal/vl"
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
