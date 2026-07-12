// crashhelper 供 crash_test.go 編譯成獨立 binary 用:持續以 50 筆/tx 寫入,
// 每次 commit 成功後才 fsync 累計數到 counter 檔 → 保證 DB count 恆 >= counter 值。
// 放在 testdata/ 下不被一般 go build ./... 納入;測試以顯式路徑編譯。
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"docklog/internal/model"
	"docklog/internal/store"
)

func main() {
	dataDir, counter := os.Args[1], os.Args[2]
	st, err := store.Open(dataDir)
	if err != nil {
		panic(err)
	}
	cf, err := os.OpenFile(counter, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		panic(err)
	}
	var committed int64
	now := time.Now().UTC()
	for {
		batch := make([]model.LogEntry, 50)
		for i := range batch {
			batch[i] = model.LogEntry{
				TS: now, IngestedAt: now, Service: "api",
				Level: model.Info, Message: "x", Attrs: "{}",
			}
		}
		if err := st.Insert(context.Background(), batch); err != nil {
			panic(err)
		}
		committed += 50
		// commit 成功「之後」才寫 counter → DB count 恆 >= 檔案值。
		cf.Seek(0, 0)
		cf.WriteString(strconv.FormatInt(committed, 10) + "\n")
		cf.Sync()
		fmt.Fprintln(os.Stderr, committed)
	}
}
