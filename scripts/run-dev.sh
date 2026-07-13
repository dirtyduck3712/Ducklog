#!/usr/bin/env bash
# run-dev.sh — 本機開發用:起 VictoriaLogs,或 build ducklog-mcp 並印出 Claude Code 註冊指令。
#
# 用法:
#   scripts/run-dev.sh vl     # 前景啟動 VictoriaLogs（127.0.0.1:9428）
#   scripts/run-dev.sh mcp    # build ducklog-mcp 並印出 MCP 註冊指令
#
# 環境變數:
#   VL_BINARY   victoria-logs-prod 的路徑（vl 子命令用；未設則試 PATH，再提示下載）
#   VL_DATA     storage 目錄（預設 ./data/victoria-logs）
#   VL_URL      MCP server 要連的 VL 位址（mcp 子命令用；預設 http://127.0.0.1:9428）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VL_ADDR="127.0.0.1:9428"
VL_DATA="${VL_DATA:-$ROOT/data/victoria-logs}"

download_hint() {
  cat >&2 <<'EOF'
找不到 VictoriaLogs binary。請下載 OSS 單一執行檔（非 -enterprise 版）:

  https://github.com/VictoriaMetrics/VictoriaLogs/releases/latest
  取 victoria-logs-linux-amd64-v*.tar.gz，解出 victoria-logs-prod。

然後用 VL_BINARY 指向它，例如:

  VL_BINARY=/path/to/victoria-logs-prod scripts/run-dev.sh vl
EOF
}

resolve_vl_binary() {
  if [[ -n "${VL_BINARY:-}" ]]; then
    if [[ ! -x "$VL_BINARY" ]]; then
      echo "VL_BINARY 指向的檔案不存在或不可執行: $VL_BINARY" >&2
      exit 1
    fi
    echo "$VL_BINARY"
    return
  fi
  if command -v victoria-logs-prod >/dev/null 2>&1; then
    command -v victoria-logs-prod
    return
  fi
  download_hint
  exit 1
}

cmd_vl() {
  local bin
  bin="$(resolve_vl_binary)"
  mkdir -p "$VL_DATA"
  echo "啟動 VictoriaLogs: $bin" >&2
  echo "vmui:  http://$VL_ADDR/select/vmui" >&2
  echo "資料:  $VL_DATA（retention 30d）" >&2
  exec "$bin" \
    -storageDataPath="$VL_DATA" \
    -retentionPeriod=30d \
    -httpListenAddr="$VL_ADDR"
}

cmd_mcp() {
  local vl_url="${VL_URL:-http://127.0.0.1:9428}"
  echo "build ducklog-mcp..." >&2
  ( cd "$ROOT" && go build -o ducklog-mcp ./cmd/ducklog-mcp )
  local bin="$ROOT/ducklog-mcp"
  echo "已 build: $bin" >&2
  cat <<EOF

ducklog-mcp 是 stdio MCP server，由 Claude Code 啟動，不用手動跑。
在 Claude Code 註冊:

  claude mcp add ducklog --env VL_URL=$vl_url -- $bin

或等價的 JSON 設定:

  {
    "mcpServers": {
      "ducklog": {
        "command": "$bin",
        "env": { "VL_URL": "$vl_url" }
      }
    }
  }

注意: ducklog-mcp 啟動時會 Ping VL（$vl_url），VL 不可達則 fail fast。
先用 'scripts/run-dev.sh vl' 起 VL 再註冊。
EOF
}

case "${1:-}" in
  vl)  cmd_vl ;;
  mcp) cmd_mcp ;;
  *)
    echo "用法: $(basename "$0") {vl|mcp}" >&2
    exit 2
    ;;
esac
