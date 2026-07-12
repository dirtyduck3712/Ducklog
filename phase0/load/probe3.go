//go:build ignore

// 專攻:ducklake_flush_inlined_data 的正確呼叫方式,把 inline 真正落成 Parquet。
//   go run probe3.go
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/marcboeker/go-duckdb/v2"
)

func main() {
	base := os.Getenv("SCRATCH")
	if base == "" {
		base = "/tmp"
	}
	dir := base + "/probe3"
	os.RemoveAll(dir)
	os.MkdirAll(dir+"/logs", 0o755)

	db, _ := sql.Open("duckdb", "")
	defer db.Close()
	db.Exec("INSTALL ducklake")
	db.Exec("LOAD ducklake")
	db.Exec(fmt.Sprintf("ATTACH 'ducklake:%s/logs.ducklake' AS lake (DATA_PATH '%s/logs/')", dir, dir))
	db.Exec("CREATE TABLE lake.logs (seq BIGINT, msg VARCHAR)")
	db.Exec("CALL lake.set_option('data_inlining_row_limit', '1000')")

	// 寫一批 inline 資料
	for i := 0; i < 50; i++ {
		db.Exec("INSERT INTO lake.logs SELECT ? + range, 'm' FROM range(100)", i*100)
	}

	// 函式簽名
	fmt.Println("--- ducklake_flush_inlined_data 簽名 ---")
	rows, _ := db.Query(`SELECT function_name, parameters, parameter_types
		FROM duckdb_functions() WHERE function_name = 'ducklake_flush_inlined_data'`)
	for rows.Next() {
		var n string
		var p, pt any
		rows.Scan(&n, &p, &pt)
		fmt.Printf("   %s(params=%v types=%v)\n", n, p, pt)
	}
	rows.Close()

	pq := func() int { m, _ := filepath.Glob(dir + "/logs/*.parquet"); return len(m) }
	fmt.Printf("起始 parquet=%d\n", pq())

	// 各種呼叫形式
	tries := []string{
		"CALL ducklake_flush_inlined_data('lake')",
		"CALL ducklake_flush_inlined_data('lake', 'main', 'logs')",
		"SELECT * FROM ducklake_flush_inlined_data('lake')",
		"SELECT * FROM ducklake_flush_inlined_data(lake)",
		"CALL ducklake_flush_inlined_data(catalog => 'lake')",
	}
	for _, q := range tries {
		_, err := db.Exec(q)
		if err != nil {
			fmt.Printf("✗ %s\n    -> %s\n", q, firstLine(err.Error()))
		} else {
			fmt.Printf("✅ %s  → parquet=%d\n", q, pq())
		}
	}

	// 也試在明確 transaction 內、或用 Query 掃描
	fmt.Println("--- 在明確 transaction / 掃描形式 ---")
	tx, err := db.Begin()
	if err == nil {
		r, err := tx.Query("SELECT * FROM ducklake_flush_inlined_data('lake', 'main', 'logs')")
		if err != nil {
			fmt.Printf("✗ tx SELECT flush(3-arg): %s\n", firstLine(err.Error()))
		} else {
			r.Close()
			fmt.Println("✅ tx SELECT flush(3-arg) 無錯")
		}
		tx.Commit()
		fmt.Printf("   commit 後 parquet=%d\n", pq())
	}

	// 若成功,再 checkpoint 清 WAL,看 catalog wal 縮小
	db.Exec("CHECKPOINT lake")
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		fi, _ := f.Info()
		fmt.Printf("   %s = %d bytes\n", f.Name(), fi.Size())
	}
	fmt.Printf("最終 parquet=%d\n", pq())
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
