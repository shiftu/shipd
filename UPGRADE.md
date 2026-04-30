# Upgrading shipd v0.9 → v1.0

This document covers what changes between v0.9 and v1.0 from an operator's
point of view: what migrates automatically, what changes behavior, and what
might bite a deployment that has been running v0.9 in production.

The short version: **drop in the new binary, restart, you're done.** All
schema and data migrations run automatically and are idempotent. Existing
tokens, releases, blobs, and install URLs continue to work. The only
behavior change worth knowing about is install-page download URL signing,
which is on by default with a 30-minute TTL — see "If you upgrade and
something breaks" below.

## What migrates automatically

On the first start of a v1.0 binary against a v0.9 data directory:

- **SQLite schema** gains three columns via idempotent `ALTER TABLE` in
  `internal/storage/store.go::migrate`:
  - `releases.yanked_at INTEGER NOT NULL DEFAULT 0` — used by `shipd gc`
    to filter by yank age. Existing yanked rows get `yanked_at = 0`,
    which `GCCandidates` treats as "the dawn of time", i.e., always
    eligible if the operator opts into a 0-day window.
  - `tokens.expires_at INTEGER NOT NULL DEFAULT 0` — `0` means "never
    expires", which is what every pre-v1.0 token gets. Their auth
    behavior is unchanged.
  - (cleanup of pre-existing migrations: `bundle_id`, `display_name`,
    `platform` already migrated in v0.4 / v0.9.)
- **Blob layout** is unchanged. Existing `<dataDir>/blobs/<sha[:2]>/<sha[2:]>`
  files (or S3 objects under `<prefix>/<sha[:2]>/<sha[2:]>`) are read
  directly by the new code; no rewriting, no relocation.
- **A new file appears** at `<dataDir>/install_url_secret` (mode 0600):
  the HMAC key for signed install URLs, auto-generated on first start.
  Add this to your data-dir backups; restoring without it invalidates
  in-flight signed URLs (users who tap "install" within their TTL get
  403'd; they recover by reloading the install page).

Verified end-to-end: a v0.9 data dir with apps, releases (live + yanked),
tokens, and content-addressed blobs upgraded to v1.0 with zero data loss.
The `yanked_reason` of a pre-upgrade yank is preserved verbatim; download
URLs with API-level auth (`/api/v1/.../download`) return byte-identical
artifacts.

## Behavior changes worth knowing about

### Install download URLs are signed by default (30-minute TTL)

The biggest change. `--install-url-ttl` defaults to `30m`, so:

- `GET /install/{app}/{version}/download` and `/manifest.plist` now
  require an HMAC `?exp=&sig=` query string.
- The HTML install page (`/install/{app}` or `/install/{app}/{version}`)
  remains public; on every render it mints fresh signed URLs and
  embeds them in the install button + plist.
- Direct accesses without a valid signature:
  - `download` → **303 redirect** to the install page with `?expired=1`
    (the page renders an amber banner; one tap re-mints).
  - `manifest.plist` → **410 Gone** with a plain-text message. iOS can't
    render an HTML redirect, so an iOS user whose tap landed past TTL
    has to reload the install page manually.

**Who this breaks:** scripts or docs that hardcoded
`https://your-host/install/{app}/{version}/download` as a permanent
direct URL. `curl -L` follows the redirect and ends up with the install
page HTML instead of an IPA. Two mitigations:

- **Recommended:** switch the script to the API path
  `/api/v1/apps/{name}/releases/{version}/download` with an
  `X-Auth-Token` header — that endpoint never signed and never will.
- **Quick revert:** start with `--install-url-ttl=0` to fully disable
  signing and restore v0.9 behavior. Public install routes are then
  exactly as they were.

**Multi-replica deployments:** if you run shipd on more than one host
behind a load balancer, set `--install-url-secret` (or
`$SHIPD_INSTALL_URL_SECRET`) to a shared 32-byte hex value across all
replicas — otherwise each replica auto-generates its own secret and
signatures don't validate across the fleet.

### `shipd token list` adds an `EXPIRES` column

```
NAME    SCOPE  CREATED           EXPIRES  LAST_USED      ← v1.0
NAME    SCOPE  CREATED           LAST_USED               ← v0.9
```

Pre-existing tokens render `EXPIRES = never`. Shell scripts that parse
by header name keep working; scripts that index columns by position
need to bump the LAST_USED column index. The token plaintext on
`shipd token create` stdout is unchanged — the
`SHIPD_BOOTSTRAP_TOKEN=$(shipd token create ...)` pattern still works.

### `/install/{app}` (no version) is now multi-platform-aware

If you publish only iOS, no visible change. If you publish both iOS and
Android under the same app name, `/install/zendiac` now serves the
platform-appropriate primary based on the visitor's User-Agent (iOS UA
→ ios primary, Android UA → android primary, desktop → alphabetical
fallback) and lists the others under a small "Also available" section.

This fixes a genuine v0.9 bug where the latest-uploaded release won
regardless of platform — iOS users had no way to reach the iOS install
page after an Android release was published.

### Bootstrap-via-env-var creates an admin token now

When `$SHIPD_BOOTSTRAP_TOKEN` is set AND no tokens exist in the DB, the
auto-created token has scope `admin` (was `rw` in v0.9). This only
affects fresh installs; **existing token rows are not modified by the
upgrade.**

If your existing bootstrap token is `rw` and you want to use the new
admin endpoints (`/api/v1/admin/gc`, `/api/v1/admin/tokens`,
`/metrics`), either:

- create a new admin token: `shipd token create ops --scope admin --data-dir ./data`
- or upgrade the existing one: `sqlite3 ./data/meta.db "UPDATE tokens SET scope='admin' WHERE name='bootstrap'"`

The `requireRead` and `requireWrite` checks on every existing endpoint
treat `admin` as "rw or better", so an admin token can do everything
an rw token can do plus the new admin operations.

### The `shipd publish` happy path is faster (and cheaper on S3)

When a re-publish hits the same `(app, version, channel)` triple, v1.0
short-circuits with `409 Conflict` before reading the request body, so
a CI job retrying a publish no longer pays the upload cost twice. The
response shape is unchanged; CI scripts already handling `409` work
without modification.

## Breaking changes that don't apply to most operators

- **Go toolchain bump 1.24 → 1.25.** Only matters if you build shipd
  from source. The Dockerfile is updated to `golang:1.25-alpine`.
- **`server.New` Go API now returns `(*Server, error)`.** Internal-only;
  the only caller is `internal/cli/serve.go`, which is updated.
- **`BlobStore` interface gains `Delete(ctx, key) error`.** Internal
  Go API; only `FSBlobStore` and `S3BlobStore` implement it, both in
  the same package.
- **`Asker.Ask` and `DispatchFn` gain a stream callback parameter.**
  Internal Go API for the gateway adapter contract. All in-tree
  adapters and tests are updated.
- **`Store.CreateToken` validates scope against a whitelist.** Anyone
  manually inserting tokens with arbitrary scope strings via raw SQL
  was already in degenerate territory; now `r`, `rw`, and `admin`
  are the only accepted values.

## What you can use that's new

- `shipd promote <app>@<version> --to <channel>` — beta → stable
  without re-uploading bytes.
- `shipd gc [--delete] [--older-than 30d] [--keep-last 1]` — reclaim
  storage from yanked releases. Run on a cron.
- `shipd unyank <app>@<version>` — recovery path for `gc --keep-last`.
- `GET /metrics` (admin scope) — Prometheus exposition format. 16
  metric families covering catalog gauges, publish/yank/download
  counts, gc activity, auth failures.
- `GET /api/v1/stats` (rw scope) — the JSON sibling of `/metrics` for
  programmatic consumers.
- `shipd_stats` MCP tool and `stats` chat verb — same data as
  `/api/v1/stats`, formatted as a compact text summary.
- Slack adapter via Socket Mode (no public webhook URL needed).
- Streamed `ask` progress in chat — every intermediate model turn
  surfaces "📡 calling shipd_X" and any commentary the model wrote,
  so users see the agent thinking instead of staring at a frozen
  prompt for 10–30 seconds.

## Rollback

If you need to roll back from v1.0 to v0.9:

1. Stop v1.0.
2. Start the v0.9 binary against the same data dir.

v0.9 SELECTs against the new columns (yanked_at, expires_at) ignore
them harmlessly. v0.9 INSERTs into `releases` and `tokens` don't
specify the new columns, so they take their `DEFAULT 0` values — no
schema-level rejection. Tokens with `expires_at > 0` would be
re-honored as live (v0.9 doesn't check expiry), so an expired token
could come back to life under a rollback; this is a security
consideration but not a data-integrity one.

The `install_url_secret` file is harmless to v0.9, which doesn't read
it. Signed URLs minted under v1.0 will all fail under v0.9 (which
returns the file body without checking the signature), so there's no
broken-link surface from rollback.
