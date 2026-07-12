package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestEndToEndWireContract 啟動真正的 docklog server binary,直接把 client 傳輸層
// 產生的 wire 格式(NDJSON + Bearer auth + canonical UUID trace)POST 進去,斷言
// server 回 accepted:3 / rejected:0。這證明真 server 接受 client 的 wire 契約 ——
// 這是用 httptest fake 的單元測試無法涵蓋的互通性。
//
// 手動執行:cd client && go test -run EndToEnd ./...
// -short 會直接 skip(server build 走 CGO go-duckdb,首次較慢)。
func TestEndToEndWireContract(t *testing.T) {
	if testing.Short() {
		t.Skip("端到端測試較重:需跨模組 build 真 server(CGO go-duckdb)")
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "docklogd")

	// server module(../)需要 go 1.24;client module 只要 1.22。GOTOOLCHAIN 只設在
	// 這個 build 指令上。從 client 目錄 build `../cmd/docklog` 會因兩者是獨立 module 而
	// 失敗("outside main module"),故直接把工作目錄設為 repo 根(..)、target 用
	// server module 內的 ./cmd/docklog。
	build := exec.Command("go", "build", "-o", bin, "./cmd/docklog")
	build.Dir = ".." // repo 根 = server module
	build.Env = append(os.Environ(), "GOTOOLCHAIN=go1.24.0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build server binary: %v\n%s", err, out)
	}

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	dataDir := filepath.Join(tmp, "data")

	// rate_limits.default 必填:未設時 default rate = 0,server 會丟棄所有寫入。
	cfg := fmt.Sprintf(`listen: "%s"
data_dir: "%s"
checkpoint_interval: "1h"
rate_limits:
  default: 1000
api_keys:
  - key: "e2e-key"
    name: "e2e"
    scopes: ["ingest"]
`, addr, dataDir)
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stderr syncBuffer
	srv := exec.Command(bin, "-config", cfgPath)
	srv.Stderr = &stderr
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer func() {
		_ = srv.Process.Kill()
		_, _ = srv.Process.Wait()
	}()

	healthURL := "http://" + addr + "/health"
	if !waitHealthy(healthURL, 10*time.Second) {
		t.Fatalf("server 未在 10s 內回 /health 200\nstderr:\n%s", stderr.String())
	}

	// --- 核心斷言:client wire 格式打進真 server ---
	// 用 package 自己的 entry + encodeNDJSON,確保送的就是 sender 會送的位元組。
	entries := []entry{
		{TS: "2026-07-13T00:00:00Z", Service: "e2e", Level: "error",
			TraceID: "550e8400-e29b-41d4-a716-446655440000", Message: "boom"},
		{TS: "2026-07-13T00:00:01Z", Service: "e2e", Level: "info", Message: "hello"},
		{TS: "2026-07-13T00:00:02Z", Service: "e2e", Level: "warn", Message: "careful"},
	}
	var body bytes.Buffer
	if err := encodeNDJSON(&body, entries); err != nil {
		t.Fatalf("encodeNDJSON: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/ingest", &body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer e2e-key")
	req.Header.Set("Content-Type", "application/x-ndjson")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /ingest: %v\nstderr:\n%s", err, stderr.String())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /ingest 狀態 = %d, want 200\nbody: %s\nstderr:\n%s",
			resp.StatusCode, raw, stderr.String())
	}

	var ir struct {
		Status    string `json:"status"`
		Accepted  int    `json:"accepted"`
		Rejected  int    `json:"rejected"`
		Malformed int    `json:"malformed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ir); err != nil {
		t.Fatalf("decode ingest response: %v", err)
	}
	if ir.Accepted != 3 || ir.Rejected != 0 || ir.Malformed != 0 {
		t.Fatalf("ingest 回應 = %+v, want accepted:3 rejected:0 malformed:0", ir)
	}

	// --- 煙霧測試:RemoteHandler 對真 server 送實際流量 ---
	h := NewRemoteHandler(RemoteConfig{
		Endpoint:      "http://" + addr + "/ingest",
		APIKey:        "e2e-key",
		Service:       "e2e-handler",
		Level:         slog.LevelDebug,
		FlushInterval: 50 * time.Millisecond,
		Fallback:      io.Discard, // 別把雙寫吐進測試輸出
	})
	log := slog.New(h)
	ctx := context.Background()
	log.ErrorContext(ctx, "e1", "n", 1)
	log.InfoContext(ctx, "e2", "n", 2)
	log.WarnContext(ctx, "e3", "n", 3)
	if err := h.Close(); err != nil { // 排空 queue 並 flush
		t.Fatalf("handler Close: %v", err)
	}

	// server 沒被 handler 的真實流量弄掛。
	if !waitHealthy(healthURL, 3*time.Second) {
		t.Fatalf("handler 送完後 server 不健康\nstderr:\n%s", stderr.String())
	}
}

// freePort 綁一個臨時 socket 拿到空閒 port 再關掉。config-file server 無法用 :0,
// 所以先探測、關閉、把 port 交給 server(短暫 race window,實務可接受)。
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
