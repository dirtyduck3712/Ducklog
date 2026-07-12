# Phase 0 — 驗證結果與規格修正

**結論:PASS。** DuckLake 架構在目前可用工具鏈上可行,v4 不需退回 v3。
但規格有數個假設與實際不符,正式實作前必須套用以下修正。

**環境**:Go 1.22.2(build 需 `GOTOOLCHAIN=go1.24.0`)、gcc 13.3、x86_64、CGO。
**依賴**:`github.com/marcboeker/go-duckdb/v2 v2.4.3`(bundle **DuckDB v1.4.1**)。

---

## 驗證矩陣

| 項目 | 結果 | 關鍵數字 |
|---|---|---|
| **Gate** go-duckdb 載入 DuckLake | ✅ | INSTALL/LOAD/ATTACH/CREATE/INSERT/SELECT/CHECKPOINT 全通 |
| **1** inline 寫入吞吐 | ✅ | 穩定 1000/s;盡全力 ~17,900/s(sql loop)、bulk range ~1M/s → 需求的 18–1000× |
| **2** crash 安全性 | ✅ | 寫入中 kill -9:count 精確吻合、為 batch 倍數(無 torn txn);checkpoint 中 kill -9:重開乾淨、無重複無遺失 |
| **3** checkpoint 成本 | ✅ | 增量 CHECKPOINT ~363ms、期間 ingest 阻塞 ~80ms;5M 首次落地 ~3s |
| **4** 查詢效能(5M 筆) | ✅ | 範圍+service 22ms、GROUP BY 47ms、LIKE 全掃 6ms、明細+LIMIT 10ms |

---

## 必須套用的規格修正(已實測驗證)

### 1. 版本假設錯誤 —— 實際是 DuckDB 1.4.1,不是 1.5.2+
go-duckdb 最新 `v2.4.3` bundle 的是 DuckDB **1.4.1**;規格宣稱的「DuckLake v1.0(2026-04)、production-ready、保證向後相容」**無法驗證**,實際跑在較早版本上。功能可用,但要拿掉「v1.0 穩定」的假設,並在升級 go-duckdb 時重測本清單。

### 2. Catalog backend 必須用 DuckDB,不能用 SQLite
規格失敗模式 #3 力薦 SQLite,但 **SQLite backend 的 `CHECKPOINT` 直接壞掉**:
extension 內部清理查詢 `WHERE schedule_start < NOW() - INTERVAL '2 days'` 拿 SQLite 的 TEXT
timestamp 去比 `TIMESTAMP WITH TIME ZONE`,報 `Cannot compare VARCHAR and TIMESTAMP`。
改不了 extension 內部 SQL。DuckDB backend 正常,且規格選型表本就列它為合法選項。

- ATTACH 改成:`ATTACH 'ducklake:data/logs.ducklake' AS lake (DATA_PATH 'data/logs/')`(無 `sqlite:` 前綴)
- **代價**:失去 SQLite 的成熟度背書 → crash 安全改由 item 2 實測背書(已通過)。

### 3. inline row limit 的設定語法不同
規格的 `SET ducklake_default_data_inlining_row_limit = 1000` 在 1.4.1 **不存在**。正解:
```sql
CALL lake.set_option('data_inlining_row_limit', '1000');   -- per-catalog,值是字串
```

### 4. `CHECKPOINT lake` 確實會 flush inline → Parquet(規格論述成立)
10000 筆 inline → Parquet=0;`CHECKPOINT lake` 後 → Parquet=1。規格核心論述正確。
`ducklake_flush_inlined_data` 這個函式在 1.4.1 **完全不能用**(所有呼叫形式都回
`Scanning a DuckLake table after the transaction has ended - not yet supported`),
但**不需要它** —— CHECKPOINT 已完成 flush。

### 5. Parquet 落在巢狀路徑,監控/計數要注意
資料檔在 `DATA_PATH/<schema>/<table>/*.parquet`(如 `data/logs/main/logs/ducklake-*.parquet`),
不是頂層。任何「數 Parquet 檔」的健康檢查要遞迴。

### 6. Catalog 大小監控要算兩個檔 + 定期修剪
inline 資料實際在 **`logs.ducklake.wal`**(catalog 的 DuckDB WAL),主檔 `logs.ducklake` 很小。
- 健康檢查的 `catalog_size_mb` 要算 `logs.ducklake` + `logs.ducklake.wal`。
- catalog WAL 不被 `CHECKPOINT lake` 截斷,靠 DuckDB 預設 16MB autocheckpoint / 重連自理。
- 主 catalog 隨 snapshot 累積變大(20000 筆→4.6MB metadata),定期維護必須包含:
  ```sql
  CALL ducklake_expire_snapshots('lake', older_than => now() - INTERVAL 7 DAY);
  CALL ducklake_cleanup_old_files('lake', cleanup_all => true);
  ```
  (注意:`expire_snapshots`/`cleanup_old_files` 要用 `ducklake_` 前綴 + catalog 名當第一參數;
  `merge_adjacent_files` 則可用 `lake.merge_adjacent_files()` 的 catalog 方法形式。)

### 7. Build 需要 Go 1.24 toolchain
go-duckdb v2.4.3 的 `go.mod` 要求 go 1.24。本機 go 1.22.2 → 用 `GOTOOLCHAIN=go1.24.0`
(auto 抓特定 patch 版會失敗,釘明確版號可)。

---

## 對正式實作的影響

- **維護 goroutine** 除了 `CHECKPOINT lake`,還要週期性 `expire_snapshots` + `cleanup_old_files`
  來 bound catalog 成長(規格的 Checkpoint 章節只寫了 CHECKPOINT,不夠)。
- **失敗模式 #5(inline 累積過多)** 的監控指標要用「`.ducklake` + `.ducklake.wal` 合計大小」。
- **LIKE 全掃描**在 1 天量只需個位數 ms;規格的「2–5s」是 10GB 悲觀值。策略不變,但可放寬對它的恐懼。
- **spike 程式碼在 `phase0/`,不進正式 codebase。**
