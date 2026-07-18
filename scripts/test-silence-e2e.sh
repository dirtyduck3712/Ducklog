#!/usr/bin/env bash
# test-silence-e2e.sh — 驗 service 靜默偵測:
#   dead service(曾產 log 後停)→ ServiceSilent firing(value=0);alive(持續產)→ 不 firing;webhook sink 收到 dead。
# 用短窗 _time:1m 驗機制,不等 10m 預設窗。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
COMPOSE=(docker compose --env-file .env -f docker-compose.alert.yml -f docker-compose.e2e.yml)
RULE=alert-rules/zz-silence-e2e.yml
INGEST="http://127.0.0.1:19428/insert/jsonline?_time_field=ts&_msg_field=message&_stream_fields=service"
VLQ="http://127.0.0.1:19428/select/logsql/query"
AM="http://127.0.0.1:18880/api/v1/alerts"

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

# --max-time 防止單次 curl 卡死拖垮 alive 背景 loop(否則窗內斷 log → alive 假 firing)
ingest() { curl -sf --max-time 8 -X POST "$INGEST" -H 'Content-Type: application/stream+json' --data-binary "$1" >/dev/null; }
vl_count() { curl -s --max-time 5 "$VLQ" --data-urlencode "query=$1 | stats count() as n" | grep -oE '[0-9]+' | head -1; }
# 印出正在 firing 的 service(逗號分隔);curl/JSON 失敗印 ERR(視為未就緒 → 續等,不誤判為乾淨)
firing() { curl -s --max-time 5 "$AM" | python3 -c '
import sys, json
try: alerts = json.load(sys.stdin)["data"]["alerts"]
except Exception: print("ERR"); sys.exit()
print(",".join(sorted(a["labels"].get("service","?") for a in alerts if a.get("state")=="firing")) or "none")'; }

scripts/render-alertmanager-config.sh .env
"${COMPOSE[@]}" up -d
echo "等 stack 就緒..."; sleep 10

# dead 產一筆後就不再產(模擬掛掉);先確認這筆真的入 VL(排除 ingest 失敗 → 否則後面 firing 只是空窗假象)
ingest "{\"ts\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",\"service\":\"dead\",\"level\":\"info\",\"message\":\"last gasp\"}"
echo "確認 dead last gasp 入 VL..."
for i in $(seq 1 8); do [[ "$(vl_count 'service:=dead _time:1m')" -ge 1 ]] && { echo "dead 已入庫"; break; }; sleep 3; done
[[ "$(vl_count 'service:=dead _time:1m')" -ge 1 ]] || { echo "FAIL: dead log 未入 VL(ingest 失敗,非設計問題)"; exit 1; }

# alive 背景每 5s 產一筆(< 1m 窗,確保窗內恆有 log);跑滿全程
( for i in $(seq 1 72); do
    ingest "{\"ts\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",\"service\":\"alive\",\"level\":\"info\",\"message\":\"heartbeat $i\"}" || true
    sleep 5
  done ) &
ALIVE_PID=$!
kill_alive() { kill "$ALIVE_PID" >/dev/null 2>&1 || true; }

# 就緒閘:stack 冷啟時 VL 尚無資料,vmalert 首輪 eval 對空窗會把 dead/alive 都判 firing(n=0,正是本設計要測的行為)。
# 等 alive 進窗、vmalert 重評把冷啟假 firing 全清(firing==none)、且 dead 仍在窗內(此刻 dead 亦 non-firing)→ baseline 乾淨。
# 這樣後面偵測到的 dead firing 才是「那筆滑出 1m 窗」的真 firing,而非冷啟空窗殘影。最多 150s。
echo "等 baseline 乾淨(alive 進窗、冷啟假 firing 清光)..."
READY=0
for i in $(seq 1 30); do
  if [[ "$(vl_count 'service:=alive _time:1m')" -ge 1 && "$(firing)" == "none" ]]; then
    READY=1; echo "baseline 乾淨(無殘留 firing,dead 已 non-firing)"; break
  fi
  sleep 5
done
if [[ "$READY" != 1 ]]; then echo "FAIL: 就緒閘未達成(最後 firing=$(firing))"; kill_alive; exit 1; fi

# 等 dead 那筆滑出 1m 窗(~70s)+ vmalert eval(interval 15s)→ dead firing(value=0)。最多 180s
echo "等 dead 滑出 1m 窗 → firing..."
DEAD_FIRING=0
for i in $(seq 1 36); do
  if echo "$(firing)" | grep -q 'dead'; then
    DEAD_FIRING=1
    VAL=$(curl -s --max-time 5 "$AM" | python3 -c '
import sys, json
for a in json.load(sys.stdin)["data"]["alerts"]:
    if a["labels"].get("service")=="dead": print(a["value"]); break' 2>/dev/null || echo "?")
    echo "dead firing(value=$VAL)"; break
  fi
  sleep 5
done
if [[ "$DEAD_FIRING" != 1 ]]; then
  echo "FAIL: dead 未 firing。若已確認 rule/ingest 正確,這推翻設計地基(vmalert 不把 value=0 當 firing)。"
  kill_alive; exit 1
fi

# 對照:此刻 alive 背景仍在跑、窗內有 log → alive 不該 firing(在 kill alive 之前查,最穩)。
# fail-closed:firing() 若回 ERR(curl/JSON 失敗)視為未就緒 → 重試;連續失敗即真 FAIL,不當「無 alive」通過。
f=ERR
for i in $(seq 1 5); do
  f=$(firing); [[ "$f" != ERR ]] && break; sleep 3
done
if [[ "$f" == ERR ]]; then
  echo "FAIL: alive 對照查詢連續失敗(firing=ERR),無法確認 alive 狀態"
  kill_alive; exit 1
fi
if echo "$f" | grep -q 'alive'; then
  echo "FAIL: alive 窗內有 log 卻 firing(對照失敗)。firing=$f VL alive _time:1m=$(vl_count 'service:=alive _time:1m')"
  echo "===DEBUG alerts==="; curl -s --max-time 5 "$AM" | python3 -m json.tool 2>/dev/null || true
  kill_alive; exit 1
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
