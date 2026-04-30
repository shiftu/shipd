package gateway

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// SlackConfig holds the credentials needed for Socket Mode + the Web API.
//
// AppToken is the App-Level token (xapp-...) issued under Slack App config →
// Basic Information → App-Level Tokens, with `connections:write` scope. It
// dials the Socket Mode gateway and authenticates the long connection.
//
// BotToken is the Bot User OAuth token (xoxb-...) issued under OAuth &
// Permissions, used to call chat.postMessage when shipd replies.
//
// The two tokens have non-overlapping purposes; both are required.
type SlackConfig struct {
	AppToken string
	BotToken string
}

// SlackAdapter is the chat-platform bridge for Slack workspaces.
//
// It uses Socket Mode rather than the HTTP webhook flow, mirroring the
// Feishu adapter's WebSocket default: outbound HTTPS is enough, no public
// URL or TLS termination required. Socket Mode also auto-reconnects on
// transient drops via the slack-go SDK.
//
// Inbound events shipd cares about:
//   - app_mention   (channels: "@shipd-bot list")
//   - message.im    (DMs: "list" sent privately to the bot)
//
// Replies (final + streamed progress) go through the Web API's
// chat.postMessage. Streamed progress lines arrive as separate messages
// rather than edits to a single bubble — every Slack workspace supports
// posting; not every client renders edits the same.
type SlackAdapter struct {
	cfg SlackConfig
	log *log.Logger
}

// NewSlackAdapter validates configuration and returns an adapter ready to
// start. Returns an error rather than panicking when tokens are missing so
// `shipd gateway serve` can surface a friendly CLI message.
func NewSlackAdapter(cfg SlackConfig, logger *log.Logger) (*SlackAdapter, error) {
	if cfg.AppToken == "" {
		return nil, errors.New("slack: app token (xapp-...) is required for Socket Mode")
	}
	if cfg.BotToken == "" {
		return nil, errors.New("slack: bot token (xoxb-...) is required to send replies")
	}
	if !strings.HasPrefix(cfg.AppToken, "xapp-") {
		return nil, errors.New("slack: app token must start with 'xapp-'")
	}
	if !strings.HasPrefix(cfg.BotToken, "xoxb-") {
		return nil, errors.New("slack: bot token must start with 'xoxb-'")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &SlackAdapter{cfg: cfg, log: logger}, nil
}

func (a *SlackAdapter) Name() string { return "slack" }

func (a *SlackAdapter) Run(ctx context.Context, dispatch DispatchFn) error {
	api := slack.New(a.cfg.BotToken,
		slack.OptionAppLevelToken(a.cfg.AppToken),
	)
	sm := socketmode.New(api,
		socketmode.OptionLog(log.New(a.log.Writer(), "slack/socketmode ", 0)),
	)

	// The slack-go socketmode loop runs in its own goroutine while we
	// drain its event channel here. Acks must happen synchronously per
	// event so Slack's Socket Mode protocol is happy; the actual dispatch
	// can run concurrently because each handleEvent makes its own HTTP
	// calls back to Slack.
	errCh := make(chan error, 1)
	go func() {
		errCh <- sm.RunContext(ctx)
	}()

	a.log.Printf("slack adapter started (socket mode)")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case evt, ok := <-sm.Events:
			if !ok {
				return nil
			}
			a.handleSocketEvent(ctx, dispatch, api, sm, evt)
		}
	}
}

func (a *SlackAdapter) handleSocketEvent(ctx context.Context, dispatch DispatchFn, api *slack.Client, sm *socketmode.Client, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnecting:
		a.log.Printf("slack: connecting")
	case socketmode.EventTypeConnected:
		a.log.Printf("slack: connected")
	case socketmode.EventTypeConnectionError:
		a.log.Printf("slack: connection error: %v", evt.Data)
	case socketmode.EventTypeDisconnect:
		a.log.Printf("slack: disconnect")
	case socketmode.EventTypeEventsAPI:
		eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		// Ack first — Slack retries any event we don't ack within a few
		// seconds, which would replay shipd commands.
		if evt.Request != nil {
			sm.Ack(*evt.Request)
		}
		if eventsAPIEvent.Type != slackevents.CallbackEvent {
			return
		}
		switch ev := eventsAPIEvent.InnerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			a.handleEvent(ctx, dispatch, api, ev.Channel, ev.User, ev.Text)
		case *slackevents.MessageEvent:
			// Skip bot-emitted messages (including our own replies) and
			// edits / deletions / channel-join chrome.
			if ev.SubType != "" || ev.BotID != "" {
				return
			}
			// message.im DMs deliver ChannelType="im". Channel mentions
			// don't come through here — those land as AppMentionEvent.
			if ev.ChannelType != "im" {
				return
			}
			a.handleEvent(ctx, dispatch, api, ev.Channel, ev.User, ev.Text)
		}
	}
}

func (a *SlackAdapter) handleEvent(ctx context.Context, dispatch DispatchFn, api *slack.Client, channel, user, text string) {
	text = stripSlackMention(text)
	if text == "" {
		return
	}

	stream := func(line string) {
		if line == "" {
			return
		}
		if _, _, err := api.PostMessageContext(ctx, channel,
			slack.MsgOptionText(line, false),
			slack.MsgOptionDisableLinkUnfurl(),
		); err != nil {
			a.log.Printf("slack: stream send failed: %v", err)
		}
	}
	reply := dispatch(ctx, Message{Text: text, ChatID: channel, UserID: user}, stream)
	if reply.Text == "" {
		return
	}
	if _, _, err := api.PostMessageContext(ctx, channel,
		slack.MsgOptionText(reply.Text, false),
		slack.MsgOptionDisableLinkUnfurl(),
	); err != nil {
		a.log.Printf("slack: send failed: %v", err)
	}
}

// stripSlackMention removes a leading bot mention from the message text.
// Slack delivers AppMentionEvent text like "<@U07ABC123> list myapp"; the
// router doesn't care about the mention prefix.
//
// MessageEvent text in DMs has no leading mention but may include them
// inline ("hey <@U...> can you?"); we only strip the leading one.
func stripSlackMention(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "<@") {
		return s
	}
	end := strings.Index(s, ">")
	if end < 0 {
		return s
	}
	return strings.TrimSpace(s[end+1:])
}

// Compile-time assertion that the adapter implements the gateway contract.
var _ Adapter = (*SlackAdapter)(nil)
