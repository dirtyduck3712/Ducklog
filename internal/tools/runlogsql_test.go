package tools

import (
	"context"
	"testing"
	"time"

	"ducklog/internal/vl"
	"ducklog/internal/vltest"
)

// pipe 查詢能跑，且證明 _time: 被注入到第一個 pipe「之前」。
// 若錯誤地 append 到 pipeline 尾端(... count() as n _time:1h),VL 會語法錯 → QUERY_FAILED。
func TestRunLogsQLRunsPipedQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVL(t)
	c := vl.New(base, 10*time.Second)
	ingestErrors(t, base) // 50 筆 service=api 的 error
	e := RunLogsQL(context.Background(), c, "level:=error | stats by (service) count() as n", "1h", 100)
	if e.Status != "ok" {
		t.Fatalf("pipe 查詢應成功: %+v", e)
	}
	rows, _ := e.Data.([]map[string]any)
	if len(rows) != 1 || str(rows[0]["service"]) != "api" || str(rows[0]["n"]) != "50" {
		t.Fatalf("聚合結果不符,want 1 列 service=api n=50,得: %v", rows)
	}
}

// TestInjectTimeFilter 直接測 unexported injectTimeFilter(純字串、免 VL)。
// 重點驗證 bug ①:引號/正則內的 '|' 不可被當 pipe 切分,且 filter 段被括號包住。
func TestInjectTimeFilter(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"no pipe", "level:=info", "_time:1h (level:=info)"},
		{"single pipe", "level:=error | stats by (service) count() as n", "_time:1h (level:=error) | stats by (service) count() as n"},
		{"multi pipe", "a | b | c", "_time:1h (a) | b | c"},
		{"regex with pipe", `_msg:~"timeout|panic"`, `_time:1h (_msg:~"timeout|panic")`},
		{"quoted phrase with pipe", `_msg:"user|admin" | stats count()`, `_time:1h (_msg:"user|admin") | stats count()`},
		{"top-level OR", "error OR warn", "_time:1h (error OR warn)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := injectTimeFilter(tc.query, "1h"); got != tc.want {
				t.Fatalf("injectTimeFilter(%q)\n  got  %q\n  want %q", tc.query, got, tc.want)
			}
		})
	}
}

func TestRunLogsQLRequiresTimeRange(t *testing.T) {
	c := vl.New("http://127.0.0.1:1", time.Second)
	e := RunLogsQL(context.Background(), c, "level:=info", "", 100)
	if e.Status != "error" || e.ErrorCode != "MISSING_TIME_RANGE" {
		t.Fatalf("空 range 應拒: %+v", e)
	}
}

func TestRunLogsQLRejectsEmptyQuery(t *testing.T) {
	c := vl.New("http://127.0.0.1:1", time.Second)
	e := RunLogsQL(context.Background(), c, "   ", "1h", 100)
	if e.Status != "error" || e.ErrorCode != "MALFORMED_QUERY" {
		t.Fatalf("空 query 應拒: %+v", e)
	}
}

func TestRunLogsQLMarksTruncation(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVL(t)
	c := vl.New(base, 10*time.Second)
	ingestN(t, base, 500) // 500 筆 info(無 pipe 查詢路徑)
	e := RunLogsQL(context.Background(), c, "level:=info", "1h", 100)
	if !e.Truncated {
		t.Fatalf("命中 limit 應標 Truncated: %+v", e)
	}
	if e.ReturnedN > 100 {
		t.Fatalf("returned 應 ≤ limit,得 %d", e.ReturnedN)
	}
	// bug ②:截斷時不做真 Count,TotalMatched 應清零(不吐誤導值)。
	if e.TotalMatched != 0 {
		t.Fatalf("截斷時 TotalMatched 應為 0(避免與 truncated 矛盾),得 %d", e.TotalMatched)
	}
}

// TestRunLogsQLTimeRangeActuallyFilters 證明時間注入真的過濾(bug ① 的行為面),
// 且正則含 '|' 不會被誤切成 pipe。
func TestRunLogsQLTimeRangeActuallyFilters(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVL(t)
	c := vl.New(base, 10*time.Second)
	ingestErrors(t, base) // 30 timeout + 20 panic(error)+ 50 info,皆「當下」

	// 過去窗(year 2000)必定排除當下 log → 證明時間注入真的過濾。
	past := RunLogsQL(context.Background(), c, "level:=error", "2000", 100)
	if past.Status != "ok" {
		t.Fatalf("過去窗查詢應成功: %+v", past)
	}
	if past.ReturnedN != 0 {
		t.Fatalf("過去窗應排除當下 log,得 %d 筆", past.ReturnedN)
	}

	// 當下窗 + 正則含 '|':| 不可被當 pipe 切分,30 timeout + 20 panic = 50 筆。
	now := RunLogsQL(context.Background(), c, `_msg:~"timeout|panic"`, "1h", 100)
	if now.Status != "ok" {
		t.Fatalf("正則查詢應成功(| 不可弄壞查詢): %+v", now)
	}
	rows, _ := now.Data.([]map[string]any)
	if len(rows) != 50 {
		t.Fatalf("正則應命中 50 筆(timeout+panic),得 %d: %v", len(rows), rows)
	}
}

// TestRunLogsQLQueryFailedOnBadSyntax 證明壞查詢誠實回報 QUERY_FAILED,而非靜默。
func TestRunLogsQLQueryFailedOnBadSyntax(t *testing.T) {
	if testing.Short() {
		t.Skip("需 VL")
	}
	base := vltest.StartVL(t)
	c := vl.New(base, 10*time.Second)
	e := RunLogsQL(context.Background(), c, "| nonsense", "1h", 100)
	if e.Status != "error" || e.ErrorCode != "QUERY_FAILED" {
		t.Fatalf("壞查詢應回 QUERY_FAILED: %+v", e)
	}
}
