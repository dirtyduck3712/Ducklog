// Package bound 實作 MCP 的防幻覺契約:token-bounding、明確降級、絕不靜默。
package bound

import "encoding/json"

const MaxTokens = 4000

// EstimateTokens 以 ~4 chars/token 粗估(足夠當降級門檻)。
func EstimateTokens(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 1 << 30
	}
	return len(b) / 4
}

type Envelope struct {
	Status       string `json:"status"`
	Downgraded   bool   `json:"downgraded,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Returned     string `json:"returned_kind,omitempty"`
	TotalMatched int64  `json:"total_matched,omitempty"`
	ReturnedN    int    `json:"returned"`
	Truncated    bool   `json:"truncated,omitempty"`
	Hint         string `json:"hint,omitempty"`
	SchemaHint   string `json:"schema_hint"`
	ErrorCode    string `json:"error_code,omitempty"`
	Message      string `json:"message,omitempty"`
	Data         any    `json:"data,omitempty"`
}

func OK(data any, schemaHint string) Envelope {
	n := 0
	if s, ok := data.([]map[string]any); ok {
		n = len(s)
	}
	return Envelope{Status: "ok", ReturnedN: n, SchemaHint: schemaHint, Data: data}
}

func Err(code, msg, hint string) Envelope {
	return Envelope{Status: "error", ErrorCode: code, Message: msg, Hint: hint}
}

// BoundRaw:raw logs 若超過 token 預算,依序降級 full → 抽樣 → count-only,並明確標示。
func BoundRaw(rows []map[string]any, schemaHint string) Envelope {
	total := int64(len(rows))
	if EstimateTokens(rows) <= MaxTokens {
		e := OK(rows, schemaHint)
		e.TotalMatched = total
		return e
	}
	// 抽樣:回傳「放得進 token 預算的最大均勻抽樣」。
	// 先用每筆平均成本估初始筆數,放不下就減半重試(floor 估算會高估,故需重試)。
	keep := MaxTokens / max(1, EstimateTokens(rows)/max(1, len(rows)))
	for keep >= 1 {
		if keep >= len(rows) {
			keep = len(rows) - 1 // 全量已超標,抽樣必須比全量少
		}
		step := len(rows) / keep
		sampled := make([]map[string]any, 0, keep)
		for i := 0; i < len(rows); i += step {
			sampled = append(sampled, rows[i])
		}
		if EstimateTokens(sampled) <= MaxTokens {
			return Envelope{
				Status: "ok", Downgraded: true, Returned: "sampled_logs",
				Reason:       "原始結果超過 token 上限,已均勻抽樣",
				TotalMatched: total, ReturnedN: len(sampled), Truncated: true,
				Hint:       "縮小時間範圍或加 service 過濾可取得原始 log",
				SchemaHint: schemaHint, Data: sampled,
			}
		}
		keep /= 2 // 還是太大,減半再試
	}
	// 最終退到 count-only
	return Envelope{
		Status: "ok", Downgraded: true, Returned: "count",
		Reason:       "原始結果過大,連抽樣都超過 token 上限",
		TotalMatched: total, ReturnedN: 0, Truncated: true,
		Hint:       "請大幅縮小時間範圍或加更嚴格過濾",
		SchemaHint: schemaHint, Data: map[string]any{"count": total},
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
