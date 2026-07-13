package tools

import "testing"

func TestDiffPatterns(t *testing.T) {
	// VL 回傳的 count 是字串,toInt 會 parse。
	p1 := []map[string]any{
		{"_msg": "timeout", "n": "10"},
		{"_msg": "spike", "n": "10"},
		{"_msg": "small", "n": "10"},
		{"_msg": "same", "n": "10"},
	}
	p2 := []map[string]any{
		{"_msg": "timeout", "n": "10"}, // 未變,不 flag
		{"_msg": "spike", "n": "30"},   // 10→30,>2×,spike
		{"_msg": "small", "n": "15"},   // 10→15,≤2×,不 flag
		{"_msg": "same", "n": "10"},    // 相等,不 flag
		{"_msg": "fresh", "n": "5"},    // p1 無,new
	}

	got := map[string]map[string]any{}
	for _, d := range diffPatterns(p1, p2) {
		got[str(d["pattern"])] = d
	}

	if d, ok := got["fresh"]; !ok {
		t.Fatalf("fresh 應被 flag new")
	} else if d["new"] != true {
		t.Fatalf("fresh 應 new:true,得 %v", d["new"])
	}
	if d, ok := got["spike"]; !ok {
		t.Fatalf("spike 應被 flag")
	} else if d["new"] != false {
		t.Fatalf("spike 應 new:false,得 %v", d["new"])
	}
	if _, ok := got["small"]; ok {
		t.Fatalf("small (10→15, ≤2×) 不應被 flag")
	}
	if _, ok := got["same"]; ok {
		t.Fatalf("same (相等) 不應被 flag")
	}
	if _, ok := got["timeout"]; ok {
		t.Fatalf("timeout (未增) 不應被 flag")
	}
}
