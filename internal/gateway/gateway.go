// Package gateway turns chat messages into shipd tool calls.
//
// Architecture: an Adapter owns the transport (stdio REPL, Feishu webhook,
// Slack RTM, etc.) and converts platform-native events into Message values.
// The Router parses the Message text into a tool invocation, dispatches it
// against the shared MCP tool Registry, and hands back a Reply that the
// Adapter ships back over its native transport.
//
// Reusing the MCP Registry means a chat user gets exactly the same surface as
// an Agent — one implementation, two frontends.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shiftu/shipd/internal/mcp"
)

// Message is a normalized chat message handed to the Router.
type Message struct {
	Text   string // command text, mention prefix already stripped by the adapter
	ChatID string // adapter-specific chat/channel identifier (used for replies)
	UserID string // adapter-specific sender identifier (logging only, for now)
}

// Reply is the Router's response. Adapters render Text in whatever way fits
// their platform (plain text, code block, markdown, etc.).
type Reply struct {
	Text    string
	IsError bool
}

// Adapter is implemented by every transport (stdio, Feishu, ...).
//
// Run blocks until ctx is cancelled or the underlying transport closes. The
// adapter calls dispatch for each incoming message; dispatch returns the
// Reply to send back.
type Adapter interface {
	Name() string
	Run(ctx context.Context, dispatch DispatchFn) error
}

// DispatchFn is the function adapters call when they receive a message.
type DispatchFn func(ctx context.Context, msg Message) Reply

// Router holds the shared tool registry and turns Messages into Replies.
type Router struct {
	reg *mcp.Registry
}

func NewRouter(reg *mcp.Registry) *Router { return &Router{reg: reg} }

// Dispatch parses msg.Text, looks up the tool, calls it, and formats the result.
func (r *Router) Dispatch(ctx context.Context, msg Message) Reply {
	cmd, err := parseCommand(msg.Text)
	if err != nil {
		return Reply{Text: "error: " + err.Error(), IsError: true}
	}
	if cmd.Help {
		return Reply{Text: r.helpText()}
	}
	tool, ok := r.reg.Get(cmd.Tool)
	if !ok {
		return Reply{Text: fmt.Sprintf("unknown command %q. Try 'help'.", cmd.Verb), IsError: true}
	}
	args, err := json.Marshal(cmd.Args)
	if err != nil {
		return Reply{Text: "error: " + err.Error(), IsError: true}
	}
	res, err := tool.Call(ctx, args)
	if err != nil {
		return Reply{Text: "error: " + err.Error(), IsError: true}
	}
	return Reply{Text: renderToolResult(res), IsError: res.IsError}
}

// helpText is the response to the literal "help" command. We surface every
// registered tool plus the chat aliases declared in the parser.
func (r *Router) helpText() string {
	var sb strings.Builder
	sb.WriteString("Commands:\n")
	for _, alias := range chatAliases {
		sb.WriteString("  ")
		sb.WriteString(alias.usage)
		sb.WriteString("\n      ")
		sb.WriteString(alias.desc)
		sb.WriteString("\n")
	}
	sb.WriteString("  help\n      Show this message.\n")
	return strings.TrimRight(sb.String(), "\n")
}

// renderToolResult unwraps the MCP Content slice into a single string. For
// shipd tools the content is always a single text item, frequently
// pretty-printed JSON. We pass it through verbatim — chat clients render it
// fine in code blocks, and agents can re-parse it.
func renderToolResult(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var parts []string
	for _, c := range res.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}
