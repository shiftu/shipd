# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build / test / run

```bash
make build       # CGO_ENABLED=0 go build → ./shipd (single static binary)
make run         # build then ./shipd serve --data-dir ./data
make test        # go test ./...
make tidy        # go mod tidy
make docker      # docker build -t shipd:$VERSION .
```

Run a single test or package:

```bash
go test ./internal/storage -run TestFSBlobStoreRoundTrip
go test ./internal/gateway -v
```

There is no separate lint/format step in the Makefile — use `go vet ./...` and `gofmt -w` directly. Module is Go 1.24; **CGO must stay disabled** (the codebase deliberately uses `modernc.org/sqlite`, a pure-Go SQLite, so the binary stays single-file and statically linkable).

## High-level architecture

shipd is a **single binary** that exposes the same release-management surface through three frontends — CLI, MCP (over stdio), and chat gateway (Feishu / WeChat Work / WeChat personal / stdio REPL) — and one HTTP API. The three frontends are deliberately thin; they all funnel into the same storage layer and tool registry.

```
       CLI ─────┐
       MCP ─────┼──► HTTP API ──► storage.Store ──► SQLite (meta)
   Gateway ─────┘                                ╰► BlobStore: FS | S3
```

Layout:

```
cmd/shipd/         entry point (delegates to internal/cli.NewRoot)
internal/cli/      cobra subcommands — root.go wires them all
internal/client/   tiny Go SDK; CLI and MCP tools both call through it
internal/server/   HTTP handlers + auth middleware + install pages (Go std lib net/http)
internal/storage/  SQLite metadata (store.go) + BlobStore interface (blob.go, s3.go)
internal/mcp/      JSON-RPC stdio MCP server + Registry + shipd tool implementations
internal/gateway/  Adapter pattern over chat platforms; reuses the MCP Registry
internal/ai/       hand-rolled Anthropic /v1/messages client + tool-use agent + release-notes generator
internal/pkginfo/  filename → platform / app-name inference
```

### Two cross-cutting patterns to know before editing

**1. The MCP `Registry` is shared between MCP and the gateway.** Adding a tool in `internal/mcp/tools.go` (via `RegisterShipdTools`) automatically exposes it to:
   - MCP clients (Claude Desktop, Cursor) via `shipd mcp serve`
   - Chat users via `shipd gateway serve` — the gateway's `Router` parses chat verbs into `tool.Call` invocations against the same registry
   - The free-form `ask` LLM agent in `internal/ai/agent.go` — it builds Anthropic `Tool` definitions from `Registry.List()`, so any registered MCP tool is callable by Claude
   
   When adding a new shipd verb, register one MCP tool and add a chat alias in `internal/gateway/parser.go::chatAliases` — do not duplicate handler logic.

**2. The `BlobStore` interface owns content addressing.** Every blob is keyed by SHA-256, computed by streaming the body through `stagedBlob()` (in `internal/storage/blob.go`) before the backend sees the bytes. Any new backend MUST hash via `stagedBlob` so two uploads of identical content produce the same key (dedup) and so the metadata layer's invariant holds. The metadata layer (`Store`) does not know whether bytes live on disk or in S3; it just calls `blobs.Put` / `blobs.Get`.

### Smaller architectural notes

- **Storage atomicity.** A failed metadata write leaves the orphan blob behind on purpose: a future `PutRelease` of the same content collapses onto the same content-addressed key, so cleanup costs (especially S3 round-trips) outweigh the benefit. See `Store.PutRelease`.
- **Schema migrations** are idempotent `ALTER TABLE` statements in `storage.migrate()`. Add a new column by appending one statement; "duplicate column" errors are tolerated so older DBs upgrade in place.
- **HTTP routing** uses Go 1.22+ `ServeMux` pattern syntax (`GET /api/v1/apps/{name}`). Auth is enforced by `requireRead` / `requireWrite` middleware; install pages (`/install/...`) are deliberately **public** so a phone scanning a QR code can fetch them without a token. Gate them with a reverse proxy if you need privacy.
- **Tokens** are random 24-byte URL-safe strings prefixed `shipd_`, hashed with SHA-256 at rest. `shipd token` subcommands (create/list/revoke) talk to the local SQLite directly via `storage.Open` — they are intended to be run on the server host, not over the API.
- **Anthropic client is hand-rolled** (`internal/ai/anthropic.go`) — no Go SDK dependency. Supports prompt caching (`CacheControl{Type:"ephemeral"}`) and tool-use blocks. Default model is `claude-sonnet-4-6`; configurable via `--ai-model`. The agent loop (`internal/ai/agent.go`) caps at 8 iterations and 1024 output tokens per call to defend against runaway models.
- **Gateway adapters** are ~150-300 LOC each. The `Adapter` interface is intentionally small: `Run(ctx, dispatch DispatchFn) error`. New transports plug in by implementing it and adding a case in `cli/gateway.go::buildAdapter`. The Feishu adapter has two transports selected by `--feishu-mode` (websocket via `lark-oapi-sdk-go`, default; or webhook). The WeChat personal adapter (`weixin_ilink.go`) targets a **reverse-engineered** Tencent endpoint and may break without notice.
- **iOS install URLs** use `itms-services://`. The `installPageData.InstallURL` field is typed `template.URL` to bypass `html/template`'s scheme allowlist (otherwise `itms-services` is rewritten to `#ZgotmplZ`). Don't change that type.

## Common workflow

```bash
# Bootstrap a token + run the server
SHIPD_BOOTSTRAP_TOKEN=$(./shipd token create bootstrap --data-dir ./data 2>/dev/null) \
  ./shipd serve --data-dir ./data --addr :8080 &
export SHIPD_SERVER=http://localhost:8080
export SHIPD_TOKEN=<the printed token>

./shipd publish ./build.ipa --version 1.0.0 --bundle-id com.example.app
./shipd list
./shipd info myapp
./shipd yank myapp@1.0.0 --reason "crash on iOS 18"
```

S3 backend (production): `--blob-backend s3 --s3-bucket NAME --s3-region REGION` (plus `--s3-endpoint` and `--s3-path-style` for MinIO / R2 / OSS). Credentials come from the AWS SDK chain — never pass them on the command line.

## Design reference

`docs/design.md` contains the full design rationale, data model, auth model, and roadmap. When adding a feature that crosses multiple layers (e.g. a new gateway adapter, a new storage backend), skim that doc first — the non-goals section is load-bearing.
