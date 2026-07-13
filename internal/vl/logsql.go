package vl

import (
	"fmt"
	"strings"
)

// Escape 轉義 LogsQL 的 field-value(用引號包住並跳脫引號)。
func Escape(v string) string {
	return `"` + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(v) + `"`
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
