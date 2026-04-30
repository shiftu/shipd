package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// WechatWorkConfig holds the credentials shipd needs to terminate a WeChat
// Work (企业微信 / WeCom) app's callback. CorpID, AgentID, Secret come from
// the app's profile in 管理后台 → 应用管理. Token and EncodingAESKey come
// from the same app's "接收消息" config block — the user generates these
// once and pastes them into shipd.
//
// Encryption mode must be "安全模式" (encrypted) on the WeChat Work side;
// shipd does not support plaintext mode.
type WechatWorkConfig struct {
	Addr           string // listen address, e.g. ":8082"
	CorpID         string
	AgentID        int
	Secret         string // app secret for /cgi-bin/gettoken
	Token          string // verification token (matches msg_signature)
	EncodingAESKey string // 43-char base64 (Tencent's notation)
	BaseURL        string // override for tests; defaults to https://qyapi.weixin.qq.com
	PublicBaseURL  string // used in the onboarding page; auto-derives if empty
}

// WechatWorkAdapter implements Adapter against a WeChat Work app.
//
// Routes the adapter exposes:
//
//	GET  /wxwork/event     URL verification handshake (echostr in query)
//	POST /wxwork/event     message event (encrypted XML body)
//	GET  /wxwork/qrcode    app QR code image (cached for 30 minutes)
//	GET  /wxwork/onboard   HTML page embedding the QR + scan instructions
type WechatWorkAdapter struct {
	cfg    WechatWorkConfig
	crypto *wxCrypto
	log    *log.Logger

	tokenMu  sync.Mutex
	tokenVal string
	tokenExp time.Time

	qrMu      sync.Mutex
	qrBytes   []byte
	qrCT      string
	qrFetched time.Time
}

func NewWechatWorkAdapter(cfg WechatWorkConfig, logger *log.Logger) (*WechatWorkAdapter, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://qyapi.weixin.qq.com"
	}
	if logger == nil {
		logger = log.Default()
	}
	c, err := newWxCrypto(cfg.Token, cfg.EncodingAESKey, cfg.CorpID)
	if err != nil {
		return nil, err
	}
	return &WechatWorkAdapter{cfg: cfg, crypto: c, log: logger}, nil
}

func (a *WechatWorkAdapter) Name() string { return "wechat-work" }

func (a *WechatWorkAdapter) Run(ctx context.Context, dispatch DispatchFn) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wxwork/event", a.handleVerify)
	mux.HandleFunc("POST /wxwork/event", a.handleEvent(ctx, dispatch))
	mux.HandleFunc("GET /wxwork/qrcode", a.handleQRCode)
	mux.HandleFunc("GET /wxwork/onboard", a.handleOnboard)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	hs := &http.Server{Addr: a.cfg.Addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = hs.Shutdown(context.Background())
	}()
	a.log.Printf("wechat-work adapter listening on %s (corp=%s agent=%d)", a.cfg.Addr, a.cfg.CorpID, a.cfg.AgentID)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// --- URL verification ---
//
// On 接收消息配置 setup, WeChat Work sends a single GET with all four params.
// The handshake passes iff we (a) verify the SHA1 signature, (b) decrypt
// echostr with our AES key, (c) write the cleartext back as the response body.

func (a *WechatWorkAdapter) handleVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sig := q.Get("msg_signature")
	ts := q.Get("timestamp")
	nonce := q.Get("nonce")
	echostr := q.Get("echostr")
	if !a.crypto.verifySig(ts, nonce, echostr, sig) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	plain, err := a.crypto.decrypt(echostr)
	if err != nil {
		http.Error(w, "decrypt failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(plain)
}

// --- message events ---

// wxOuterEnvelope is the unencrypted XML wrapper Tencent posts. The body has
// other fields too (ToUserName, AgentID, etc.) but Encrypt is the only one we
// need; signature verification covers tampering of the rest.
type wxOuterEnvelope struct {
	XMLName xml.Name `xml:"xml"`
	Encrypt string   `xml:"Encrypt"`
}

// wxInnerMessage is the cleartext XML inside Encrypt. We only handle text
// messages for now — anything else is ignored.
type wxInnerMessage struct {
	XMLName      xml.Name `xml:"xml"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Content      string   `xml:"Content"`
	MsgID        string   `xml:"MsgId"`
	AgentID      int      `xml:"AgentID"`
}

func (a *WechatWorkAdapter) handleEvent(ctx context.Context, dispatch DispatchFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var outer wxOuterEnvelope
		if err := xml.Unmarshal(body, &outer); err != nil {
			http.Error(w, "bad xml: "+err.Error(), http.StatusBadRequest)
			return
		}
		q := r.URL.Query()
		if !a.crypto.verifySig(q.Get("timestamp"), q.Get("nonce"), outer.Encrypt, q.Get("msg_signature")) {
			http.Error(w, "bad signature", http.StatusForbidden)
			return
		}
		plain, err := a.crypto.decrypt(outer.Encrypt)
		if err != nil {
			http.Error(w, "decrypt failed: "+err.Error(), http.StatusBadRequest)
			return
		}

		// ACK fast — Tencent retries on slow callbacks. The response body is
		// ignored when we reply via the active /cgi-bin/message/send API.
		w.WriteHeader(http.StatusOK)

		go a.handleMessage(ctx, dispatch, plain)
	}
}

func (a *WechatWorkAdapter) handleMessage(ctx context.Context, dispatch DispatchFn, plain []byte) {
	var msg wxInnerMessage
	if err := xml.Unmarshal(plain, &msg); err != nil {
		a.log.Printf("wxwork: bad inner xml: %v", err)
		return
	}
	if msg.MsgType != "text" {
		return
	}

	to := msg.FromUserName
	stream := func(line string) {
		if line == "" {
			return
		}
		if err := a.sendText(ctx, to, line); err != nil {
			a.log.Printf("wxwork: stream send failed: %v", err)
		}
	}
	reply := dispatch(ctx, Message{
		Text:   msg.Content,
		ChatID: to, // direct messages: chat id == user id
		UserID: to,
	}, stream)
	if reply.Text == "" {
		return
	}
	if err := a.sendText(ctx, to, reply.Text); err != nil {
		a.log.Printf("wxwork: send failed: %v", err)
	}
}

// --- replies ---

func (a *WechatWorkAdapter) sendText(ctx context.Context, toUser, text string) error {
	tok, err := a.accessToken(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"touser":  toUser,
		"agentid": a.cfg.AgentID,
		"msgtype": "text",
		"text":    map[string]string{"content": text},
	})
	url := a.cfg.BaseURL + "/cgi-bin/message/send?access_token=" + tok
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if out.ErrCode != 0 {
		return fmt.Errorf("wxwork send: %d %s", out.ErrCode, out.ErrMsg)
	}
	return nil
}

// accessToken returns a cached app access token, refreshing when within 60s
// of expiry. Same pattern as the Feishu tenant token.
func (a *WechatWorkAdapter) accessToken(ctx context.Context) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if a.tokenVal != "" && time.Until(a.tokenExp) > time.Minute {
		return a.tokenVal, nil
	}
	url := fmt.Sprintf("%s/cgi-bin/gettoken?corpid=%s&corpsecret=%s", a.cfg.BaseURL, a.cfg.CorpID, a.cfg.Secret)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ErrCode != 0 || out.AccessToken == "" {
		return "", fmt.Errorf("wxwork gettoken: %d %s", out.ErrCode, out.ErrMsg)
	}
	a.tokenVal = out.AccessToken
	a.tokenExp = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return a.tokenVal, nil
}

// --- QR code onboarding ---
//
// /wxwork/qrcode proxies the app's QR image from Tencent. The QR encodes a
// "open this app in WeChat Work" link — scanning it with the WeChat Work app
// drops the user straight into a chat with the bot.
//
// Tencent's /cgi-bin/agent/get_qrcode returns image bytes directly (PNG or
// JPEG), so we just stream them through. Cached for 30 minutes since the QR
// for an app rarely changes.

const qrCacheTTL = 30 * time.Minute

func (a *WechatWorkAdapter) handleQRCode(w http.ResponseWriter, r *http.Request) {
	if err := a.refreshQRIfNeeded(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	a.qrMu.Lock()
	body, ct := a.qrBytes, a.qrCT
	a.qrMu.Unlock()
	if ct == "" {
		ct = "image/jpeg"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=600")
	_, _ = w.Write(body)
}

func (a *WechatWorkAdapter) refreshQRIfNeeded(ctx context.Context) error {
	a.qrMu.Lock()
	fresh := a.qrBytes != nil && time.Since(a.qrFetched) < qrCacheTTL
	a.qrMu.Unlock()
	if fresh {
		return nil
	}
	tok, err := a.accessToken(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"agentid": a.cfg.AgentID, "size_type": 2})
	url := a.cfg.BaseURL + "/cgi-bin/agent/get_qrcode?access_token=" + tok
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("wxwork qrcode: status %s", resp.Status)
	}
	ct := resp.Header.Get("Content-Type")
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	// If Tencent returned JSON, it's an error response.
	if strings.HasPrefix(ct, "application/json") {
		var e struct {
			ErrCode int    `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
		}
		_ = json.Unmarshal(bodyBytes, &e)
		return fmt.Errorf("wxwork qrcode: %d %s", e.ErrCode, e.ErrMsg)
	}
	a.qrMu.Lock()
	a.qrBytes = bodyBytes
	a.qrCT = ct
	a.qrFetched = time.Now()
	a.qrMu.Unlock()
	return nil
}

// handleOnboard renders a small HTML page with the QR code embedded. Useful
// to share with co-workers: visit /wxwork/onboard, scan with WeChat Work.
func (a *WechatWorkAdapter) handleOnboard(w http.ResponseWriter, r *http.Request) {
	base := a.publicBase(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html>
<html lang="zh-CN"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>shipd · 企业微信接入</title>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, "PingFang SC", "Helvetica Neue", sans-serif; max-width: 480px; margin: 48px auto; padding: 0 20px; color: #111; line-height: 1.5; }
  @media (prefers-color-scheme: dark) { body { background: #0a0a0a; color: #f5f5f5; } }
  h1 { font-size: 22px; margin: 0 0 8px; }
  p.lead { color: #666; margin: 0 0 28px; }
  img.qr { display: block; margin: 0 auto 24px; width: 240px; height: 240px; border-radius: 12px; }
  ol { padding-left: 20px; }
  li { margin: 8px 0; }
  code { background: rgba(127,127,127,.15); padding: 2px 6px; border-radius: 4px; }
</style></head>
<body>
<h1>shipd 企业微信接入</h1>
<p class="lead">使用企业微信 App 扫描下方二维码，即可与 shipd 助手对话。</p>
<img class="qr" src="%s/wxwork/qrcode" alt="WeChat Work app QR code">
<ol>
  <li>用企业微信 App 扫码进入应用</li>
  <li>发送 <code>help</code> 查看支持的指令</li>
  <li>发送 <code>ask 最近发布了什么？</code> 试试自由问答</li>
</ol>
</body></html>`, base)
}

func (a *WechatWorkAdapter) publicBase(r *http.Request) string {
	if a.cfg.PublicBaseURL != "" {
		return strings.TrimRight(a.cfg.PublicBaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}
