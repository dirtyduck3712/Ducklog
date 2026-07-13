package tools

import (
	"context"

	"docklog/internal/bound"
	"docklog/internal/vl"
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
