package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shiftu/shipd/internal/mcp"
)

// fakeTool is a test-only mcp.Tool that records calls and returns a fixed reply.
type fakeTool struct {
	name        string
	description string
	calls       []json.RawMessage
	reply       string
}

func (f *fakeTool) Spec() mcp.ToolSpec {
	return mcp.ToolSpec{
		Name:        f.name,
		Description: f.description,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (f *fakeTool) Call(_ context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
	f.calls = append(f.calls, args)
	return &mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: f.reply}}}, nil
}

// TestAgentToolUseLoop verifies the full path: model emits tool_use, agent
// dispatches against the registry, model receives the result and produces a
// final answer.
func TestAgentToolUseLoop(t *testing.T) {
	tool := &fakeTool{name: "shipd_list_apps", description: "list apps", reply: `[{"name":"foo"}]`}
	reg := mcp.NewRegistry()
	reg.Register(tool)

	// First request → tool_use; second request → end_turn with text.
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing/wrong x-api-key header: %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing anthropic-version header")
		}
		body, _ := io.ReadAll(r.Body)
		var req messageReq
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if turn == 0 {
			// Sanity-check the first turn: it should carry the user prompt and a tool definition.
			if len(req.Tools) != 1 || req.Tools[0].Name != "shipd_list_apps" {
				t.Errorf("expected one tool named shipd_list_apps, got %+v", req.Tools)
			}
			if len(req.System) != 1 || req.System[0].CacheControl == nil {
				t.Errorf("system prompt should be present and cacheable, got %+v", req.System)
			}
			writeJSON(w, MessageResp{
				StopReason: "tool_use",
				Content: []ContentBlock{
					{Type: "tool_use", ID: "toolu_1", Name: "shipd_list_apps", Input: json.RawMessage(`{}`)},
				},
				Usage: Usage{InputTokens: 100, OutputTokens: 20},
			})
			turn++
			return
		}
		// Second turn: the loop should have appended an assistant message with
		// the tool_use block and a user message with the tool_result.
		if len(req.Messages) != 3 {
			t.Errorf("expected 3 messages on second turn, got %d", len(req.Messages))
		}
		writeJSON(w, MessageResp{
			StopReason: "end_turn",
			Content:    []ContentBlock{{Type: "text", Text: "There is one app: foo."}},
			Usage:      Usage{InputTokens: 200, OutputTokens: 8, CacheReadInputTokens: 100},
		})
	}))
	defer srv.Close()

	client := NewClient(Config{APIKey: "test-key", BaseURL: srv.URL})
	agent := NewAgent(client, reg, nil)
	got, err := agent.Ask(context.Background(), "list apps", nil)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !strings.Contains(got, "foo") {
		t.Errorf("expected reply to mention 'foo', got %q", got)
	}
	if len(tool.calls) != 1 {
		t.Errorf("tool should have been called once, got %d", len(tool.calls))
	}
}

// TestAgentMaxIterations guards against an infinitely-tool-using model.
func TestAgentMaxIterations(t *testing.T) {
	tool := &fakeTool{name: "shipd_list_apps", description: "x", reply: "{}"}
	reg := mcp.NewRegistry()
	reg.Register(tool)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Always emit tool_use → agent should bail out of the loop.
		writeJSON(w, MessageResp{
			StopReason: "tool_use",
			Content: []ContentBlock{
				{Type: "tool_use", ID: "toolu_1", Name: "shipd_list_apps", Input: json.RawMessage(`{}`)},
			},
		})
	}))
	defer srv.Close()

	agent := NewAgent(NewClient(Config{APIKey: "k", BaseURL: srv.URL}), reg, nil)
	_, err := agent.Ask(context.Background(), "loop forever", nil)
	if err == nil || !strings.Contains(err.Error(), "max iterations") {
		t.Fatalf("expected max-iterations error, got %v", err)
	}
}

// TestAgentStreamsProgress: when onProgress is non-nil, intermediate
// turns push the model's interleaved text and a "📡 calling X" line per
// tool_use block. The final-turn text is reserved for the return value
// (no duplication into the stream).
func TestAgentStreamsProgress(t *testing.T) {
	tool := &fakeTool{name: "shipd_list_apps", description: "list apps", reply: `[{"name":"foo"}]`}
	reg := mcp.NewRegistry()
	reg.Register(tool)

	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if turn == 0 {
			writeJSON(w, MessageResp{
				StopReason: "tool_use",
				Content: []ContentBlock{
					{Type: "text", Text: "Let me look up the apps first."},
					{Type: "tool_use", ID: "toolu_1", Name: "shipd_list_apps", Input: json.RawMessage(`{}`)},
				},
			})
			turn++
			return
		}
		writeJSON(w, MessageResp{
			StopReason: "end_turn",
			Content:    []ContentBlock{{Type: "text", Text: "Final answer: one app, foo."}},
		})
	}))
	defer srv.Close()

	agent := NewAgent(NewClient(Config{APIKey: "k", BaseURL: srv.URL}), reg, nil)

	var streamed []string
	final, err := agent.Ask(context.Background(), "what apps?", func(line string) {
		streamed = append(streamed, line)
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if final != "Final answer: one app, foo." {
		t.Errorf("final answer = %q", final)
	}
	wantProgress := []string{
		"Let me look up the apps first.",
		"📡 calling shipd_list_apps",
	}
	if len(streamed) != len(wantProgress) {
		t.Fatalf("progress lines = %d, want %d (%v)", len(streamed), len(wantProgress), streamed)
	}
	for i, w := range wantProgress {
		if streamed[i] != w {
			t.Errorf("progress[%d] = %q, want %q", i, streamed[i], w)
		}
	}
	// Final-iteration text must NOT appear in the stream — that would
	// double-render in chat.
	for _, line := range streamed {
		if strings.Contains(line, "Final answer") {
			t.Errorf("final text leaked into stream: %q", line)
		}
	}
}

// TestAgentNilProgressKeepsLegacyBehavior: passing nil for onProgress
// must run exactly the way the pre-streaming agent did — single-shot
// final answer, no intermediate emissions.
func TestAgentNilProgressKeepsLegacyBehavior(t *testing.T) {
	tool := &fakeTool{name: "shipd_list_apps", description: "x", reply: "{}"}
	reg := mcp.NewRegistry()
	reg.Register(tool)

	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if turn == 0 {
			writeJSON(w, MessageResp{
				StopReason: "tool_use",
				Content: []ContentBlock{
					{Type: "text", Text: "thinking..."},
					{Type: "tool_use", ID: "t1", Name: "shipd_list_apps", Input: json.RawMessage(`{}`)},
				},
			})
			turn++
			return
		}
		writeJSON(w, MessageResp{StopReason: "end_turn", Content: []ContentBlock{{Type: "text", Text: "done"}}})
	}))
	defer srv.Close()

	agent := NewAgent(NewClient(Config{APIKey: "k", BaseURL: srv.URL}), reg, nil)
	got, err := agent.Ask(context.Background(), "hi", nil)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "done" {
		t.Errorf("got %q, want done", got)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
