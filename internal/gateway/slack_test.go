package gateway

import "testing"

// TestStripSlackMention covers the message-shape Slack delivers to
// app_mention events: a leading "<@USERID>" token followed by the actual
// command text. DMs come through without the prefix; channel mentions do.
// Either form should land at the parser stripped of the mention.
func TestStripSlackMention(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"channel mention", "<@U07ABC123> list myapp", "list myapp"},
		{"channel mention, no body", "<@U07ABC123>", ""},
		{"channel mention with extra spaces", "  <@U07ABC123>   info myapp@1.0  ", "info myapp@1.0"},
		{"DM with no mention", "list myapp", "list myapp"},
		{"plain text edge: starts with < but not mention", "<not-a-mention>", "<not-a-mention>"},
		{"unbalanced opener", "<@U07ABC123 list", "<@U07ABC123 list"},
		{"only the mention prefix with whitespace", "<@U07ABC123>   ", ""},
		{"inline mentions are preserved", "list <@U07OTHER>", "list <@U07OTHER>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripSlackMention(c.in); got != c.want {
				t.Errorf("stripSlackMention(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestNewSlackAdapterValidatesTokens guards against operator typos that
// would only surface as a confusing 401 / WS handshake error from Slack.
func TestNewSlackAdapterValidatesTokens(t *testing.T) {
	cases := []struct {
		name string
		cfg  SlackConfig
		err  bool
	}{
		{"missing app token", SlackConfig{BotToken: "xoxb-foo"}, true},
		{"missing bot token", SlackConfig{AppToken: "xapp-foo"}, true},
		{"app token wrong prefix", SlackConfig{AppToken: "xoxb-foo", BotToken: "xoxb-bar"}, true},
		{"bot token wrong prefix", SlackConfig{AppToken: "xapp-foo", BotToken: "xapp-bar"}, true},
		{"valid", SlackConfig{AppToken: "xapp-foo", BotToken: "xoxb-bar"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewSlackAdapter(c.cfg, nil)
			if c.err && err == nil {
				t.Error("expected error, got nil")
			}
			if !c.err && err != nil {
				t.Errorf("expected ok, got %v", err)
			}
		})
	}
}
