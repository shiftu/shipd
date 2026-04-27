# shipd — design notes

## Premise

The self-hosted app-distribution category is full of products built for humans
clicking buttons in 2017–2020. None of them treat an **LLM Agent** as a
first-class user. As more software-delivery work flows through agents
(Claude Code, Cursor, Devin, internal copilots), this becomes a real gap.

`shipd` is a bet on an "AI-native" distribution platform whose primary user is
an Agent and whose secondary user is a human reaching the same Agent through
chat.

## Design principles

1. **CLI and API are the product.** The Web UI, if any, is a viewer, not the
   source of truth. Anything you can do in the UI must be doable in one shell
   command.
2. **Agents are first-class.** An MCP server exposes every CLI verb as a tool.
   Tokens are scoped per agent. Every error returns a structured JSON body
   with a stable shape so an Agent can reason about it.
3. **Humans reach Agents via chat.** A message gateway plugs Feishu / WeChat
   Work / Slack / Telegram into the same command surface, so `@bot publish
   ./build.ipa` is identical in semantics to running the CLI locally.
4. **Single binary, zero ceremony.** SQLite + filesystem in dev, S3 in prod —
   the binary is the same. `docker run shipd` works on day one.
5. **Boring storage, careful integrity.** Content-addressed blobs (SHA-256),
   verified on download, deduplicated by hash. Metadata in SQLite for atomic
   writes; no Postgres dependency until you outgrow a single host.

## Architecture

```
                           Humans                    Agents
                              │                         │
                  ┌───────────┴────────────┐ ┌──────────┴───────────┐
                  │ Message Gateway        │ │ MCP server           │
                  │ (Feishu/WeChat/Slack)  │ │ (publish/list/yank)  │
                  └───────────┬────────────┘ └──────────┬───────────┘
                              │                         │
                              └────────────┬────────────┘
                                           │
                                  ┌────────▼─────────┐
                                  │   HTTP API       │
                                  │   (Go std lib)   │
                                  └────────┬─────────┘
                                           │
                              ┌────────────┼────────────┐
                              │            │            │
                          ┌───▼───┐    ┌───▼────┐   ┌───▼───┐
                          │ blobs │    │ SQLite │   │  AI   │
                          │  FS/S3│    │  meta  │   │ hooks │
                          └───────┘    └────────┘   └───────┘
```

## Data model

```sql
apps(name PK, platform, created_at)
releases(app_name, version, channel, blob_key, size, sha256, filename,
         notes, yanked, yanked_reason, created_at,
         PRIMARY KEY(app_name, version, channel))
tokens(name PK, hash UNIQUE, scope, created_at, last_used_at)
```

Notes:

- `blob_key` = SHA-256 hex. Two uploads with identical bytes share storage.
- `(app, version, channel)` is the natural identity of a release. The same
  version can exist on `stable` and `beta` independently — useful for staged
  rollouts.
- `yanked` is a soft-delete: the blob and metadata stay, but `latest` skips
  the row. We never break a published download URL silently.

## Auth model

- Tokens are random 24-byte URL-safe strings, prefixed `shipd_`.
- Stored as SHA-256 hashes at rest. Plaintext is shown once on creation.
- Two scopes: `r` (read) and `rw` (default). Reads can be made public via
  `--public-reads`.
- Token admin (`shipd token create/list/revoke`) talks to the local SQLite
  directly, intentionally — the operator runs it on the server host.

## Roadmap

### Done (v0.1)
- Server + storage + CLI for the full publish/list/info/download/yank cycle
- API token auth, SHA-256 download verification

### Done (v0.2 → bumped: MCP shipped before install pages)
- `shipd mcp serve` exposes shipd verbs as MCP tools over stdio JSON-RPC
- Tools: `shipd_list_apps`, `shipd_list_releases`, `shipd_get_release`,
  `shipd_yank_release`, `shipd_publish`, `shipd_download_url`
- Hand-rolled JSON-RPC dispatcher (no external MCP dep), spec-compliant
  enough for Claude Desktop / Cursor

### Done (v0.3 — Message gateway)
- `shipd gateway serve --adapter <stdio|feishu>` shipped
- Reuses the MCP `Registry` so chat verbs and MCP tools share one impl
- Chat verbs: `list`, `info`, `url`, `yank`, `help`
- Stdio adapter is a local REPL useful for development
- Feishu adapter implements URL-verification handshake, message events
  (`im.message.receive_v1`), tenant-token caching with 60s pre-expiry
  refresh, and reply via `im/v1/messages`
- Encrypted Feishu payloads, Slack, WeChat-Work, Telegram are deferred —
  the Adapter interface is small enough that a new transport is ~150 LOC

### Done (v0.4 — Install pages)
- `/install/{app}` and `/install/{app}/{version}` render an HTML page
  with a platform-appropriate install button and an inline QR code
- `/install/{app}/{version}/manifest.plist` returns a valid iOS install
  manifest; releases without `bundle_id` get a 422 with an actionable error
- `/install/{app}/{version}/download` streams the artifact bytes without a
  token, so iOS itms-services and direct browser installs work
- All `/install/...` routes are intentionally public; gate with a reverse
  proxy if you need privacy
- Schema changes: `bundle_id`, `display_name`, and `platform` columns on
  `releases`, with idempotent `ALTER TABLE` migrations so existing DBs
  upgrade automatically
- Optional download tokens (short-lived URL-signed downloads) deferred
  until there is a real privacy use case

### v0.5 — More gateway adapters
- WeChat Work, Slack, Telegram
- Feishu encrypted payload support (AES-256-CBC unwrap)
- Streaming: long-running commands stream output back as message updates
- Optional LLM mode: free-form `@bot ask "..."` runs through an Agent with
  shipd's MCP tools available

### v0.6 — AI hooks
- `--ai-notes`: pull `git log` since the last release, generate structured
  release notes with Claude
- Crash clustering: if a `crash report` API is added, embed stacktraces and
  cluster
- Natural-language query: "which version had the highest crash rate last
  week?" → SQL/aggregate

### v0.7 — Cloud storage
- S3 / R2 / OSS / GCS blob backends via gocloud.dev/blob
- Optional CDN integration for download endpoints

## Non-goals

- **Code signing.** This is platform-specific and tightly regulated; we link
  out to platform tools rather than embed them.
- **A full ticketing system.** Yank carries a reason; that's it. Use Linear/
  Jira for incidents.
- **Multi-tenancy.** A single shipd instance serves one team. Run more
  instances if you need stronger isolation.

## Naming

`shipd` = "ship daemon". Short, verb-shaped, vaguely systemd-ish — it should
feel like infrastructure you don't think about.
