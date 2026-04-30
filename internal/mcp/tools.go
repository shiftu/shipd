package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/shiftu/shipd/internal/client"
	"github.com/shiftu/shipd/internal/pkginfo"
)

// RegisterShipdTools wires the shipd HTTP client up as a set of tools on the
// given registry. All tools are namespaced with the "shipd_" prefix so they
// don't collide with other servers a user has loaded.
func RegisterShipdTools(r *Registry, c *client.Client, baseURL string) {
	r.Register(&listAppsTool{c: c})
	r.Register(&listReleasesTool{c: c})
	r.Register(&getReleaseTool{c: c})
	r.Register(&yankReleaseTool{c: c})
	r.Register(&unyankReleaseTool{c: c})
	r.Register(&promoteReleaseTool{c: c})
	r.Register(&publishTool{c: c})
	r.Register(&downloadURLTool{c: c, baseURL: strings.TrimRight(baseURL, "/")})
	r.Register(&statsTool{c: c})
	// Admin tools: gc and token creation. They require an admin-scope token
	// on the shipd server; the MCP host typically passes one explicitly.
	r.Register(&gcTool{c: c})
	r.Register(&createTokenTool{c: c})
}

// schema is a tiny helper to keep tool schema declarations readable.
func schema(properties map[string]any, required ...string) map[string]any {
	out := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// jsonResult marshals v into a pretty JSON text result the LLM can read.
func jsonResult(v any) *CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return textError(err.Error())
	}
	return textResult(string(b))
}

// --- list_apps ---

type listAppsTool struct{ c *client.Client }

func (t *listAppsTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "shipd_list_apps",
		Description: "List all apps known to the shipd server.",
		InputSchema: schema(map[string]any{}),
	}
}

func (t *listAppsTool) Call(ctx context.Context, _ json.RawMessage) (*CallToolResult, error) {
	apps, err := t.c.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	return jsonResult(apps), nil
}

// --- list_releases ---

type listReleasesTool struct{ c *client.Client }

func (t *listReleasesTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "shipd_list_releases",
		Description: "List all releases for an app, newest first. Includes yanked releases.",
		InputSchema: schema(map[string]any{
			"app": strProp("App name."),
		}, "app"),
	}
}

func (t *listReleasesTool) Call(ctx context.Context, raw json.RawMessage) (*CallToolResult, error) {
	var args struct {
		App string `json:"app"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.App == "" {
		return textError("app is required"), nil
	}
	rels, err := t.c.ListReleases(ctx, args.App)
	if err != nil {
		return nil, err
	}
	return jsonResult(rels), nil
}

// --- get_release ---

type getReleaseTool struct{ c *client.Client }

func (t *getReleaseTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "shipd_get_release",
		Description: "Fetch metadata for a specific release. If 'version' is omitted, returns the latest non-yanked release on the channel.",
		InputSchema: schema(map[string]any{
			"app":     strProp("App name."),
			"version": strProp("Release version. Omit for latest."),
			"channel": strProp("Release channel (default: stable)."),
		}, "app"),
	}
}

func (t *getReleaseTool) Call(ctx context.Context, raw json.RawMessage) (*CallToolResult, error) {
	var args struct {
		App     string `json:"app"`
		Version string `json:"version"`
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.App == "" {
		return textError("app is required"), nil
	}
	if args.Version == "" {
		rel, err := t.c.Latest(ctx, args.App, args.Channel)
		if err != nil {
			return nil, err
		}
		return jsonResult(rel), nil
	}
	rel, err := t.c.GetRelease(ctx, args.App, args.Version, args.Channel)
	if err != nil {
		return nil, err
	}
	return jsonResult(rel), nil
}

// --- yank_release ---

type yankReleaseTool struct{ c *client.Client }

func (t *yankReleaseTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "shipd_yank_release",
		Description: "Mark a published release as withdrawn so it stops appearing as 'latest'. The blob is preserved; existing pinned downloads continue to work.",
		InputSchema: schema(map[string]any{
			"app":     strProp("App name."),
			"version": strProp("Release version."),
			"channel": strProp("Release channel (default: stable)."),
			"reason":  strProp("Human-readable reason."),
		}, "app", "version"),
	}
}

func (t *yankReleaseTool) Call(ctx context.Context, raw json.RawMessage) (*CallToolResult, error) {
	var args struct {
		App     string `json:"app"`
		Version string `json:"version"`
		Channel string `json:"channel"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.App == "" || args.Version == "" {
		return textError("app and version are required"), nil
	}
	if err := t.c.Yank(ctx, args.App, args.Version, args.Channel, args.Reason); err != nil {
		return nil, err
	}
	return textResult(fmt.Sprintf("yanked %s@%s", args.App, args.Version)), nil
}

// --- unyank_release ---

type unyankReleaseTool struct{ c *client.Client }

func (t *unyankReleaseTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "shipd_unyank_release",
		Description: "Reverse a yank — bring a withdrawn release back to live status. " +
			"Useful when a yanked release's bytes were preserved by gc's --keep-last safety net " +
			"and the operator wants to re-enable installs without re-publishing. Idempotent: " +
			"calling on a non-yanked release succeeds.",
		InputSchema: schema(map[string]any{
			"app":     strProp("App name."),
			"version": strProp("Release version."),
			"channel": strProp("Release channel (default: stable)."),
		}, "app", "version"),
	}
}

func (t *unyankReleaseTool) Call(ctx context.Context, raw json.RawMessage) (*CallToolResult, error) {
	var args struct {
		App     string `json:"app"`
		Version string `json:"version"`
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.App == "" || args.Version == "" {
		return textError("app and version are required"), nil
	}
	if err := t.c.Unyank(ctx, args.App, args.Version, args.Channel); err != nil {
		return nil, err
	}
	return textResult(fmt.Sprintf("unyanked %s@%s", args.App, args.Version)), nil
}

// --- stats ---

type statsTool struct{ c *client.Client }

func (t *statsTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "shipd_stats",
		Description: "Show shipd's runtime stats — catalog gauges (apps, releases, disk usage) " +
			"plus operational counters since the server started (publishes, downloads, yanks, " +
			"auth failures, gc runs). Useful for 'is shipd healthy?' or 'how active is this " +
			"team's release flow?'. Requires rw scope; matches the JSON shape served at " +
			"GET /api/v1/stats but rendered as compact text for direct chat display.",
		InputSchema: schema(map[string]any{}),
	}
}

func (t *statsTool) Call(ctx context.Context, _ json.RawMessage) (*CallToolResult, error) {
	s, err := t.c.Stats(ctx)
	if err != nil {
		return nil, err
	}
	return textResult(formatStats(s)), nil
}

// formatStats renders a Stats snapshot as a compact human-readable summary.
// Both LLMs (via the MCP tool) and chat users (via the gateway) read the
// same output — the format keeps the line-per-fact shape so an agent can
// extract values without parsing prose.
func formatStats(s *client.Stats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apps              %d\n", s.Apps)
	fmt.Fprintf(&b, "releases          %d live, %d yanked\n", s.ReleasesLive, s.ReleasesYanked)
	fmt.Fprintf(&b, "storage           %s (after dedup)\n", humanBytes(s.BlobBytes))
	fmt.Fprintf(&b, "tokens            %d active\n", s.TokensActive)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "since server start:")
	fmt.Fprintf(&b, "  publish         ok=%d conflict=%d error=%d\n", s.PublishOK, s.PublishConflict, s.PublishError)
	fmt.Fprintf(&b, "  yank/unyank     %d / %d\n", s.Yank, s.Unyank)
	fmt.Fprintf(&b, "  promote         %d\n", s.Promote)
	fmt.Fprintf(&b, "  download        api=%d install=%d\n", s.DownloadAPI, s.DownloadInstall)
	fmt.Fprintf(&b, "  install_page    %d renders, %d sig failures\n", s.InstallPageRenders, s.InstallSigFail)
	fmt.Fprintf(&b, "  gc              dry-run=%d delete=%d (rows=%d, blobs=%d)\n",
		s.GCDryRunRuns, s.GCDeleteRuns, s.GCRowsDeleted, s.GCBlobsDeleted)
	fmt.Fprintf(&b, "  tokens_created  %d\n", s.TokensCreated)
	fmt.Fprintf(&b, "  auth_failure    invalid=%d expired=%d forbidden=%d\n",
		s.AuthInvalid, s.AuthExpired, s.AuthForbidden)
	return strings.TrimRight(b.String(), "\n")
}

// humanBytes renders a byte count as a small KiB/MiB/GiB string. Mirrors
// internal/cli/util.go's humanSize but kept local so the mcp package
// doesn't reach into cli.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), []string{"KiB", "MiB", "GiB", "TiB"}[exp])
}

// --- gc ---

type gcTool struct{ c *client.Client }

func (t *gcTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "shipd_gc",
		Description: "Reclaim storage from yanked releases. By default runs in dry-run mode and " +
			"returns the candidates that WOULD be deleted; pass delete=true to actually remove " +
			"the metadata rows and free the backing blobs. Requires an admin-scope token on the " +
			"shipd server. The --keep-last safety net (default 1) protects the most-recent " +
			"release per (app, channel, platform) regardless of yank state.",
		InputSchema: schema(map[string]any{
			"older_than": strProp("Minimum age since yank, e.g. 30d, 4w, 12h. '0' disables the age filter. Default: 30d."),
			"keep_last":  map[string]any{"type": "integer", "description": "Releases per (app, channel, platform) to protect regardless of yank state. Default 1; pass 0 for full cleanup."},
			"delete":     map[string]any{"type": "boolean", "description": "If true, actually delete. Default false (dry-run)."},
		}),
	}
}

func (t *gcTool) Call(ctx context.Context, raw json.RawMessage) (*CallToolResult, error) {
	var args struct {
		OlderThan string `json:"older_than"`
		KeepLast  *int   `json:"keep_last"`
		Delete    bool   `json:"delete"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	keep := 1
	if args.KeepLast != nil {
		if *args.KeepLast < 0 {
			return textError("keep_last must be >= 0"), nil
		}
		keep = *args.KeepLast
	}
	res, err := t.c.GC(ctx, args.OlderThan, keep, args.Delete)
	if err != nil {
		return nil, err
	}
	return jsonResult(res), nil
}

// --- create_token ---

type createTokenTool struct{ c *client.Client }

func (t *createTokenTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "shipd_create_token",
		Description: "Mint a new shipd auth token. Requires an admin-scope token on the shipd " +
			"server. The plaintext value is returned in the result and is shown only once — " +
			"surface it to the operator immediately and store it securely. Use this to provision " +
			"scoped tokens for CI jobs (rw), sub-agents (r), or other admins (admin).",
		InputSchema: schema(map[string]any{
			"name":  strProp("Unique token name (visible to operators in `shipd token list`)."),
			"scope": strProp("Token scope: r (read-only) | rw (read+write, default) | admin (gc + token mgmt)."),
			"ttl":   strProp("Token lifetime, e.g. 90d, 12h, 4w. Empty/omitted = never expires."),
		}, "name"),
	}
}

func (t *createTokenTool) Call(ctx context.Context, raw json.RawMessage) (*CallToolResult, error) {
	var args struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
		TTL   string `json:"ttl"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.Name == "" {
		return textError("name is required"), nil
	}
	res, err := t.c.CreateToken(ctx, args.Name, args.Scope, args.TTL)
	if err != nil {
		return nil, err
	}
	return jsonResult(res), nil
}

// --- promote_release ---

type promoteReleaseTool struct{ c *client.Client }

func (t *promoteReleaseTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "shipd_promote_release",
		Description: "Copy a release onto another channel (e.g. beta → stable) without re-uploading bytes. " +
			"The destination row points at the same content-addressed blob as the source, so this is fast " +
			"and cheap. If 'from_channel' is omitted, the version must exist on exactly one channel.",
		InputSchema: schema(map[string]any{
			"app":          strProp("App name."),
			"version":      strProp("Release version to promote."),
			"to_channel":   strProp("Destination channel (e.g. stable)."),
			"from_channel": strProp("Source channel. Omit to auto-detect when the version is on exactly one channel."),
		}, "app", "version", "to_channel"),
	}
}

func (t *promoteReleaseTool) Call(ctx context.Context, raw json.RawMessage) (*CallToolResult, error) {
	var args struct {
		App         string `json:"app"`
		Version     string `json:"version"`
		ToChannel   string `json:"to_channel"`
		FromChannel string `json:"from_channel"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.App == "" || args.Version == "" || args.ToChannel == "" {
		return textError("app, version, and to_channel are required"), nil
	}
	rel, err := t.c.Promote(ctx, args.App, args.Version, args.FromChannel, args.ToChannel)
	if err != nil {
		return nil, err
	}
	return jsonResult(rel), nil
}

// --- publish ---

// The MCP server runs locally with the agent, so it can read files from the
// agent's filesystem and stream them up to the (possibly remote) shipd server.
type publishTool struct{ c *client.Client }

func (t *publishTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "shipd_publish",
		Description: "Upload a build artifact to shipd. The agent must provide a local file path; the file is streamed to the server. " +
			"App name and platform are inferred from the filename when not provided. " +
			"For iOS install pages to work, set bundle_id to the artifact's CFBundleIdentifier.",
		InputSchema: schema(map[string]any{
			"file_path":    strProp("Absolute path to the artifact (.ipa, .apk, .dmg, etc.)."),
			"version":      strProp("Release version, e.g. '1.2.3'."),
			"app":          strProp("App name (default: inferred from filename)."),
			"channel":      strProp("Release channel (default: stable)."),
			"platform":     strProp("Platform (default: inferred from extension)."),
			"notes":        strProp("Release notes."),
			"bundle_id":    strProp("iOS CFBundleIdentifier (required for itms-services install)."),
			"display_name": strProp("Human-readable title shown on install pages."),
		}, "file_path", "version"),
	}
}

func (t *publishTool) Call(ctx context.Context, raw json.RawMessage) (*CallToolResult, error) {
	var args struct {
		FilePath    string `json:"file_path"`
		Version     string `json:"version"`
		App         string `json:"app"`
		Channel     string `json:"channel"`
		Platform    string `json:"platform"`
		Notes       string `json:"notes"`
		BundleID    string `json:"bundle_id"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.FilePath == "" || args.Version == "" {
		return textError("file_path and version are required"), nil
	}
	f, err := os.Open(args.FilePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	base := filepath.Base(args.FilePath)
	if args.App == "" {
		args.App = pkginfo.InferAppName(base)
	}
	if args.Platform == "" {
		args.Platform = string(pkginfo.Detect(args.FilePath))
	}
	rel, err := t.c.Publish(ctx, client.PublishOpts{
		App:         args.App,
		Version:     args.Version,
		Channel:     args.Channel,
		Platform:    args.Platform,
		Notes:       args.Notes,
		Filename:    base,
		BundleID:    args.BundleID,
		DisplayName: args.DisplayName,
	}, f, info.Size())
	if err != nil {
		return nil, err
	}
	return jsonResult(rel), nil
}

// --- download_url ---

// Returning a URL rather than streaming bytes keeps the tool result small and
// lets the agent decide what to do with the artifact (download, hand off to a
// human, etc.).
type downloadURLTool struct {
	c       *client.Client
	baseURL string
}

func (t *downloadURLTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "shipd_download_url",
		Description: "Return a direct download URL for a release. If 'version' is omitted, the latest non-yanked release is used. The URL requires the same auth token unless the server has --public-reads.",
		InputSchema: schema(map[string]any{
			"app":     strProp("App name."),
			"version": strProp("Release version. Omit for latest."),
			"channel": strProp("Release channel (default: stable)."),
		}, "app"),
	}
}

func (t *downloadURLTool) Call(ctx context.Context, raw json.RawMessage) (*CallToolResult, error) {
	var args struct {
		App     string `json:"app"`
		Version string `json:"version"`
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.App == "" {
		return textError("app is required"), nil
	}
	version := args.Version
	if version == "" {
		latest, err := t.c.Latest(ctx, args.App, args.Channel)
		if err != nil {
			return nil, err
		}
		version = latest.Version
	}
	u := t.baseURL + "/api/v1/apps/" + url.PathEscape(args.App) +
		"/releases/" + url.PathEscape(version) + "/download"
	if args.Channel != "" {
		u += "?channel=" + url.QueryEscape(args.Channel)
	}
	return textResult(u), nil
}

