# MCP smoke tests / debugging

Manual curl examples against a running `miranda-yazio` instance, captured
while verifying the service end to end against the real YAZIO API. Useful
for debugging the MCP wiring itself (auth, session handling, tool schemas)
independent of any MCP client.

## Prerequisites

```bash
# from the repo root
cp .env.example .env   # fill in YAZIO_MCP_TOKEN, YAZIO_USERNAME, YAZIO_PASSWORD
make run                # or: ./miranda-yazio
```

## Files

- **`mcp-call.sh`** — reusable helper. Handles the MCP Streamable HTTP
  handshake (`initialize` + `notifications/initialized` + the
  `Mcp-Session-Id` header every client needs) and then sends one JSON-RPC
  request, printing the raw response.

  ```bash
  ./scripts/smoke/mcp-call.sh scripts/smoke/requests/search_products.json
  # or pipe a one-off request:
  echo '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' | ./scripts/smoke/mcp-call.sh
  ```

- **`requests/*.json`** — one example JSON-RPC request body per tool,
  captured from a real working call. `product_id`/`item_id` values in
  these files are real IDs that existed in YAZIO's database at capture
  time (chicken soup, "Куриный суп") — re-run `search_products.json`
  first if they've since changed or you want different products.

- **`run-all.sh`** — the full sequence used to verify this service:
  `/healthz` → `tools/list` → `search_products` → `get_product` →
  `get_consumed_items` → `add_consumed_item` → `get_consumed_items`
  (confirms it landed) → `remove_consumed_item` → `get_consumed_items`
  (confirms cleanup). **This performs one real write-then-delete against
  the configured YAZIO account's diary** — it cleans up after itself, but
  it is not a dry run.

  ```bash
  ./scripts/smoke/run-all.sh
  ```

## Notes from building this

- MCP's Streamable HTTP transport replies to `initialize` with
  `Content-Type: text/event-stream` and one `event: message` /
  `data: {...}` frame, not a bare JSON body — `mcp-call.sh` doesn't parse
  this (it just prints it), but a client needs to.
- The `Mcp-Session-Id` response header from `initialize` must be echoed
  back as a request header on every subsequent call on that session.
- `GET /user/consumed-items` returns `{"products": [...], "recipe_portions":
  [...], "simple_products": [...]}`, not a bare array — this is what
  `yazio_public_api`'s swagger doc claims, but the live API disagrees.
  `internal/yazio/diary.go`'s `GetConsumedItems` was fixed to match the
  real shape after this was caught by testing against the live API with
  these exact scripts.
