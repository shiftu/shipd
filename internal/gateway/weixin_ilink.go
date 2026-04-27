package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// iLink (https://ilinkai.weixin.qq.com) is Tencent's bot-protocol entry point
// for personal WeChat accounts. It is NOT an officially documented public API
// — the surface here is reverse-engineered from observed traffic and from
// public reference implementations (notably Hermes Agent's weixin.py). Treat
// this adapter as best-effort: header layouts and endpoint paths can change
// without notice, and this connection mode may not be ToS-compliant in every
// jurisdiction. Use it only with explicit user consent.

const (
	iLinkBaseURL          = "https://ilinkai.weixin.qq.com"
	iLinkChannelVersion   = "2.2.0"
	iLinkAppID            = "bot"
	iLinkAppClientVersion = (2 << 16) | (2 << 8) | 0 // 131584

	iLinkEPGetBotQR    = "ilink/bot/get_bot_qrcode"
	iLinkEPQRStatus    = "ilink/bot/get_qrcode_status"
	iLinkEPGetUpdates  = "ilink/bot/getupdates"
	iLinkEPSendMessage = "ilink/bot/sendmessage"

	iLinkLongPollTimeout = 35 * time.Second
	iLinkAPITimeout      = 15 * time.Second
)

// iLinkClient is a thin HTTP wrapper handling the auth headers, the
// base_info envelope wrap, and timeout/error normalization. The methods that
// poll for updates and send messages live as free functions below — keeping
// the client minimal makes it easy to stub out in tests via httptest.
type iLinkClient struct {
	baseURL string
	token   string // bearer token; empty before QR login confirms
	http    *http.Client
}

func newILinkClient(baseURL, token string) *iLinkClient {
	if baseURL == "" {
		baseURL = iLinkBaseURL
	}
	return &iLinkClient{
		baseURL: baseURL,
		token:   token,
		// One generous transport timeout. Per-call timeouts are passed via ctx
		// so long-poll requests can run longer than the default.
		http: &http.Client{Timeout: 0},
	}
}

// iLinkHeaders builds the request headers iLink expects. The X-WECHAT-UIN
// value is a per-request random — Hermes uses base64(decimal(random_uint32)),
// which we mirror exactly.
func iLinkHeaders(token string, contentLength int) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(contentLength))
	h.Set("AuthorizationType", "ilink_bot_token")
	h.Set("X-WECHAT-UIN", randomWeChatUIN())
	h.Set("iLink-App-Id", iLinkAppID)
	h.Set("iLink-App-ClientVersion", strconv.Itoa(iLinkAppClientVersion))
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h
}

func randomWeChatUIN() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	n := binary.BigEndian.Uint32(b[:])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(n), 10)))
}

// iLinkResp is the common envelope every endpoint returns. Endpoint-specific
// fields live alongside it via json.RawMessage so callers can decode further.
type iLinkResp struct {
	Ret     int    `json:"ret"`
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// iLinkPost wraps payload with base_info, posts to the endpoint, and decodes
// into out. Pass nil for out when you don't care about the response body
// beyond the error envelope.
func (c *iLinkClient) post(ctx context.Context, endpoint string, payload, out any) error {
	body, err := wrapWithBaseInfo(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/"+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = iLinkHeaders(c.token, len(body))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ilink %s: HTTP %d: %s", endpoint, resp.StatusCode, truncate(string(raw), 200))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("ilink %s: decode: %w (body=%s)", endpoint, err, truncate(string(raw), 200))
		}
	}
	return nil
}

// get is the unauthenticated path used during QR login. Same headers minus
// the bearer token; no body wrap (it's a GET).
func (c *iLinkClient) get(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("iLink-App-Id", iLinkAppID)
	req.Header.Set("iLink-App-ClientVersion", strconv.Itoa(iLinkAppClientVersion))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ilink %s: HTTP %d: %s", endpoint, resp.StatusCode, truncate(string(raw), 200))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("ilink %s: decode: %w", endpoint, err)
		}
	}
	return nil
}

// wrapWithBaseInfo merges payload with the always-required base_info field.
// Implemented as a marshal-then-merge so callers can pass either map[string]any
// or any other JSON-marshalable struct.
func wrapWithBaseInfo(payload any) ([]byte, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("payload must be a JSON object, got: %w", err)
	}
	m["base_info"] = map[string]any{"channel_version": iLinkChannelVersion}
	return json.Marshal(m)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
