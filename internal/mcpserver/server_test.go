package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"ducklog/internal/bound"
	"ducklog/internal/vl"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// fakeVL 是一個最小的 VictoriaLogs stub:count 查詢回 n=2,一般查詢回兩筆 log。
func fakeVL(t *testing.T) *vl.Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/x-ndjson")
		if strings.Contains(q, "stats count()") {
			_, _ = w.Write([]byte(`{"n":"2"}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"_time":"2026-07-13T00:00:00Z","service":"api","level":"error","_msg":"boom"}` + "\n" +
			`{"_time":"2026-07-13T00:00:01Z","service":"api","level":"error","_msg":"boom2"}` + "\n"))
	}))
	t.Cleanup(ts.Close)
	return vl.New(ts.URL, 5*time.Second)
}

// TestWiring 端到端驗證 wiring:透過 in-process MCP client 走完整 protocol
// (Initialize → ListTools → CallTool),確認 4 個 tool 註冊正確,且一次真正的
// tool 呼叫會回傳 JSON 序列化的 bound.Envelope。
func TestWiring(t *testing.T) {
	s := NewServer(fakeVL(t))

	cli, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	ctx := context.Background()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cli.Close()

	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "0.0.0"}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// 1) 4 個 tool 以預期名稱註冊。
	lt, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := make([]string, 0, len(lt.Tools))
	for _, tool := range lt.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)
	want := []string{"compare_periods", "get_trace", "search_logs", "summarize_errors"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registered tools = %v, want %v", got, want)
	}

	// 2) 驅動一個 handler:回傳應是 JSON 化的 Envelope(status=ok)。
	var call mcp.CallToolRequest
	call.Params.Name = "search_logs"
	call.Params.Arguments = map[string]any{"query": "service:=api", "time_range": "1h"}
	res, err := cli.CallTool(ctx, call)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("CallTool returned no content")
	}
	text, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] type = %T, want mcp.TextContent", res.Content[0])
	}
	var env bound.Envelope
	if err := json.Unmarshal([]byte(text.Text), &env); err != nil {
		t.Fatalf("result is not a JSON Envelope: %v (raw=%q)", err, text.Text)
	}
	if env.Status != "ok" {
		t.Fatalf("envelope status = %q, want ok (raw=%q)", env.Status, text.Text)
	}
	if env.TotalMatched != 2 {
		t.Fatalf("envelope total_matched = %d, want 2", env.TotalMatched)
	}
}
