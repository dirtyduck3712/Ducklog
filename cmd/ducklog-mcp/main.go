// ducklog-mcp:把 VictoriaLogs 包成 AI-first MCP 工具(stdio)。
package main

import (
	"context"
	"log"
	"os"
	"time"

	"ducklog/internal/mcpserver"
	"ducklog/internal/vl"
)

const defaultVLURL = "http://127.0.0.1:9428"

// resolveVLURL 讀 VL_URL 環境變數,未設則回預設位址。
func resolveVLURL() string {
	if u := os.Getenv("VL_URL"); u != "" {
		return u
	}
	return defaultVLURL
}

func main() {
	vlURL := resolveVLURL()
	c := vl.New(vlURL, 30*time.Second)
	if u := os.Getenv("VL_USERNAME"); u != "" {
		c = vl.NewWithAuth(vlURL, 30*time.Second, u, os.Getenv("VL_PASSWORD"))
	}
	if err := c.Ping(context.Background()); err != nil {
		log.Fatalf("VictoriaLogs 不可達(%s): %v", vlURL, err)
	}
	// /health 不受 -httpAuth 保護,Ping 驗不到憑證;用一筆極小的受保護查詢
	// 讓憑證錯誤在啟動就 fail-fast,而非每次 tool 呼叫才 401。
	if _, err := c.Query(context.Background(), "* _time:1s", 1); err != nil {
		log.Fatalf("VictoriaLogs 查詢驗證失敗(常見原因:VL_USERNAME/VL_PASSWORD 錯誤;%s): %v", vlURL, err)
	}
	if err := mcpserver.Serve(context.Background(), c); err != nil {
		log.Fatalf("MCP server: %v", err)
	}
}
