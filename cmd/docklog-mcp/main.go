// docklog-mcp:把 VictoriaLogs 包成 AI-first MCP 工具(stdio)。
package main

import (
	"context"
	"log"
	"os"
	"time"

	"docklog/internal/mcpserver"
	"docklog/internal/vl"
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
	if err := c.Ping(context.Background()); err != nil {
		log.Fatalf("VictoriaLogs 不可達(%s): %v", vlURL, err)
	}
	if err := mcpserver.Serve(context.Background(), c); err != nil {
		log.Fatalf("MCP server: %v", err)
	}
}
