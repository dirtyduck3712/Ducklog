# service 靜默偵測(absence)Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在既有 vmalert 告警 stack 上加一類 rule,當受監控 service 在時間窗內完全沒 log(靜默)時觸發告警。

**Architecture:** 延續現有 alert 功能,不新增 infra、不動 Go/MCP。每個手動列舉的 service 一條**不分組** `service:=X _time:W | stats count() as n | filter n:0` vlogs rule(零匹配回一行 n=0 → 觸發)。新 rule 檔透過 glob 掛進現有 vmalert,沿用現有 alertmanager。

**Tech Stack:** vmalert(vlogs rule)、VictoriaLogs、docker-compose、bash、curl、python(sink)。

## Global Constraints

- **不改 Go/MCP 程式碼**:只新增 `alert-rules/silence.yml`、改一處 compose glob、新增一支 e2e script、補 README。
- **off-by-default**:`silence.yml` 預設 `rules: []`(空 group,vmalert 載入 0 rule、無害),使用者填真 service 才生效。避免預設對不存在的 service 恆 firing。
- **LogsQL 慣例**:group `type: vlogs`;absence 必用**不分組** `stats count() as n | filter n:0`(不可用 `stats by (service)` —— 靜默 service 不會出現在分組結果);對齊現有 `service:=` 慣例。
- **掛載**:vmalert rule flag 從多個明確 `-rule=` 改成單一 glob `-rule=/etc/alert-rules/*.yml`。
- **必驗點(spec)**:vmalert 對「stats_query 回一行 value=0」是否判 firing,只在 stats_query 層實測過(回一行 n=0),**vmalert 端的 firing 判定必須 e2e 端到端確認**。若 e2e 發現 dead service 始終不 firing(且已排除 rule/ingest 錯誤),這推翻設計地基 → 回報 BLOCKED,不要自行繞路。
- **版本 pin 不變**:沿用現有 `.env.example`(VL v1.51.0 / VM v1.147.0 / vmalert v1.147.0 / alertmanager v0.33.1)。
- **命名**:檔名/命令 English;YAML/shell 註解正體中文;commit message 不加 `Co-Authored-By`。

---

### Task 1: silence.yml 範本 + compose glob 掛載

**Files:**
- Create: `alert-rules/silence.yml`
- Modify: `docker-compose.alert.yml:33-34`(兩個 `-rule=` → 一個 glob)

**Interfaces:**
- Produces: 空 group `ducklog-service-silence`(供 e2e 與使用者填 rule);vmalert 改用 glob 掛載 `alert-rules/*.yml`。

- [ ] **Step 1: 建 `alert-rules/silence.yml`**

```yaml
groups:
  - name: ducklog-service-silence
    type: vlogs
    interval: 1m
    rules: []
      # 取消註解、移除上面的 [],並改成你真的要監控靜默的 service。
      # service 名必須「存在過」(曾產過 log),否則不分組 stats 對不存在的 service 恆回 n=0 → 恆 firing。
      # - alert: ServiceSilent
      #   expr: 'service:=checkout _time:10m | stats count() as n | filter n:0'
      #   for: 0s
      #   labels: { severity: critical, service: checkout }
      #   annotations: { summary: 'service checkout 已靜默 ≥10m(_time:10m 內 0 筆 log)' }
```

- [ ] **Step 2: 改 `docker-compose.alert.yml` 的 vmalert rule flag(line 33-34)為 glob**

把:
```yaml
      - -rule=/etc/alert-rules/error-rate.yml
      - -rule=/etc/alert-rules/pattern.yml
```
改成:
```yaml
      - -rule=/etc/alert-rules/*.yml
```

- [ ] **Step 3: 驗證 dryRun 三個 rule 檔都合法載入**

Run:
```bash
docker run --rm -v "$PWD/alert-rules:/etc/alert-rules:ro" \
  victoriametrics/vmalert:v1.147.0 \
  -rule='/etc/alert-rules/*.yml' -rule.defaultRuleType=vlogs -dryRun
```
Expected: 載入 error-rate、pattern、silence 三檔無 parse error,exit 0(silence 的空 group 載入 0 rule 屬正常)。

- [ ] **Step 4: 起 stack 確認 vmalert 載入 silence group**

Run:
```bash
cp -n .env.example .env 2>/dev/null || true
scripts/render-alertmanager-config.sh .env
docker compose --env-file .env -f docker-compose.alert.yml config >/dev/null && echo "config OK"
docker compose --env-file .env -f docker-compose.alert.yml up -d
sleep 8
curl -s http://127.0.0.1:18880/api/v1/rules | grep -q ducklog-service-silence && echo "silence group loaded"
docker compose --env-file .env -f docker-compose.alert.yml down -v
```
Expected: `config OK`、`silence group loaded` 都出現。

- [ ] **Step 5: Commit**

```bash
git add alert-rules/silence.yml docker-compose.alert.yml
git commit -m "feat(alert): service 靜默偵測 rule 範本 + vmalert glob 掛載"
```

---

### Task 2: silence 端到端 e2e(驗 vmalert 對 n=0 firing + 對照組)

**Files:**
- Create: `scripts/test-silence-e2e.sh`
- Modify: `.gitignore`(加 `alert-rules/zz-silence-e2e.yml`)

**Interfaces:**
- Consumes: Task 1 的 glob 掛載、現有 `webhook-sink.py`、`render-alertmanager-config.sh`、`.env`。
- Produces: 一鍵驗證「曾有 log → 靜默 → firing;alive 對照不 firing;sink 收到」。

- [ ] **Step 1: 建 `scripts/test-silence-e2e.sh`**

```bash
#!/usr/bin/env bash
# test-silence-e2e.sh — 驗 service 靜默偵測:
#   dead service(曾產 log 後停)→ ServiceSilent firing;alive(持續產)→ 不 firing;webhook sink 收到 dead。
# 用短窗 _time:1m 驗機制,不等 10m 預設窗。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
COMPOSE=(docker compose --env-file .env -f docker-compose.alert.yml -f docker-compose.e2e.yml)
RULE=alert-rules/zz-silence-e2e.yml
INGEST="http://127.0.0.1:19428/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service"

cleanup() { "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true; rm -f "$RULE" docker-compose.e2e.yml; }
trap cleanup EXIT

[[ -f .env ]] || cp .env.example .env

# e2e sink override(同 test-alert-e2e.sh)
cat > docker-compose.e2e.yml <<'YAML'
services:
  sink:
    image: python:3-alpine
    volumes:
      - ./scripts/webhook-sink.py:/sink.py:ro
      - sink-out:/out
    command: ["python3", "/sink.py"]
  alertmanager:
    depends_on: [sink]
volumes:
  sink-out:
YAML

# 短窗 silence rule(alive + dead,_time:1m);glob 會抓到
cat > "$RULE" <<'YAML'
groups:
  - name: ducklog-silence-e2e
    type: vlogs
    interval: 15s
    rules:
      - alert: ServiceSilent
        expr: 'service:=dead _time:1m | stats count() as n | filter n:0'
        for: 0s
        labels: { severity: critical, service: dead }
        annotations: { summary: 'service dead 靜默' }
      - alert: ServiceSilent
        expr: 'service:=alive _time:1m | stats count() as n | filter n:0'
        for: 0s
        labels: { severity: critical, service: alive }
        annotations: { summary: 'service alive 靜默' }
YAML

ingest() { curl -sf -X POST "$INGEST" -H 'Content-Type: application/stream+json' --data-binary "$1" >/dev/null; }

scripts/render-alertmanager-config.sh .env
"${COMPOSE[@]}" up -d
echo "等 stack 就緒..."; sleep 10

# dead 產一筆後就不再產(模擬掛掉)
ingest "{\"ts\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",\"service\":\"dead\",\"level\":\"info\",\"message\":\"last gasp\"}"

# alive 背景每 10s 產一筆(< 1m 窗,確保窗內恆有 log)
( for i in $(seq 1 18); do
    ingest "{\"ts\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",\"service\":\"alive\",\"level\":\"info\",\"message\":\"heartbeat $i\"}" || true
    sleep 10
  done ) &
ALIVE_PID=$!
kill_alive() { kill "$ALIVE_PID" >/dev/null 2>&1 || true; }

# 等 dead 那筆滑出 1m 窗(~70s)+ vmalert eval(interval 15s),最多 150s
echo "等 dead 滑出 1m 窗 → firing..."
DEAD_FIRING=0
for i in $(seq 1 30); do
  ALERTS=$(curl -s http://127.0.0.1:18880/api/v1/alerts)
  if echo "$ALERTS" | grep -q '"service":"dead"'; then DEAD_FIRING=1; echo "dead firing"; break; fi
  sleep 5
done
if [[ "$DEAD_FIRING" != 1 ]]; then
  echo "FAIL: dead 未 firing。若已確認 rule/ingest 正確,這推翻設計地基(vmalert 不把 value=0 當 firing)。"
  kill_alive; exit 1
fi

# 對照:此刻 alive 背景仍在跑、窗內有 log → alive 不該 firing(在 kill alive 之前查,最穩)
ALERTS=$(curl -s http://127.0.0.1:18880/api/v1/alerts)
if echo "$ALERTS" | python3 -c '
import sys, json
d = json.load(sys.stdin)
alerts = d.get("data", {}).get("alerts", [])
bad = [a for a in alerts
       if a.get("labels", {}).get("service") == "alive"
       and a.get("labels", {}).get("alertname") == "ServiceSilent"
       and a.get("state") == "firing"]
sys.exit(0 if bad else 1)'; then
  echo "FAIL: alive 窗內有 log 卻 firing(對照失敗)"; kill_alive; exit 1
fi
echo "alive 未 firing(對照正確)"
kill_alive

# webhook sink 收到 dead 的 ServiceSilent(group_wait 30s),最多 120s
echo "等 sink 收到 dead..."
for i in $(seq 1 24); do
  "${COMPOSE[@]}" exec -T sink sh -c 'test -s /out/hits.log' 2>/dev/null && \
    "${COMPOSE[@]}" exec -T sink sh -c 'grep -q "\"service\":\"dead\"" /out/hits.log' 2>/dev/null && break
  sleep 5
done
"${COMPOSE[@]}" exec -T sink sh -c 'grep -q "\"service\":\"dead\"" /out/hits.log && grep -q ServiceSilent /out/hits.log' \
  && echo "PASS: silence e2e 綠燈(dead firing + alive 不 firing + sink 收到 dead)" \
  || { echo "FAIL: sink 未收到 dead 的 ServiceSilent"; exit 1; }
```

- [ ] **Step 2: 加 e2e 生成的臨時 rule 檔進 `.gitignore`**

在 `.gitignore` 追加:
```
alert-rules/zz-silence-e2e.yml
```

- [ ] **Step 3: 執行 e2e 到綠燈**

Run:
```bash
chmod +x scripts/test-silence-e2e.sh
scripts/test-silence-e2e.sh
```
Expected: 依序印出 `dead firing` → `alive 未 firing(對照正確)` → `PASS: silence e2e 綠燈`,exit 0。若 timing 不足,依既有慣例延長輪詢上限(不改 rule 的 `_time:`/`interval` 語意)。**若 dead 始終不 firing 且已排除 rule/ingest 錯誤:回報 BLOCKED(推翻設計地基,需改走 recording rule + VM PromQL 路線)。**

- [ ] **Step 4: 確認穩定(連跑 2 次)**

Run: `scripts/test-silence-e2e.sh && scripts/test-silence-e2e.sh`
Expected: 兩次都 `PASS`,trap cleanup 每次無殘留容器/volume,`alert-rules/zz-silence-e2e.yml` 與 `docker-compose.e2e.yml` 測後皆被刪。

- [ ] **Step 5: Commit**

```bash
git add scripts/test-silence-e2e.sh .gitignore
git commit -m "test(alert): service 靜默偵測 e2e(dead firing + alive 對照)"
```

---

### Task 3: README 補「service 靜默偵測」段

**Files:**
- Modify: `README.md`(在既有「告警」段內或其後補一小節)

- [ ] **Step 1: 在 README「告警」段補一小節**

內容需涵蓋(對齊 README 現有語氣):
```markdown
### service 靜默偵測

`alert-rules/silence.yml` 預設空(不啟)。要監控某 service 靜默,取消範例註解、移除 `rules: []` 的 `[]`,每個 service 加一條:

​```yaml
- alert: ServiceSilent
  expr: 'service:=<你的service> _time:10m | stats count() as n | filter n:0'
  for: 0s
  labels: { severity: critical, service: <你的service> }
  annotations: { summary: 'service <你的service> 已靜默 ≥10m' }
​```

- `_time:` 是靜默判定窗(預設 10m,依 service 產 log 頻率各自調;低頻/批次型調大或不納入)。
- ⚠️ service 名必須「存在過」(曾產過 log)。打錯名字或監控從未存在的 service → 不分組 stats 恆回 0 → **恆 firing**。
- 自測機制:`scripts/test-silence-e2e.sh`(短窗驗 dead firing + alive 對照)。
```

- [ ] **Step 2: 檢查連貫性**

Run: `grep -n "靜默" README.md`
Expected: 新小節出現,標題層級與周圍一致(建議 H3,置於既有「告警」H2 段內)。

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs(alert): service 靜默偵測使用說明 + 恆 firing 警示"
```

---

## 完成後

全部 task 完成後在 `feat/service-silence` branch。手動驗收:在 `silence.yml` 填真實 service、跑一次確認正常 service 不誤報。

**Backlog(本 plan 不做)**:volume 異常(log 量暴跌但非 0,同機制換 `filter n:<下限>`)、error-rate 真突增(recording rule + VM 基線)。
