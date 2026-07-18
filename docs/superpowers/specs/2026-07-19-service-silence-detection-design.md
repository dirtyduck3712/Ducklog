# service 靜默偵測(absence)設計

日期：2026-07-19
狀態：已通過 brainstorming,待實作

## 目的

在既有 vmalert 告警 stack 上,加一類 rule:當某個「預期該持續產 log」的 service 在時間窗內完全沒有 log(靜默/掛掉),觸發告警。延續現有 alert 功能(2026-07-19 merged),**不新增任何 infra、不動 Go/MCP code**,只加一個 rule 檔 + 一處 compose glob 改動。

## 背景與決策

- **absence 的技術地基(實測 VL v1.51.0 確立,非查文件)**:
  - `service:=X | stats count() as n`(**不分組**)對零匹配輸入 → **回一行 `n=0`**(非空結果),`filter n:0` 保留它 → stats_query 回非空。故 vmalert 可評估、可觸發。
  - 對照:`stats by (service) count()` **分組**時,沒 log 的 service **根本不出現在結果**。→ 無法用單一分組 rule 偵測所有 service 靜默。
  - 結論:**不需要** recording rule → VM → PromQL `absent_over_time` 那條複雜路線;直接用不分組 `filter n:0` 即可。memory 記的「vmalert 對無資料需額外處理」坑,被「不分組 stats 回 0」化解。
- **受監控 service 清單來源(brainstorming 決定 A:手動列舉)**:每個要監控靜默的 service 各一條 rule。absence 天生需要一份「預期該一直在」的人為名單 —— 靜默的 service 不在當下結果裡,無法自動發現,也無法區分「正常下線」與「異常靜默」。動態基線發現有雞生蛋問題,不採。
- **靜默窗預設(決定:10m)**:`_time:10m` 內 0 筆即報,`for: 0s`(窗本身已是「10m 都沒有」,不需再 for 累積)。窗寫在各 rule 的 `_time:`,天生 per-service 可覆寫。
- **沿用現有 stack**:同一 vmalert(掛新 rule 檔)、同一 alertmanager receiver(webhook + Telegram)。severity `critical`(service 掛了比 error-rate 的 warning 更該吵)。
- **掛載方式(決定:glob)**:vmalert 的多個明確 `-rule=` 改成 `-rule=/etc/alert-rules/*.yml`,未來加 rule 檔不用再動 compose。唯一碰現有 compose 的改動。
- **防呆(決定)**:`silence.yml` 預設 `rules: []`(空 group,vmalert 載入 0 rule、無害)+ 註解範本。因為若預設放 `service:=checkout` 而使用者無該 service → 恆 firing(實測 2 證實不存在的 service 永遠回 n=0)。

## 元件

### 1. `alert-rules/silence.yml`(新增)

```yaml
groups:
  - name: ducklog-service-silence
    type: vlogs
    interval: 1m
    rules: []
      # 取消註解並改成你真的要監控靜默的 service(名字必須存在過,否則恆 firing):
      # - alert: ServiceSilent
      #   expr: 'service:=checkout _time:10m | stats count() as n | filter n:0'
      #   for: 0s
      #   labels: { severity: critical, service: checkout }
      #   annotations: { summary: 'service checkout 已靜默 ≥10m(_time:10m 內 0 筆 log)' }
```

每個受監控 service 複製一條,改 service 名與(可選)`_time:` 窗。

### 2. `docker-compose.alert.yml`(改一處)

vmalert 的兩個明確 `-rule=/etc/alert-rules/error-rate.yml`、`-rule=/etc/alert-rules/pattern.yml` 改成單一 glob:
```
- -rule=/etc/alert-rules/*.yml
```
其餘 vmalert 設定不動。

### 3. e2e(擴充 `scripts/test-alert-e2e.sh` 或新增 `scripts/test-silence-e2e.sh`)

用短窗(`_time:1m`)驗機制,不等 10m:
1. 監控清單含 `alive` + `dead` 兩個 service,silence rule 用 `_time:1m`
2. ingest `dead@T0` 一筆(讓它「存在過」)、`alive` 持續 ingest
3. 等 ~70s(讓 dead 那筆滑出 1m 窗)→ verify: vmalert `/api/v1/alerts` 出現 `ServiceSilent{service=dead}` firing
4. 對照:verify alerts 裡**無** `ServiceSilent{service=alive}`
5. verify: webhook sink 收到 `ServiceSilent{service=dead}`

真正證明「曾有 log → 靜默 → 觸發」,且對照組(alive)排除「無腦全報」。

### 4. `README.md`(補一段)

「service 靜默偵測」小段:如何在 `silence.yml` 加 service、`_time:` 窗語意、清單維護與打錯名字恆 firing 的警示。

## 測試策略

- **地基已實測**:stats_query 對零匹配回一行 `n=0`(探測 2/3)。
- **必驗點**:vmalert 對「回一行 value=0」的 firing 判定 —— 只在 stats_query 層實測過,vmalert 端的 firing 要 e2e 端到端確認(不憑 stats_query 回 0 就斷定)。
- **e2e 成功標準**:短窗 e2e 綠燈(dead firing + alive 不 firing + sink 收到)。

## Accepted v1 caveats

- 需手動維護清單;service 名打錯/從未存在 → 恆 firing(防呆:預設空 group + README 警示)。
- 低頻/批次型 service 需調大 `_time:` 窗或不納入(否則正常間歇誤報)。
- e2e 用短窗(`_time:1m`)驗機制;10m 預設窗不進自動測試(同 error-rate `for:5m` 取捨)。
- **冷啟/重啟瞬時誤報**(e2e 實測浮現):vmalert 剛啟動、VL 剛清空、或某 service 剛部署還沒產第一批 log 時,`_time:W | filter n:0` 因窗內 0 筆而短暫 firing。屬預期(service 確實還沒 log),但 vmalert 重啟後第一個窗會有瞬時全體 `ServiceSilent` 雜訊。緩解:rule 加 `for:`(如 `for: 2m`)要求持續靜默才告警,或靠 Alertmanager `group_wait`/`repeat_interval` 收斂。已記入 README。
- volume 異常(log 量暴跌但非 0)不在本 task 範圍;同機制換 `filter n:<下限>` 可做,屬另一條 backlog。
