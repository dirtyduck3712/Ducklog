// Phase 0 — item 4:查詢效能(500 萬筆假資料)。
//   go run ./query
// 目標:時間範圍+service 過濾 <1s;GROUP BY 聚合 <1s;LIKE '%timeout%' 量實際數字。
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/marcboeker/go-duckdb/v2"
)

var dir string

func main() {
	base := os.Getenv("SCRATCH")
	if base == "" {
		base = "/tmp"
	}
	dir = base + "/querydata"
	must(os.RemoveAll(dir))
	must(os.MkdirAll(dir+"/logs", 0o755))

	db, _ := sql.Open("duckdb", "")
	defer db.Close()
	exec(db, "INSTALL ducklake")
	exec(db, "LOAD ducklake")
	exec(db, fmt.Sprintf("ATTACH 'ducklake:%s/logs.ducklake' AS lake (DATA_PATH '%s/logs/')", dir, dir))
	exec(db, `CREATE TABLE lake.logs (
		ts TIMESTAMP, ingested_at TIMESTAMP, service VARCHAR,
		level UTINYINT, trace_id UUID, message VARCHAR, attrs JSON)`)

	// ---- 產 500 萬筆:50 chunk × 10 萬,每 chunk > inline limit → 直接寫 Parquet ----
	fmt.Println("• 產生 500 萬筆假資料…")
	const chunks, per = 50, 100_000
	t0 := time.Now()
	for c := 0; c < chunks; c++ {
		// ts 分散在過去 24 小時;service 10 種;level 0-3;message 含少量 timeout
		exec(db, fmt.Sprintf(`INSERT INTO lake.logs
			SELECT
				now() - INTERVAL (range %% 86400) SECOND,
				now(),
				'svc-' || (range %% 10),
				(range %% 4)::UTINYINT,
				gen_random_uuid(),
				CASE WHEN range %% 50 = 0
				     THEN 'connection timeout after ' || (range %% 60) || 's to 10.0.1.5:5432'
				     ELSE 'request handled in ' || (range %% 200) || 'ms' END,
				'{}'
			FROM range(%d, %d)`, c*per, (c+1)*per))
	}
	fmt.Printf("  寫入耗時 %v(%.0f 筆/s)\n", time.Since(t0),
		float64(chunks*per)/time.Since(t0).Seconds())

	// 落地 + 壓縮 + 清理,模擬穩態
	timeit(db, "CHECKPOINT lake", "CHECKPOINT lake")
	timeit(db, "merge_adjacent_files", "CALL lake.merge_adjacent_files()")
	tryExec(db, "CALL ducklake_expire_snapshots('lake', older_than => now())")
	tryExec(db, "CALL ducklake_cleanup_old_files('lake', cleanup_all => true)")

	pqN, pqSz := nestedParquet()
	var total int64
	db.QueryRow("SELECT count(*) FROM lake.logs").Scan(&total)
	fmt.Printf("• 穩態:rows=%d  parquet檔數=%d  parquet總大小=%.1fMB  catalog=%.1fMB\n",
		total, pqN, float64(pqSz)/1e6,
		float64(fileSize("logs.ducklake")+fileSize("logs.ducklake.wal"))/1e6)

	fmt.Println("\n=== 查詢效能(各跑 3 次取最好) ===")
	// A. 時間範圍 + service 過濾
	bench(db, "時間範圍+service 過濾 (<1s?)",
		"SELECT count(*) FROM lake.logs WHERE ts > now() - INTERVAL 1 HOUR AND service = 'svc-3'")
	// B. GROUP BY 聚合(summarize 類)
	bench(db, "GROUP BY service,level 聚合 (<1s?)",
		"SELECT service, level, count(*) FROM lake.logs WHERE ts > now() - INTERVAL 6 HOUR GROUP BY 1,2")
	// C. 全文 LIKE 全掃描(預期慢,量實際)
	bench(db, "LIKE '%timeout%' 全掃描(預期慢)",
		"SELECT count(*) FROM lake.logs WHERE message LIKE '%timeout%'")
	// D. 取一批 error 明細 + LIMIT(實際查詢型態)
	bench(db, "level=3 明細 + LIMIT 100",
		"SELECT ts, service, message FROM lake.logs WHERE level = 3 ORDER BY ts DESC LIMIT 100")
}

func bench(db *sql.DB, label, q string) {
	best := time.Hour
	var rows int64
	for i := 0; i < 3; i++ {
		t0 := time.Now()
		r, err := db.Query(q)
		must(err)
		var cnt int64
		for r.Next() {
			cnt++
		}
		r.Close()
		if d := time.Since(t0); d < best {
			best = d
			rows = cnt
		}
	}
	flag := "✅"
	if best > time.Second {
		flag = "⚠️ "
	}
	fmt.Printf("  %s %-38s %8v  (回傳 %d 列/群)\n", flag, label, best.Round(time.Millisecond), rows)
}

func timeit(db *sql.DB, label, q string) {
	t0 := time.Now()
	exec(db, q)
	fmt.Printf("  %s: %v\n", label, time.Since(t0).Round(time.Millisecond))
}

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

func exec(db *sql.DB, q string) {
	if _, err := db.Exec(q); err != nil {
		panic(fmt.Sprintf("%s: %v", q, err))
	}
}
func tryExec(db *sql.DB, q string) {
	if _, err := db.Exec(q); err != nil {
		fmt.Printf("  (maintenance skip) %s -> %v\n", q, err)
	}
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
