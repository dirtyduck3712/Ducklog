# vmalert 告警設計

日期：2026-07-18
狀態：已通過 brainstorming，待實作

## 目的

為 ducklog 加上告警能力：對 VictoriaLogs 裡的 log 做規則評估，觸發時通知到 webhook 與 Telegram。走 VictoriaMetrics 官方標準組合（vmalert + VictoriaMetrics + Alertmanager），純 ops infra，**不碰現有 Go/MCP 程式碼**。整套 off-by-default（獨立 compose profile，預設不啟），與專案既有慣例一致。

## 背景與決策

- **vmalert 對 VictoriaLogs 告警機制**：vmalert 透過 VL 的 `/select/logsql/stats_query` 與 `/select/logsql/stats_query_range` 端點查詢，這兩個端點回傳 Prometheus 相容格式的聚合結果。故 rule 的 LogsQL **必須含 `stats` pipe**（把 log 聚合成數值），再以 `filter` 比門檻。group 層設 `type: vlogs`（或 vmalert 啟動帶 `-rule.defaultRuleType=vlogs`）。
- **交付範圍（brainstorming 決定 A）**：純 ops infra。只跑起告警鏈 + 寫 rule 集 + 接通知出口。**不新增 MCP tool、不動 Go code**，alert 鏈與現有 VL/MCP 解耦。
- **是否加 VictoriaMetrics（決定：加，標準全家桶）**：`-remoteWrite.url` 對 alerting rules 是 optional（只用來 persist `for:` 狀態 `ALERTS`/`ALERTS_FOR_STATE`，重啟能還原），只有 recording rules 才強制需要。本 task 兩條 rule 皆為 alerting rule，理論上可砍掉 VM 降為 4 process；但 brainstorming 選標準全家桶，理由：`for:` 狀態重啟不丟、且為未來 recording rule（如 error-rate 基線）預留。VM 走 OSS 單 binary。
- **部署形態（決定：docker-compose）**：現有 VL+MCP 是 `scripts/` 手動跑 binary；5 process 手動起太痛，改用 docker-compose 一鍵起整套。這是新引入的 docker 依賴，但只作用於 alert profile，不改現有手動流程。
- **通知出口（決定：webhook + Telegram）**：Alertmanager 單一 receiver 同時掛 `webhook_configs` 與 `telegram_configs`，兩條 rule 觸發都同送兩個出口。Telegram 需 Alertmanager v0.24+ 內建 `telegram_configs`。
- **rule 集（決定：2 條）**：error-rate 門檻 + fatal-pattern 硬觸發。fingerprint 出現、service 靜默/volume 異常進 backlog，本 task 不做。
- **Basic Auth 串接**：VL 若開了既有 Basic Auth（見 `2026-07-18-vl-basic-auth-design.md`，off-by-default），vmalert datasource 需帶 `-datasource.basicAuth.username/password`。VM 與 Alertmanager 在 compose 內網，v1 不加 auth（信任 compose 網路），與現有威脅模型一致。

## 元件

### 1. docker-compose（`docker-compose.alert.yml` 或 profile）

5 個 service，獨立 compose 檔 / profile，預設不啟：

| service | image / binary | 角色 | 關鍵設定 |
|---|---|---|---|
| `victorialogs` | VictoriaLogs OSS | 現有，ingest+query，vmalert datasource | port `9428` |
| `victoriametrics` | VictoriaMetrics OSS | **新**，存 vmalert 的 `ALERTS`/`ALERTS_FOR_STATE` | port `8428` |
| `vmalert` | vmalert OSS | **新**，跑 rule | 見下 |
| `alertmanager` | Alertmanager v0.24+ | **新**，通知路由/去重/靜默 | port `9093`，掛 `alertmanager.yml` |
| `ducklog-mcp` | 現有 binary | 現有，可選 profile，不參與 alert 鏈 | stdio |

vmalert 啟動參數：
```
-datasource.url=http://victorialogs:9428
-rule.defaultRuleType=vlogs
-rule=/etc/alert-rules/*.yml
-remoteWrite.url=http://victoriametrics:8428
-remoteRead.url=http://victoriametrics:8428
-notifier.url=http://alertmanager:9093
# VL 開 Basic Auth 時追加：
-datasource.basicAuth.username=${DUCKLOG_VL_USERNAME}
-datasource.basicAuth.password=${DUCKLOG_VL_PASSWORD}
```

### 2. Rule 集

**`alert-rules/error-rate.yml`** — 固定門檻：
```yaml
groups:
  - name: ducklog-error-rate
    type: vlogs
    interval: 1m
    rules:
      - alert: HighErrorRate
        expr: 'level:=error | stats by (service) count() as errs | filter errs:>50'
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: '{{ index .Labels "service" }}: {{ $value }} errors in 1m window'
```

**`alert-rules/pattern.yml`** — fatal-pattern 硬觸發：
```yaml
groups:
  - name: ducklog-fatal-pattern
    type: vlogs
    interval: 1m
    rules:
      - alert: FatalPattern
        expr: '"panic" OR "out of memory" OR "SIGSEGV" | stats count() as hits | filter hits:>0'
        for: 0s
        labels: { severity: critical }
        annotations:
          summary: '{{ $value }} fatal-pattern hits in last 1m'
```

門檻、關鍵字、`for`、`interval` 全在 rule 檔，調整不需改 compose。LogsQL 對齊現有 `level:=error` 慣例（見 `internal/vl/logsql.go`）。

### 3. Secrets 與 Alertmanager 配置

Secrets 走 `.env`（`.gitignore` 已排除），compose 變數插值。repo 附 `.env.example`（空值 + 註解）：
```
DUCKLOG_VL_USERNAME / DUCKLOG_VL_PASSWORD   # VL Basic Auth（若開）
DUCKLOG_ALERT_WEBHOOK_URL                    # 通用 webhook 目的地
DUCKLOG_TELEGRAM_BOT_TOKEN                   # Telegram bot token
DUCKLOG_TELEGRAM_CHAT_ID                     # 目標 chat id
```

**`alertmanager/alertmanager.yml`**：
```yaml
route:
  receiver: ducklog-default
  group_by: [alertname, service]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
receivers:
  - name: ducklog-default
    webhook_configs:
      - url: <DUCKLOG_ALERT_WEBHOOK_URL>
        send_resolved: true
    telegram_configs:
      - bot_token: <DUCKLOG_TELEGRAM_BOT_TOKEN>
        chat_id: <DUCKLOG_TELEGRAM_CHAT_ID>
        parse_mode: HTML
        send_resolved: true
```

## 測試策略

這是 infra/config，非 Go code，測試 = 驗整條鏈能通：

1. **Rule 檔語法驗證**（cheap，可進 CI）：`vmalert -rule=alert-rules/*.yml -dryRun -datasource.url=…` 解析驗證 rule 語法/型別。
2. **端到端 smoke test**（核心）：用現有 `internal/vltest` / ingest helper 灌測試 log，跑完整鏈——
   1. `docker-compose up`（alert profile）→ verify: 5 service 起、vmalert 連 VL 通
   2. ingest 一批超門檻 `level:=error` log
   3. 等 1~2 個 interval → verify: vmalert `/api/v1/alerts` 出現 `HighErrorRate` firing
   4. 起一個 throwaway HTTP sink 收 webhook POST → verify: sink 收到 alert JSON，含 `service` label
   5. ingest 一筆含 `panic` 的 log → verify: `FatalPattern` 幾乎立即 firing → sink 收到
3. **手動驗收清單**（寫進實作交付）：Telegram 真的收到訊息、`send_resolved` 解除通知會來、Basic Auth 開啟時 vmalert 仍能查 VL。

**成功標準**：第 2 層 smoke test 綠燈（webhook sink 收到兩種 alert）+ 第 3 層手動清單勾完。Telegram 出口不進自動測試（需真 bot token/chat + 外部服務），僅手動驗一次。

## Accepted v1 caveats

- **error-rate 是固定門檻，非真「突增」**：真正 rate-of-change 突增要跟歷史基線比，基線需 recording rule 存進 VM 再比——另一層複雜度。v1 用絕對固定門檻（`errs:>N`，per-service 可各自調），語意是「error 量超過 X 就報」。要基線比較留給未來 recording rule（此時 VM 正好派上用場）。
- **Alertmanager 配置檔的 `${ENV}` 插值**有版本/方式差異（open item）：可能需 compose 端先渲染 `alertmanager.yml`，或改用 `bot_token_file` / `url_file` 讀掛載 secret 檔。實作時用能跑通的那條。
- **Telegram 出口不進自動測試**，僅手動驗收。
- **compose 內網不加 auth**（VM、Alertmanager、vmalert 互連信任 compose 網路），與現有 v1 威脅模型一致。
- **新增 docker 依賴**：alert profile 需 docker-compose；現有 VL+MCP 手動 binary 流程不受影響。
