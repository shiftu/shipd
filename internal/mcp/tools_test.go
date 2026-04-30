package mcp

import (
	"strings"
	"testing"

	"github.com/shiftu/shipd/internal/client"
)

// TestFormatStats checks the human-readable summary the shipd_stats tool
// emits. The format doubles as the chat verb's response, so the line-per-
// fact shape matters — an LLM agent reading this output should be able to
// answer "how many apps?" or "any auth failures lately?" without parsing
// prose.
func TestFormatStats(t *testing.T) {
	s := &client.Stats{
		Apps:               5,
		ReleasesLive:       12,
		ReleasesYanked:     3,
		TokensActive:       4,
		BlobBytes:          1024 * 1024 * 50,
		PublishOK:          7,
		PublishConflict:    2,
		PublishError:       1,
		Yank:               3,
		Unyank:             1,
		Promote:            0,
		DownloadAPI:        11,
		DownloadInstall:    22,
		InstallPageRenders: 33,
		InstallSigFail:     5,
		GCDryRunRuns:       6,
		GCDeleteRuns:       2,
		GCRowsDeleted:      8,
		GCBlobsDeleted:     7,
		TokensCreated:      9,
		AuthInvalid:        40,
		AuthExpired:        2,
		AuthForbidden:      13,
	}
	out := formatStats(s)

	wantSubstrings := []string{
		"apps              5",
		"releases          12 live, 3 yanked",
		"storage           50.0 MiB",
		"tokens            4 active",
		"publish         ok=7 conflict=2 error=1",
		"yank/unyank     3 / 1",
		"download        api=11 install=22",
		"install_page    33 renders, 5 sig failures",
		"gc              dry-run=6 delete=2 (rows=8, blobs=7)",
		"auth_failure    invalid=40 expired=2 forbidden=13",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(out, want) {
			t.Errorf("formatStats output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestHumanBytes covers the ranges shipd actually produces in stats output.
func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{2 * 1024 * 1024, "2.0 MiB"},
		{int64(3.5 * 1024 * 1024 * 1024), "3.5 GiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
