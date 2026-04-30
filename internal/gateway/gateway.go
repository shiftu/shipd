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
// final Reply, and may also push intermediate progress lines through the
// stream callback during long-running tool-use loops (the `ask` verb).
type Adapter interface {
	Name() string
	Run(ctx context.Context, dispatch DispatchFn) error
}

// DispatchFn is the function adapters call when they receive a message.
//
// stream is invoked with intermediate progress lines (e.g. "📡 calling
// shipd_list_apps") that the agent emits between tool-use iterations of
// the `ask` verb. Adapters wire it to whatever lets them post a
// stand-alone message in their native channel — Feishu's IM REST,
// WeChat Work's send-message API, fmt.Fprintln for stdio. Adapters that
// don't want progress can pass nil; the agent then runs in single-shot
// mode and only the final Reply is delivered.
type DispatchFn func(ctx context.Context, msg Message, stream func(text string)) Reply

// Router holds the shared tool registry and turns Messages into Replies.
type Router struct {
	reg   *mcp.Registry
	agent Asker // optional; nil disables the "ask" verb
}

// Asker is the surface the gateway needs from an LLM agent — small enough
// that ai.Agent can implement it without the gateway depending on internal/ai
// at build time.
//
// onProgress, when non-nil, is invoked with intermediate progress lines as
// the agent's tool-use loop iterates. The final answer is the return value.
// Implementations that don't support streaming must still accept the
// callback and may simply ignore it.
type Asker interface {
	Ask(ctx context.Context, prompt string, onProgress func(text string)) (string, error)
}

func NewRouter(reg *mcp.Registry) *Router { return &Router{reg: reg} }

// WithAgent enables the "ask" chat verb. Pass an Asker (typically *ai.Agent)
// that has access to the same MCP registry the router uses.
func (r *Router) WithAgent(a Asker) *Router {
	r.agent = a
	return r
}

// Dispatch parses msg.Text, looks up the tool, calls it, and formats the
// result. stream may be nil; when non-nil, intermediate progress lines (from
// the `ask` verb's tool-use loop) flow through it before the final Reply.
func (r *Router) Dispatch(ctx context.Context, msg Message, stream func(text string)) Reply {
	cmd, err := parseCommand(msg.Text)
	if err != nil {
		return Reply{Text: "error: " + err.Error(), IsError: true}
	}
	if cmd.Help {
		return Reply{Text: r.helpText()}
	}
	if cmd.Ask != "" {
		if r.agent == nil {
			return Reply{Text: "ask is not enabled (no ANTHROPIC_API_KEY configured on the server)", IsError: true}
		}
		answer, err := r.agent.Ask(ctx, cmd.Ask, stream)
		if err != nil {
			return Reply{Text: "error: " + err.Error(), IsError: true}
		}
		return Reply{Text: answer}
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
