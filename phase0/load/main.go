// Phase 0 — item 1(inline 寫入吞吐)+ item 3(checkpoint 成本)。
//   go run ./load
//
// A. 以 ~1000 筆/秒持續寫入,取樣 catalog 大小 / Parquet 檔數 → 觀察 inline 行為
// B. 量 CHECKPOINT 停頓時間,以及 checkpoint 期間 ingest 是否被阻塞
// C. 順便量「盡全力寫」的最大吞吐,看離 1000/s 需求有多少 headroom
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/marcboeker/go-duckdb/v2"
)

var dataDir string

func main() {
	base := os.Getenv("SCRATCH")
	if base == "" {
		base = "/tmp"
	}
	dataDir = base + "/loaddata"
	must(os.RemoveAll(dataDir))
	must(os.MkdirAll(dataDir+"/logs", 0o755))

	db := openLake()
	defer db.Close()
	ensureSchema(db)

	fmt.Println("=== A. 持續 ~1000 筆/秒 × 30s,觀察 inline → Parquet ===")
	sustainedLoad(db, 30*time.Second)

	fmt.Println("\n=== B. CHECKPOINT 成本 + 期間 ingest 是否阻塞 ===")
	checkpointCost(db)

	fmt.Println("\n=== C. 盡全力寫的最大吞吐(headroom) ===")
	maxThroughput(db)
}

// ---------- A ----------

func sustainedLoad(db *sql.DB, dur time.Duration) {
	const batch = 100 // 10 batch/s = 1000 筆/s
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	sampler := time.NewTicker(5 * time.Second)
	defer sampler.Stop()

	start := time.Now()
	var total int64
	seq := int64(0)
	for {
		select {
		case <-ticker.C:
			insertBatch(db, seq, batch)
			seq += batch
			total += batch
		case <-sampler.C:
			fmt.Printf("  t=%2.0fs  rows=%-7d catalog=%6.2fMB parquet=%d\n",
				time.Since(start).Seconds(), total, catalogMB(), parquetCount())
		}
		if time.Since(start) >= dur {
			break
		}
	}
	fmt.Printf("  [結束] 共寫 %d 筆,實際 %.0f 筆/s;catalog=%.2fMB parquet=%d\n",
		total, float64(total)/time.Since(start).Seconds(), catalogMB(), parquetCount())
}

// ---------- B ----------

func checkpointCost(db *sql.DB) {
	// 背景持續 ingest,主執行緒觸發 CHECKPOINT,量 (a) checkpoint 耗時 (b) ingest 停頓
	var stop atomic.Bool
	var maxGap, batches int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		seq := int64(1_000_000)
		for !stop.Load() {
			t0 := time.Now()
			insertBatch(db, seq, 100)
			gap := time.Since(t0).Milliseconds()
			if gap > atomic.LoadInt64(&maxGap) {
				atomic.StoreInt64(&maxGap, gap)
			}
			seq += 100
			atomic.AddInt64(&batches, 1)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	time.Sleep(500 * time.Millisecond) // 讓背景 ingest 先跑起來
	t0 := time.Now()
	_, err := db.Exec("CHECKPOINT lake")
	ckDur := time.Since(t0)
	must(err)
	time.Sleep(500 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	fmt.Printf("  CHECKPOINT 耗時 = %v\n", ckDur)
	fmt.Printf("  期間背景 ingest 單 batch 最大停頓 = %dms(遠大於正常 batch 時間 → 表示被阻塞)\n",
		atomic.LoadInt64(&maxGap))
	fmt.Printf("  checkpoint 後 catalog=%.2fMB parquet=%d\n", catalogMB(), parquetCount())
}

// ---------- C ----------

func maxThroughput(db *sql.DB) {
	const dur = 5 * time.Second
	start := time.Now()
	var total int64
	seq := int64(9_000_000)
	for time.Since(start) < dur {
		insertBatch(db, seq, 500)
		seq += 500
		total += 500
	}
	fmt.Printf("  盡全力:%d 筆 / %.1fs = %.0f 筆/s(需求 1000/s 的 %.0f×)\n",
		total, dur.Seconds(), float64(total)/dur.Seconds(),
		float64(total)/dur.Seconds()/1000)
}

// ---------- 共用 ----------

func insertBatch(db *sql.DB, startSeq, n int64) {
	// 單條 INSERT ... SELECT range,模擬一次 batch flush
	_, err := db.Exec(
		`INSERT INTO lake.logs
		 SELECT ? + range, now(), now(),
		        CASE range % 3 WHEN 0 THEN 'api' WHEN 1 THEN 'worker' ELSE 'db' END,
		        (range % 4)::UTINYINT,
		        'connection timeout after ' || (range % 60) || 's to 10.0.1.5:5432',
		        '{}'
		 FROM range(?)`, startSeq, n)
	must(err)
}

func openLake() *sql.DB {
	db, err := sql.Open("duckdb", "")
	must(err)
	for _, s := range []string{"INSTALL ducklake", "LOAD ducklake"} {
		_, err := db.Exec(s)
		must(err)
	}
	_, err = db.Exec(fmt.Sprintf(
		"ATTACH 'ducklake:%s/logs.ducklake' AS lake (DATA_PATH '%s/logs/')", dataDir, dataDir))
	must(err)
	return db
}

func ensureSchema(db *sql.DB) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS lake.logs (
		seq BIGINT, ts TIMESTAMP, ingested_at TIMESTAMP,
		service VARCHAR, level UTINYINT, message VARCHAR, attrs JSON)`)
	must(err)
	_, err = db.Exec("CALL lake.set_option('data_inlining_row_limit', '1000')")
	must(err)
}

func catalogMB() float64 {
	fi, err := os.Stat(dataDir + "/logs.ducklake")
	if err != nil {
		return 0
	}
	return float64(fi.Size()) / 1024 / 1024
}

func parquetCount() int {
	m, _ := filepath.Glob(dataDir + "/logs/*.parquet")
	return len(m)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
