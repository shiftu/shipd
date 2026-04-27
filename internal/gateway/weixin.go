package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// iLink message item types — only the text type is wired in v1; image/voice/
// video/file would each require CDN download + AES-128-ECB decryption (see
// Hermes weixin.py for the reference implementation).
const (
	wxItemText  = 1
	wxItemImage = 2
	wxItemVoice = 3
	wxItemFile  = 4
	wxItemVideo = 5

	wxMsgTypeBot   = 2
	wxMsgStateDone = 2

	wxSessionExpired = -14

	wxBackoff       = 30 * time.Second
	wxRetryDelay    = 2 * time.Second
	wxMaxFailStreak = 3
)

// WeixinConfig holds the runtime configuration for a personal-WeChat
// adapter. AccountID is the iLink-issued bot id; Token is the bearer
// credential. Both come from QRLoginWeixin.
//
// State is persisted under StateDir so a restart resumes from the previous
// long-poll cursor instead of replaying messages.
type WeixinConfig struct {
	AccountID string
	Token     string
	BaseURL   string // override; empty → iLinkBaseURL
	StateDir  string // where sync_buf is persisted
}

// WeixinAdapter connects to iLink, long-polls for messages, dispatches them
// through the router, and posts replies back.
//
// It echoes the most recent context_token observed for each peer back on
// outbound messages (Tencent rejects sends without it after the first turn);
// tokens live in memory only — that's good enough for short-lived gateways
// and avoids leaking conversation handles to disk.
type WeixinAdapter struct {
	cfg WeixinConfig
	cli *iLinkClient
	log *log.Logger

	tokensMu sync.Mutex
	tokens   map[string]string // peer user_id → latest context_token
}

func NewWeixinAdapter(cfg WeixinConfig, logger *log.Logger) (*WeixinAdapter, error) {
	if cfg.AccountID == "" || cfg.Token == "" {
		return nil, errors.New("WeixinConfig requires AccountID and Token; run `shipd gateway weixin-login` first")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &WeixinAdapter{
		cfg:    cfg,
		cli:    newILinkClient(cfg.BaseURL, cfg.Token),
		log:    logger,
		tokens: map[string]string{},
	}, nil
}

func (a *WeixinAdapter) Name() string { return "weixin" }

// Run drives the long-poll loop until ctx is cancelled. Returns the first
// fatal error encountered; transient failures back off and retry.
func (a *WeixinAdapter) Run(ctx context.Context, dispatch DispatchFn) error {
	a.log.Printf("weixin adapter started account=%s base=%s", a.cfg.AccountID, a.cli.baseURL)
	syncBuf := loadSyncBuf(a.cfg.StateDir, a.cfg.AccountID)
	failStreak := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// Use a per-call context so the long-poll request can run longer
		// than other API timeouts but still respects ctx cancellation.
		callCtx, cancel := context.WithTimeout(ctx, iLinkLongPollTimeout+5*time.Second)
		resp, err := a.getUpdates(callCtx, syncBuf)
		cancel()
		if err != nil {
			failStreak++
			a.log.Printf("weixin: getupdates error (%d/%d): %v", failStreak, wxMaxFailStreak, err)
			delay := wxRetryDelay
			if failStreak >= wxMaxFailStreak {
				delay = wxBackoff
				failStreak = 0
			}
			if !sleep(ctx, delay) {
				return ctx.Err()
			}
			continue
		}

		if resp.Ret == wxSessionExpired || resp.ErrCode == wxSessionExpired {
			a.log.Printf("weixin: session expired; pausing 10 minutes (re-run weixin-login to refresh)")
			if !sleep(ctx, 10*time.Minute) {
				return ctx.Err()
			}
			continue
		}
		if resp.Ret != 0 || resp.ErrCode != 0 {
			a.log.Printf("weixin: getupdates ret=%d errcode=%d errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
			failStreak++
			if !sleep(ctx, wxRetryDelay) {
				return ctx.Err()
			}
			continue
		}
		failStreak = 0

		if resp.GetUpdatesBuf != "" && resp.GetUpdatesBuf != syncBuf {
			syncBuf = resp.GetUpdatesBuf
			if err := saveSyncBuf(a.cfg.StateDir, a.cfg.AccountID, syncBuf); err != nil {
				a.log.Printf("weixin: persist sync_buf: %v", err)
			}
		}

		for _, raw := range resp.Msgs {
			a.handleInbound(ctx, dispatch, raw)
		}
	}
}

// --- inbound ---

type weixinUpdatesResp struct {
	iLinkResp
	GetUpdatesBuf      string            `json:"get_updates_buf"`
	LongPollTimeoutMs  int               `json:"longpolling_timeout_ms"`
	Msgs               []json.RawMessage `json:"msgs"`
}

func (a *WeixinAdapter) getUpdates(ctx context.Context, syncBuf string) (*weixinUpdatesResp, error) {
	var out weixinUpdatesResp
	err := a.cli.post(ctx, iLinkEPGetUpdates, map[string]any{"get_updates_buf": syncBuf}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// weixinMessage describes the inbound payload shape we care about. iLink
// returns much more (group metadata, reference messages, media), but for v1
// we only handle text in 1:1 chats.
type weixinMessage struct {
	FromUserID    string         `json:"from_user_id"`
	ToUserID      string         `json:"to_user_id"`
	RoomID        string         `json:"room_id"`
	MessageID     string         `json:"message_id"`
	ContextToken  string         `json:"context_token"`
	MsgType       int            `json:"msg_type"`
	ItemList      []weixinItem   `json:"item_list"`
}

type weixinItem struct {
	Type     int `json:"type"`
	TextItem struct {
		Text string `json:"text"`
	} `json:"text_item"`
	VoiceItem struct {
		Text string `json:"text"` // Tencent attaches a transcript when ASR is on
	} `json:"voice_item"`
}

func (a *WeixinAdapter) handleInbound(ctx context.Context, dispatch DispatchFn, raw json.RawMessage) {
	var msg weixinMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		a.log.Printf("weixin: bad message json: %v", err)
		return
	}
	if msg.FromUserID == "" || msg.FromUserID == a.cfg.AccountID {
		return // skip own echoes
	}

	// Track the most recent context_token per peer; outbound messages must
	// echo it after the first turn.
	if msg.ContextToken != "" {
		a.tokensMu.Lock()
		a.tokens[msg.FromUserID] = msg.ContextToken
		a.tokensMu.Unlock()
	}

	text := extractWeixinText(msg.ItemList)
	if text == "" {
		return
	}

	chatID := msg.FromUserID
	if msg.RoomID != "" {
		chatID = msg.RoomID
	}
	reply := dispatch(ctx, Message{
		Text:   text,
		ChatID: chatID,
		UserID: msg.FromUserID,
	})
	if reply.Text == "" {
		return
	}
	if err := a.sendText(ctx, msg.FromUserID, reply.Text); err != nil {
		a.log.Printf("weixin: send failed to=%s: %v", shortID(msg.FromUserID), err)
	}
}

// extractWeixinText walks item_list for the first text-bearing item. Voice
// items may carry an ASR transcript; we use that as text when present so the
// router gets a usable string instead of dropping the message.
func extractWeixinText(items []weixinItem) string {
	for _, it := range items {
		if it.Type == wxItemText && it.TextItem.Text != "" {
			return it.TextItem.Text
		}
	}
	for _, it := range items {
		if it.Type == wxItemVoice && it.VoiceItem.Text != "" {
			return it.VoiceItem.Text
		}
	}
	return ""
}

// --- outbound ---

func (a *WeixinAdapter) sendText(ctx context.Context, toUser, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("empty text")
	}
	a.tokensMu.Lock()
	contextToken := a.tokens[toUser]
	a.tokensMu.Unlock()

	clientID := newClientID()
	msg := map[string]any{
		"from_user_id":  "",
		"to_user_id":    toUser,
		"client_id":     clientID,
		"message_type":  wxMsgTypeBot,
		"message_state": wxMsgStateDone,
		"item_list": []any{
			map[string]any{"type": wxItemText, "text_item": map[string]any{"text": text}},
		},
	}
	if contextToken != "" {
		msg["context_token"] = contextToken
	}
	callCtx, cancel := context.WithTimeout(ctx, iLinkAPITimeout)
	defer cancel()

	var resp iLinkResp
	if err := a.cli.post(callCtx, iLinkEPSendMessage, map[string]any{"msg": msg}, &resp); err != nil {
		return err
	}
	if resp.Ret != 0 || resp.ErrCode != 0 {
		return fmt.Errorf("ret=%d errcode=%d errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
	}
	return nil
}

func newClientID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "…"
}

// sleep returns false if ctx was cancelled before the duration elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
