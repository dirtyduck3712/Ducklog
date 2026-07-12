// Package store 封裝 DuckLake 儲存層。所有 DuckLake 相關語法已於 phase0 驗證。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"docklog/internal/model"

	_ "github.com/marcboeker/go-duckdb/v2"
)

type Store struct {
	db      *sql.DB
	catalog string // logs.ducklake 檔的絕對路徑
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	// 單 writer:序列化到單一連線,避免 DuckLake single-writer 衝突。
	db.SetMaxOpenConns(1)

	catalog := filepath.Join(dataDir, "logs.ducklake")
	setup := []string{
		"INSTALL ducklake",
		"LOAD ducklake",
		fmt.Sprintf("ATTACH 'ducklake:%s' AS lake (DATA_PATH '%s')",
			catalog, filepath.Join(dataDir, "logs")+string(os.PathSeparator)),
		`CREATE TABLE IF NOT EXISTS lake.logs (
			ts TIMESTAMP, ingested_at TIMESTAMP, service VARCHAR,
			level UTINYINT, trace_id UUID, message VARCHAR, attrs JSON)`,
		"CALL lake.set_option('data_inlining_row_limit', '1000')",
	}
	for _, stmt := range setup {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("store setup %q: %w", stmt, err)
		}
	}
	return &Store{db: db, catalog: catalog}, nil
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Insert(ctx context.Context, entries []model.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // Commit 成功後為 no-op
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO lake.logs VALUES (?, ?, ?, ?, ?::UUID, ?, ?::JSON)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		var traceID any // 空字串 → NULL
		if e.TraceID != "" {
			traceID = e.TraceID
		}
		attrs := e.Attrs
		if attrs == "" {
			attrs = "{}"
		}
		if _, err := stmt.ExecContext(ctx,
			e.TS.UTC(), e.IngestedAt.UTC(), e.Service, uint8(e.Level),
			traceID, e.Message, attrs); err != nil {
			return fmt.Errorf("insert row: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) Count(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM lake.logs").Scan(&n)
	return n, err
}

// Checkpoint flush inline → Parquet,並修剪過期 snapshot / 清理孤兒檔以 bound catalog 成長。
func (s *Store) Checkpoint(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "CHECKPOINT lake"); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	// 維護類失敗不致命(可能無事可做),記錄但不中斷。
	_, _ = s.db.ExecContext(ctx,
		"CALL ducklake_expire_snapshots('lake', older_than => now() - INTERVAL 7 DAY)")
	_, _ = s.db.ExecContext(ctx,
		"CALL ducklake_cleanup_old_files('lake', cleanup_all => true)")
	return nil
}

// CatalogSizeBytes 回傳 .ducklake + .ducklake.wal 合計(inline 資料在 .wal)。
func (s *Store) CatalogSizeBytes() (int64, error) {
	var total int64
	for _, p := range []string{s.catalog, s.catalog + ".wal"} {
		fi, err := os.Stat(p)
		if err == nil {
			total += fi.Size()
		} else if !os.IsNotExist(err) {
			return 0, err
		}
	}
	return total, nil
}

func (s *Store) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM lake.logs WHERE ingested_at < ?", cutoff.UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.ExecContext(ctx, "CHECKPOINT lake"); err != nil {
		return n, fmt.Errorf("checkpoint after delete: %w", err)
	}
	return n, nil
}

func (s *Store) Close() error { return s.db.Close() }
