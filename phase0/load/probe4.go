//go:build ignore

// 決定架構分岔:
//   A. inline flush 到底能不能用(named-arg 形式)
//   B. inline 關掉 → 直接寫 Parquet → merge_adjacent_files 壓縮,這條路可行嗎
//   go run probe4.go
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/marcboeker/go-duckdb/v2"
)

func attach(dir string, inlineLimit string) *sql.DB {
	os.RemoveAll(dir)
	os.MkdirAll(dir+"/logs", 0o755)
	db, _ := sql.Open("duckdb", "")
	db.Exec("INSTALL ducklake")
	db.Exec("LOAD ducklake")
	db.Exec(fmt.Sprintf("ATTACH 'ducklake:%s/logs.ducklake' AS lake (DATA_PATH '%s/logs/')", dir, dir))
	db.Exec("CREATE TABLE lake.logs (seq BIGINT, msg VARCHAR)")
	if inlineLimit != "" {
		db.Exec("CALL lake.set_option('data_inlining_row_limit', '" + inlineLimit + "')")
	}
	return db
}

func pq(dir string) int { m, _ := filepath.Glob(dir + "/logs/*.parquet"); return len(m) }

func walMB(dir string) float64 {
	fi, err := os.Stat(dir + "/logs.ducklake.wal")
	if err != nil {
		return 0
	}
	return float64(fi.Size()) / 1024 / 1024
}

func main() {
	base := os.Getenv("SCRATCH")
	if base == "" {
		base = "/tmp"
	}

	// ---- A. named-arg flush ----
	fmt.Println("=== A. inline flush named-arg ===")
	{
		dir := base + "/p4a"
		db := attach(dir, "1000")
		for i := 0; i < 50; i++ {
			db.Exec("INSERT INTO lake.logs SELECT ? + range, 'm' FROM range(100)", i*100)
		}
		fmt.Printf("寫 5000 筆 inline 後: parquet=%d wal=%.2fMB\n", pq(dir), walMB(dir))
		tries := []string{
			"CALL ducklake_flush_inlined_data('lake', table_name => 'logs', schema_name => 'main')",
			"CALL ducklake_flush_inlined_data('lake', table_name => 'logs')",
			"CALL ducklake_flush_inlined_data('lake', schema_name => 'main', table_name => 'logs')",
		}
		for _, q := range tries {
			_, err := db.Exec(q)
			if err != nil {
				fmt.Printf("✗ %s\n   -> %s\n", short(q), firstLine(err.Error()))
			} else {
				fmt.Printf("✅ %s → parquet=%d wal=%.2fMB\n", short(q), pq(dir), walMB(dir))
			}
		}
		db.Close()
	}

	// ---- B. inline 關掉,直接寫 Parquet ----
	fmt.Println("\n=== B. inline off(limit=0)→ 直接寫 Parquet ===")
	{
		dir := base + "/p4b"
		db := attach(dir, "0")
		// 300 個 100 筆 batch(模擬 30s×10batch/s)
		for i := 0; i < 300; i++ {
			db.Exec("INSERT INTO lake.logs SELECT ? + range, 'm' FROM range(100)", i*100)
		}
		var n int64
		db.QueryRow("SELECT count(*) FROM lake.logs").Scan(&n)
		fmt.Printf("寫 300×100=%d 筆後: parquet檔數=%d wal=%.2fMB\n", n, pq(dir), walMB(dir))
		fmt.Println("→ 小檔爆炸程度:", pq(dir), "個 Parquet(300 batch)")

		// merge_adjacent_files 壓縮
		if _, err := db.Exec("CALL lake.merge_adjacent_files()"); err != nil {
			fmt.Println("✗ merge_adjacent_files:", firstLine(err.Error()))
		} else {
			fmt.Printf("✅ merge_adjacent_files → parquet檔數=%d\n", pq(dir))
		}
		// 清理過期檔
		db.Exec("CALL lake.expire_snapshots(older_than => now())")
		db.Exec("CALL lake.cleanup_old_files(cleanup_all => true)")
		fmt.Printf("expire+cleanup 後 → parquet檔數=%d\n", pq(dir))

		// 查一下確認資料還在且查得到
		db.QueryRow("SELECT count(*) FROM lake.logs").Scan(&n)
		fmt.Printf("最終 count=%d(資料仍完整)\n", n)
		db.Close()
	}
}

func short(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
