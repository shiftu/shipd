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
- ✅ Message gateway: stdio + Feishu + WeChat-Work + WeChat-personal (iLink) adapters
- ✅ Install pages with QR codes + iOS plist (`/install/{app}/{version}`)
- ✅ AI release notes (`shipd publish --ai-notes`)
- ✅ Free-form `ask` verb on the gateway (LLM picks tools to answer)
- ✅ Pluggable blob backend: filesystem (default) or S3-compatible (AWS / MinIO / R2 / OSS)
- 🚧 Message gateway: Slack / Telegram adapters (planned)

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

# storage backends — fs is the default, point at S3 for production:
./shipd serve --blob-backend s3 --s3-bucket my-shipd \
              --s3-region us-east-1
# MinIO / R2 / OSS — set --s3-endpoint and --s3-path-style:
./shipd serve --blob-backend s3 --s3-bucket my-shipd \
              --s3-endpoint https://minio.example.com --s3-path-style

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

Two transports — pick by `--feishu-mode`:

#### WebSocket long-connection (default, no public URL needed)

```bash
shipd gateway serve --adapter feishu \
  --feishu-app-id     $FEISHU_APP_ID \
  --feishu-app-secret $FEISHU_APP_SECRET
```

shipd dials `open.feishu.cn` over outbound HTTPS, holds a long connection,
and receives events through it. Auto-reconnects on transient drops (provided
by `lark-oapi-sdk-go`). This is what Hermes Agent uses by default — no
public webhook URL, no SSL termination.

In the Feishu Open Platform:
1. Enable "事件订阅" → "长连接 (WebSocket)" mode
2. Subscribe to `im.message.receive_v1`
3. Add the bot to a chat — `@bot list`, `@bot info myapp`, etc.

#### Webhook (when you have a public URL anyway)

```bash
shipd gateway serve --adapter feishu --feishu-mode webhook --addr :8081 \
  --feishu-app-id              $FEISHU_APP_ID \
  --feishu-app-secret          $FEISHU_APP_SECRET \
  --feishu-verification-token  $FEISHU_VERIFICATION_TOKEN
```

Set the event subscription URL to `https://your-host:8081/feishu/event` and
subscribe to `im.message.receive_v1`. Encrypted payloads are not supported
in this build.

### WeChat (personal account) via iLink

> ⚠️ **iLink is reverse-engineered.** This adapter targets Tencent's iLink
> bot endpoint (`ilinkai.weixin.qq.com`), the same protocol Hermes Agent
> uses. It is not an officially documented public API and may stop working
> without notice. Operating personal-account bots may also conflict with
> WeChat's terms of service in some jurisdictions — only use this adapter
> with explicit user consent.

```bash
# 1. Interactive QR login (one time per account).
#    Prints a URL and an ASCII QR; scan with WeChat to authorize.
shipd gateway weixin-login --state-dir ./data/weixin

# saved ./data/weixin/<account_id>.json — start the gateway with --weixin-account-id <account_id>

# 2. Run the adapter.
shipd gateway serve --adapter weixin \
  --weixin-account-id <account_id> \
  --weixin-state-dir   ./data/weixin
```

Behavior:
- Long-polls iLink's `getupdates` (35-second windows) for new messages
- Per-peer `context_token` is tracked in memory and echoed on replies
- Long-poll cursor (`get_updates_buf`) is persisted so restarts don't replay
- Session expiry (errcode `-14`) → 10-minute pause and re-poll; if it persists,
  re-run `weixin-login` to refresh the bot token
- Text only in v1; image/voice/video/file inbound are silently dropped

### WeChat Work / 企业微信 (扫码接入)

```bash
shipd gateway serve --adapter wechat-work --addr :8082 \
  --wxwork-corp-id     $WXWORK_CORP_ID \
  --wxwork-agent-id    $WXWORK_AGENT_ID \
  --wxwork-secret      $WXWORK_SECRET \
  --wxwork-token       $WXWORK_TOKEN \
  --wxwork-aes-key     $WXWORK_ENCODING_AES_KEY
```

Then in 企业微信管理后台 → 应用管理 → your app → 接收消息:

1. Set 接收消息的 URL to `https://your-host:8082/wxwork/event`
2. Use the same Token and EncodingAESKey you passed to shipd
3. Encryption mode must be **安全模式** (encrypted)

Onboarding (the QR-code flow):

- Open `https://your-host:8082/wxwork/onboard` in any browser
- A QR code is shown — fetched live from Tencent and cached for 30 minutes
- Scan it with the **企业微信 App** to enter the bot's chat
- Send `help`, `info myapp`, or `ask 最近发布了什么？`

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
   Gateway ─────┘                          ╰► BlobStore: FS | S3-compatible
```

- **Storage**: blobs are content-addressed (SHA-256), so an identical artifact
  uploaded twice is deduplicated for free. The S3 backend uses `HeadObject`
  before upload to skip redundant network transfer for the same content.
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
internal/gateway/  chat-message router + stdio + Feishu + WeChat-Work + Weixin adapters
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
