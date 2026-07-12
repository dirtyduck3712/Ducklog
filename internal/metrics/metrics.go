// Package metrics 管獨立的 metrics.duckdb(不進 DuckLake,因為需要 PRIMARY KEY upsert)。
package metrics

import (
	"context"
	"database/sql"
	"time"

	"docklog/internal/model"

	_ "github.com/marcboeker/go-duckdb/v2"
)

type Bucket struct {
	TS      time.Time
	Service string
	Level   model.Level
	Count   int64
}

type MetricsStore struct{ db *sql.DB }

func Open(path string) (*MetricsStore, error) {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	schema := []string{
		`CREATE TABLE IF NOT EXISTS metrics (
			ts TIMESTAMP, service VARCHAR, level UTINYINT, count BIGINT,
			PRIMARY KEY (ts, service, level))`,
		`CREATE TABLE IF NOT EXISTS dropped (
			ts TIMESTAMP, service VARCHAR, count BIGINT,
			PRIMARY KEY (ts, service))`,
	}
	for _, s := range schema {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &MetricsStore{db: db}, nil
}

func (m *MetricsStore) DB() *sql.DB { return m.db }

// Add 以分鐘為 bucket upsert 累加。ts 由呼叫端對齊到分鐘。
func (m *MetricsStore) Add(ctx context.Context, buckets []Bucket) error {
	if len(buckets) == 0 {
		return nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO metrics VALUES (?, ?, ?, ?)
		 ON CONFLICT (ts, service, level) DO UPDATE SET count = count + excluded.count`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, b := range buckets {
		if _, err := stmt.ExecContext(ctx,
			b.TS.UTC().Truncate(time.Minute), b.Service, uint8(b.Level), b.Count); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (m *MetricsStore) AddDropped(ctx context.Context, ts time.Time, service string, n int64) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO dropped VALUES (?, ?, ?)
		 ON CONFLICT (ts, service) DO UPDATE SET count = count + excluded.count`,
		ts.UTC().Truncate(time.Minute), service, n)
	return err
}

func (m *MetricsStore) DroppedSince(ctx context.Context, since time.Time) (int64, error) {
	var n sql.NullInt64
	err := m.db.QueryRowContext(ctx,
		"SELECT sum(count) FROM dropped WHERE ts >= ?", since.UTC()).Scan(&n)
	return n.Int64, err
}

func (m *MetricsStore) Close() error { return m.db.Close() }
