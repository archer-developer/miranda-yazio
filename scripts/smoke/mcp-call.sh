#!/usr/bin/env bash
# Sends one MCP JSON-RPC request to a running miranda-yazio instance,
# handling the Streamable HTTP handshake (initialize + notifications/
# initialized + the Mcp-Session-Id header) automatically first.
#
# Usage:
#   ./scripts/smoke/mcp-call.sh requests/search_products.json
#   echo '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' | ./scripts/smoke/mcp-call.sh
#
# Reads YAZIO_MCP_TOKEN from .env (repo root) unless already set in the
# environment. Override the target with MIRANDA_YAZIO_URL (default
# http://localhost:8790).
set -euo pipefail
cd "$(dirname "$0")/../.."

if [ -f .env ]; then
  set -a; source .env; set +a
fi
: "${YAZIO_MCP_TOKEN:?YAZIO_MCP_TOKEN not set (check .env or export it)}"

base_url="${MIRANDA_YAZIO_URL:-http://localhost:8790}"

init_headers="$(mktemp)"
trap 'rm -f "$init_headers"' EXIT

curl -fsS -D "$init_headers" -o /dev/null -X POST "$base_url/mcp" \
  -H "Authorization: Bearer $YAZIO_MCP_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke-test","version":"0.1.0"}}}'

session="$(grep -i '^Mcp-Session-Id:' "$init_headers" | tr -d '\r' | awk '{print $2}')"
if [ -z "$session" ]; then
  echo "failed to obtain Mcp-Session-Id from initialize response — is the service running (make run) and /healthz OK?" >&2
  exit 1
fi

curl -fsS -o /dev/null -X POST "$base_url/mcp" \
  -H "Authorization: Bearer $YAZIO_MCP_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Mcp-Session-Id: $session" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'

body="$(cat "${1:-/dev/stdin}")"

curl -fsS -X POST "$base_url/mcp" \
  -H "Authorization: Bearer $YAZIO_MCP_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Mcp-Session-Id: $session" \
  -d "$body"
echo
