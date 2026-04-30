// Package client is a tiny HTTP SDK used by the CLI (and any Go consumer)
// to talk to a shipd server.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/shiftu/shipd/internal/storage"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    http.DefaultClient,
	}
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("shipd: %d: %s", e.Status, e.Message) }

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.Token != "" {
		req.Header.Set("X-Auth-Token", c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		msg := body.Error
		if msg == "" {
			msg = resp.Status
		}
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}
	return resp, nil
}

// --- read ops ---

func (c *Client) ListApps(ctx context.Context) ([]storage.App, error) {
	resp, err := c.get(ctx, "/api/v1/apps", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Apps []storage.App `json:"apps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Apps, nil
}

func (c *Client) GetApp(ctx context.Context, name string) (*storage.App, error) {
	resp, err := c.get(ctx, "/api/v1/apps/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var a storage.App
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (c *Client) ListReleases(ctx context.Context, app string) ([]storage.Release, error) {
	resp, err := c.get(ctx, "/api/v1/apps/"+url.PathEscape(app)+"/releases", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Releases []storage.Release `json:"releases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Releases, nil
}

func (c *Client) GetRelease(ctx context.Context, app, version, channel string) (*storage.Release, error) {
	q := url.Values{}
	if channel != "" {
		q.Set("channel", channel)
	}
	resp, err := c.get(ctx, "/api/v1/apps/"+url.PathEscape(app)+"/releases/"+url.PathEscape(version), q)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return decodeRelease(resp.Body)
}

func (c *Client) Latest(ctx context.Context, app, channel string) (*storage.Release, error) {
	q := url.Values{}
	if channel != "" {
		q.Set("channel", channel)
	}
	resp, err := c.get(ctx, "/api/v1/apps/"+url.PathEscape(app)+"/latest", q)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return decodeRelease(resp.Body)
}

// Download streams the release blob into w. It returns the release metadata
// from response headers.
func (c *Client) Download(ctx context.Context, app, version, channel string, w io.Writer) (sha256, filename string, n int64, err error) {
	q := url.Values{}
	if channel != "" {
		q.Set("channel", channel)
	}
	resp, err := c.get(ctx, "/api/v1/apps/"+url.PathEscape(app)+"/releases/"+url.PathEscape(version)+"/download", q)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	sha256 = resp.Header.Get("X-Content-SHA256")
	filename = parseFilename(resp.Header.Get("Content-Disposition"))
	n, err = io.Copy(w, resp.Body)
	return sha256, filename, n, err
}

// --- write ops ---

type PublishOpts struct {
	App         string
	Version     string
	Channel     string
	Platform    string
	Notes       string
	Filename    string
	BundleID    string // iOS CFBundleIdentifier — required for itms-services install
	DisplayName string // human-readable title shown on the install page
}

// Publish streams body to the server. ContentLength may be 0 for chunked uploads.
func (c *Client) Publish(ctx context.Context, opts PublishOpts, body io.Reader, contentLength int64) (*storage.Release, error) {
	if opts.App == "" || opts.Version == "" {
		return nil, errors.New("app and version are required")
	}
	q := url.Values{}
	q.Set("version", opts.Version)
	if opts.Channel != "" {
		q.Set("channel", opts.Channel)
	}
	if opts.Platform != "" {
		q.Set("platform", opts.Platform)
	}
	if opts.Notes != "" {
		q.Set("notes", opts.Notes)
	}
	if opts.Filename != "" {
		q.Set("filename", opts.Filename)
	}
	if opts.BundleID != "" {
		q.Set("bundle_id", opts.BundleID)
	}
	if opts.DisplayName != "" {
		q.Set("display_name", opts.DisplayName)
	}
	u := c.BaseURL + "/api/v1/apps/" + url.PathEscape(opts.App) + "/releases?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if contentLength > 0 {
		req.ContentLength = contentLength
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return decodeRelease(resp.Body)
}

// Promote copies a release onto toChannel without re-uploading the blob.
// fromChannel may be empty to auto-detect, in which case the version must
// exist on exactly one channel.
func (c *Client) Promote(ctx context.Context, app, version, fromChannel, toChannel string) (*storage.Release, error) {
	if toChannel == "" {
		return nil, errors.New("toChannel is required")
	}
	q := url.Values{}
	q.Set("to", toChannel)
	if fromChannel != "" {
		q.Set("from", fromChannel)
	}
	u := c.BaseURL + "/api/v1/apps/" + url.PathEscape(app) + "/releases/" + url.PathEscape(version) + "/promote?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return decodeRelease(resp.Body)
}

func (c *Client) Yank(ctx context.Context, app, version, channel, reason string) error {
	q := url.Values{}
	if channel != "" {
		q.Set("channel", channel)
	}
	if reason != "" {
		q.Set("reason", reason)
	}
	u := c.BaseURL + "/api/v1/apps/" + url.PathEscape(app) + "/releases/" + url.PathEscape(version) + "/yank?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// --- helpers ---

func (c *Client) get(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func decodeRelease(r io.Reader) (*storage.Release, error) {
	var rel storage.Release
	if err := json.NewDecoder(r).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func parseFilename(contentDisposition string) string {
	const k = `filename=`
	i := strings.Index(contentDisposition, k)
	if i < 0 {
		return ""
	}
	v := contentDisposition[i+len(k):]
	v = strings.TrimSpace(v)
	v = strings.Trim(v, `"`)
	return v
}
