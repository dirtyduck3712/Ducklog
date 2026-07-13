package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestEndToEndWireContract 啟動真正的 VictoriaLogs(VL)binary,用現有的 RemoteHandler
// 把 client 傳輸層產生的 wire 格式(NDJSON:{ts,service,level,trace_id,message,attrs})
// POST 進 VL 的 /insert/jsonline endpoint,再用 LogsQL 查回,斷言:三筆都在、_msg 等於
// 我們的 message、trace_id 可過濾、巢狀 attrs 的 host 值查得回。這證明現有 transport 的
// wire 契約與 VL 原生相容 —— 這是用 httptest fake 的單元測試無法涵蓋的互通性。
//
// 手動執行:cd client && VL_BINARY=<path> go test -run EndToEnd ./... -v
// -short 會直接 skip(需 VL binary + 較重)。
func TestEndToEndWireContract(t *testing.T) {
	if testing.Short() {
		t.Skip("端到端測試較重:需真正的 VictoriaLogs binary")
	}

	base := startVL(t)

	// Endpoint 指向 VL 的 jsonline ingest,附欄位映射:ts→_time、message→_msg、
	// service→stream field。transport 程式碼不變,只是換 Endpoint。
	endpoint := base + "/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service"

	const traceID = "550e8400-e29b-41d4-a716-446655440000" // canonical UUIDv4
	const hostVal = "10.0.1.5"

	h := NewRemoteHandler(RemoteConfig{
		Endpoint:      endpoint,
		APIKey:        "", // VL OSS 無 auth
		Service:       "e2e",
		Level:         slog.LevelDebug,
		FlushInterval: 50 * time.Millisecond,
		Fallback:      io.Discard, // 別把雙寫吐進測試輸出
	})

	log := slog.New(h)
	ctx := ContextWithTraceID(context.Background(), traceID)
	bg := context.Background()

	// 三筆:第一筆帶 canonical trace_id + 巢狀 attrs={"host":"10.0.1.5"}。
	log.ErrorContext(ctx, "boom", "host", hostVal)
	log.InfoContext(bg, "hello")
	log.WarnContext(bg, "careful")

	if err := h.Close(); err != nil { // 排空 queue 並 flush
		t.Fatalf("handler Close: %v", err)
	}

	// VL 查詢可見性落後 ingest(有緩衝)。輪詢查全部,直到看見 3 筆或逾時。
	var rows []map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rows = queryVL(t, base, "*")
		if len(rows) >= 3 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(rows) != 3 {
		t.Fatalf("VL 查回 %d 筆, want 3(10s 內)", len(rows))
	}

	// _msg 對得上我們的 message。
	msgs := map[string]map[string]any{}
	for _, r := range rows {
		m, _ := r["_msg"].(string)
		msgs[m] = r
	}
	for _, want := range []string{"boom", "hello", "careful"} {
		if _, ok := msgs[want]; !ok {
			t.Errorf("查回結果缺少 _msg=%q;實得 keys=%v", want, keysOf(msgs))
		}
	}

	// trace_id 過濾:精確查回帶 trace 的那筆。
	filtered := queryVL(t, base, `trace_id:="`+traceID+`"`)
	if len(filtered) != 1 {
		t.Fatalf("trace_id 過濾查回 %d 筆, want 1", len(filtered))
	}
	if got, _ := filtered[0]["_msg"].(string); got != "boom" {
		t.Errorf("trace_id 過濾的 _msg = %q, want \"boom\"", got)
	}

	// 巢狀 attrs 處理:不硬綁形態(VL 可能存成 attrs.host 或別的)。
	// 掃描 boom 那筆的所有欄位,找到值 == hostVal,記錄實際欄位名。
	boom := msgs["boom"]
	attrField := ""
	for k, v := range boom {
		if s, ok := v.(string); ok && s == hostVal {
			attrField = k
			break
		}
	}
	if attrField == "" {
		t.Fatalf("boom 那筆找不到 host 值 %q;欄位=%v", hostVal, boom)
	}
	t.Logf("attrs 巢狀 host 值以欄位 %q = %q 形式存於 VL(完整列:%v)", attrField, hostVal, boom)
}

// startVL 找到並啟動一個真正的 VL 進程,回傳 base URL(http://127.0.0.1:<port>)。
// binary 來源:env VL_BINARY,否則 spike 下載到的 scratch 預設路徑;兩者皆不存在則 skip。
func startVL(t *testing.T) string {
	t.Helper()

	bin := os.Getenv("VL_BINARY")
	if bin == "" {
		bin = "/tmp/claude-1000/-home-dva-workspace-docklog/" +
			"f9022b92-4536-49e5-8e38-c24f296b0368/scratchpad/vl/victoria-logs-prod"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("找不到 VL binary(設 VL_BINARY 指向 victoria-logs-prod):%v", err)
	}

	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	dataDir := t.TempDir()

	var stderr syncBuffer
	// VL 的 -httpListenAddr 不吃 :0,故用 bind-probe 取到的實際 port。
	vl := exec.Command(bin,
		"-storageDataPath="+dataDir,
		"-httpListenAddr="+addr,
		"-retentionPeriod=30d",
	)
	vl.Stderr = &stderr
	if err := vl.Start(); err != nil {
		t.Fatalf("start VL: %v", err)
	}
	t.Cleanup(func() {
		_ = vl.Process.Kill()
		_, _ = vl.Process.Wait()
	})

	base := "http://" + addr
	if !waitHealthy(base+"/health", 10*time.Second) {
		t.Fatalf("VL 未在 10s 內回 /health 200\nstderr:\n%s", stderr.String())
	}
	return base
}

// queryVL 用 LogsQL 查 VL,回傳每列一個 map。空結果回空 slice。
func queryVL(t *testing.T, base, logsql string) []map[string]any {
	t.Helper()
	u := base + "/select/logsql/query?query=" + url.QueryEscape(logsql)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("query VL %q: %v", logsql, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("query VL %q 狀態 = %d, want 200\nbody: %s", logsql, resp.StatusCode, raw)
	}
	var out []map[string]any
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("解析 VL 查詢結果列 %q: %v", line, err)
		}
		out = append(out, row)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("讀 VL 查詢結果: %v", err)
	}
	return out
}

func keysOf(m map[string]map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// freePort 綁一個臨時 socket 拿到空閒 port 再關掉。VL 的 -httpListenAddr 不吃 :0,
// 所以先探測、關閉、把 port 交給 VL(短暫 race window,實務可接受)。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitHealthy 輪詢 /health 直到回 200 或逾時。
func waitHealthy(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// syncBuffer 讓 exec 的 stderr 複製 goroutine 與測試讀取不 race。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
