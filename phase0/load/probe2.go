//go:build ignore

// 釐清:inline 資料實際存在哪、CHECKPOINT 有沒有 flush、正確 flush 成 Parquet 的方式。
//   go run probe2.go
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
	dir := base + "/probe2"
	os.RemoveAll(dir)
	os.MkdirAll(dir+"/logs", 0o755)

	db, _ := sql.Open("duckdb", "")
	defer db.Close()
	db.Exec("INSTALL ducklake")
	db.Exec("LOAD ducklake")
	db.Exec(fmt.Sprintf("ATTACH 'ducklake:%s/logs.ducklake' AS lake (DATA_PATH '%s/logs/')", dir, dir))
	db.Exec("CREATE TABLE lake.logs (seq BIGINT, msg VARCHAR)")
	db.Exec("CALL lake.set_option('data_inlining_row_limit', '1000')")

	report := func(tag string) {
		var n int64
		db.QueryRow("SELECT count(*) FROM lake.logs").Scan(&n)
		files, _ := os.ReadDir(dir)
		sizes := map[string]int64{}
		for _, f := range files {
			if fi, err := f.Info(); err == nil {
				sizes[f.Name()] = fi.Size()
			}
		}
		pq, _ := filepath.Glob(dir + "/logs/*.parquet")
		fmt.Printf("[%s] count=%d  files=%v  parquet=%d\n", tag, n, sizes, len(pq))
	}

	// 1. 很多小 batch(每 100 筆,< inline limit)
	for i := 0; i < 100; i++ {
		db.Exec("INSERT INTO lake.logs SELECT ? + range, 'm' FROM range(100)", i*100)
	}
	report("100×100 小 batch 後")

	// 2. CHECKPOINT
	if _, err := db.Exec("CHECKPOINT lake"); err != nil {
		fmt.Println("CHECKPOINT err:", err)
	}
	report("CHECKPOINT lake 後")

	// 3. 找 DuckLake 提供的 flush / inline 相關函式
	fmt.Println("--- ducklake 相關 table function ---")
	rows, err := db.Query(`SELECT DISTINCT function_name FROM duckdb_functions()
		WHERE function_name ILIKE '%ducklake%' OR function_name ILIKE '%inlin%' OR function_name ILIKE '%flush%'
		ORDER BY 1`)
	if err == nil {
		for rows.Next() {
			var fn string
			rows.Scan(&fn)
			fmt.Println("   ", fn)
		}
		rows.Close()
	}

	// 4. 試 flush_inlined_data
	for _, q := range []string{
		"CALL lake.flush_inlined_data()",
		"CALL ducklake_flush_inlined_data('lake')",
		"CALL lake.merge_adjacent_files()",
	} {
		if _, err := db.Exec(q); err != nil {
			fmt.Printf("   ✗ %s -> %s\n", q, firstLine(err.Error()))
		} else {
			fmt.Printf("   ✅ %s\n", q)
		}
	}
	report("試各種 flush 後")

	// 5. 單條大 insert(5000 > inline limit 1000)→ 會不會直接寫 Parquet?
	db.Exec("INSERT INTO lake.logs SELECT 9000000 + range, 'big' FROM range(5000)")
	report("單條 5000 大 insert 後(未 checkpoint)")
	db.Exec("CHECKPOINT lake")
	report("大 insert + CHECKPOINT 後")

	// 6. 看 catalog 內部 inline 資料表(DuckLake metadata)
	fmt.Println("--- catalog 內 ducklake metadata 表 ---")
	rows, err = db.Query(`SELECT table_name FROM information_schema.tables
		WHERE table_name ILIKE '%ducklake%' OR table_name ILIKE '%inlin%' ORDER BY 1 LIMIT 40`)
	if err == nil {
		for rows.Next() {
			var t string
			rows.Scan(&t)
			fmt.Println("   ", t)
		}
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
