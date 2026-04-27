package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// FeishuConfig is the credentials/tuning bundle a Feishu adapter needs.
//
// AppID and AppSecret come from the Feishu Open Platform app dashboard.
// VerificationToken comes from the same dashboard's "Event Subscription" tab
// and is the value compared against header.token on every incoming event in
// webhook mode (ignored in websocket mode).
//
// Mode selects the transport:
//
//	"websocket" — long connection out to open.feishu.cn (default; no public
//	              webhook URL needed). Mirrors Hermes Agent's default.
//	"webhook"   — HTTP receiver on Addr, requires a public-reachable URL.
//
// Encrypted webhook payloads are NOT supported. Leave encryption disabled
// in your Feishu app's event-subscription config when using webhook mode.
type FeishuConfig struct {
	Mode              string // "websocket" (default) | "webhook"
	Addr              string // webhook listen address (webhook mode only)
	AppID             string
	AppSecret         string
	VerificationToken string // webhook mode only
	BaseURL           string // override for testing; defaults to https://open.feishu.cn
}

// FeishuAdapter implements Adapter against the Feishu Open Platform.
//
// The HTTP route is fixed: /feishu/event receives webhook events, including
// the URL-verification handshake when the user sets up the subscription.
type FeishuAdapter struct {
	cfg FeishuConfig
	log *log.Logger

	tokenMu   sync.Mutex
	tokenVal  string
	tokenExpA time.Time
}

func NewFeishuAdapter(cfg FeishuConfig, logger *log.Logger) *FeishuAdapter {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://open.feishu.cn"
	}
	if logger == nil {
		logger = log.Default()
	}
	return &FeishuAdapter{cfg: cfg, log: logger}
}

func (f *FeishuAdapter) Name() string { return "feishu" }

// Run dispatches based on the configured mode. websocket is the default — it
// matches the Hermes Agent default and doesn't require a public callback URL.
func (f *FeishuAdapter) Run(ctx context.Context, dispatch DispatchFn) error {
	mode := f.cfg.Mode
	if mode == "" {
		mode = "websocket"
	}
	switch mode {
	case "websocket":
		return f.runWebSocket(ctx, dispatch)
	case "webhook":
		return f.runWebhook(ctx, dispatch)
	default:
		return fmt.Errorf("feishu: unknown mode %q (want websocket or webhook)", mode)
	}
}

// runWebhook is the original HTTP-receiver path, kept available for users
// whose deployments terminate inbound webhooks through a reverse proxy.
func (f *FeishuAdapter) runWebhook(ctx context.Context, dispatch DispatchFn) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /feishu/event", f.handleEvent(ctx, dispatch))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	hs := &http.Server{Addr: f.cfg.Addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = hs.Shutdown(context.Background())
	}()
	f.log.Printf("feishu adapter listening on %s (mode=webhook)", f.cfg.Addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// --- event handling ---

// feishuEventEnvelope is the outer shape Feishu posts. URL-verification events
// use the older flat shape (Type/Challenge/Token at the top level); message
// events use the nested v2 schema (Header/Event).
type feishuEventEnvelope struct {
	// v1 url_verification fields
	Type      string `json:"type,omitempty"`
	Challenge string `json:"challenge,omitempty"`
	Token     string `json:"token,omitempty"`

	// v2 nested fields
	Schema string          `json:"schema,omitempty"`
	Header *feishuHeader   `json:"header,omitempty"`
	Event  json.RawMessage `json:"event,omitempty"`
}

type feishuHeader struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	Token      string `json:"token"`
	AppID      string `json:"app_id"`
	CreateTime string `json:"create_time"`
}

type feishuMessageEvent struct {
	Sender struct {
		SenderID struct {
			OpenID string `json:"open_id"`
			UserID string `json:"user_id"`
		} `json:"sender_id"`
	} `json:"sender"`
	Message struct {
		MessageID   string `json:"message_id"`
		ChatID      string `json:"chat_id"`
		MessageType string `json:"message_type"`
		Content     string `json:"content"` // JSON-encoded string
		Mentions    []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"mentions"`
	} `json:"message"`
}

func (f *FeishuAdapter) handleEvent(ctx context.Context, dispatch DispatchFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var env feishuEventEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		// URL verification handshake on subscription setup.
		if env.Type == "url_verification" {
			if !f.verifyToken(env.Token) {
				http.Error(w, "bad token", http.StatusForbidden)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"challenge": env.Challenge})
			return
		}

		// v2 events.
		if env.Header == nil {
			http.Error(w, "unrecognized payload", http.StatusBadRequest)
			return
		}
		if !f.verifyToken(env.Header.Token) {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		// ACK quickly so Feishu doesn't retry.
		writeJSON(w, http.StatusOK, map[string]int{"code": 0})

		// Dispatch in the background so the HTTP handler returns immediately.
		go f.handleMessage(ctx, dispatch, env)
	}
}

func (f *FeishuAdapter) handleMessage(ctx context.Context, dispatch DispatchFn, env feishuEventEnvelope) {
	if env.Header.EventType != "im.message.receive_v1" {
		return
	}
	var ev feishuMessageEvent
	if err := json.Unmarshal(env.Event, &ev); err != nil {
		f.log.Printf("feishu: bad event payload: %v", err)
		return
	}
	if ev.Message.MessageType != "text" {
		return
	}
	text, err := extractFeishuText(ev.Message.Content)
	if err != nil {
		f.log.Printf("feishu: bad content: %v", err)
		return
	}
	text = stripFeishuMentions(text)

	reply := dispatch(ctx, Message{
		Text:   text,
		ChatID: ev.Message.ChatID,
		UserID: ev.Sender.SenderID.OpenID,
	})
	if reply.Text == "" {
		return
	}
	if err := f.sendText(ctx, ev.Message.ChatID, reply.Text); err != nil {
		f.log.Printf("feishu: send failed: %v", err)
	}
}

// extractFeishuText pulls the plain text out of Feishu's JSON-encoded content.
// For text messages, content looks like: {"text":"@_user_1 list"}
func extractFeishuText(content string) (string, error) {
	var c struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &c); err != nil {
		return "", err
	}
	return c.Text, nil
}

var feishuMentionRE = regexp.MustCompile(`@_user_\d+`)

// stripFeishuMentions removes Feishu's mention placeholders ("@_user_1", etc.)
// so the parser only sees the actual command text.
func stripFeishuMentions(text string) string {
	return strings.TrimSpace(feishuMentionRE.ReplaceAllString(text, ""))
}

// --- replies ---

func (f *FeishuAdapter) sendText(ctx context.Context, chatID, text string) error {
	tok, err := f.tenantToken(ctx)
	if err != nil {
		return err
	}
	contentJSON, _ := json.Marshal(map[string]string{"text": text})
	body, _ := json.Marshal(map[string]string{
		"receive_id": chatID,
		"msg_type":   "text",
		"content":    string(contentJSON),
	})
	url := f.cfg.BaseURL + "/open-apis/im/v1/messages?receive_id_type=chat_id"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("feishu send: %s: %s", resp.Status, string(b))
	}
	return nil
}

// tenantToken returns a cached tenant_access_token, refreshing it 60s before
// expiry so we never use a token that expires mid-request.
func (f *FeishuAdapter) tenantToken(ctx context.Context) (string, error) {
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()
	if f.tokenVal != "" && time.Until(f.tokenExpA) > time.Minute {
		return f.tokenVal, nil
	}
	body, _ := json.Marshal(map[string]string{
		"app_id":     f.cfg.AppID,
		"app_secret": f.cfg.AppSecret,
	})
	url := f.cfg.BaseURL + "/open-apis/auth/v3/tenant_access_token/internal"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Code != 0 || out.TenantAccessToken == "" {
		return "", fmt.Errorf("feishu auth: %d %s", out.Code, out.Msg)
	}
	f.tokenVal = out.TenantAccessToken
	f.tokenExpA = time.Now().Add(time.Duration(out.Expire) * time.Second)
	return f.tokenVal, nil
}

func (f *FeishuAdapter) verifyToken(got string) bool {
	if f.cfg.VerificationToken == "" {
		// allow when not configured — useful for local testing
		return true
	}
	return got == f.cfg.VerificationToken
}

// writeJSON is duplicated rather than shared with internal/server to keep this
// package self-contained.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
