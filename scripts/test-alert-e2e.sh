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

# 等 vmalert 評估(interval 1m,for 0s);firing 穩定落在第 2 個 eval cycle(~ingest 後 90s),
# 故輪詢上限拉到 240s 以涵蓋 2~3 個 cycle,避免 flaky(見 task-4-report)
for i in $(seq 1 48); do
  if curl -s http://127.0.0.1:18880/api/v1/alerts | grep -q '"FatalPattern"'; then
    echo "vmalert: FatalPattern firing"; break
  fi
  sleep 5
done
curl -s http://127.0.0.1:18880/api/v1/alerts | grep -q '"FatalPattern"' || { echo "FAIL: FatalPattern 未 firing"; exit 1; }

# 等 alertmanager 送到 sink(group_wait 30s);輪詢最多 120s
for i in $(seq 1 24); do
  if "${COMPOSE[@]}" exec -T sink sh -c 'test -s /out/hits.log' 2>/dev/null; then
    echo "sink: 收到 webhook"; break
  fi
  sleep 5
done
"${COMPOSE[@]}" exec -T sink sh -c 'grep -q FatalPattern /out/hits.log' \
  && echo "PASS: e2e 綠燈(sink 收到 FatalPattern alert)" \
  || { echo "FAIL: sink 未收到 FatalPattern"; exit 1; }
