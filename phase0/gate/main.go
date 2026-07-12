// Phase 0 — Gate: 驗證 marcboeker/go-duckdb 能否載入 DuckLake 並跑通完整寫入路徑。
// 這是整個 v4 架構的地基。過不了 → 退回 v3(自寫 WAL + manifest)。
// throwaway spike,用完即丟。
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/marcboeker/go-duckdb/v2"
)

func main() {
	if err := run(); err != nil {
		fmt.Printf("\n❌ GATE FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n✅ GATE PASS — go-duckdb 能載入 DuckLake 並跑通 ATTACH/CREATE/INSERT/SELECT\n")
}

func run() error {
	// 乾淨環境:每次重跑都刪掉舊 data
	_ = os.RemoveAll("data")
	if err := os.MkdirAll("data/logs", 0o755); err != nil {
		return err
	}

	db, err := sql.Open("duckdb", "") // in-memory root,catalog 用 ATTACH
	if err != nil {
		return fmt.Errorf("sql.Open: %w", err)
	}
	defer db.Close()

	// 0. 實際 DuckDB 版本(模組版號 ≠ DuckDB 版號,實測才算數)
	var ver string
	if err := db.QueryRow("SELECT version()").Scan(&ver); err != nil {
		return fmt.Errorf("SELECT version(): %w", err)
	}
	fmt.Printf("• DuckDB version: %s\n", ver)

	// 1. INSTALL / LOAD ducklake(需要網路下載 extension)
	for _, stmt := range []string{
		"INSTALL ducklake",
		"LOAD ducklake",
		"INSTALL sqlite", // sqlite catalog backend 需要
		"LOAD sqlite",
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%q: %w", stmt, err)
		}
		fmt.Printf("• %s ✓\n", stmt)
	}

	// 2. ATTACH DuckLake catalog(DuckDB backend + Parquet DATA_PATH)
	//    註:sqlite backend 在 DuckDB 1.4.1 的 DuckLake 上 CHECKPOINT 會壞
	//    (TEXT timestamp vs TIMESTAMP 比較),故改用 duckdb catalog。
	attach := "ATTACH 'ducklake:data/logs.ducklake' AS lake (DATA_PATH 'data/logs/')"
	if _, err := db.Exec(attach); err != nil {
		return fmt.Errorf("ATTACH: %w", err)
	}
	fmt.Printf("• ATTACH ducklake:duckdb ✓\n")

	// 3. schema(照規格)
	createTable := `CREATE TABLE lake.logs (
		ts           TIMESTAMP,
		ingested_at  TIMESTAMP,
		service      VARCHAR,
		level        UTINYINT,
		trace_id     UUID,
		message      VARCHAR,
		attrs        JSON
	)`
	if _, err := db.Exec(createTable); err != nil {
		return fmt.Errorf("CREATE TABLE: %w", err)
	}
	fmt.Printf("• CREATE TABLE lake.logs ✓\n")

	// 4. data inlining 門檻調高(log batch 通常 100 筆)
	//    DuckDB 1.4.1 的 DuckLake:用 per-catalog option,非全域 SET。
	if _, err := db.Exec("CALL lake.set_option('data_inlining_row_limit', '1000')"); err != nil {
		return fmt.Errorf("set data_inlining_row_limit: %w", err)
	}
	fmt.Printf("• CALL lake.set_option('data_inlining_row_limit','1000') ✓\n")

	// 5. INSERT 幾筆(混 UUID / JSON / 各 level)
	insert := `INSERT INTO lake.logs VALUES
		(TIMESTAMP '2026-07-13 10:00:00', now(), 'api',   3, gen_random_uuid(), 'connection timeout after 30s', '{"host":"10.0.1.5"}'),
		(TIMESTAMP '2026-07-13 10:00:01', now(), 'api',   1, gen_random_uuid(), 'request ok',                   '{}'),
		(TIMESTAMP '2026-07-13 10:00:02', now(), 'worker',2, gen_random_uuid(), 'retrying job 42',              '{"attempt":2}')`
	if _, err := db.Exec(insert); err != nil {
		return fmt.Errorf("INSERT: %w", err)
	}
	fmt.Printf("• INSERT 3 rows ✓\n")

	// 6. SELECT count + 讀回一筆確認資料完整
	var n int
	if err := db.QueryRow("SELECT count(*) FROM lake.logs").Scan(&n); err != nil {
		return fmt.Errorf("SELECT count: %w", err)
	}
	if n != 3 {
		return fmt.Errorf("count = %d, 期望 3", n)
	}
	fmt.Printf("• SELECT count(*) = %d ✓\n", n)

	var svc, msg string
	if err := db.QueryRow(
		"SELECT service, message FROM lake.logs WHERE level = 3",
	).Scan(&svc, &msg); err != nil {
		return fmt.Errorf("SELECT error row: %w", err)
	}
	fmt.Printf("• 讀回 error 筆: service=%s message=%q ✓\n", svc, msg)

	// 7. Parquet 檔數:checkpoint 前應為 0(資料全在 catalog inline)
	parquets, _ := filepath.Glob("data/logs/*.parquet")
	fmt.Printf("• checkpoint 前 Parquet 檔數 = %d (期望 0,inline 生效)\n", len(parquets))

	// 8. CHECKPOINT lake(規格的 rollover 一行版)
	if _, err := db.Exec("CHECKPOINT lake"); err != nil {
		return fmt.Errorf("CHECKPOINT lake: %w", err)
	}
	parquetsAfter, _ := filepath.Glob("data/logs/*.parquet")
	fmt.Printf("• CHECKPOINT lake ✓ — 之後 Parquet 檔數 = %d\n", len(parquetsAfter))

	return nil
}
