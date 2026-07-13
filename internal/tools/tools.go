// Package tools 把 VL LogsQL 包成 AI-first 工具,套用防幻覺契約。
package tools

import (
	"strconv"

	"docklog/internal/bound"
)

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

// str 把 VL 回傳的欄位值轉字串(VL 多以字串回)。
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// toInt 把 VL 回傳的數字(通常是字串)轉 int64。
func toInt(v any) int64 {
	switch x := v.(type) {
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case float64:
		return int64(x)
	}
	return 0
}
