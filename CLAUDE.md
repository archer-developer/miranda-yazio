# miranda-yazio — project notes for Claude Code

An MCP server that lets Miranda read and write YAZIO food diaries for a
small, config-defined set of accounts (one per household member). Sibling
project to [miranda-code-execution-sandbox](../miranda-code-execution-sandbox)
and [miranda-diary](../miranda-diary) — same stack, same conventions, built
from [miranda-service-skeleton](../miranda-service-skeleton).

## Conventions

Write explanatory comments — doc-comments on exported symbols, comments on
non-obvious logic and on *why* a decision was made. This family of
projects intentionally diverges from a terse/no-comments default: these
are small home-infra services maintained intermittently, and future-you
(or Claude, months later) benefits more from carried-forward reasoning
than from terse code. Config follows the same `Default()`-in-Go +
YAML-overrides-only pattern as every sibling service; secrets are never in
YAML, only the *name* of the environment variable holding them. Also
mirroring [miranda](../miranda): `config/config.yaml.dist` is a
fully-documented, git-tracked template that the service itself never
reads; the service instead globs every `config/*.yaml` file (gitignored,
per-deployment) and merges them in order (`internal/config.Load`) — see
`README.md`'s Configuration section for the full workflow.

**No Docker, no CGO.** Single static Go binary (`CGO_ENABLED=0`), runs
host-native under `systemd --user`.

## Architecture

```
Miranda <--Streamable HTTP (bearer token)--> httpserver
                                                  |
                                             mcpserver
                          search_products  get_product  get_consumed_items
                              add_consumed_item  remove_consumed_item
                               (every tool takes a required "user" param)
                                                  |
                                  map[string]internal/yazio.Client
                              (one client per configured yazio.users[] entry)
                                                  |
                                   YAZIO unofficial v15 REST API
                                     (https://yzapi.yazio.com/v15)
```

**This is a multi-tenant service, but a small/closed one.** Each YAZIO
account belongs to a household member configured in `config/*.yaml`'s
`yazio.users` list (name + the env var names holding that account's
login/password — see the top-level Conventions on secrets). `main.go`
builds one `internal/yazio.Client` per configured user at startup and
hands `mcpserver.New` a `map[string]YazioClient` keyed by `yazio.users[].name`.
Every MCP tool's `user` parameter is resolved against that map
(`internal/mcpserver/server.go`'s `resolveClient`) before any YAZIO call is
made — an empty or unmatched `user` is a caller error, not something
worth guessing a default for. This is deliberately *not* the general
"Multi-tenant / multi-user services" pattern from the skeleton's
`CLAUDE.md` (no per-request auth/identity resolution, no dynamic
tenant provisioning) — the user set is a short, config-defined list of
named accounts, not open registration.

**YAZIO has no official public API.** `internal/yazio` talks to the same
unofficial v15 REST API the YAZIO mobile app uses, reverse-engineered the
same way every other open-source YAZIO client does (`yazio_public_api`,
`go-yazio`, the `juriadams` JS client, ...). Because it's unofficial and
undocumented, every method wraps decode/HTTP errors instead of panicking
if a response doesn't match the expected shape — see
`internal/yazio/errors.go` for the sentinel errors
(`ErrUnauthorized`, `ErrRateLimited`, `ErrServiceUnavailable`,
`ErrNotFound`, `ErrInvalidCredentials`) callers should check with
`errors.Is`.

**OAuth client_id/client_secret are hardcoded, not secrets.** YAZIO has no
developer program to register an app with, so every open-source client —
including this one — uses the same client_id/client_secret pair extracted
from the official Android app (`internal/yazio/auth.go`). This identifies
"the YAZIO app" to YAZIO's backend, not an individual account, so unlike
each user's username/password (`yazio.users[].username_env`/
`password_env`) it doesn't go through an env var.

### Auth: login, refresh, and the token cache

Each `internal/yazio.Client` in `main.go`'s `map[string]YazioClient` is a
fully independent single-account client — `internal/yazio` itself has no
concept of "user"; that's purely a `main.go`/`mcpserver` wiring concern
(see Architecture above).

1. `internal/yazio.authenticator` holds one account's access/refresh
   token pair in memory, protected by a mutex (safe for concurrent tool
   calls against that account).
2. On first use it tries to load a cached token from disk
   (`internal/yazio/token.go`, `TokenStore`) before falling back to a
   password-grant login (`POST /oauth/token`, `grant_type=password`).
3. Every subsequent call reuses the in-memory token as long as it's more
   than 5 minutes (`tokenRefreshBuffer`) from expiry; otherwise it
   refreshes (`grant_type=refresh_token`) before the in-memory token is
   used, minimizing full logins.
4. `Client.do` additionally retries exactly once on an unexpected 401 by
   forcing a refresh — YAZIO can invalidate a token server-side before its
   advertised expiry (e.g. after a password change).
5. On every successful login/refresh, the new pair is persisted to
   `$XDG_CONFIG_HOME/yazio-mcp/token-<name>.json` (or
   `~/.config/yazio-mcp/token-<name>.json`), mode `0600` — one file per
   configured user, named after `yazio.users[].name`
   (`yazio.TokenCachePathForUser`, `yazio.TokenCacheDir` overrides the
   directory) — so a service restart doesn't force a fresh password login
   for any account. A failed save is logged as a warning, not fatal — the
   in-memory token still works for the current run. **The access/refresh
   token and the account password are never logged**, only
   lengths/presence or the surrounding error context.

### Request flow for `add_consumed_item` (the non-obvious tool)

This is the tool behind the target scenario: "log 400g of chicken soup,
200g of mashed potato, and 2 cutlets at 140 kcal each for lunch" — the
caller (Miranda) is expected to reason in grams *after* looking up serving
sizes, not the end user.

0. Every call carries a `user` naming which configured YAZIO account
   (`yazio.users[].name`) the diary entry belongs to — Miranda picks this
   from whoever it's talking to, and it's the same value on every tool
   call in a given conversation.
1. Miranda calls `search_products` with a dish/brand name to get
   candidate `product_id`s and each result's per-gram macros.
2. For a household-unit quantity ("2 cutlets"), Miranda calls
   `get_product` on the chosen `product_id` to see the `servings` list —
   e.g. `{"type": "piece", "amount_grams": 70}` — and computes
   `amount_grams = serving.amount_grams * quantity` itself. If the user
   already gave a gram figure directly, this step is skipped.
3. Miranda calls `add_consumed_item` once per distinct food item with the
   computed `amount_grams`, a `meal_type`, and an optional `date`
   (defaults to today).
4. `internal/mcpserver` resolves `user` to a `YazioClient`
   (`resolveClient`, erroring on an empty/unknown user before any YAZIO
   call is made), validates the rest of the input, normalizes `meal_type`
   to lowercase, and calls `yazio.Client.AddConsumedItem`, which generates
   a random UUIDv4 for the diary-entry `id` YAZIO's API requires (distinct
   from `product_id`) and POSTs to `/user/consumed-items`.
5. YAZIO does **not** validate `product_id` against its product database —
   an unknown ID is silently discarded rather than rejected. Callers
   should only ever pass IDs that came from `search_products` or
   `get_product` in the same conversation.

`get_consumed_items` / `remove_consumed_item` are the read/undo halves of
the same flow — `remove_consumed_item` takes the diary-entry `id` from
`get_consumed_items`, not a `product_id`.

### Search locale

YAZIO's product database is region-scoped: searching a local brand name
without the matching `countries`/`locales` query params often returns
nothing. `internal/config.YazioConfig` (`default_country`, `default_locales`,
default `"BY"` / `["by_BY", "ru_RU", "en_EN"]`) sets these for every
`search_products` call, shared across every configured `yazio.users[]`
account, rather than exposing them as a per-call MCP parameter or a
per-user config field — every household member configured here is assumed
to share a region, so there's nothing for Miranda to decide, and nothing
extra to fill in when adding a `users[]` entry beyond that user's
login/password. `default_locales` is a
priority-ordered fallback list, joined with commas on the wire
(`internal/yazio/client.go`) — mirrors how every open-source YAZIO client
sends multiple locales despite the API's swagger doc showing only a
single example value.

## Testing

```bash
make test    # go test ./... -race
```

`internal/yazio`'s tests run against an `httptest.Server` fake that
answers `/oauth/token` and the product/diary endpoints
(`internal/yazio/client_test.go`, `diary_test.go`) — this is what verifies
the login → cache → reuse → refresh-on-401 flow end to end without
touching the real YAZIO API. `internal/mcpserver`'s tests use a
hand-written `fakeYazioClient` (the `YazioClient` interface exists
specifically to make this possible) rather than a mocking framework.

## Deploying

`scripts/deploy.sh` cross-compiles for `linux/amd64` and deploys as a
`systemd --user` service on port `:8790`. `config/*.yaml` and `.env`
are **never touched by deploy** — create `~/miranda-yazio/.env` by hand on
first deploy with `YAZIO_MCP_TOKEN` plus one username/password env var
pair per user configured in `config/*.yaml`'s `yazio.users` (the env var
*names* come from that user's `username_env`/`password_env`, e.g. the
default single-user setup — no `config/*.yaml` file is even required if
that default suffices):

```
YAZIO_MCP_TOKEN=<openssl rand -hex 32>
YAZIO_USERNAME_ARCHER=<archer's YAZIO login email>
YAZIO_PASSWORD_ARCHER=<archer's YAZIO password>
```

The service refuses to start if `YAZIO_MCP_TOKEN` or any configured
user's username/password env var is unset. The token cache directory
(`~/.config/yazio-mcp/`) is created automatically on first successful
login for each user — no manual setup needed there.

## What's deliberately not here

- **`countries`/`locales`/`sex` as per-user config or per-call MCP
  parameters** — every configured `yazio.users[]` account shares one
  region/locale/sex default (`yazio.default_country`/`default_locales`/
  `default_sex`), not something a tool caller decides per request or a
  household member sets individually. Revisit only if a real household
  member is actually in a different region.
- **Water intake, exercises, goals, settings, weight** — YAZIO's API
  exposes these too, but the target use case is only the food diary. Add
  a tool + `internal/yazio` method the same way `add_consumed_item` is
  built if one of these becomes needed.
- **A local product cache/database** — every `search_products`/
  `get_product` call hits YAZIO directly. Worth revisiting only if rate
  limiting (`ErrRateLimited`) becomes a real problem for actual usage
  patterns.
