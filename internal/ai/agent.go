package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/shiftu/shipd/internal/mcp"
)

// agentSystem is the system prompt for the chat-mode "ask" verb. The MCP
// tools are documented to the model via the tools array, so this prompt only
// covers behavior, not the tool schema.
const agentSystem = `You are the shipd assistant — a release-management copilot for build artifacts.

You have tools that let you list apps, look up release metadata, generate download URLs, and withdraw published releases. Use them. Do not invent facts you cannot retrieve.

Style:
- Be concise. Chat clients show your output as plain text or simple Markdown.
- For lists, summarize key facts (version, size, when published) — do not dump raw JSON.
- If the user references an app by partial name, call shipd_list_apps first to find the right one.
- If something the user asks isn't answerable with the tools, say so directly.

Safety:
- Never call shipd_yank_release without an explicit instruction from the user. Confirm the exact app and version you intend to yank in your final reply.`

// Agent runs Claude with the shipd MCP tools available, looping over
// tool_use → tool_result → ... until the model emits an end_turn response.
//
// One Agent instance is reusable across many Ask calls.
type Agent struct {
	client *Client
	reg    *mcp.Registry
	log    *log.Logger

	maxIterations int
	maxTokens     int
}

// NewAgent constructs an Agent. The MCP Registry is shared with the gateway
// and MCP server, so adding a tool there automatically exposes it to the
// agent too.
func NewAgent(client *Client, reg *mcp.Registry, logger *log.Logger) *Agent {
	if logger == nil {
		logger = log.Default()
	}
	return &Agent{
		client:        client,
		reg:           reg,
		log:           logger,
		maxIterations: 8,    // catch runaway loops without truncating reasonable conversations
		maxTokens:     1024, // chat replies should fit in this; bump if you see truncation
	}
}

// Ask runs the tool-use loop and returns the model's final text answer.
//
// The shape of the loop:
//  1. Send user prompt + tool definitions.
//  2. If the response is end_turn, extract text and return.
//  3. If the response is tool_use, execute every tool_use block, append a
//     user-role message with the tool_result blocks, and call again.
//  4. Bail with maxIterations to keep a misbehaving model from spending forever.
//
// onProgress, when non-nil, is called per intermediate iteration with the
// model's interleaved text and a "📡 calling <tool>" line per tool_use
// block, so chat users see the agent's reasoning unfold rather than waiting
// 10–30 seconds in silence. The final answer is the return value, never
// streamed through onProgress, so callers don't have to dedupe.
func (a *Agent) Ask(ctx context.Context, prompt string, onProgress func(text string)) (string, error) {
	tools := a.toolsForAnthropic()

	messages := []Message{{
		Role:    "user",
		Content: []ContentBlock{{Type: "text", Text: prompt}},
	}}

	for i := 0; i < a.maxIterations; i++ {
		resp, err := a.client.Messages(ctx, messageReq{
			MaxTokens: a.maxTokens,
			System: []SystemBlock{{
				Type:         "text",
				Text:         agentSystem,
				CacheControl: &CacheControl{Type: "ephemeral"},
			}},
			Tools:    tools,
			Messages: messages,
		})
		if err != nil {
			return "", err
		}
		a.log.Printf("ai/agent iter=%d stop=%s in=%d out=%d cache_read=%d cache_create=%d",
			i, resp.StopReason,
			resp.Usage.InputTokens, resp.Usage.OutputTokens,
			resp.Usage.CacheReadInputTokens, resp.Usage.CacheCreationInputTokens)

		if resp.StopReason != "tool_use" {
			text := extractText(resp.Content)
			if text == "" {
				text = "(no response)"
			}
			return text, nil
		}

		// Intermediate iteration: stream the model's reasoning + the tool
		// calls it's about to make, so the chat user sees progress instead
		// of staring at a frozen prompt. Final-iteration text is reserved
		// for the return value to avoid duplication.
		if onProgress != nil {
			streamProgress(resp.Content, onProgress)
		}

		// Append the assistant turn verbatim — tool_use blocks must be paired
		// with their tool_result blocks in the next user turn.
		messages = append(messages, Message{Role: "assistant", Content: resp.Content})

		results := a.runToolCalls(ctx, resp.Content)
		messages = append(messages, Message{Role: "user", Content: results})
	}
	return "", fmt.Errorf("agent: hit max iterations (%d) without end_turn", a.maxIterations)
}

// streamProgress emits one onProgress line per text block (the model's
// own commentary) and one per tool_use block ("📡 calling shipd_X"). It
// keeps the call simple — no args truncation, no result echoing — because
// chat clients prefer short discrete messages over long composed ones.
func streamProgress(blocks []ContentBlock, onProgress func(string)) {
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				onProgress(t)
			}
		case "tool_use":
			onProgress("📡 calling " + b.Name)
		}
	}
}

// runToolCalls executes every tool_use block and returns one tool_result
// block per call, in order. A failed tool produces a tool_result with
// is_error=true so the model can recover gracefully.
func (a *Agent) runToolCalls(ctx context.Context, blocks []ContentBlock) []ContentBlock {
	var out []ContentBlock
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		tool, ok := a.reg.Get(b.Name)
		if !ok {
			out = append(out, ContentBlock{
				Type: "tool_result", ToolUseID: b.ID,
				Content: fmt.Sprintf("unknown tool %q", b.Name), IsError: true,
			})
			continue
		}
		result, err := tool.Call(ctx, b.Input)
		if err != nil {
			a.log.Printf("ai/agent tool=%s error=%v", b.Name, err)
			out = append(out, ContentBlock{
				Type: "tool_result", ToolUseID: b.ID,
				Content: err.Error(), IsError: true,
			})
			continue
		}
		text := mcpResultText(result)
		out = append(out, ContentBlock{
			Type: "tool_result", ToolUseID: b.ID,
			Content: text, IsError: result.IsError,
		})
	}
	return out
}

// toolsForAnthropic translates the MCP tool registry into the shape
// /v1/messages expects. The schemas are byte-for-byte compatible — only the
// JSON tag for input_schema differs from MCP's inputSchema.
func (a *Agent) toolsForAnthropic() []Tool {
	mcpTools := a.reg.List()
	out := make([]Tool, 0, len(mcpTools))
	for _, t := range mcpTools {
		spec := t.Spec()
		out = append(out, Tool{
			Name:        spec.Name,
			Description: spec.Description,
			InputSchema: spec.InputSchema,
		})
	}
	return out
}

// mcpResultText concatenates the text content blocks of an MCP result so we
// can hand it back to Anthropic as a single tool_result string.
func mcpResultText(r *mcp.CallToolResult) string {
	if r == nil {
		return ""
	}
	if len(r.Content) == 1 {
		return r.Content[0].Text
	}
	b, _ := json.Marshal(r.Content)
	return string(b)
}
