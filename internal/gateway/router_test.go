package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/shiftu/shipd/internal/mcp"
)

// fakeAsker captures Ask invocations and lets a test drive what the
// onProgress callback receives.
type fakeAsker struct {
	gotPrompt    string
	progressFeed []string
	finalAnswer  string
	err          error
}

func (f *fakeAsker) Ask(_ context.Context, prompt string, onProgress func(string)) (string, error) {
	f.gotPrompt = prompt
	if onProgress != nil {
		for _, line := range f.progressFeed {
			onProgress(line)
		}
	}
	return f.finalAnswer, f.err
}

// TestRouterStreamsAskProgress: when an adapter passes a stream callback
// to Dispatch, every line the agent emits via onProgress must reach that
// callback in order, and the final answer must arrive as the Reply (NOT
// duplicated into the stream).
func TestRouterStreamsAskProgress(t *testing.T) {
	asker := &fakeAsker{
		progressFeed: []string{
			"Let me check.",
			"📡 calling shipd_list_apps",
		},
		finalAnswer: "Final answer: one app foo.",
	}
	r := NewRouter(mcp.NewRegistry()).WithAgent(asker)

	var streamed []string
	reply := r.Dispatch(context.Background(),
		Message{Text: "ask what apps?"},
		func(line string) { streamed = append(streamed, line) },
	)

	if asker.gotPrompt != "what apps?" {
		t.Errorf("agent saw prompt %q, want %q", asker.gotPrompt, "what apps?")
	}
	if reply.Text != asker.finalAnswer {
		t.Errorf("final reply = %q, want %q", reply.Text, asker.finalAnswer)
	}
	if len(streamed) != len(asker.progressFeed) {
		t.Fatalf("streamed %d lines (%v), want %d", len(streamed), streamed, len(asker.progressFeed))
	}
	for i, want := range asker.progressFeed {
		if streamed[i] != want {
			t.Errorf("streamed[%d] = %q, want %q", i, streamed[i], want)
		}
	}
	for _, line := range streamed {
		if strings.Contains(line, "Final answer") {
			t.Errorf("final answer leaked into stream: %q", line)
		}
	}
}

// TestRouterNilStreamStillWorks: an adapter that doesn't care about
// progress (passes stream=nil) gets the legacy single-Reply path. The
// agent receives a nil onProgress and runs in single-shot mode.
func TestRouterNilStreamStillWorks(t *testing.T) {
	asker := &fakeAsker{
		progressFeed: []string{"would-stream"},
		finalAnswer:  "ok",
	}
	r := NewRouter(mcp.NewRegistry()).WithAgent(asker)

	reply := r.Dispatch(context.Background(),
		Message{Text: "ask anything"},
		nil,
	)
	if reply.Text != "ok" {
		t.Errorf("reply = %q, want ok", reply.Text)
	}
}

// TestRouterNonAskCommandsIgnoreStream: stream is a no-op for tool-call
// verbs (list/info/etc.) — they're synchronous and have nothing to push.
// The Reply path stays unchanged.
func TestRouterNonAskCommandsIgnoreStream(t *testing.T) {
	r := NewRouter(mcp.NewRegistry())

	called := false
	reply := r.Dispatch(context.Background(),
		Message{Text: "help"},
		func(string) { called = true },
	)
	if called {
		t.Error("stream callback should not fire for the 'help' verb")
	}
	if reply.Text == "" {
		t.Error("expected help text in Reply")
	}
}
