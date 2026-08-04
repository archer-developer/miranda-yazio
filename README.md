# miranda-yazio

An MCP server that lets Miranda read and write a [YAZIO](https://www.yazio.com/)
food diary — search products, log meals in household-friendly quantities
("2 cutlets", "a cup of tea"), and review or correct what's already logged.
YAZIO has no official public API; `internal/yazio` talks to the same
unofficial v15 REST API the YAZIO mobile app uses.

Sibling project to [miranda](../miranda) (the "brain" — LLM orchestration,
MCP *client*) and [miranda-diary](../miranda-diary), built from
[miranda-service-skeleton](../miranda-service-skeleton). See `CLAUDE.md`
for the full architecture and conventions.

```
Miranda <--Streamable HTTP (bearer token)--> httpserver
                                                  |
                                             mcpserver
                          search_products  get_product  get_consumed_items
                              add_consumed_item  remove_consumed_item
                                                  |
                                          internal/yazio.Client
                                                  |
                                   YAZIO unofficial v15 REST API
```

## MCP tools

- **search_products** — find candidate foods by name/brand.
- **get_product** — full detail for one product, including every serving
  type it supports (e.g. "piece", "portion", "glass") and its weight in
  grams. This is how a household quantity like "2 cutlets" gets converted
  to grams before logging.
- **get_consumed_items** — the diary entries logged for a given date
  (defaults to today).
- **add_consumed_item** — log one food item against a meal
  (breakfast/lunch/dinner/snack) and date.
- **remove_consumed_item** — delete a diary entry by ID, to correct a
  mistake.

## Building

Requires Go 1.25+. No Docker, no CGO.

```bash
go build -o miranda-yazio ./cmd/miranda-yazio
# or
make build
```

## Running

```bash
cp .env.example .env
# fill in YAZIO_MCP_TOKEN (openssl rand -hex 32), YAZIO_USERNAME, YAZIO_PASSWORD

make run
```

The server listens on `:8790` by default: `GET /healthz` (unauthenticated)
and `/mcp` (the MCP endpoint, requires `Authorization: Bearer <token>`).

```bash
curl -s http://localhost:8790/healthz
```

On first request that needs YAZIO auth, the service logs in with
`YAZIO_USERNAME`/`YAZIO_PASSWORD` and caches the resulting access/refresh
token pair at `$XDG_CONFIG_HOME/yazio-mcp/token.json` (or
`~/.config/yazio-mcp/token.json`), mode `0600`. Subsequent restarts reuse
the cached token instead of logging in again.

## Testing and quality

```bash
make test    # go test ./... -race
make lint    # golangci-lint run ./...
make fmt     # gofmt + goimports
make check   # fmt + lint + test — run this before every commit
```

`make tools` installs `golangci-lint` and `goimports` if they're not
already on `PATH`. There is no CI configured — `make check` is the
enforcement mechanism, so run it yourself before committing.

## Deploying

```bash
MIRANDA_DEPLOY_HOST=user@host ./scripts/deploy.sh
```

Cross-compiles for `linux/amd64`, ships the binary over SSH, and installs
it as a `systemd --user` service. `config/config.yaml` and `.env` are
**never uploaded** — create `.env` on the server by hand on first deploy
with `YAZIO_MCP_TOKEN`, `YAZIO_USERNAME`, and `YAZIO_PASSWORD`.

## Configuration

Every field has a built-in default (`internal/config.Default()`), so
`config/config.yaml` only needs to override what differs. Secrets are
never stored in `config.yaml` — only the *name* of the environment
variable to read them from. See `config/config.yaml`'s comments for every
available field (search locale/country, request timeout, token cache
path, ...).

## Project layout

```
cmd/miranda-yazio/        entrypoint: config, wiring, HTTP listen, graceful shutdown
internal/config/          Default() + YAML config + validation
internal/envfile/         .env loader (real env always wins)
internal/yazio/           YAZIO API client: OAuth (login/refresh/cache), search, diary CRUD
internal/mcpserver/       the five MCP tools, wired to internal/yazio
internal/httpserver/      bearer-token auth + /healthz
scripts/deploy.sh         cross-compile + SSH deploy + systemd --user restart
```

See `CLAUDE.md` for the full set of architecture, testing, and
code-quality requirements.
