package ai

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// notesSystem is the system prompt for release-notes generation. Marked
// cacheable so a user generating notes for several apps in sequence pays full
// price once, then cache-read price for the rest.
const notesSystem = `You are a release-notes writer. The user gives you a git log and a version number; write user-facing release notes in concise Markdown.

Rules:
- Group commits as "## Features", "## Fixes", "## Other". Omit empty sections.
- Each bullet is one short line, present tense, written from the user's perspective.
- Skip merge commits, formatting-only commits, and dependency bumps unless they fix a CVE or change behavior.
- Do not include commit SHAs, PR numbers, or author names unless they're already in the subject line and meaningful.
- No preamble like "Here are the notes:" — output the markdown directly.
- If the log is empty or contains only noise, output the single line: "Maintenance release."`

// GenerateReleaseNotes calls git log between sinceRef and HEAD, sends the
// result to Claude, and returns markdown release notes.
//
// sinceRef is a git revision (tag, branch, sha) marking the start of the
// range. HEAD is always the end. The caller is responsible for resolving
// sinceRef (e.g. from the previous shipd release version) before calling.
func GenerateReleaseNotes(ctx context.Context, c *Client, version, sinceRef, repoDir string) (string, error) {
	gitLog, err := runGitLog(ctx, sinceRef, repoDir)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(gitLog) == "" {
		return "Maintenance release.", nil
	}

	user := fmt.Sprintf("Version: %s\n\nGit log (oldest to newest):\n\n%s", version, gitLog)
	resp, err := c.Messages(ctx, messageReq{
		MaxTokens: 800,
		System: []SystemBlock{{
			Type:         "text",
			Text:         notesSystem,
			CacheControl: &CacheControl{Type: "ephemeral"},
		}},
		Messages: []Message{{
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: user}},
		}},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(extractText(resp.Content)), nil
}

// runGitLog returns one line per commit in `<oldest-first>` order, formatted
// "<subject>". Reverse chronological is what `git log` defaults to, so we
// pass --reverse to give the model a natural narrative.
func runGitLog(ctx context.Context, sinceRef, repoDir string) (string, error) {
	args := []string{
		"log",
		"--no-merges",
		"--reverse",
		"--pretty=format:- %s",
	}
	if sinceRef != "" {
		args = append(args, sinceRef+"..HEAD")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	if repoDir != "" {
		cmd.Dir = repoDir
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		stderr := ""
		if e, ok := err.(*exec.ExitError); ok {
			ee = e
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		if stderr != "" {
			return "", fmt.Errorf("git log: %s", stderr)
		}
		return "", fmt.Errorf("git log: %w", err)
	}
	return string(out), nil
}

// ResolveSinceRef returns a git revision suitable for "log <ref>..HEAD".
// userSpecified wins if non-empty. Otherwise we try, in order:
//  1. tag matching prevVersion exactly (e.g. "1.2.0")
//  2. tag matching "v" + prevVersion (e.g. "v1.2.0")
//
// Returns ("", error) if nothing resolves; the caller should print a clear
// hint pointing at --ai-since.
func ResolveSinceRef(ctx context.Context, userSpecified, prevVersion, repoDir string) (string, error) {
	if userSpecified != "" {
		return userSpecified, nil
	}
	if prevVersion == "" {
		return "", fmt.Errorf("no previous version found and --ai-since not set")
	}
	for _, candidate := range []string{prevVersion, "v" + prevVersion} {
		if revParseExists(ctx, candidate, repoDir) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no git tag found for previous version %q (tried %q and \"v%s\"); pass --ai-since explicitly",
		prevVersion, prevVersion, prevVersion)
}

func revParseExists(ctx context.Context, ref, repoDir string) bool {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", ref+"^{}")
	if repoDir != "" {
		cmd.Dir = repoDir
	}
	return cmd.Run() == nil
}
