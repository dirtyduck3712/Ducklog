package vl

import "testing"

func TestEscape(t *testing.T) {
	if got := Escape("api"); got != `"api"` {
		t.Fatalf("plain: got %q", got)
	}
	if got := Escape(`a"b`); got != `"a\"b"` {
		t.Fatalf("embedded quote: got %q", got)
	}
	// 以 \ 結尾:反斜線先被跳脫成 \\,結尾不能吃掉封閉引號。
	got := Escape(`c:\`)
	if got != `"c:\\"` {
		t.Fatalf("trailing backslash: got %q, want %q", got, `"c:\\"`)
	}
	// 封閉引號完好:去掉頭尾引號後,内容以偶數個反斜線結尾。
	if got[len(got)-1] != '"' {
		t.Fatalf("closing quote missing: %q", got)
	}
}

func TestTimeFilter(t *testing.T) {
	if got := TimeFilter("1h"); got != "_time:1h" {
		t.Fatalf(`TimeFilter("1h") = %q`, got)
	}
	if got := TimeFilter(""); got != "" {
		t.Fatalf(`TimeFilter("") = %q`, got)
	}
}
