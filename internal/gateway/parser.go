package gateway

import (
	"fmt"
	"strings"
)

// command is the parsed form of a chat message.
type command struct {
	Verb string         // user-facing verb the chat user typed
	Tool string         // resolved MCP tool name
	Args map[string]any // arguments to marshal as the tool's JSON input
	Help bool           // true for an empty message or "help"
	Ask  string         // non-empty → free-form LLM question
}

// chatAliases describes the chat verbs we accept. Used by the help text.
var chatAliases = []struct {
	usage string
	desc  string
}{
	{usage: "list", desc: "List all apps."},
	{usage: "list <app>", desc: "List releases for an app, newest first."},
	{usage: "info <app>[@<version>]", desc: "Show release metadata (latest by default)."},
	{usage: "url <app>[@<version>]", desc: "Print a direct download URL."},
	{usage: "yank <app>@<version> [reason=\"...\"]", desc: "Withdraw a published release."},
	{usage: "ask <question...>", desc: "Free-form question; an LLM picks tools to answer (requires ANTHROPIC_API_KEY)."},
}

// parseCommand turns a chat string into a tool invocation. It accepts the
// short verbs documented in chatAliases.
//
// Tokenization is simple: spaces split tokens, double-quotes group them.
// Arguments like reason="long text with spaces" are handled.
func parseCommand(text string) (*command, error) {
	text = strings.TrimSpace(text)
	if text == "" || text == "help" {
		return &command{Help: true}, nil
	}

	// "ask" is parsed before tokenization so the question keeps its original
	// punctuation, quotes, and casing — the LLM gets the user's text verbatim.
	if rest, ok := stripVerbPrefix(text, "ask"); ok {
		if rest == "" {
			return nil, fmt.Errorf("ask requires a question")
		}
		return &command{Verb: "ask", Ask: rest}, nil
	}

	tokens := tokenize(text)
	if len(tokens) == 0 {
		return &command{Help: true}, nil
	}
	verb := strings.ToLower(tokens[0])
	rest := tokens[1:]

	switch verb {
	case "list":
		if len(rest) == 0 {
			return &command{Verb: verb, Tool: "shipd_list_apps", Args: map[string]any{}}, nil
		}
		return &command{Verb: verb, Tool: "shipd_list_releases", Args: map[string]any{"app": rest[0]}}, nil

	case "info":
		if len(rest) < 1 {
			return nil, fmt.Errorf("info requires <app[@version]>")
		}
		args := refToArgs(rest[0])
		return &command{Verb: verb, Tool: "shipd_get_release", Args: args}, nil

	case "url":
		if len(rest) < 1 {
			return nil, fmt.Errorf("url requires <app[@version]>")
		}
		args := refToArgs(rest[0])
		return &command{Verb: verb, Tool: "shipd_download_url", Args: args}, nil

	case "yank":
		if len(rest) < 1 {
			return nil, fmt.Errorf("yank requires <app@version>")
		}
		args := refToArgs(rest[0])
		if _, ok := args["version"]; !ok {
			return nil, fmt.Errorf("yank requires <app@version>")
		}
		// Optional key=value pairs (e.g. reason="crash on iOS").
		for _, kv := range rest[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return nil, fmt.Errorf("expected key=value, got %q", kv)
			}
			args[k] = v
		}
		return &command{Verb: verb, Tool: "shipd_yank_release", Args: args}, nil

	default:
		return nil, fmt.Errorf("unknown verb %q (try 'help')", verb)
	}
}

// stripVerbPrefix returns the text after the leading verb (case-insensitive)
// followed by whitespace. Returns ok=false if text doesn't start with verb.
func stripVerbPrefix(text, verb string) (string, bool) {
	if len(text) < len(verb) {
		return "", false
	}
	if !strings.EqualFold(text[:len(verb)], verb) {
		return "", false
	}
	if len(text) == len(verb) {
		return "", true
	}
	if text[len(verb)] != ' ' && text[len(verb)] != '\t' {
		return "", false
	}
	return strings.TrimSpace(text[len(verb)+1:]), true
}

// refToArgs splits "name" or "name@version" into the args map shape used by
// the shipd tools.
func refToArgs(ref string) map[string]any {
	app, version, _ := strings.Cut(ref, "@")
	out := map[string]any{"app": app}
	if version != "" {
		out["version"] = version
	}
	return out
}

// tokenize splits on whitespace, treating "double-quoted" runs as a single
// token. Quotes are stripped; unbalanced quotes are forgiven (the trailing
// run is taken as one token).
func tokenize(s string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case (c == ' ' || c == '\t') && !inQuote:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return tokens
}
