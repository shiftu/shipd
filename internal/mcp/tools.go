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

// RegisterShipdTools wires the shipd HTTP client up as a set of MCP tools.
// All tools are namespaced with the "shipd_" prefix so they don't collide with
// other servers a user has loaded.
func RegisterShipdTools(s *Server, c *client.Client, baseURL string) {
	s.Register(&listAppsTool{c: c})
	s.Register(&listReleasesTool{c: c})
	s.Register(&getReleaseTool{c: c})
	s.Register(&yankReleaseTool{c: c})
	s.Register(&publishTool{c: c})
	s.Register(&downloadURLTool{c: c, baseURL: strings.TrimRight(baseURL, "/")})
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

// --- publish ---

// The MCP server runs locally with the agent, so it can read files from the
// agent's filesystem and stream them up to the (possibly remote) shipd server.
type publishTool struct{ c *client.Client }

func (t *publishTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "shipd_publish",
		Description: "Upload a build artifact to shipd. The agent must provide a local file path; the file is streamed to the server. " +
			"App name and platform are inferred from the filename when not provided.",
		InputSchema: schema(map[string]any{
			"file_path": strProp("Absolute path to the artifact (.ipa, .apk, .dmg, etc.)."),
			"version":   strProp("Release version, e.g. '1.2.3'."),
			"app":       strProp("App name (default: inferred from filename)."),
			"channel":   strProp("Release channel (default: stable)."),
			"platform":  strProp("Platform (default: inferred from extension)."),
			"notes":     strProp("Release notes."),
		}, "file_path", "version"),
	}
}

func (t *publishTool) Call(ctx context.Context, raw json.RawMessage) (*CallToolResult, error) {
	var args struct {
		FilePath string `json:"file_path"`
		Version  string `json:"version"`
		App      string `json:"app"`
		Channel  string `json:"channel"`
		Platform string `json:"platform"`
		Notes    string `json:"notes"`
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
		App:      args.App,
		Version:  args.Version,
		Channel:  args.Channel,
		Platform: args.Platform,
		Notes:    args.Notes,
		Filename: base,
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

