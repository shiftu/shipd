// Package ai talks to Anthropic's API for the two LLM-powered features in
// shipd: AI-generated release notes (publish --ai-notes) and the chat-mode
// "ask" verb on the gateway.
//
// We hand-roll the HTTP client rather than pulling the official Go SDK so
// shipd stays a small, single-binary tool with a stable dep set. The
// /v1/messages endpoint is small enough that this is a good trade-off.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	defaultBaseURL    = "https://api.anthropic.com"
	defaultAPIVersion = "2023-06-01"
	defaultModel      = "claude-sonnet-4-6"
)

// Config holds the bits every Anthropic call needs.
type Config struct {
	APIKey     string        // ANTHROPIC_API_KEY
	Model      string        // empty → defaultModel
	BaseURL    string        // empty → defaultBaseURL (overridable for tests)
	APIVersion string        // empty → defaultAPIVersion
	Timeout    time.Duration // empty → 60s
	HTTP       *http.Client  // empty → http.DefaultClient
}

func (c *Config) normalize() Config {
	out := *c
	if out.Model == "" {
		out.Model = defaultModel
	}
	if out.BaseURL == "" {
		out.BaseURL = defaultBaseURL
	}
	if out.APIVersion == "" {
		out.APIVersion = defaultAPIVersion
	}
	if out.Timeout == 0 {
		out.Timeout = 60 * time.Second
	}
	if out.HTTP == nil {
		out.HTTP = http.DefaultClient
	}
	return out
}

// --- request types ---

type messageReq struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    []SystemBlock  `json:"system,omitempty"`
	Messages  []Message      `json:"messages"`
	Tools     []Tool         `json:"tools,omitempty"`
}

// SystemBlock is one segment of the system prompt. We split it because
// cache_control is per-block, and we want to mark the long, stable system
// preamble as cacheable while leaving any dynamic suffix uncached.
type SystemBlock struct {
	Type         string        `json:"type"` // always "text"
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// CacheControl signals to Anthropic to cache a block; subsequent requests
// within the cache TTL re-use the cached prefix and pay only for the new
// suffix tokens.
type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// Message is one turn in the conversation. Content is a slice of blocks
// because tool-use and tool-result interactions interleave structured blocks
// with plain text.
type Message struct {
	Role    string         `json:"role"` // "user" | "assistant"
	Content []ContentBlock `json:"content"`
}

// ContentBlock is the union of every block Anthropic accepts/returns. Fields
// are populated based on Type — readers should switch on Type before reading.
type ContentBlock struct {
	Type string `json:"type"`

	// type=text
	Text string `json:"text,omitempty"`

	// type=tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type=tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Tool is the shape Anthropic expects when describing tools the model may
// call. Note that the JSON tag is input_schema, distinct from the MCP
// ToolSpec's inputSchema — callers building a Tool from an MCP ToolSpec must
// rename the field.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// --- response types ---

// MessageResp is what /v1/messages returns. We expose only the fields shipd
// actually uses.
type MessageResp struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Content    []ContentBlock `json:"content"`
	Usage      Usage          `json:"usage"`
}

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// --- client ---

// Client is a tiny wrapper that holds Config so Messages doesn't need a long
// argument list.
type Client struct {
	cfg Config
}

func NewClient(cfg Config) *Client {
	return &Client{cfg: cfg.normalize()}
}

// APIError carries Anthropic's structured error payload so callers can
// distinguish "rate limited" from "invalid API key" without parsing the
// message string.
type APIError struct {
	StatusCode int
	Type       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic %d: %s: %s", e.StatusCode, e.Type, e.Message)
}

// Messages calls /v1/messages once. The caller manages the conversation; we
// don't loop on tool_use here — that lives in agent.go.
func (c *Client) Messages(ctx context.Context, req messageReq) (*MessageResp, error) {
	if c.cfg.APIKey == "" {
		return nil, errors.New("anthropic: API key not set")
	}
	if req.Model == "" {
		req.Model = c.cfg.Model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.cfg.APIKey)
	httpReq.Header.Set("anthropic-version", c.cfg.APIVersion)

	resp, err := c.cfg.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(respBody, &e)
		return nil, &APIError{StatusCode: resp.StatusCode, Type: e.Error.Type, Message: e.Error.Message}
	}
	var out MessageResp
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w (body=%s)", err, string(respBody))
	}
	return &out, nil
}

// extractText concatenates all type=text blocks from a response, ignoring
// tool_use blocks. Useful when the caller only cares about the final answer.
func extractText(blocks []ContentBlock) string {
	var buf bytes.Buffer
	for _, b := range blocks {
		if b.Type == "text" {
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(b.Text)
		}
	}
	return buf.String()
}
