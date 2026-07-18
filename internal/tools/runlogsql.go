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
		// 未做真 Count,TotalMatched 若沿用回傳筆數會謊稱「只此數命中」;
		// 截斷時清零(omitempty 會省略),避免 total/returned/truncated 自相矛盾。
		e.TotalMatched = 0
	}
	return e
}

// injectTimeFilter 把時間過濾 AND 進 filter 段(第一個 pipe「之前」)。
// _time: 是 filter,不能放在 pipeline 尾端;故以第一個「引號外」的 '|' 切分。
// filter 段用括號包住再 AND 時間,避免 top-level OR 因 LogsQL 的 AND 優先序高於 OR 而漏過濾。
func injectTimeFilter(query, timeRange string) string {
	tf := vl.TimeFilter(timeRange)
	idx := firstUnquotedPipe(query)
	if idx < 0 {
		return tf + " (" + strings.TrimSpace(query) + ")"
	}
	filter := strings.TrimSpace(query[:idx])
	rest := strings.TrimSpace(query[idx+1:])
	return tf + " (" + filter + ") | " + rest
}

// firstUnquotedPipe 回傳第一個「不在引號內」的 '|' 的 byte index,無則 -1。
// 處理 LogsQL 三種引號:雙引號 " 、單引號 ' 、反引號 ` 。
// 雙/單引號內遇 '\' 跳脫下一字元;反引號內不處理跳脫。
// 引號與 '|'、'\' 皆為 ASCII,UTF-8 多位元組字元不含這些位元組,故 byte 掃描安全。
func firstUnquotedPipe(s string) int {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\'':
			for i++; i < len(s); i++ {
				if s[i] == '\\' {
					i++ // 跳過被跳脫的字元
					continue
				}
				if s[i] == c {
					break
				}
			}
		case '`':
			for i++; i < len(s); i++ {
				if s[i] == '`' {
					break
				}
			}
		case '|':
			return i
		}
	}
	return -1
}
