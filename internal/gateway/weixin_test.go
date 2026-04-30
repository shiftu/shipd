package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestRandomWeChatUIN verifies the X-WECHAT-UIN format Hermes uses:
// base64 of the decimal-string representation of a random uint32.
func TestRandomWeChatUIN(t *testing.T) {
	for i := 0; i < 32; i++ {
		v := randomWeChatUIN()
		raw, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			t.Fatalf("not valid base64: %q", v)
		}
		if _, err := strconv.ParseUint(string(raw), 10, 32); err != nil {
			t.Errorf("decoded UIN %q is not a uint32 decimal: %v", string(raw), err)
		}
	}
}

// TestWrapWithBaseInfo confirms every iLink request body carries the
// channel_version envelope. The merge must NOT clobber existing keys at the
// top level — Tencent's endpoints care about both base_info and the payload
// fields being present.
func TestWrapWithBaseInfo(t *testing.T) {
	body, err := wrapWithBaseInfo(map[string]any{"foo": "bar"})
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["foo"] != "bar" {
		t.Errorf("foo lost in wrap")
	}
	bi, ok := m["base_info"].(map[string]any)
	if !ok {
		t.Fatalf("base_info missing or wrong type: %T", m["base_info"])
	}
	if bi["channel_version"] != iLinkChannelVersion {
		t.Errorf("channel_version mismatch: %v", bi["channel_version"])
	}
}

// TestILinkPostHeaders pins the headers an authenticated post sends. These
// have to match what Tencent's iLink expects byte-for-byte; a regression
// here is silent in tests but breaks real auth.
func TestILinkPostHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"ret":0}`))
	}))
	defer srv.Close()

	cli := newILinkClient(srv.URL, "tok-abc")
	if err := cli.post(context.Background(), "ilink/bot/test", map[string]any{}, nil); err != nil {
		t.Fatalf("post: %v", err)
	}
	checks := map[string]string{
		"Content-Type":            "application/json",
		"Authorization":           "Bearer tok-abc",
		"Authorizationtype":       "ilink_bot_token",
		"Ilink-App-Id":            iLinkAppID,
		"Ilink-App-Clientversion": strconv.Itoa(iLinkAppClientVersion),
	}
	for k, want := range checks {
		if got.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, got.Get(k), want)
		}
	}
	if got.Get("X-Wechat-Uin") == "" {
		t.Error("X-WECHAT-UIN header missing")
	}
	if got.Get("Content-Length") == "" {
		t.Error("Content-Length not set")
	}
}

// TestExtractWeixinTextPicksTextThenVoice mirrors the precedence Hermes uses:
// any text item wins; voice transcripts are a fallback so ASR-only messages
// still reach the router.
func TestExtractWeixinTextPicksTextThenVoice(t *testing.T) {
	cases := []struct {
		name  string
		items []weixinItem
		want  string
	}{
		{
			name: "text wins",
			items: []weixinItem{
				{Type: wxItemVoice, VoiceItem: struct {
					Text string `json:"text"`
				}{Text: "voice transcript"}},
				{Type: wxItemText, TextItem: struct {
					Text string `json:"text"`
				}{Text: "hello"}},
			},
			want: "hello",
		},
		{
			name: "voice fallback",
			items: []weixinItem{
				{Type: wxItemVoice, VoiceItem: struct {
					Text string `json:"text"`
				}{Text: "ASR result"}},
			},
			want: "ASR result",
		},
		{
			name:  "empty",
			items: []weixinItem{{Type: wxItemImage}},
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractWeixinText(tc.items); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSyncBufRoundTrip ensures persisting and reloading the long-poll
// cursor preserves the value. The cursor is the only piece of state that
// matters for resuming after a restart.
func TestSyncBufRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := saveSyncBuf(dir, "acct1", "cursor-abc"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := loadSyncBuf(dir, "acct1"); got != "cursor-abc" {
		t.Errorf("got %q, want cursor-abc", got)
	}
	// Missing file → empty string, not an error.
	if got := loadSyncBuf(dir, "no-such-acct"); got != "" {
		t.Errorf("expected empty for missing file, got %q", got)
	}
}

// TestSaveLoadAccountRoundTrip verifies we persist with 0600 perms and that
// LoadWeixinAccount round-trips the credentials.
func TestSaveLoadAccountRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &WeixinAccount{AccountID: "acct1", Token: "tok-abc", BaseURL: "https://example", UserID: "u1"}
	if err := SaveWeixinAccount(dir, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "acct1.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perms = %o, want 0600", perm)
	}
	got, err := LoadWeixinAccount(dir, "acct1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Token != want.Token || got.BaseURL != want.BaseURL || got.UserID != want.UserID {
		t.Errorf("round-trip mismatch:\nwant %+v\ngot  %+v", want, got)
	}
}

// TestAdapterContextTokenEcho confirms the adapter echoes the latest
// per-peer context_token back on the next outbound message — Tencent
// enforces this after the first turn.
func TestAdapterContextTokenEcho(t *testing.T) {
	var lastSendBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, iLinkEPSendMessage) {
			lastSendBody = body
		}
		_, _ = w.Write([]byte(`{"ret":0}`))
	}))
	defer srv.Close()

	a, err := NewWeixinAdapter(WeixinConfig{AccountID: "a", Token: "t", BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Simulate having received a message from peer "u1" with a context_token.
	raw, _ := json.Marshal(map[string]any{
		"from_user_id":  "u1",
		"context_token": "ctx-xyz",
		"item_list":     []map[string]any{{"type": wxItemText, "text_item": map[string]any{"text": "ping"}}},
	})
	a.handleInbound(context.Background(), func(_ context.Context, _ Message, _ func(string)) Reply {
		return Reply{Text: "pong"}
	}, raw)

	if lastSendBody == nil {
		t.Fatal("no send observed")
	}
	var sent map[string]any
	if err := json.Unmarshal(lastSendBody, &sent); err != nil {
		t.Fatalf("decode send body: %v", err)
	}
	msg, ok := sent["msg"].(map[string]any)
	if !ok {
		t.Fatalf("send body missing msg: %s", string(lastSendBody))
	}
	if msg["context_token"] != "ctx-xyz" {
		t.Errorf("context_token not echoed: got %v", msg["context_token"])
	}
	if msg["to_user_id"] != "u1" {
		t.Errorf("wrong to_user_id: %v", msg["to_user_id"])
	}
}
