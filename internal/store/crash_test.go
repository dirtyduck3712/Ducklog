package store_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"docklog/internal/store"
)

// TestCrashDuringIngestKeepsCommittedData 兌現 spec「測試要求 → Crash 恢復」:
// build 一個真正的 writer binary、寫入途中 SIGKILL(kill -9)、再用 Store 重開驗證
// DuckLake/DuckDB 的 ACID:count 為 batch 倍數(無 torn transaction)且不少於已 commit 數(無遺失)。
//
// 必須 build 真 binary 再殺子行程;不能 go run 後殺,否則 kill 只打到 go run wrapper 而非 child。
func TestCrashDuringIngestKeepsCommittedData(t *testing.T) {
	if testing.Short() {
		t.Skip("crash 測試較慢,-short 略過")
	}
	dir := t.TempDir()
	helper := buildHelper(t, dir)
	dataDir := filepath.Join(dir, "data")
	counter := filepath.Join(dir, "committed.txt")

	cmd := exec.Command(helper, dataDir, counter)
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=go1.24.0")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("啟動 helper 失敗: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL 失敗: %v", err)
	}
	_ = cmd.Wait()

	// 重開驗證:count 為 batch 倍數(無半個 transaction)且 >= 已 commit 數(無遺失)。
	committed := readInt(t, counter)
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("crash 後重開失敗: %v", err)
	}
	defer st.Close()
	n, err := st.Count(context.Background())
	if err != nil {
		t.Fatalf("crash 後 Count 失敗: %v", err)
	}
	t.Logf("crash 後 DB count=%d,已 commit(counter 檔)=%d", n, committed)
	// 防止 vacuous pass:若 helper 一筆都沒 commit(store.Open panic / schema 不符 /
	// 開機即崩),counter 檔缺席 → committed=0、重開空 dir → count=0,兩個不變式都成立
	// 而綠燈通過。要求至少一個完整 batch 已 commit,證明 crash 真的落在 ingest 途中。
	if committed < 50 {
		t.Fatalf("helper 只 commit 了 %d 筆(<一個 batch)—— crash 未發生在 ingest 途中,或 store 根本沒動作;測試無意義", committed)
	}
	if n%50 != 0 {
		t.Fatalf("原子性違反:count %d 非 50 倍數 → 有半個 transaction", n)
	}
	if n < committed {
		t.Fatalf("持久性違反:count %d < 已 commit %d → 掉資料", n, committed)
	}
}

// buildHelper 把 testdata/crashhelper 編成暫存 binary,回傳其路徑。
// 測試的工作目錄即 package 目錄,故相對路徑 ./testdata/crashhelper 可直接編譯。
func buildHelper(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "crashhelper")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/crashhelper")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=go1.24.0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build crashhelper 失敗: %v\n%s", err, out)
	}
	return bin
}

func readInt(t *testing.T, path string) int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0 // helper 尚未寫出任何 commit
	}
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
