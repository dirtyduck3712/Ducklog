// Phase 0 — item 2:crash 安全性。
// 驗證 DuckDB catalog 在 kill -9 下的 ACID:寫入中崩潰不丟已 commit 資料、不留半個 transaction;
// checkpoint 中崩潰重開乾淨、無重複無遺失。
//
// 用法(driver 會自己 build 出真正的 child binary 再 SIGKILL):
//   go run ./crash
//
// 環境變數 CRASH_ROLE 決定角色:
//   ingest     — 不斷以 batch transaction 寫入,每次 commit 後 fsync 累計數到 counter 檔
//   checkpoint — 先寫定量並記錄,再狂跑 CHECKPOINT 直到被殺
package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	_ "github.com/marcboeker/go-duckdb/v2"
)

const (
	batch       = 50 // 每個 transaction 寫 50 筆 → count 必為 50 的倍數(原子性)
	inlineLimit = "1000"
)

func main() {
	switch os.Getenv("CRASH_ROLE") {
	case "ingest":
		childIngest()
	case "checkpoint":
		childCheckpoint()
	default:
		driver()
	}
}

// ---------- 共用:開 catalog ----------

func openLake(dataDir string) *sql.DB {
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
		seq BIGINT, ts TIMESTAMP, service VARCHAR, message VARCHAR)`)
	must(err)
	_, _ = db.Exec("CALL lake.set_option('data_inlining_row_limit', '" + inlineLimit + "')")
}

// ---------- child:持續寫入 ----------

func childIngest() {
	dataDir := os.Getenv("CRASH_DATA")
	counterFile := os.Getenv("CRASH_COUNTER")
	db := openLake(dataDir)
	ensureSchema(db)

	committed := int64(0)
	cf, err := os.OpenFile(counterFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	must(err)

	for {
		tx, err := db.Begin()
		must(err)
		for i := int64(0); i < batch; i++ {
			seq := committed + i
			_, err := tx.Exec(
				"INSERT INTO lake.logs VALUES (?, now(), 'api', 'msg')", seq)
			must(err)
		}
		must(tx.Commit())
		committed += batch
		// commit 成功「之後」才 fsync 累計數。→ DB count 恆 >= 檔案值。
		writeCounter(cf, committed)
	}
}

func writeCounter(cf *os.File, n int64) {
	must2(cf.Seek(0, 0))
	must2(cf.WriteString(strconv.FormatInt(n, 10) + "\n"))
	must(cf.Sync())
}

// ---------- child:狂跑 checkpoint ----------

func childCheckpoint() {
	dataDir := os.Getenv("CRASH_DATA")
	counterFile := os.Getenv("CRASH_COUNTER")
	db := openLake(dataDir)
	ensureSchema(db)

	// 先寫超過 inline 門檻的量(確保 checkpoint 真的要 flush Parquet),並記錄已 commit 數。
	// 用單條 range() bulk insert,毫秒級,確保 preload 遠早於 kill 完成。
	const preload = 5000
	_, err := db.Exec(
		"INSERT INTO lake.logs SELECT range, now(), 'api', 'msg' FROM range(?)", preload)
	must(err)
	cf, err := os.OpenFile(counterFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	must(err)
	writeCounter(cf, preload)

	// 通知 driver:preload 已 commit,可以開始 kill 計時 → 保證殺在 checkpoint loop 中。
	must(os.WriteFile(os.Getenv("CRASH_READY"), []byte("1"), 0o644))

	// 狂跑 checkpoint,直到被 SIGKILL。
	for {
		_, _ = db.Exec("CHECKPOINT lake")
	}
}

// ---------- driver:spawn child、SIGKILL、verify ----------

func driver() {
	bin := os.Getenv("SCRATCH") + "/crashbin"
	if os.Getenv("SCRATCH") == "" {
		bin = "/tmp/crashbin"
	}
	fmt.Println("• building child binary…")
	build := exec.Command("go", "build", "-o", bin, "./crash")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=go1.24.0")
	build.Stderr = os.Stderr
	must(build.Run())

	// ingest:固定跑 600ms 累積資料後殺。checkpoint:等 ready 檔(preload 已 commit)再殺。
	runCase("2a. crash 於寫入中", bin, "ingest", 600*time.Millisecond)
	runCase("2b. crash 於 CHECKPOINT 中", bin, "checkpoint", 250*time.Millisecond)
}

func runCase(title, bin, role string, killAfter time.Duration) {
	fmt.Printf("\n=== %s ===\n", title)
	dataDir := freshDir(role)
	counter := dataDir + "/committed.txt"
	ready := dataDir + "/ready"

	child := exec.Command(bin)
	child.Env = append(os.Environ(),
		"CRASH_ROLE="+role, "CRASH_DATA="+dataDir, "CRASH_COUNTER="+counter,
		"CRASH_READY="+ready)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	must(child.Start())

	if role == "checkpoint" {
		// 等 preload commit 完成(ready 檔出現),確保 kill 落在 checkpoint loop 中。
		waitForFile(ready, 10*time.Second)
		fmt.Printf("• preload 已 commit,child pid=%d,%v 後 SIGKILL(於 checkpoint 中)\n",
			child.Process.Pid, killAfter)
	} else {
		fmt.Printf("• child pid=%d,%v 後 SIGKILL\n", child.Process.Pid, killAfter)
	}
	time.Sleep(killAfter)
	must(child.Process.Signal(syscall.SIGKILL))
	_ = child.Wait()
	fmt.Println("• child 已被 kill -9")

	// 重開 catalog(驗證能乾淨恢復),讀 count,對照 counter 檔。
	committed := readCounter(counter)
	db := openLake(dataDir)
	defer db.Close()
	var n int64
	if err := db.QueryRow("SELECT count(*) FROM lake.logs").Scan(&n); err != nil {
		fmt.Printf("❌ 重開後查詢失敗(catalog 恢復失敗?): %v\n", err)
		return
	}
	fmt.Printf("• 重開成功。DB count=%d,counter 檔已 commit=%d\n", n, committed)

	ok := true
	if role == "ingest" {
		// 原子性:count 必為 batch 倍數(無半個 transaction)
		if n%batch != 0 {
			fmt.Printf("❌ 原子性違反:count %% %d = %d ≠ 0(有半個 transaction)\n", batch, n%batch)
			ok = false
		}
		// 持久性:已 commit 的資料不能少;至多領先 counter 一個 batch
		if n < committed {
			fmt.Printf("❌ 持久性違反:DB count %d < 已 commit %d(掉資料)\n", n, committed)
			ok = false
		}
		if n > committed+batch {
			fmt.Printf("❌ DB count %d 領先 counter %d 超過一個 batch\n", n, committed)
			ok = false
		}
	} else {
		// checkpoint:重開乾淨 + 無重複無遺失(count 必等於 preload)
		if n != committed {
			fmt.Printf("❌ count %d ≠ 已 commit %d(checkpoint 崩潰造成重複或遺失)\n", n, committed)
			ok = false
		}
		// 額外:重讀一次確認 seq 無重複
		var distinct int64
		db.QueryRow("SELECT count(DISTINCT seq) FROM lake.logs").Scan(&distinct)
		if distinct != n {
			fmt.Printf("❌ seq 有重複:distinct=%d vs count=%d\n", distinct, n)
			ok = false
		}
		pq, _ := filepathGlob(dataDir + "/logs/*.parquet")
		fmt.Printf("• 恢復後 Parquet 檔數=%d(>0 代表 kill 前 checkpoint 已在 flush)\n", pq)
	}
	if ok {
		fmt.Printf("✅ %s — ACID 不變式全數成立\n", title)
	}
}

// ---------- 小工具 ----------

func freshDir(tag string) string {
	base := os.Getenv("SCRATCH")
	if base == "" {
		base = "/tmp"
	}
	dir := base + "/crashdata-" + tag
	must(os.RemoveAll(dir))
	must(os.MkdirAll(dir+"/logs", 0o755))
	return dir
}

func filepathGlob(pattern string) (int, error) {
	m, err := filepath.Glob(pattern)
	return len(m), err
}

func waitForFile(path string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	panic("等 ready 檔逾時: " + path)
}

func readCounter(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func must2(_ any, err error) { must(err) }
