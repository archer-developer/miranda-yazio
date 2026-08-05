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

This service can hold multiple YAZIO accounts at once (one per household
member, configured in `yazio.users` — see Configuration below), so every
tool below takes a required `user` parameter selecting which account's
diary to read or write.

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
# fill in YAZIO_MCP_TOKEN (openssl rand -hex 32), YAZIO_USERNAME_ARCHER, YAZIO_PASSWORD_ARCHER

make run
```

The server listens on `:8790` by default: `GET /healthz` (unauthenticated)
and `/mcp` (the MCP endpoint, requires `Authorization: Bearer <token>`).

```bash
curl -s http://localhost:8790/healthz
```

On first request that needs YAZIO auth for a given user, the service logs
in with that user's configured username/password and caches the resulting
access/refresh token pair at
`$XDG_CONFIG_HOME/yazio-mcp/token-<name>.json` (or
`~/.config/yazio-mcp/token-<name>.json`), mode `0600` — one file per
configured user. Subsequent restarts reuse the cached tokens instead of
logging in again.

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
it as a `systemd --user` service. `config/*.yaml` and `.env` are **never
uploaded** — create `.env` on the server by hand on first deploy with
`YAZIO_MCP_TOKEN` and one `YAZIO_USERNAME_<NAME>`/`YAZIO_PASSWORD_<NAME>`
pair per user configured in `config/*.yaml`'s `yazio.users` (e.g.
`YAZIO_USERNAME_ARCHER`/`YAZIO_PASSWORD_ARCHER`).

## Configuration

Every field has a built-in default (`internal/config.Default()`); the
service loads and merges **every** `config/*.yaml` file at startup (later
file wins on any field both set — see `internal/config.Load`), so a
deployment only needs to add a `config/*.yaml` file if something differs
from the default. `config/config.yaml.dist` is checked into git as
documentation and a copy-paste starting point — it is never read by the
service (it doesn't end in `.yaml`). `config/*.yaml` itself is gitignored,
since it typically holds real usernames and other environment-specific
detail; secrets themselves are never stored in any `config/*.yaml` file,
only the *name* of the environment variable to read them from. See
`config/config.yaml.dist`'s comments for every available field (search
locale/country, request timeout, token cache dir, ...). Override the
config directory itself with `YAZIO_MCP_CONFIG_DIR` (default `config`).

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
