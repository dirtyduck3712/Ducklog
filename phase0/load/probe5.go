//go:build ignore

// 乾淨、正確(遞迴數 parquet)地回答核心問題:
//   Q1. inline 批次寫入後,CHECKPOINT lake 會不會把 inline flush 成 Parquet?
//   Q2. 若不會,inline 資料何時落地?(db.Close?從不?)
//   Q3. flush_inlined_data 真的完全不能用?
// 全程單一連線,量測點明確。
//   go run probe5.go
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/marcboeker/go-duckdb/v2"
)

var dir string

func nestedParquet() (int, int64) {
	var n int
	var sz int64
	filepath.Walk(dir+"/logs", func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && filepath.Ext(p) == ".parquet" {
			n++
			sz += fi.Size()
		}
		return nil
	})
	return n, sz
}

func fileSize(name string) int64 {
	fi, err := os.Stat(dir + "/" + name)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func snap(tag string, db *sql.DB) {
	var n int64
	db.QueryRow("SELECT count(*) FROM lake.logs").Scan(&n)
	pqN, pqSz := nestedParquet()
	fmt.Printf("[%-28s] rows=%-7d parquet=%-3d(%6.1fKB) main=%5.1fKB wal=%6.1fKB\n",
		tag, n, pqN, float64(pqSz)/1024,
		float64(fileSize("logs.ducklake"))/1024, float64(fileSize("logs.ducklake.wal"))/1024)
}

func main() {
	base := os.Getenv("SCRATCH")
	if base == "" {
		base = "/tmp"
	}
	dir = base + "/probe5"
	os.RemoveAll(dir)
	os.MkdirAll(dir+"/logs", 0o755)

	db, _ := sql.Open("duckdb", "")
	db.Exec("INSTALL ducklake")
	db.Exec("LOAD ducklake")
	db.Exec(fmt.Sprintf("ATTACH 'ducklake:%s/logs.ducklake' AS lake (DATA_PATH '%s/logs/')", dir, dir))
	db.Exec("CREATE TABLE lake.logs (seq BIGINT, msg VARCHAR)")
	db.Exec("CALL lake.set_option('data_inlining_row_limit', '1000')")

	// 100 個 100 筆 inline batch = 10000 筆,全部 < inline limit → 應全 inline
	for i := 0; i < 100; i++ {
		db.Exec("INSERT INTO lake.logs SELECT ? + range, 'm' FROM range(100)", i*100)
	}
	snap("10000 筆 inline 寫入後", db)

	db.Exec("CHECKPOINT lake")
	snap("CHECKPOINT lake 後", db) // Q1:parquet 有沒有從 0 變多?

	// merge / expire / cleanup 組合
	db.Exec("CALL lake.merge_adjacent_files()")
	snap("merge_adjacent_files 後", db)

	// 再壓一批,checkpoint,看 wal 會不會無限長
	for i := 100; i < 200; i++ {
		db.Exec("INSERT INTO lake.logs SELECT ? + range, 'm' FROM range(100)", i*100)
	}
	db.Exec("CHECKPOINT lake")
	snap("再寫 10000 + CHECKPOINT", db)

	// Q3:重開一個新連線後,flush_inlined_data 是否可用?
	// (那個 not-implemented 錯誤可能跟「同一 txn 內剛寫剛讀」有關,換乾淨連線再試)
	db.Close()
	db2, _ := sql.Open("duckdb", "")
	db2.Exec("INSTALL ducklake")
	db2.Exec("LOAD ducklake")
	db2.Exec(fmt.Sprintf("ATTACH 'ducklake:%s/logs.ducklake' AS lake (DATA_PATH '%s/logs/')", dir, dir))
	fmt.Println("--- 重開連線 ---")
	snap("Close+重開後(WAL 應併入主檔)", db2)
	_, err := db2.Exec("CALL ducklake_flush_inlined_data('lake', table_name => 'logs', schema_name => 'main')")
	if err != nil {
		fmt.Printf("✗ 重開後 flush_inlined_data 仍失敗: %s\n", firstLine(err.Error()))
	} else {
		snap("重開後 flush_inlined_data 成功", db2)
	}
	db2.Close()
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
