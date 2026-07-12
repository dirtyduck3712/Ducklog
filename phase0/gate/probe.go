//go:build ignore

// 探針:針對 gate 撞到的兩個問題做定點驗證。
//   go run probe.go
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/marcboeker/go-duckdb/v2"
)

func main() {
	// ---- 假設 A:改用 DuckDB catalog backend,CHECKPOINT 是否通? ----
	fmt.Println("=== A. DuckDB catalog backend + CHECKPOINT ===")
	testBackend("duckdb", "ducklake:data/dk.ducklake") // 注意:無 sqlite: 前綴 = duckdb catalog

	fmt.Println("\n=== B. sqlite catalog backend + CHECKPOINT(對照組) ===")
	testBackend("sqlite", "ducklake:sqlite:data/sq.ducklake")

	// ---- 假設 B:這版設定 inline row limit 的正確方式 ----
	fmt.Println("\n=== C. 探索 ducklake inline 設定與 options ===")
	exploreOptions()
}

func testBackend(tag, dsn string) {
	dir := "data"
	_ = os.RemoveAll(dir + "/" + tag)
	_ = os.MkdirAll(dir+"/"+tag+"/logs", 0o755)
	dsn2 := dsn[:len("ducklake:")] + insertDir(dsn[len("ducklake:"):], tag)

	db, _ := sql.Open("duckdb", "")
	defer db.Close()
	for _, s := range []string{"INSTALL ducklake", "LOAD ducklake", "INSTALL sqlite", "LOAD sqlite"} {
		if _, err := db.Exec(s); err != nil {
			fmt.Printf("  %s: %v\n", s, err)
			return
		}
	}
	attach := fmt.Sprintf("ATTACH '%s' AS lake (DATA_PATH '%s/%s/logs/')", dsn2, dir, tag)
	if _, err := db.Exec(attach); err != nil {
		fmt.Printf("  ATTACH: %v\n", err)
		return
	}
	if _, err := db.Exec("CREATE TABLE lake.logs (ts TIMESTAMP, service VARCHAR, msg VARCHAR)"); err != nil {
		fmt.Printf("  CREATE: %v\n", err)
		return
	}
	db.Exec("INSERT INTO lake.logs VALUES (now(), 'api', 'hi'), (now(), 'w', 'yo')")
	if _, err := db.Exec("CHECKPOINT lake"); err != nil {
		fmt.Printf("  ❌ CHECKPOINT: %v\n", err)
	} else {
		pq, _ := filepath.Glob(dir + "/" + tag + "/logs/*.parquet")
		fmt.Printf("  ✅ CHECKPOINT OK — Parquet 檔數 = %d\n", len(pq))
	}
}

// 把 tag 插進 catalog 檔名,避免兩組相撞
func insertDir(rest, tag string) string {
	// rest 形如 "data/dk.ducklake" 或 "sqlite:data/sq.ducklake"
	return rest // 已用不同檔名,直接回傳
}

func exploreOptions() {
	db, _ := sql.Open("duckdb", "")
	defer db.Close()
	for _, s := range []string{"INSTALL ducklake", "LOAD ducklake", "INSTALL sqlite", "LOAD sqlite"} {
		db.Exec(s)
	}
	_ = os.MkdirAll("data/opt/logs", 0o755)
	db.Exec("ATTACH 'ducklake:data/opt.ducklake' AS lake (DATA_PATH 'data/opt/logs/')")

	// 嘗試多種設定 inline row limit 的形式
	tries := []string{
		"CALL lake.set_option('data_inlining_row_limit', '1000')",
		"CALL ducklake_set_option('data_inlining_row_limit', 1000, catalog := 'lake')",
		"SET ducklake_data_inlining_row_limit = 1000",
		"ALTER SCHEMA lake SET OPTION data_inlining_row_limit = 1000",
	}
	for _, t := range tries {
		if _, err := db.Exec(t); err != nil {
			fmt.Printf("  ✗ %s\n      -> %v\n", t, firstLine(err.Error()))
		} else {
			fmt.Printf("  ✅ %s\n", t)
		}
	}

	// 列出所有 ducklake_* 設定,看有沒有 inlining 相關
	fmt.Println("  --- 現有 ducklake_* / inlining 相關設定 ---")
	rows, err := db.Query(`SELECT name, value FROM duckdb_settings() WHERE name ILIKE '%inlin%' OR name ILIKE '%ducklake%'`)
	if err != nil {
		fmt.Printf("  duckdb_settings 查詢失敗: %v\n", err)
		return
	}
	defer rows.Close()
	any := false
	for rows.Next() {
		var n, v string
		rows.Scan(&n, &v)
		fmt.Printf("    %s = %s\n", n, v)
		any = true
	}
	if !any {
		fmt.Println("    (無 inlining/ducklake 全域設定)")
	}

	// DuckLake 專屬的 options 系統表 / 函式
	fmt.Println("  --- 試 ducklake options 函式 ---")
	for _, q := range []string{
		"SELECT * FROM ducklake_options('lake')",
		"CALL lake.options()",
	} {
		rows, err := db.Query(q)
		if err != nil {
			fmt.Printf("  ✗ %s -> %v\n", q, firstLine(err.Error()))
			continue
		}
		cols, _ := rows.Columns()
		fmt.Printf("  ✅ %s cols=%v\n", q, cols)
		rows.Close()
	}
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
