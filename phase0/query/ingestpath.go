//go:build ignore

// 驗證真實 ingest 寫入路徑:client 給字串 trace_id / JSON attrs,如何正確寫進
// UUID / JSON 欄位;batch prepared statement 是否可行。
//   go run ingestpath.go
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/marcboeker/go-duckdb/v2"
)

func main() {
	base := os.Getenv("SCRATCH")
	if base == "" {
		base = "/tmp"
	}
	dir := base + "/ingestpath"
	os.RemoveAll(dir)
	os.MkdirAll(dir+"/logs", 0o755)

	db, _ := sql.Open("duckdb", "")
	defer db.Close()
	for _, s := range []string{"INSTALL ducklake", "LOAD ducklake"} {
		if _, err := db.Exec(s); err != nil {
			panic(err)
		}
	}
	mustExec(db, fmt.Sprintf("ATTACH 'ducklake:%s/logs.ducklake' AS lake (DATA_PATH '%s/logs/')", dir, dir))
	mustExec(db, `CREATE TABLE lake.logs (
		ts TIMESTAMP, ingested_at TIMESTAMP, service VARCHAR,
		level UTINYINT, trace_id UUID, message VARCHAR, attrs JSON)`)

	// A. 單筆:字串 trace_id + JSON 字串,用 ::CAST
	_, err := db.Exec(
		`INSERT INTO lake.logs VALUES (?, now(), ?, ?, ?::UUID, ?, ?::JSON)`,
		"2026-07-13 10:00:00", "api", 3,
		"0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4050",
		"connection timeout", `{"host":"10.0.1.5","port":5432}`)
	if err != nil {
		fmt.Println("A. 單筆 CAST insert 失敗:", err)
	} else {
		fmt.Println("A. 單筆 CAST insert ✅")
	}

	// B. trace_id 可能為空(client 沒帶)→ 用 NULL
	_, err = db.Exec(
		`INSERT INTO lake.logs VALUES (?, now(), ?, ?, ?, ?, ?::JSON)`,
		"2026-07-13 10:00:01", "api", 1, nil, "no trace", `{}`)
	if err != nil {
		fmt.Println("B. NULL trace_id 失敗:", err)
	} else {
		fmt.Println("B. NULL trace_id ✅")
	}

	// C. batch:同一 tx,prepared statement 重複執行
	tx, _ := db.Begin()
	stmt, err := tx.Prepare(`INSERT INTO lake.logs VALUES (?, now(), ?, ?, ?::UUID, ?, ?::JSON)`)
	if err != nil {
		fmt.Println("C. Prepare 失敗:", err)
	} else {
		okC := true
		for i := 0; i < 100; i++ {
			_, err := stmt.Exec(
				"2026-07-13 10:00:02", "worker", 2,
				"0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4051",
				fmt.Sprintf("job %d", i), `{}`)
			if err != nil {
				fmt.Println("C. batch exec 失敗:", err)
				okC = false
				break
			}
		}
		stmt.Close()
		tx.Commit()
		if okC {
			fmt.Println("C. batch prepared(100 筆同 tx)✅")
		}
	}

	// D. 讀回驗證:UUID / JSON 欄位取值,attrs->>'key'
	var svc, tid, host string
	var lvl int
	err = db.QueryRow(
		`SELECT service, level, trace_id::VARCHAR, attrs->>'host'
		 FROM lake.logs WHERE level = 3`).Scan(&svc, &lvl, &tid, &host)
	if err != nil {
		fmt.Println("D. 讀回失敗:", err)
	} else {
		fmt.Printf("D. 讀回 ✅ service=%s level=%d trace_id=%s attrs.host=%s\n", svc, lvl, tid, host)
	}

	var n int
	db.QueryRow("SELECT count(*) FROM lake.logs").Scan(&n)
	fmt.Printf("總筆數=%d\n", n)
}

func mustExec(db *sql.DB, q string) {
	if _, err := db.Exec(q); err != nil {
		panic(fmt.Sprintf("%s: %v", q, err))
	}
}
