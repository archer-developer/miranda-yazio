#!/usr/bin/env bash
# Full smoke-test sequence against a running miranda-yazio instance:
#   /healthz -> tools/list -> search_products -> get_product ->
#   add_consumed_item -> get_consumed_items (confirms it landed) ->
#   remove_consumed_item -> get_consumed_items (confirms cleanup)
#
# This performs one real write-then-delete against the "archer" YAZIO
# account's diary (a "snack" entry for whatever search_products finds
# first) — it cleans up after itself, but it is not a dry run.
#
# Requires the service to be running with a valid .env:
#   cp .env.example .env   # fill in YAZIO_MCP_TOKEN/YAZIO_USERNAME_ARCHER/YAZIO_PASSWORD_ARCHER
#   make run
set -euo pipefail
cd "$(dirname "$0")/../.."

base_url="${MIRANDA_YAZIO_URL:-http://localhost:8790}"
smoke_dir="scripts/smoke"

pass() { echo "  OK: $1"; }
fail() { echo "  FAIL: $1" >&2; exit 1; }

# extract_result reads a raw mcp-call.sh response (an SSE "data: {...}"
# frame) from stdin and prints the JSON-RPC result as compact JSON,
# failing loudly on a JSON-RPC-level error or a tool-level isError result.
extract_result() {
  python3 -c '
import json, sys

raw = sys.stdin.read()
line = next((l for l in raw.splitlines() if l.startswith("data: ")), None)
if line is None:
    print("no SSE data line in response:\n" + raw, file=sys.stderr)
    sys.exit(1)

payload = json.loads(line[len("data: "):])
if "error" in payload:
    print("JSON-RPC error: " + json.dumps(payload["error"]), file=sys.stderr)
    sys.exit(1)

result = payload["result"]
if result.get("isError"):
    text = "".join(c.get("text", "") for c in result.get("content", []))
    print("tool error: " + text, file=sys.stderr)
    sys.exit(1)

print(json.dumps(result))
'
}

echo "==> GET /healthz"
curl -fsS "$base_url/healthz" >/dev/null && pass "service is up" || fail "is the service running? (make run)"

echo "==> tools/list"
tools_result="$("$smoke_dir/mcp-call.sh" "$smoke_dir/requests/tools_list.json" | extract_result)"
tool_count="$(python3 -c 'import json,sys; print(len(json.load(sys.stdin)["tools"]))' <<<"$tools_result")"
[ "$tool_count" -eq 5 ] && pass "5 tools registered" || fail "expected 5 tools, got $tool_count"

echo "==> search_products"
search_result="$("$smoke_dir/mcp-call.sh" "$smoke_dir/requests/search_products.json" | extract_result)"
product_id="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["structuredContent"]["products"][0]["product_id"])' <<<"$search_result")"
[ -n "$product_id" ] && pass "found product $product_id" || fail "search_products returned no results"

echo "==> get_product $product_id"
get_product_req="$(python3 -c "import json; print(json.dumps({'jsonrpc':'2.0','id':4,'method':'tools/call','params':{'name':'get_product','arguments':{'user':'archer','product_id':'$product_id'}}}))")"
product_result="$(echo "$get_product_req" | "$smoke_dir/mcp-call.sh" | extract_result)"
default_amount="$(python3 -c 'import json,sys; p=json.load(sys.stdin)["structuredContent"]["product"]; s=p.get("servings") or []; print(s[0]["amount_grams"] if s else 100)' <<<"$product_result")"
pass "product detail fetched, will log amount_grams=$default_amount"

echo "==> add_consumed_item"
add_req="$(python3 -c "import json; print(json.dumps({'jsonrpc':'2.0','id':6,'method':'tools/call','params':{'name':'add_consumed_item','arguments':{'user':'archer','product_id':'$product_id','amount_grams':$default_amount,'meal_type':'snack'}}}))")"
add_result="$(echo "$add_req" | "$smoke_dir/mcp-call.sh" | extract_result)"
logged="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["structuredContent"]["logged"])' <<<"$add_result")"
[ "$logged" = "True" ] && pass "logged test entry" || fail "add_consumed_item did not report logged=true"

echo "==> get_consumed_items (confirm it landed)"
items_result="$("$smoke_dir/mcp-call.sh" "$smoke_dir/requests/get_consumed_items.json" | extract_result)"
item_id="$(python3 -c "
import json, sys
d = json.load(sys.stdin)['structuredContent']
matches = [i for i in d['items'] if i['product_id'] == '$product_id']
print(matches[-1]['id'] if matches else '')
" <<<"$items_result")"
[ -n "$item_id" ] && pass "confirmed entry $item_id in today's diary" || fail "test entry not found in get_consumed_items"

echo "==> remove_consumed_item $item_id (cleanup)"
remove_req="$(python3 -c "import json; print(json.dumps({'jsonrpc':'2.0','id':7,'method':'tools/call','params':{'name':'remove_consumed_item','arguments':{'user':'archer','item_id':'$item_id'}}}))")"
remove_result="$(echo "$remove_req" | "$smoke_dir/mcp-call.sh" | extract_result)"
removed="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["structuredContent"]["removed"])' <<<"$remove_result")"
[ "$removed" = "True" ] && pass "removed test entry" || fail "remove_consumed_item did not report removed=true"

echo "==> get_consumed_items (confirm cleanup)"
items_after="$("$smoke_dir/mcp-call.sh" "$smoke_dir/requests/get_consumed_items.json" | extract_result)"
python3 -c "
import json, sys
d = json.load(sys.stdin)['structuredContent']
assert not any(i['id'] == '$item_id' for i in d['items']), 'test entry still present after remove'
" <<<"$items_after"
pass "diary is clean"

echo
echo "All smoke tests passed."
