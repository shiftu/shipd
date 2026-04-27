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

### Done (v0.5 — AI hooks)
- `shipd publish --ai-notes`: pulls `git log <prev-version-tag>..HEAD`,
  sends to Claude with a cacheable system prompt, fills in the release
  notes. `--ai-since` overrides the auto-detected previous version.
- Free-form `ask <text>` verb on the gateway: the message is handed to a
  tool-use agent that has the same six tools the MCP server exposes.
  When `ANTHROPIC_API_KEY` is unset on the gateway server, `ask` replies
  with a clear "not enabled" message instead of hanging.
- Hand-rolled Anthropic /v1/messages client (no Go SDK dependency); ~150
  LOC, supports prompt caching and tool-use blocks. Default model is
  Sonnet 4.6; configurable via `--ai-model`.
- Bounded loop: agent caps at 8 tool-use iterations per Ask call to
  defend against runaway models, and 1024 output tokens per call to keep
  chat replies short.

### Done (v0.8 — Feishu WebSocket long-connection mode)
- `shipd gateway serve --adapter feishu` now defaults to `--feishu-mode
  websocket`, mirroring Hermes Agent's default
- Long-connection path uses `github.com/larksuite/oapi-sdk-go/v3/ws` —
  the official Lark Go SDK handles dial, auth, heartbeat, the proprietary
  protobuf-framed event stream, and auto-reconnect
- Inbound events go through `dispatcher.OnP2MessageReceiveV1`; the
  webhook path is preserved as `--feishu-mode webhook`
- Replies still go via the IM REST API (`Im.V1.Message.Create`) — Lark's
  WS channel only carries inbound, even when both directions look "live"
- Operationally this removes shipd's hard dependency on a public-reachable
  HTTPS endpoint and SSL termination for Feishu — outbound HTTPS is enough
- Binary size: +2.1 MB from the SDK pull (gogo/protobuf, gorilla/websocket,
  lark service models). Acceptable for the UX win

### Done (v0.7 — WeChat personal-account adapter via iLink)
- `shipd gateway weixin-login` runs an interactive QR login (ASCII QR
  rendered in the terminal) against Tencent's iLink endpoint and persists
  the resulting `bot_token` to `<state-dir>/<account_id>.json` (mode 0600)
- `shipd gateway serve --adapter weixin` long-polls `getupdates` for 35s
  windows, dispatches text messages through the same Router/Asker the
  other adapters use, and replies via `sendmessage` echoing the per-peer
  `context_token` Tencent requires after the first turn
- Long-poll cursor (`get_updates_buf`) is persisted so a restart resumes
  from the previous boundary instead of replaying messages
- Session expiry (errcode `-14`) triggers a 10-minute pause; the operator
  re-runs `weixin-login` to refresh the bot token
- Crypto-light v1: text only. Image/voice/video/file inbound are silently
  dropped — wiring them in requires CDN AES-128-ECB decrypt + media
  upload flow that's a separate slice
- Reverse-engineered protocol: documented prominently in the help text
  and the README that this is best-effort and not guaranteed ToS-compliant

### Done (v0.6 — WeChat Work adapter)
- `shipd gateway serve --adapter wechat-work` shipped
- Implements the full WeChat Work app callback contract: SHA1 msg_signature
  verification, AES-256-CBC message decryption with PKCS#7 unpadding,
  corp_id check on every payload, fast 200-ack with async dispatch
- Replies via the active `/cgi-bin/message/send` API; access tokens are
  cached with 60s pre-expiry refresh
- "Scan to chat" onboarding: `/wxwork/qrcode` proxies the app's QR image
  from Tencent (cached 30 min); `/wxwork/onboard` renders a Chinese
  HTML page embedding the QR + scan instructions for non-developers
- Hand-rolled crypto with round-trip and signature tests passing; no
  external crypto deps

### v0.7 — More AI
- Crash clustering: if a `crash report` API is added, embed stacktraces
  and cluster.
- Natural-language query: "which version had the highest crash rate last
  week?" → tool-use agent with a SQL aggregation tool.
- Stream tool-use loops back into the chat as live updates so a user
  sees "calling shipd_list_releases..." during long ask sessions.

### v0.8 — Cloud storage
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
