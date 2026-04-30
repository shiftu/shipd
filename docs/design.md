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

## Changelog

Version order, newest first.

### v0.9 — Pluggable blob storage + S3 backend
- New `storage.BlobStore` interface decouples the metadata layer (still
  SQLite) from the bytes-on-disk layer. Two implementations live in
  `internal/storage/`: `FSBlobStore` (the existing local FS path,
  unchanged behavior) and `S3BlobStore` (`aws-sdk-go-v2`)
- Content addressing is preserved across backends: the SHA-256 is computed
  by streaming the body through a temp file (`stagedBlob`) before naming
  the destination, so the same bytes always land at the same key
- S3 backend: `PutObject` is preceded by `HeadObject` to short-circuit
  re-uploads of identical content; verified with a fake-S3 test that the
  second publish of the same bytes does NOT re-upload
- CLI flags on `shipd serve`: `--blob-backend fs|s3`, plus `--s3-bucket`,
  `--s3-region`, `--s3-endpoint` (for MinIO / R2 / OSS), `--s3-prefix`,
  `--s3-path-style`. Auth via the standard AWS SDK chain — never on the
  command line, so no secrets leak into shell history
- Tests: `FSBlobStoreRoundTrip` (round-trip + dedup),
  `S3BlobStoreRoundTripAgainstFake` (key composition, dedup short-circuit,
  byte round-trip), `S3GetMissing` (NotFound mapping)
- Binary size: +11 MB from the AWS SDK (sso/sts/cognito clients are pulled
  transitively by the default credential chain). Acceptable for now;
  build tags to drop S3 from FS-only builds is a future option

### v0.8 — Feishu WebSocket long-connection mode
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

### v0.7 — WeChat personal-account adapter via iLink
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

### v0.6 — WeChat Work adapter
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

### v0.5 — AI hooks
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

### v0.4 — Install pages
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

### v0.3 — Message gateway (stdio + Feishu webhook)
- `shipd gateway serve --adapter <stdio|feishu>` shipped
- Reuses the MCP `Registry` so chat verbs and MCP tools share one impl
- Chat verbs: `list`, `info`, `url`, `yank`, `help`
- Stdio adapter is a local REPL useful for development
- Feishu adapter implements URL-verification handshake, message events
  (`im.message.receive_v1`), tenant-token caching with 60s pre-expiry
  refresh, and reply via `im/v1/messages`

### v0.2 — MCP server
- `shipd mcp serve` exposes shipd verbs as MCP tools over stdio JSON-RPC
- Tools: `shipd_list_apps`, `shipd_list_releases`, `shipd_get_release`,
  `shipd_yank_release`, `shipd_publish`, `shipd_download_url`
- Hand-rolled JSON-RPC dispatcher (no external MCP dep), spec-compliant
  enough for Claude Desktop / Cursor

### v0.1 — Server + storage + CLI
- Server + storage + CLI for the full publish/list/info/download/yank cycle
- API token auth, SHA-256 download verification

## Roadmap

The goal of v1.0 is "good enough to run for someone else": hardening, the
gaps that today require a sysadmin to work around, and the chat-UX cliff in
the `ask` verb. Items below are roughly in ROI order — not all are required
for a 1.0 cut, but they're the next things worth touching.

### v1.0 — production-readiness

- **Streaming `ask` replies.** The `ask` verb's tool-use loop blocks silently
  for 5–30s before any text reaches the chat — looks hung. The `Asker`
  interface should grow a streaming variant that emits `calling X...` /
  partial-text updates per iteration; adapters that support message edits
  (Feishu, Slack) update in place, others fall back to the current behavior.

- **Slack adapter.** The agent-first audience overlaps far more with Slack
  than Telegram. Should reuse the WeChat Work playbook (signing-secret
  verification, async dispatch, `chat.postMessage` reply). ~300 LOC.

- **Channel promotion verb.** `releases.channel` exists in the schema but
  there's no API to promote a release between channels. Add `shipd promote
  <app>@<version> --to stable` (CLI + HTTP + MCP tool). Implementation is a
  zero-byte copy: insert a new releases row pointing at the same `blob_key`.

- **Concurrent-publish dedup.** Two clients publishing the same
  (app, version, channel) today both upload the full blob before the UNIQUE
  constraint trips one of them. Wasted S3 PUTs cost real money. Cheap fix:
  pre-check the row inside `Store.PutRelease` before opening the body
  stream; the UNIQUE constraint stays as the source-of-truth backstop.

- **Short-lived signed download URLs.** `/install/{app}/{version}/download`
  is fully public today. Once anyone fronts shipd on the open internet, that
  becomes a real privacy hole. Add an HMAC-signed query param with a short
  TTL (e.g. 10 min); install pages mint signed URLs at render time.

- **Token expiry / rotation.** Tokens never expire; this fails enterprise
  audits. Add `expires_at` to `tokens`, default-null = never, and a
  `--ttl 90d` flag on `token create`. Lookup checks expiry.

- **Blob garbage collection.** `Store.PutRelease` deliberately leaves orphan
  blobs on metadata failure (the comment is in `store.go`). Over time S3
  bills grow. Add `shipd gc --dry-run` that lists blob keys with no
  referencing release row and is older than N days; a `--delete` flag
  actually removes them. Run it from cron.

- **Basic observability.** No `/metrics`, no structured logs. A small
  `expvar` or Prometheus endpoint exposing publish/download/yank counts,
  blob-size histograms, and per-tool MCP call counts covers 80% of
  ops blindness for ~50 LOC.

### Beyond v1.0

- **Telegram adapter.** Same shape as Slack/Feishu; deferred behind Slack
  because the audience fit is weaker.
- **GCS blob backend.** Native SDK, mirroring `S3BlobStore`. Open this when
  someone actually asks for it.
- **Optional CDN integration for downloads.** Sign and redirect to a
  CloudFront / R2-public URL when configured, keeping origin behind shipd.
- **Inbound media for the WeChat iLink adapter.** Image/voice/video/file
  decode (CDN AES-128-ECB unwrap + media-upload flow). Significant work,
  only worth doing if there's user demand.
- **Streaming long-running publish.** Show progress as a reply update for
  large artifacts.

### Explicitly dropped from earlier roadmaps

- **Crash clustering / stacktrace embedding.** Would require building a
  whole `crashes` API + client SDKs + symbolication pipeline (dSYM/PDB
  uploads, etc.) — a different product. shipd's job is distribution; crash
  reporting is what Sentry / Crashlytics / Bugsnag do well already.
- **Feishu webhook encrypted-payload support (AES-256-CBC unwrap).**
  WebSocket mode shipped in v0.8 and is the default; it sidesteps any need
  for incoming-payload crypto. Maintaining a Feishu-specific AES path for
  the rarely-used webhook fallback is not worth the surface area.
- **`gocloud.dev/blob` abstraction layer.** v0.9 already covers
  S3 / R2 / MinIO / OSS via the native AWS SDK with `--s3-endpoint`. An
  extra abstraction would mean another big dependency for marginal benefit;
  if/when GCS support is needed, add it as a sibling to `S3BlobStore`
  rather than refactoring everything onto gocloud.

## Non-goals

- **Code signing.** Platform-specific and tightly regulated; we link out to
  platform tools rather than embed them.
- **Crash reporting / APM.** Stacktraces, symbolication, alerting,
  aggregation — those are Sentry / Crashlytics / Bugsnag territory. shipd
  carries release identity (sha256, version, channel) so those tools can
  attribute crashes; it does not collect them.
- **A full ticketing system.** Yank carries a reason; that's it. Use Linear
  / Jira for incidents.
- **Multi-tenancy.** A single shipd instance serves one team. Run more
  instances if you need stronger isolation.

## Naming

`shipd` = "ship daemon". Short, verb-shaped, vaguely systemd-ish — it should
feel like infrastructure you don't think about.
