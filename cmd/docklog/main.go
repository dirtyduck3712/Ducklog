// docklog server:單 binary 的日誌收集器。Phase 1a = ingest + 儲存 + 防護。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"docklog/internal/auth"
	"docklog/internal/checkpoint"
	"docklog/internal/config"
	"docklog/internal/diskguard"
	"docklog/internal/health"
	"docklog/internal/ingest"
	"docklog/internal/metrics"
	"docklog/internal/ratelimit"
	"docklog/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "設定檔路徑")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("載入 config: %v", err)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("開啟 store: %v", err)
	}
	defer st.Close()
	ms, err := metrics.Open(filepath.Join(cfg.DataDir, "metrics.duckdb"))
	if err != nil {
		log.Fatalf("開啟 metrics: %v", err)
	}
	defer ms.Close()

	guard := diskguard.New(cfg.DataDir, nil)
	loop := checkpoint.NewLoop(st, cfg.CheckpointInterval)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go loop.Run(ctx)

	mux := http.NewServeMux()
	mux.Handle("/ingest", ingest.New(ingest.Deps{
		Store: st, Metrics: ms, Keys: auth.New(cfg.APIKeys),
		Limiter: ratelimit.New(cfg.RateLimits.Default, cfg.RateLimits.Overrides),
		Guard:   guard, Now: time.Now,
	}))
	mux.Handle("/health", health.New(health.Deps{
		Store: st, Metrics: ms, Guard: guard, LastCheckpoint: loop.LastCheckpoint,
	}))

	srv := &http.Server{Addr: cfg.Listen, Handler: mux}
	go func() {
		log.Printf("docklog 監聽 %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("關閉中…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	_ = os.Stdout.Sync()
}
