# shipd

> AI-native package distribution. CLI-first, agent-friendly, single binary.

`shipd` is a build-artifact distribution platform designed for the LLM-agent
era. Where traditional platforms (app-space, zealot, fir.im) put a Web UI
first, `shipd` is built around a CLI and a JSON API, with planned MCP and
message-gateway frontends so that **an Agent can publish, list, query, yank,
and roll back releases as fluently as a human can**.

## Status

MVP scaffold. Working today:

- ✅ Single-binary server (`shipd serve`) — SQLite metadata + content-addressed blob storage
- ✅ CLI: `publish`, `list`, `info`, `download`, `yank`, `token`
- ✅ API token auth (write tokens vs. public reads)
- ✅ SHA-256 verification on download
- ✅ MCP server (`shipd mcp serve`) — exposes 6 tools to any MCP client
- ✅ Message gateway: stdio REPL + Feishu adapter (`shipd gateway serve`)
- ✅ Install pages with QR codes + iOS plist (`/install/{app}/{version}`)
- ✅ AI release notes (`shipd publish --ai-notes`)
- ✅ Free-form `ask` verb on the gateway (LLM picks tools to answer)
- 🚧 Message gateway: WeChat-Work / Slack / Telegram adapters (planned)
- 🚧 S3 / R2 / OSS blob backends (planned)

## Quick start

```bash
# build
make build

# create a data dir + bootstrap token
SHIPD_BOOTSTRAP_TOKEN=$(./shipd token create bootstrap --data-dir ./data 2>/dev/null) \
  ./shipd serve --data-dir ./data --addr :8080 &

# point the CLI at it
export SHIPD_SERVER=http://localhost:8080
export SHIPD_TOKEN=<the token printed above>

# publish
./shipd publish ./mybuild.ipa --version 1.0.0 --notes "first release" \
  --bundle-id com.example.mybuild --display-name "My Build"

# list
./shipd list
./shipd list mybuild

# inspect the latest
./shipd info mybuild

# download by version (sha256 verified)
./shipd download mybuild@1.0.0 -o ./out

# pull a release
./shipd yank mybuild@1.0.0 --reason "crash on iOS 18"

# generate release notes from git log via Claude
export ANTHROPIC_API_KEY=sk-ant-...
./shipd publish ./mybuild.ipa --version 1.1.0 --ai-notes
```

## Install pages

`shipd serve` exposes public install pages for each release:

```
/install/{app}                       latest non-yanked release (HTML)
/install/{app}/{version}             specific release (HTML)
/install/{app}/{version}/manifest.plist   iOS install manifest
/install/{app}/{version}/download    direct artifact (no token required)
```

The HTML page renders a platform-aware install button (`itms-services://` for
iOS, direct download for Android and others) and a QR code so a desktop user
can scan from a phone. iOS releases need `--bundle-id` set on publish; the
plist endpoint returns `422` with an actionable error otherwise.

For correct install URLs behind TLS termination, set `--public-base-url
https://your-host` (or `$SHIPD_PUBLIC_BASE_URL`) on `shipd serve`. Without it,
shipd auto-detects from `Host` and `X-Forwarded-Proto`.

These routes are intentionally PUBLIC — a phone scanning a QR code has no
token. Front shipd with a reverse proxy if you need to gate them.

## Using shipd from an Agent (MCP)

`shipd mcp serve` runs an [MCP](https://modelcontextprotocol.io) server on
stdio. Wire it into Claude Desktop, Cursor, or any MCP-compatible host:

```json
{
  "mcpServers": {
    "shipd": {
      "command": "/usr/local/bin/shipd",
      "args": ["mcp", "serve", "--server", "https://shipd.example.com"],
      "env": { "SHIPD_TOKEN": "shipd_..." }
    }
  }
}
```

The agent then sees these tools:

| Tool | Purpose |
|---|---|
| `shipd_list_apps` | enumerate apps |
| `shipd_list_releases` | releases for one app, newest first |
| `shipd_get_release` | release metadata (latest if no version) |
| `shipd_publish` | upload a local file as a new release |
| `shipd_download_url` | direct download URL for a release |
| `shipd_yank_release` | mark a release as withdrawn |

## Using shipd from chat (Gateway)

`shipd gateway serve` runs an adapter that turns chat messages into the same
tool calls. Two adapters today:

### Local REPL (development)

```bash
shipd gateway serve --adapter stdio
shipd> list
shipd> info myapp
shipd> yank myapp@1.0.0 reason="crash on iOS 18"
```

### Feishu / Lark

```bash
shipd gateway serve --adapter feishu --addr :8081 \
  --feishu-app-id $FEISHU_APP_ID \
  --feishu-app-secret $FEISHU_APP_SECRET \
  --feishu-verification-token $FEISHU_VERIFICATION_TOKEN
```

Then in the Feishu Open Platform:
1. Set the event subscription URL to `https://your-host:8081/feishu/event`
2. Subscribe to `im.message.receive_v1`
3. Add the bot to a group; chat with it: `@bot list`, `@bot info myapp`,
   `@bot yank myapp@1.0.0 reason="crash"`

Chat verbs (same as the stdio REPL):

| Verb | Behavior |
|---|---|
| `list` | list all apps |
| `list <app>` | list releases for an app |
| `info <app>[@<version>]` | release metadata, latest by default |
| `url <app>[@<version>]` | direct download URL |
| `yank <app>@<version> [reason="..."]` | withdraw a release |
| `ask <question...>` | free-form question; an LLM picks tools to answer (requires `$ANTHROPIC_API_KEY` on the gateway server) |
| `help` | show this list |

## Why another distribution platform?

The existing self-hosted options (app-space, zealot, fir.im, significa) were
built for humans clicking buttons in 2017–2020. None of them treat an Agent
as a first-class user. `shipd`'s differentiation is:

| | app-space | zealot | significa | **shipd** |
|---|---|---|---|---|
| CLI is a first-class interface | ❌ | fastlane plugin | ❌ | ✅ |
| MCP server | ❌ | ❌ | ❌ | ✅ |
| Message gateway (Feishu/WeChat/Slack) | ❌ | ❌ | ❌ | 🚧 |
| AI-generated release notes | ❌ | ❌ | ❌ | 🚧 |
| Single binary deployment | ❌ | ❌ | ✅ | ✅ |

## Architecture

```
       CLI ─────┐
       MCP ─────┼──► HTTP API ──► Storage ──► SQLite (meta)
   Gateway ─────┘                          ╰► Filesystem / S3 (blobs)
```

- **Storage**: blobs are content-addressed (SHA-256), so an identical artifact
  uploaded twice is deduplicated for free.
- **Auth**: tokens are SHA-256 hashed at rest; the plaintext is shown once on
  creation and never recoverable.
- **Single binary**: no CGO, statically linked, deployable as a distroless
  container or a plain executable.

## Layout

```
cmd/shipd/         entry point
internal/cli/      cobra subcommands (CLI surface)
internal/client/   tiny Go SDK used by the CLI
internal/server/   HTTP server + handlers + auth middleware + install pages
internal/storage/  SQLite metadata + blob filesystem
internal/mcp/      JSON-RPC stdio MCP server + tool registry + shipd tools
internal/gateway/  chat-message router + stdio + Feishu adapters
internal/ai/       Anthropic API client, release-notes generator, tool-use agent
internal/pkginfo/  artifact platform detection + app-name inference
docs/              design notes
```

## API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/healthz` | — | liveness |
| GET | `/api/v1/apps` | read | list apps |
| GET | `/api/v1/apps/{name}` | read | app metadata |
| GET | `/api/v1/apps/{name}/releases` | read | list releases |
| GET | `/api/v1/apps/{name}/releases/{version}` | read | release metadata |
| GET | `/api/v1/apps/{name}/releases/{version}/download` | read | download blob |
| GET | `/api/v1/apps/{name}/latest` | read | latest non-yanked release |
| POST | `/api/v1/apps/{name}/releases?version=...` | write | publish |
| POST | `/api/v1/apps/{name}/releases/{version}/yank` | write | mark yanked |
| GET | `/install/{name}` / `/install/{name}/{version}` | **public** | install page (HTML) |
| GET | `/install/{name}/{version}/manifest.plist` | **public** | iOS install manifest |
| GET | `/install/{name}/{version}/download` | **public** | direct artifact |

Token goes in `X-Auth-Token` or `Authorization: Bearer ...`.

## Roadmap

See [docs/design.md](docs/design.md) for the full design rationale and the
6-week roadmap toward MCP + message-gateway support.

## License

GPL-3.0 — see [LICENSE](LICENSE).
