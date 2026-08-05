#!/usr/bin/env bash
# Full smoke-test sequence against a running miranda-yazio instance:
#
#   Product flow:
#     /healthz -> tools/list -> search_products -> get_product ->
#     add_consumed_item -> get_consumed_items (confirm) ->
#     remove_consumed_item -> get_consumed_items (confirm cleanup)
#
#   Recipe flow:
#     create_recipe -> list_recipes -> get_recipe ->
#     add_consumed_recipe -> get_consumed_items (confirm recipe portion) ->
#     remove_consumed_recipe -> delete_recipe ->
#     list_recipes (confirm gone)
#
# Writes and immediately deletes against the "archer" YAZIO account —
# cleans up after itself, but is not a dry run.
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
[ "$tool_count" -eq 12 ] && pass "12 tools registered" || fail "expected 12 tools, got $tool_count"

# -----------------------------------------------------------------------
# Product flow
# -----------------------------------------------------------------------

echo
echo "--- product flow ---"

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

echo "==> get_consumed_items (confirm product entry landed)"
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

echo "==> get_consumed_items (confirm product cleanup)"
items_after="$("$smoke_dir/mcp-call.sh" "$smoke_dir/requests/get_consumed_items.json" | extract_result)"
python3 -c "
import json, sys
d = json.load(sys.stdin)['structuredContent']
assert not any(i['id'] == '$item_id' for i in d['items']), 'test entry still present after remove'
" <<<"$items_after"
pass "product diary is clean"

# -----------------------------------------------------------------------
# Recipe flow
# -----------------------------------------------------------------------

echo
echo "--- recipe flow ---"

# Use the same product_id twice with different amounts as the two required
# ingredients — YAZIO has no server-side deduplication constraint.
echo "==> create_recipe (2 ingredients, 2 portions)"
create_req="$(python3 -c "
import json
print(json.dumps({
  'jsonrpc': '2.0', 'id': 20, 'method': 'tools/call',
  'params': {
    'name': 'create_recipe',
    'arguments': {
      'user': 'archer',
      'name': 'Smoke Test Recipe (delete me)',
      'ingredients': [
        {'product_id': '$product_id', 'amount_grams': 100},
        {'product_id': '$product_id', 'amount_grams': 200},
      ],
      'portion_count': 2,
      'instructions': ['Step 1: smoke test', 'Step 2: clean up'],
    }
  }
}))
")"
create_result="$(echo "$create_req" | "$smoke_dir/mcp-call.sh" | extract_result)"
recipe_id="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["structuredContent"]["recipe_id"])' <<<"$create_result")"
created="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["structuredContent"]["created"])' <<<"$create_result")"
[ "$created" = "True" ] && [ -n "$recipe_id" ] && pass "created recipe $recipe_id" || fail "create_recipe did not report created=true or returned no recipe_id"

echo "==> list_recipes (confirm recipe appears)"
list_req="$(python3 -c "import json; print(json.dumps({'jsonrpc':'2.0','id':21,'method':'tools/call','params':{'name':'list_recipes','arguments':{'user':'archer'}}}))")"
list_result="$(echo "$list_req" | "$smoke_dir/mcp-call.sh" | extract_result)"
python3 -c "
import json, sys
d = json.load(sys.stdin)['structuredContent']
ids = [r['recipe_id'] for r in d['recipes']]
assert '$recipe_id' in ids, f'new recipe $recipe_id not found in list_recipes: {ids}'
" <<<"$list_result"
pass "recipe appears in list_recipes"

echo "==> get_recipe $recipe_id"
get_recipe_req="$(python3 -c "import json; print(json.dumps({'jsonrpc':'2.0','id':22,'method':'tools/call','params':{'name':'get_recipe','arguments':{'user':'archer','recipe_id':'$recipe_id'}}}))")"
get_recipe_result="$(echo "$get_recipe_req" | "$smoke_dir/mcp-call.sh" | extract_result)"
python3 -c "
import json, sys
r = json.load(sys.stdin)['structuredContent']['recipe']
assert r['recipe_id'] == '$recipe_id', f'wrong recipe_id: {r[\"recipe_id\"]}'
assert r['portion_count'] == 2, f'expected portion_count=2, got {r[\"portion_count\"]}'
assert len(r['ingredients']) == 2, f'expected 2 ingredients, got {len(r[\"ingredients\"])}'
" <<<"$get_recipe_result"
pass "get_recipe returned correct detail"

echo "==> add_consumed_recipe (1.5 portions, snack)"
add_recipe_req="$(python3 -c "import json; print(json.dumps({'jsonrpc':'2.0','id':23,'method':'tools/call','params':{'name':'add_consumed_recipe','arguments':{'user':'archer','recipe_id':'$recipe_id','portions':1.5,'meal_type':'snack'}}}))")"
add_recipe_result="$(echo "$add_recipe_req" | "$smoke_dir/mcp-call.sh" | extract_result)"
recipe_logged="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["structuredContent"]["logged"])' <<<"$add_recipe_result")"
[ "$recipe_logged" = "True" ] && pass "logged recipe portion to diary" || fail "add_consumed_recipe did not report logged=true"

echo "==> get_consumed_items (confirm recipe portion landed)"
items_with_recipe="$("$smoke_dir/mcp-call.sh" "$smoke_dir/requests/get_consumed_items.json" | extract_result)"
recipe_entry_id="$(python3 -c "
import json, sys
d = json.load(sys.stdin)['structuredContent']
matches = [p for p in d.get('recipe_portions', []) if p['recipe_id'] == '$recipe_id']
print(matches[-1]['entry_id'] if matches else '')
" <<<"$items_with_recipe")"
[ -n "$recipe_entry_id" ] && pass "confirmed recipe portion $recipe_entry_id in today's diary" || fail "recipe portion not found in get_consumed_items recipe_portions"

echo "==> remove_consumed_recipe $recipe_entry_id (cleanup diary)"
remove_recipe_req="$(python3 -c "import json; print(json.dumps({'jsonrpc':'2.0','id':24,'method':'tools/call','params':{'name':'remove_consumed_recipe','arguments':{'user':'archer','entry_id':'$recipe_entry_id'}}}))")"
remove_recipe_result="$(echo "$remove_recipe_req" | "$smoke_dir/mcp-call.sh" | extract_result)"
recipe_removed="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["structuredContent"]["removed"])' <<<"$remove_recipe_result")"
[ "$recipe_removed" = "True" ] && pass "removed recipe diary entry" || fail "remove_consumed_recipe did not report removed=true"

echo "==> delete_recipe $recipe_id (cleanup recipe)"
delete_recipe_req="$(python3 -c "import json; print(json.dumps({'jsonrpc':'2.0','id':25,'method':'tools/call','params':{'name':'delete_recipe','arguments':{'user':'archer','recipe_id':'$recipe_id'}}}))")"
delete_recipe_result="$(echo "$delete_recipe_req" | "$smoke_dir/mcp-call.sh" | extract_result)"
recipe_deleted="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["structuredContent"]["deleted"])' <<<"$delete_recipe_result")"
[ "$recipe_deleted" = "True" ] && pass "deleted recipe" || fail "delete_recipe did not report deleted=true"

echo "==> list_recipes (confirm recipe is gone)"
list_after="$(echo "$list_req" | "$smoke_dir/mcp-call.sh" | extract_result)"
python3 -c "
import json, sys
d = json.load(sys.stdin)['structuredContent']
ids = [r['recipe_id'] for r in d['recipes']]
assert '$recipe_id' not in ids, f'deleted recipe $recipe_id still appears in list_recipes'
" <<<"$list_after"
pass "recipe is gone from list_recipes"

# -----------------------------------------------------------------------
# Daily summary
# -----------------------------------------------------------------------

echo
echo "--- daily summary ---"

echo "==> get_daily_summary (today)"
summary_req="$(python3 -c "import json; print(json.dumps({'jsonrpc':'2.0','id':30,'method':'tools/call','params':{'name':'get_daily_summary','arguments':{'user':'archer'}}}))")"
summary_result="$(echo "$summary_req" | "$smoke_dir/mcp-call.sh" | extract_result)"
python3 -c "
import json, sys
d = json.load(sys.stdin)['structuredContent']
assert 'consumed' in d, 'missing consumed field'
assert 'goals' in d, 'missing goals field'
assert 'remaining' in d, 'missing remaining field'
assert 'energy_kcal' in d['consumed'], 'missing consumed.energy_kcal'
assert 'energy_kcal' in d['goals'], 'missing goals.energy_kcal'
assert 'energy_kcal' in d['remaining'], 'missing remaining.energy_kcal'
# goals should be non-zero if the user has configured them in YAZIO
assert d['goals']['energy_kcal'] > 0, f\"goals.energy_kcal is 0 — check that archer's YAZIO account has daily goals configured\"
" <<<"$summary_result"
consumed_kcal="$(python3 -c 'import json,sys; print(round(json.load(sys.stdin)["structuredContent"]["consumed"]["energy_kcal"]))' <<<"$summary_result")"
remaining_kcal="$(python3 -c 'import json,sys; print(round(json.load(sys.stdin)["structuredContent"]["remaining"]["energy_kcal"]))' <<<"$summary_result")"
pass "summary returned: consumed ${consumed_kcal} kcal, remaining ${remaining_kcal} kcal"

echo
echo "All smoke tests passed."
