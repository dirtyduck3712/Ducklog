// Package vltest 提供啟動臨時 VictoriaLogs 實例的測試輔助。
// 僅由 _test.go 匯入,不會進入正式 build。
package vltest

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// scratchBinary 是 task brief 指定的 VL binary 預設位置。
const scratchBinary = "/tmp/claude-1000/-home-dva-workspace-ducklog/f9022b92-4536-49e5-8e38-c24f296b0368/scratchpad/vl/victoria-logs-prod"

// StartVL 啟動一個臨時 VictoriaLogs 實例,回傳 base URL(如 http://127.0.0.1:12345)。
// 找不到 binary 時 t.Skip。程序在 t.Cleanup 時被 kill。
func StartVL(t testing.TB) string {
	t.Helper()

	bin := os.Getenv("VL_BINARY")
	if bin == "" {
		bin = scratchBinary
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("VL binary 不存在,設 VL_BINARY")
	}

	port := freePort(t)

	cmd := exec.Command(bin,
		"-storageDataPath="+t.TempDir(),
		"-httpListenAddr=127.0.0.1:"+strconv.Itoa(port),
		"-retentionPeriod=30d",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("啟動 VL 失敗: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return base
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("VL /health 未在時限內就緒 (%s)", base)
	return ""
}

// Ingest POST lines 到 VL 的 jsonline endpoint,並 poll 直到資料可查詢。
func Ingest(t testing.TB, base string, lines ...string) {
	t.Helper()

	body := strings.Join(lines, "\n")
	url := base + "/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service"
	resp, err := http.Post(url, "application/x-ndjson", strings.NewReader(body))
	if err != nil {
		t.Fatalf("ingest POST 失敗: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("ingest 回傳 %d", resp.StatusCode)
	}

	// VL ingest 為緩衝式,查詢可見性約落後 ~1s,poll 到列數就緒。
	want := len(lines)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if countAll(t, base) >= want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("ingest 後未在時限內看到 %d 列", want)
}

// countAll 查詢目前 VL 中的總列數。
func countAll(t testing.TB, base string) int {
	t.Helper()
	resp, err := http.Get(base + "/select/logsql/query?query=" + "*+%7C+stats+count()+as+n")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0
	}
	b, _ := io.ReadAll(resp.Body)
	// 回傳 NDJSON,例如 {"n":"2"};以簡單方式取數字。
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0
	}
	idx := strings.Index(s, `"n":"`)
	if idx < 0 {
		return 0
	}
	rest := s[idx+len(`"n":"`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return 0
	}
	n, _ := strconv.Atoi(rest[:end])
	return n
}

// freePort bind-probe 一個空 port(VL 不接受 :0)。
func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("找不到空 port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}
