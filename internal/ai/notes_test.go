package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGenerateReleaseNotesPromptShape verifies that the request to Anthropic
// carries the cacheable system prompt, the user prompt with the version and
// git log, and no tools (notes generation is a one-shot, no tool use).
func TestGenerateReleaseNotesPromptShape(t *testing.T) {
	var capturedReq messageReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &capturedReq)
		writeJSON(w, MessageResp{
			StopReason: "end_turn",
			Content:    []ContentBlock{{Type: "text", Text: "## Features\n- thing"}},
		})
	}))
	defer srv.Close()

	// Inject a fake git log via a small wrapper. Since runGitLog calls real
	// git, we instead test by going through GenerateReleaseNotes with a
	// sinceRef that yields no commits — the function returns "Maintenance
	// release." short-circuit. To exercise the full path, we point sinceRef
	// at HEAD itself which produces an empty log.
	client := NewClient(Config{APIKey: "k", BaseURL: srv.URL})
	got, err := GenerateReleaseNotes(context.Background(), client, "1.2.3", "HEAD", ".")
	if err != nil {
		t.Fatalf("GenerateReleaseNotes: %v", err)
	}
	if got != "Maintenance release." {
		t.Errorf("expected maintenance short-circuit, got %q", got)
	}

	// Now run the full path with a sinceRef that produces a non-empty log.
	// The shipd repo has multiple commits; using HEAD~1 should return one line.
	got, err = GenerateReleaseNotes(context.Background(), client, "1.2.3", "HEAD~1", ".")
	if err != nil {
		t.Fatalf("GenerateReleaseNotes (full path): %v", err)
	}
	if !strings.Contains(got, "thing") {
		t.Errorf("expected mocked reply to contain 'thing', got %q", got)
	}
	if len(capturedReq.System) != 1 || capturedReq.System[0].CacheControl == nil {
		t.Errorf("system prompt should be cacheable, got %+v", capturedReq.System)
	}
	if len(capturedReq.Tools) != 0 {
		t.Errorf("notes generation should pass no tools, got %d", len(capturedReq.Tools))
	}
	if len(capturedReq.Messages) != 1 || capturedReq.Messages[0].Role != "user" {
		t.Errorf("expected single user message, got %+v", capturedReq.Messages)
	}
	if !strings.Contains(capturedReq.Messages[0].Content[0].Text, "1.2.3") {
		t.Errorf("user prompt should mention version, got %q", capturedReq.Messages[0].Content[0].Text)
	}
}

// TestResolveSinceRef verifies the precedence: explicit user value wins, then
// auto-detected tag, then a clear error.
func TestResolveSinceRef(t *testing.T) {
	ctx := context.Background()

	// User-specified beats everything.
	got, err := ResolveSinceRef(ctx, "deadbeef", "1.0.0", "")
	if err != nil || got != "deadbeef" {
		t.Errorf("user override: got=%q err=%v", got, err)
	}

	// No previous version and no override → clear error.
	if _, err := ResolveSinceRef(ctx, "", "", ""); err == nil {
		t.Error("expected error when no inputs given")
	}

	// Previous version with no matching tag → mentions both attempts in the error.
	if _, err := ResolveSinceRef(ctx, "", "doesnotexist-9.9.9", ""); err == nil ||
		!strings.Contains(err.Error(), "doesnotexist-9.9.9") {
		t.Errorf("expected error mentioning the version, got %v", err)
	}
}
