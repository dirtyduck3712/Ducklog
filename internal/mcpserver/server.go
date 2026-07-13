// Package mcpserver 把 docklog tools 註冊成 MCP(stdio)。
package mcpserver

import (
	"context"
	"encoding/json"

	"docklog/internal/bound"
	"docklog/internal/tools"
	"docklog/internal/vl"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const version = "0.1.0"

// NewServer 建立 MCP server 並註冊 4 個 tool。抽離出來讓測試可在不阻塞 stdio 的情況下驅動。
func NewServer(c *vl.Client) *server.MCPServer {
	s := server.NewMCPServer("docklog", version)

	s.AddTool(mcp.NewTool("summarize_errors",
		mcp.WithDescription("AI 主入口:把 error 用 fingerprint 分群回 pattern+次數"),
		mcp.WithString("service", mcp.Description("服務名,可省")),
		mcp.WithString("time_range", mcp.Required(), mcp.Description("如 1h / 30m")),
	), wrap(func(ctx context.Context, a map[string]any) bound.Envelope {
		return tools.SummarizeErrors(ctx, c, str(a["service"]), str(a["time_range"]), 20)
	}))

	s.AddTool(mcp.NewTool("get_trace",
		mcp.WithDescription("拉出某 trace_id 的完整 log"),
		mcp.WithString("trace_id", mcp.Required()),
	), wrap(func(ctx context.Context, a map[string]any) bound.Envelope {
		return tools.GetTrace(ctx, c, str(a["trace_id"]))
	}))

	s.AddTool(mcp.NewTool("search_logs",
		mcp.WithDescription("結構化 LogsQL 查詢(含 truncation 標示)"),
		mcp.WithString("query", mcp.Required(), mcp.Description("LogsQL filter,如 service:=api level:=error")),
		mcp.WithString("time_range", mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("上限,預設 100")),
	), wrap(func(ctx context.Context, a map[string]any) bound.Envelope {
		return tools.SearchLogs(ctx, c, str(a["query"]), str(a["time_range"]), toInt(a["limit"]))
	}))

	s.AddTool(mcp.NewTool("compare_periods",
		mcp.WithDescription("比較兩時段的 error pattern,找暴增/新出現"),
		mcp.WithString("service", mcp.Description("服務名,可省")),
		mcp.WithString("t1", mcp.Required(), mcp.Description("時段一,如 1h")),
		mcp.WithString("t2", mcp.Required(), mcp.Description("時段二,如 2h")),
	), wrap(func(ctx context.Context, a map[string]any) bound.Envelope {
		return tools.ComparePeriods(ctx, c, str(a["service"]), str(a["t1"]), str(a["t2"]))
	}))

	return s
}

// Serve 建 server 後以 stdio 服務;把呼叫者的 ctx 注入每個 request context 讓取消可傳遞。
func Serve(ctx context.Context, c *vl.Client) error {
	s := NewServer(c)
	return server.ServeStdio(s, server.WithStdioContextFunc(func(context.Context) context.Context {
		return ctx
	}))
}

// wrap 把「回 Envelope 的函式」轉成 mcp-go handler:JSON 序列化 envelope 成 tool 文字結果。
func wrap(fn func(context.Context, map[string]any) bound.Envelope) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		env := fn(ctx, req.GetArguments())
		b, err := json.Marshal(env)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	}
}

// str 把 MCP 傳入的參數值轉字串(缺值回空字串)。
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// toInt 把 MCP 傳入的 number 參數轉 int(JSON number 為 float64;缺值回 0)。
func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}
