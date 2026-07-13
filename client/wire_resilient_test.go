package client

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// encodeNDJSON 遇到單一無法序列化的 entry 時,不可丟整批,也不可丟該行:
// 好的相鄰 entry 必須完整送出,壞的那筆保留 ts/service/level/message,
// 只把 attrs 換成 _encode_error 標記。這是 jsonSafe 之外的最後保底。
func TestEncodeNDJSONResilientToPoisonEntry(t *testing.T) {
	entries := []entry{
		{TS: "t1", Service: "s", Level: "info", Message: "good1"},
		{TS: "t2", Service: "s", Level: "error", Message: "poison", Attrs: map[string]any{"fn": func() {}}}, // 原始 func -> marshal 失敗
		{TS: "t3", Service: "s", Level: "info", Message: "good2"},
	}
	var buf bytes.Buffer
	if err := encodeNDJSON(&buf, entries); err != nil {
		t.Fatalf("單一壞 entry 不應讓 encode 失敗: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("三行都必須在(相鄰好 entry 不被牽連),得 %d 行:\n%s", len(lines), buf.String())
	}
	var msgs []string
	for _, ln := range lines {
		var e map[string]any
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("每行都須是合法 JSON,壞行: %q err=%v", ln, err)
		}
		msgs = append(msgs, e["message"].(string))
		if e["message"] == "poison" {
			attrs, _ := e["attrs"].(map[string]any)
			if _, ok := attrs["_encode_error"]; !ok {
				t.Errorf("壞 entry 應保留並標 _encode_error,得 attrs=%v", attrs)
			}
		}
	}
	want := []string{"good1", "poison", "good2"}
	for i, m := range want {
		if msgs[i] != m {
			t.Errorf("第 %d 行 message = %q, want %q", i, msgs[i], m)
		}
	}
}
