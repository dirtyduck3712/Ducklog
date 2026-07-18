# vmalert 告警 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用 vmalert + VictoriaMetrics + Alertmanager 對 VictoriaLogs 做告警,docker-compose 一鍵起,觸發時通知 webhook + Telegram。

**Architecture:** 純 ops infra,不動任何 Go/MCP 程式碼。獨立 `docker-compose.alert.yml`(5 service:自帶一套 VL + VM + vmalert + alertmanager + 可選 MCP),預設不啟。vmalert 每 interval 打 VL 的 `stats_query` 端點評估 LogsQL rule,觸發後送 Alertmanager 路由到 webhook 與 Telegram;alert 狀態 remoteWrite 到 VM。

**Tech Stack:** docker-compose、VictoriaLogs/VictoriaMetrics/vmalert(OSS 單 binary image)、Prometheus Alertmanager、envsubst、curl、bash。

## Global Constraints

- **不改 Go/MCP 程式碼**:本 plan 只新增 compose / config / script / docs,現有 `client/`、`internal/`、`cmd/`、`zapsink/` 一行不動。
- **off-by-default**:整套走獨立 `docker-compose.alert.yml`,預設不啟;現有 `scripts/run-dev.sh` 手動 binary 流程不受影響。
- **Image tags(pin,`.env` 變數注入)**:`VL_IMAGE_TAG=victoriametrics/victoria-logs:v1.51.0`(已知存在)、`VM_IMAGE_TAG=victoriametrics/victoria-metrics`、`VMALERT_IMAGE_TAG=victoriametrics/vmalert`、`ALERTMANAGER_IMAGE_TAG=prom/alertmanager`。VM/vmalert/alertmanager 的具體版本號在 Task 首次 `docker pull` 時確認當前 stable 並寫回 `.env.example`(vmalert 與 VM 通常同版本號發布;Alertmanager 需 v0.24+ 才有 `telegram_configs`)。
- **Secrets 只留 `.env`**(不進 git):`DUCKLOG_VL_USERNAME/PASSWORD`(VL Basic Auth,若開)、`DUCKLOG_ALERT_WEBHOOK_URL`、`DUCKLOG_TELEGRAM_BOT_TOKEN`、`DUCKLOG_TELEGRAM_CHAT_ID`。Alertmanager config 由 `envsubst` 從 template 渲染,產物 gitignore。
- **LogsQL rule 慣例**:group 層 `type: vlogs`;expr 必含 `stats` pipe;對齊現有 `level:=error`(見 `internal/vl/logsql.go`)。
- **網路/port**:compose 服務間走內部網路;對外 host port 用非標準值避開手動 VL 的 `9428` — VL=`19428`、vmalert=`18880`、alertmanager=`19093`、VM=`18428`。
- **命名**:檔名/命令 English;註解正體中文;commit message 不加 `Co-Authored-By`。

---

### Task 1: Rule 檔 + dryRun 語法驗證

**Files:**
- Create: `alert-rules/error-rate.yml`
- Create: `alert-rules/pattern.yml`

**Interfaces:**
- Produces: 兩個 vlogs rule 檔,供 Task 3 的 vmalert 掛載(容器內路徑 `/etc/alert-rules/`);group name `ducklog-error-rate`(alert `HighErrorRate`)、`ducklog-fatal-pattern`(alert `FatalPattern`)。

- [ ] **Step 1: 建 `alert-rules/error-rate.yml`**

```yaml
groups:
  - name: ducklog-error-rate
    type: vlogs
    interval: 1m
    rules:
      # 固定門檻(非真「突增」,見 spec caveat):每 service 1m 視窗 error 數超過門檻即報
      - alert: HighErrorRate
        expr: 'level:=error | stats by (service) count() as errs | filter errs:>50'
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: '{{ index .Labels "service" }}: {{ $value }} errors in 1m window'
```

- [ ] **Step 2: 建 `alert-rules/pattern.yml`**

```yaml
groups:
  - name: ducklog-fatal-pattern
    type: vlogs
    interval: 1m
    rules:
      # fatal 關鍵字一出現即報(門檻 0、for 0)
      - alert: FatalPattern
        expr: '"panic" OR "out of memory" OR "SIGSEGV" | stats count() as hits | filter hits:>0'
        for: 0s
        labels:
          severity: critical
        annotations:
          summary: '{{ $value }} fatal-pattern hits in last 1m'
```

- [ ] **Step 3: 用 vmalert dryRun 驗證語法(先確認 image tag 可 pull)**

Run:
```bash
docker pull victoriametrics/vmalert:latest   # 確認當前 stable tag;把實際 tag 記下供 .env.example
docker run --rm -v "$PWD/alert-rules:/etc/alert-rules:ro" \
  victoriametrics/vmalert:latest \
  -rule=/etc/alert-rules/error-rate.yml \
  -rule=/etc/alert-rules/pattern.yml \
  -rule.defaultRuleType=vlogs \
  -dryRun
```
Expected: 印出 rule 載入成功、無 parse error,程序以 exit 0 結束(`-dryRun` 只驗語法/型別,不連 datasource)。

- [ ] **Step 4: 蓄意破壞驗證 dryRun 真的會擋**

Run: 暫時破壞 `pattern.yml` 的 YAML 縮排(把 `- alert: FatalPattern` 那行往左移兩格使其脫離 `rules:` 清單),重跑 Step 3 指令。
Expected: dryRun 以非 0 exit 失敗並報 parse error。確認後把 `pattern.yml` 改回 Step 2 的正確內容。

- [ ] **Step 5: Commit**

```bash
git add alert-rules/error-rate.yml alert-rules/pattern.yml
git commit -m "feat(alert): vmalert vlogs rule 集(error-rate 門檻 + fatal-pattern)"
```

---

### Task 2: Alertmanager config template + envsubst 渲染 + amtool 驗證

**Files:**
- Create: `alertmanager/alertmanager.yml.tmpl`
- Create: `scripts/render-alertmanager-config.sh`

**Interfaces:**
- Consumes: env `DUCKLOG_ALERT_WEBHOOK_URL`、`DUCKLOG_TELEGRAM_BOT_TOKEN`、`DUCKLOG_TELEGRAM_CHAT_ID`。
- Produces: 渲染腳本輸出 `alertmanager/alertmanager.yml`(gitignore);Task 3 compose 掛載此渲染產物到容器 `/etc/alertmanager/alertmanager.yml`。

- [ ] **Step 1: 建 `alertmanager/alertmanager.yml.tmpl`**

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
      - url: ${DUCKLOG_ALERT_WEBHOOK_URL}
        send_resolved: true
    telegram_configs:
      - bot_token: ${DUCKLOG_TELEGRAM_BOT_TOKEN}
        chat_id: ${DUCKLOG_TELEGRAM_CHAT_ID}
        parse_mode: HTML
        send_resolved: true
```

- [ ] **Step 2: 建 `scripts/render-alertmanager-config.sh`**

```bash
#!/usr/bin/env bash
# render-alertmanager-config.sh — 從 .env + template 渲染 alertmanager.yml。
# Alertmanager 官方 config 不支援 env 插值,故在 host 端用 envsubst 渲染。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${1:-$ROOT/.env}"
TMPL="$ROOT/alertmanager/alertmanager.yml.tmpl"
OUT="$ROOT/alertmanager/alertmanager.yml"

[[ -f "$ENV_FILE" ]] || { echo "找不到 env 檔: $ENV_FILE" >&2; exit 1; }
set -a; source "$ENV_FILE"; set +a
: "${DUCKLOG_ALERT_WEBHOOK_URL:?未設 DUCKLOG_ALERT_WEBHOOK_URL}"
: "${DUCKLOG_TELEGRAM_BOT_TOKEN:?未設 DUCKLOG_TELEGRAM_BOT_TOKEN}"
: "${DUCKLOG_TELEGRAM_CHAT_ID:?未設 DUCKLOG_TELEGRAM_CHAT_ID}"

envsubst '${DUCKLOG_ALERT_WEBHOOK_URL} ${DUCKLOG_TELEGRAM_BOT_TOKEN} ${DUCKLOG_TELEGRAM_CHAT_ID}' \
  < "$TMPL" > "$OUT"
echo "已渲染: $OUT" >&2
```

- [ ] **Step 3: 準備臨時 env 並渲染**

Run:
```bash
chmod +x scripts/render-alertmanager-config.sh
cat > /tmp/alert.env <<'EOF'
DUCKLOG_ALERT_WEBHOOK_URL=http://sink:8080/hook
DUCKLOG_TELEGRAM_BOT_TOKEN=dummy:token
DUCKLOG_TELEGRAM_CHAT_ID=1
EOF
scripts/render-alertmanager-config.sh /tmp/alert.env
```
Expected: 生成 `alertmanager/alertmanager.yml`,內容中三個 `${...}` 已被替換為上面的值。

- [ ] **Step 4: 用 amtool 驗證渲染後 config 合法**

Run:
```bash
docker pull prom/alertmanager:latest   # 確認 stable tag,記下供 .env.example(需 v0.24+)
docker run --rm -v "$PWD/alertmanager/alertmanager.yml:/tmp/am.yml:ro" \
  prom/alertmanager:latest amtool check-config /tmp/am.yml
```
Expected: `amtool` 回報 config 合法(1 receiver、1 route);印出 "Found ... receiver(s)"。

- [ ] **Step 5: Commit(只提 template 與 script,不提渲染產物)**

```bash
git add alertmanager/alertmanager.yml.tmpl scripts/render-alertmanager-config.sh
git commit -m "feat(alert): alertmanager config template + envsubst 渲染腳本"
```

---

### Task 3: docker-compose + .env.example + gitignore,起整套並驗就緒

**Files:**
- Create: `docker-compose.alert.yml`
- Create: `.env.example`
- Modify: `.gitignore`

**Interfaces:**
- Consumes: Task 1 的 `alert-rules/`、Task 2 的 `alertmanager/alertmanager.yml`(渲染產物)。
- Produces: 可 `docker compose -f docker-compose.alert.yml up` 起的 5 service stack;對外 host port VL=19428、vmalert=18880、alertmanager=19093、VM=18428。

- [ ] **Step 1: 更新 `.gitignore`(追加)**

在 `.gitignore` 末尾追加:
```
.env
alertmanager/alertmanager.yml
```

- [ ] **Step 2: 建 `.env.example`**

```bash
# ── image tags(pin;首次 docker pull 後把 latest 換成實際 stable 版本號) ──
VL_IMAGE_TAG=victoriametrics/victoria-logs:v1.51.0
VM_IMAGE_TAG=victoriametrics/victoria-metrics:latest
VMALERT_IMAGE_TAG=victoriametrics/vmalert:latest
ALERTMANAGER_IMAGE_TAG=prom/alertmanager:latest

# ── VL Basic Auth(若 VL 開了 -httpAuth;沒開就留空) ──
DUCKLOG_VL_USERNAME=
DUCKLOG_VL_PASSWORD=

# ── 通知出口 ──
DUCKLOG_ALERT_WEBHOOK_URL=http://sink:8080/hook
DUCKLOG_TELEGRAM_BOT_TOKEN=dummy:token
DUCKLOG_TELEGRAM_CHAT_ID=1
```

- [ ] **Step 3: 建 `docker-compose.alert.yml`**

```yaml
# docker-compose.alert.yml — ducklog 告警 stack(off-by-default,獨立於手動 binary 流程)。
# 啟動: docker compose --env-file .env -f docker-compose.alert.yml up -d
name: ducklog-alert

services:
  victorialogs:
    image: ${VL_IMAGE_TAG}
    command:
      - -storageDataPath=/vl-data
      - -retentionPeriod=30d
      - -httpListenAddr=:9428
    volumes:
      - vl-data:/vl-data
    ports:
      - "127.0.0.1:19428:9428"

  victoriametrics:
    image: ${VM_IMAGE_TAG}
    command:
      - -storageDataPath=/vm-data
      - -httpListenAddr=:8428
    volumes:
      - vm-data:/vm-data
    ports:
      - "127.0.0.1:18428:8428"

  vmalert:
    image: ${VMALERT_IMAGE_TAG}
    depends_on: [victorialogs, victoriametrics, alertmanager]
    command:
      - -datasource.url=http://victorialogs:9428
      - -rule.defaultRuleType=vlogs
      - -rule=/etc/alert-rules/error-rate.yml
      - -rule=/etc/alert-rules/pattern.yml
      - -remoteWrite.url=http://victoriametrics:8428
      - -remoteRead.url=http://victoriametrics:8428
      - -notifier.url=http://alertmanager:9093
      - -httpListenAddr=:8880
      # VL 開 Basic Auth 時,取消下面兩行註解(值由 .env 帶入):
      # - -datasource.basicAuth.username=${DUCKLOG_VL_USERNAME}
      # - -datasource.basicAuth.password=${DUCKLOG_VL_PASSWORD}
    volumes:
      - ./alert-rules:/etc/alert-rules:ro
    ports:
      - "127.0.0.1:18880:8880"

  alertmanager:
    image: ${ALERTMANAGER_IMAGE_TAG}
    command:
      - --config.file=/etc/alertmanager/alertmanager.yml
    volumes:
      - ./alertmanager/alertmanager.yml:/etc/alertmanager/alertmanager.yml:ro
    ports:
      - "127.0.0.1:19093:9093"

  # 可選:MCP server(profile=mcp 才起);不參與 alert 鏈
  ducklog-mcp:
    profiles: [mcp]
    build:
      context: .
      dockerfile_inline: |
        FROM golang:1.24 AS build
        WORKDIR /src
        COPY . .
        RUN go build -o /ducklog-mcp ./cmd/ducklog-mcp
        FROM debian:stable-slim
        COPY --from=build /ducklog-mcp /ducklog-mcp
        ENTRYPOINT ["/ducklog-mcp"]
    environment:
      - VL_URL=http://victorialogs:9428

volumes:
  vl-data:
  vm-data:
```

- [ ] **Step 4: 準備 `.env` 並驗證 compose 語法/插值**

Run:
```bash
cp .env.example .env
# 把 VM_IMAGE_TAG / VMALERT_IMAGE_TAG / ALERTMANAGER_IMAGE_TAG 的 :latest
# 換成 Task 1/2 記下的實際 stable 版本號,並回填 .env.example
scripts/render-alertmanager-config.sh .env
docker compose --env-file .env -f docker-compose.alert.yml config >/dev/null && echo OK
```
Expected: 印出 `OK`(compose 檔語法正確、變數插值成功);無 undefined variable 警告。

- [ ] **Step 5: 起 stack 並驗證各 service 就緒**

Run:
```bash
docker compose --env-file .env -f docker-compose.alert.yml up -d
sleep 8
docker compose --env-file .env -f docker-compose.alert.yml ps
curl -sf http://127.0.0.1:19428/health && echo " VL-ok"
curl -sf http://127.0.0.1:18428/health && echo " VM-ok"
curl -sf http://127.0.0.1:19093/-/ready && echo " AM-ok"
# vmalert 就緒 = rule group 已載入(順帶證明它連上 VL datasource):
curl -s http://127.0.0.1:18880/api/v1/rules | grep -q ducklog-fatal-pattern && echo " vmalert-rules-loaded"
```
Expected: 三個 health 檢查(VL/VM/AM)都 ok、`vmalert-rules-loaded` 出現。若 vmalert 連不上 VL,`docker compose logs vmalert` 排查。

- [ ] **Step 6: 收掉 stack**

Run:
```bash
docker compose --env-file .env -f docker-compose.alert.yml down -v
```
Expected: 全部容器與 volume 移除。

- [ ] **Step 7: Commit**

```bash
git add docker-compose.alert.yml .env.example .gitignore
git commit -m "feat(alert): docker-compose alert stack(VL+VM+vmalert+alertmanager)"
```

---

### Task 4: 端到端 smoke test(webhook sink + e2e script)

**Files:**
- Create: `scripts/webhook-sink.py`
- Create: `scripts/test-alert-e2e.sh`

**Interfaces:**
- Consumes: Task 3 的 `docker-compose.alert.yml`、Task 1 的 pattern rule。
- Produces: 一鍵 e2e 綠燈腳本;webhook sink container 收到的 alert 寫入共享 volume 供斷言。

- [ ] **Step 1: 建 `scripts/webhook-sink.py`(極簡 HTTP sink,把 POST body 追加寫檔)**

```python
#!/usr/bin/env python3
# webhook-sink.py — 測試用:收 Alertmanager webhook POST,把 body 追加寫到 /out/hits.log。
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        with open("/out/hits.log", "ab") as f:
            f.write(body + b"\n")
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass

HTTPServer(("0.0.0.0", 8080), H).serve_forever()
```

- [ ] **Step 2: 建 `scripts/test-alert-e2e.sh`**

```bash
#!/usr/bin/env bash
# test-alert-e2e.sh — 端到端:起 stack + sink → ingest panic log → 驗 FatalPattern firing → 驗 sink 收到。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
COMPOSE=(docker compose --env-file .env -f docker-compose.alert.yml -f docker-compose.e2e.yml)

cleanup() { "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

# e2e override:加 sink service,並把 webhook 指向它(用 e2e 專用 .env 已設 DUCKLOG_ALERT_WEBHOOK_URL=http://sink:8080/hook)
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

scripts/render-alertmanager-config.sh .env
"${COMPOSE[@]}" up -d
echo "等 stack 就緒..."; sleep 10

# ingest 一筆含 panic 的 log(對齊現有 jsonline ingest 慣例)
TS="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
curl -sf -X POST \
  "http://127.0.0.1:19428/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service" \
  -H 'Content-Type: application/stream+json' \
  --data-binary "{\"ts\":\"$TS\",\"service\":\"checkout\",\"level\":\"error\",\"message\":\"panic: runtime error nil deref\"}"
echo "已 ingest panic log"

# 等 vmalert 評估(interval 1m,for 0s);輪詢最多 90s 等 firing
for i in $(seq 1 18); do
  if curl -s http://127.0.0.1:18880/api/v1/alerts | grep -q '"FatalPattern"'; then
    echo "vmalert: FatalPattern firing"; break
  fi
  sleep 5
done
curl -s http://127.0.0.1:18880/api/v1/alerts | grep -q '"FatalPattern"' || { echo "FAIL: FatalPattern 未 firing"; exit 1; }

# 等 alertmanager 送到 sink(group_wait 30s);輪詢最多 90s
for i in $(seq 1 18); do
  if "${COMPOSE[@]}" exec -T sink sh -c 'test -s /out/hits.log' 2>/dev/null; then
    echo "sink: 收到 webhook"; break
  fi
  sleep 5
done
"${COMPOSE[@]}" exec -T sink sh -c 'grep -q FatalPattern /out/hits.log' \
  && echo "PASS: e2e 綠燈(sink 收到 FatalPattern alert)" \
  || { echo "FAIL: sink 未收到 FatalPattern"; exit 1; }
```

- [ ] **Step 3: 準備 e2e 用 `.env` 並執行**

Run:
```bash
chmod +x scripts/test-alert-e2e.sh scripts/webhook-sink.py
# .env 需已存在(Task 3);e2e 用 DUCKLOG_ALERT_WEBHOOK_URL=http://sink:8080/hook(即 .env.example 預設)
scripts/test-alert-e2e.sh
```
Expected: 依序印出 `已 ingest panic log` → `vmalert: FatalPattern firing` → `sink: 收到 webhook` → `PASS: e2e 綠燈`,腳本 exit 0。

- [ ] **Step 4: 把 e2e override 檔納入 gitignore(它由腳本生成)**

在 `.gitignore` 追加:
```
docker-compose.e2e.yml
```

- [ ] **Step 5: Commit**

```bash
git add scripts/webhook-sink.py scripts/test-alert-e2e.sh .gitignore
git commit -m "test(alert): 端到端 smoke(ingest panic → FatalPattern → webhook sink)"
```

---

### Task 5: 文件 — 啟動說明 + Telegram 手動驗收清單

**Files:**
- Modify: `README.md`(新增「告警(alert)」段)

**Interfaces:**
- Consumes: 前四個 task 的所有交付。

- [ ] **Step 1: 在 `README.md` 適當位置新增「告警(alert)」段**

內容需涵蓋(對齊 README 現有語氣與格式):
```markdown
## 告警(alert,選用)

告警走獨立 stack,預設不啟,與手動 binary 流程解耦。

1. `cp .env.example .env`,填入 `DUCKLOG_ALERT_WEBHOOK_URL`、`DUCKLOG_TELEGRAM_BOT_TOKEN`、`DUCKLOG_TELEGRAM_CHAT_ID`;VL 若開 Basic Auth 另填 `DUCKLOG_VL_USERNAME/PASSWORD` 並取消 `docker-compose.alert.yml` 內 vmalert 的 basicAuth 兩行註解。
2. 渲染 Alertmanager config:`scripts/render-alertmanager-config.sh .env`
3. 起 stack:`docker compose --env-file .env -f docker-compose.alert.yml up -d`
4. rule 在 `alert-rules/`(門檻/關鍵字/interval 改這裡即可,不需重建 image)。

**Rule 集**:`HighErrorRate`(每 service 1m error 數 > 50,固定門檻非突增)、`FatalPattern`(panic/OOM/SIGSEGV 出現即報)。

**端到端自測**:`scripts/test-alert-e2e.sh`(驗 ingest → firing → webhook)。

**Telegram 手動驗收清單**(不進自動測試):
- [ ] Telegram 真的收到告警訊息
- [ ] 告警解除時 `send_resolved` 通知會來
- [ ] VL 開 Basic Auth 時 vmalert 仍能查 VL(`/api/v1/rules` 有 group)
```

- [ ] **Step 2: 檢查 README 連貫性**

Run: `grep -n "告警" README.md`
Expected: 新段落出現,標題層級與周圍一致。

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs(alert): 告警 stack 啟動說明 + Telegram 手動驗收清單"
```

---

## 完成後

全部 task 完成後,整份工作在 `main` 上(或依 finishing-a-development-branch 決定合併方式)。手動驗收:填真 Telegram bot token/chat id、跑一次完整 stack、勾完 README 的 Telegram 清單。

**Backlog(本 plan 不做,記於 spec)**:fingerprint 出現告警、service 靜默/volume 異常(absence)、error-rate 真突增(需 recording rule 存基線到 VM)。
