package bound

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOKFits(t *testing.T) {
	e := OK([]map[string]any{{"a": 1}}, "hint")
	if e.Status != "ok" || e.Downgraded || e.SchemaHint != "hint" {
		t.Fatalf("%+v", e)
	}
}

func TestBoundRawDowngradesLargeResult(t *testing.T) {
	// 造超過 MaxTokens 的大量 rows
	rows := make([]map[string]any, 20000)
	for i := range rows {
		rows[i] = map[string]any{"_msg": "a fairly long log message number that repeats", "service": "api", "i": i}
	}
	e := BoundRaw(rows, "hint")
	if !e.Downgraded {
		t.Fatal("超量結果必須標 downgraded")
	}
	if e.Reason == "" || e.Hint == "" {
		t.Fatal("降級必須有 reason + hint(絕不靜默)")
	}
	if EstimateTokens(e.Data) > MaxTokens && e.Returned != "count" {
		t.Fatal("降級後仍須在預算內,或最終退到 count")
	}
}

// 降級 envelope 同時帶「筆數(returned)」與「降級種類(returned_kind)」,
// 兩者必須是不同的 JSON key,否則 AI 讀到重複 key 會混淆(違反契約)。
func TestDowngradeEnvelopeHasDistinctKeys(t *testing.T) {
	rows := make([]map[string]any, 20000)
	for i := range rows {
		rows[i] = map[string]any{"_msg": "a fairly long log message number that repeats", "service": "api", "i": i}
	}
	b, err := json.Marshal(BoundRaw(rows, "hint"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Count(s, `"returned"`) != 1 {
		t.Fatalf(`"returned" 應恰好出現一次(筆數),得: %s`, s)
	}
	if !strings.Contains(s, `"returned_kind"`) {
		t.Fatalf(`降級種類應在 "returned_kind" key: %s`, s)
	}
}

// 超過預算但有空間抽樣時,應回中間層 sampled_logs(而非直接塌到 count),
// 且抽樣本身必須放得進 token 預算。
func TestBoundRawReturnsSampledTier(t *testing.T) {
	rows := make([]map[string]any, 2000)
	for i := range rows {
		rows[i] = map[string]any{"_msg": "short log", "i": i}
	}
	e := BoundRaw(rows, "hint")
	if e.Returned != "sampled_logs" {
		t.Fatalf("應回 sampled_logs 中間層,得 returned_kind=%q", e.Returned)
	}
	if !e.Downgraded || !e.Truncated || e.TotalMatched != 2000 {
		t.Fatalf("抽樣層須標 downgraded/truncated/total_matched: %+v", e)
	}
	if e.ReturnedN < 1 {
		t.Fatal("抽樣層須回至少一筆樣本")
	}
	if EstimateTokens(e.Data) > MaxTokens {
		t.Fatal("抽樣結果仍須在 token 預算內")
	}
}

func TestErr(t *testing.T) {
	e := Err("QUERY_TIMEOUT", "too slow", "narrow range")
	if e.Status != "error" || e.ErrorCode != "QUERY_TIMEOUT" || e.Hint == "" {
		t.Fatalf("%+v", e)
	}
}
