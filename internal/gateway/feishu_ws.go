package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// runWebSocket is the long-connection path. The lark-oapi-sdk-go ws client
// dials open.feishu.cn, handles the proprietary frame protocol (auth +
// heartbeat + protobuf-encoded events), and dispatches im.message.receive_v1
// callbacks. shipd hands those off to the gateway router and replies via the
// regular IM message-create API.
//
// Replies happen on a separate REST client because the WS channel only
// pushes inbound events — outbound messages still go through HTTPS.
func (f *FeishuAdapter) runWebSocket(ctx context.Context, dispatch DispatchFn) error {
	if f.cfg.AppID == "" || f.cfg.AppSecret == "" {
		return fmt.Errorf("feishu websocket mode requires AppID and AppSecret")
	}

	sendOpts := []lark.ClientOptionFunc{lark.WithLogLevel(larkcore.LogLevelWarn)}
	if f.cfg.BaseURL != "" {
		sendOpts = append(sendOpts, lark.WithOpenBaseUrl(f.cfg.BaseURL))
	}
	sendCli := lark.NewClient(f.cfg.AppID, f.cfg.AppSecret, sendOpts...)

	handler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, ev *larkim.P2MessageReceiveV1) error {
			f.onWSMessage(ctx, dispatch, sendCli, ev)
			return nil // never return errors here; we handle locally
		})

	wsCli := larkws.NewClient(f.cfg.AppID, f.cfg.AppSecret,
		larkws.WithEventHandler(handler),
		larkws.WithAutoReconnect(true),
		larkws.WithLogLevel(larkcore.LogLevelWarn),
	)

	f.log.Printf("feishu adapter started (mode=websocket app_id=%s)", f.cfg.AppID)
	// Start blocks until ctx is done or a fatal error occurs. The SDK auto-
	// reconnects on transient failures.
	return wsCli.Start(ctx)
}

// onWSMessage handles a single inbound message: extracts the text, strips
// any @-mentions of the bot, dispatches through the router, and posts the
// reply (if any) back to the same chat via the REST API.
func (f *FeishuAdapter) onWSMessage(ctx context.Context, dispatch DispatchFn, send *lark.Client, ev *larkim.P2MessageReceiveV1) {
	if ev == nil || ev.Event == nil || ev.Event.Message == nil {
		return
	}
	msg := ev.Event.Message
	if derefStr(msg.MessageType) != "text" {
		return
	}

	// Content for text messages is JSON-encoded: {"text":"@_user_1 hello"}
	text, err := extractFeishuText(derefStr(msg.Content))
	if err != nil {
		f.log.Printf("feishu ws: bad content: %v", err)
		return
	}
	text = stripFeishuMentions(text)
	if text == "" {
		return
	}

	chatID := derefStr(msg.ChatId)
	userID := ""
	if ev.Event.Sender != nil && ev.Event.Sender.SenderId != nil {
		userID = derefStr(ev.Event.Sender.SenderId.OpenId)
	}

	stream := func(line string) {
		if line == "" {
			return
		}
		if err := f.sendWS(ctx, send, chatID, line); err != nil {
			f.log.Printf("feishu ws: stream send failed: %v", err)
		}
	}
	reply := dispatch(ctx, Message{Text: text, ChatID: chatID, UserID: userID}, stream)
	if reply.Text == "" {
		return
	}
	if err := f.sendWS(ctx, send, chatID, reply.Text); err != nil {
		f.log.Printf("feishu ws: send failed: %v", err)
	}
}

// sendWS posts a text message back to chatID via the regular IM API. The WS
// channel does not carry outbound messages — Lark expects reply traffic on
// HTTPS even when inbound events come over the long connection.
func (f *FeishuAdapter) sendWS(ctx context.Context, cli *lark.Client, chatID, text string) error {
	contentJSON, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	body := larkim.NewCreateMessageReqBodyBuilder().
		ReceiveId(chatID).
		MsgType("text").
		Content(string(contentJSON)).
		Build()
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(body).
		Build()
	resp, err := cli.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("create message: %d %s", resp.Code, resp.Msg)
	}
	return nil
}

// derefStr safely dereferences SDK string pointers (Lark's generated types
// use *string everywhere).
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
