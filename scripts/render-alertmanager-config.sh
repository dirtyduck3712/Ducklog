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
