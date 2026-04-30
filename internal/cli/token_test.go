package cli

import (
	"strings"
	"testing"
	"time"
)

// TestParseTokenTTL covers the common shapes operators will type. The "d" /
// "w" extensions matter most — Go's time.ParseDuration tops out at hours, but
// token lifetimes are normally measured in days.
func TestParseTokenTTL(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{in: "", want: 0},
		{in: "0", want: 0}, // Go's ParseDuration accepts a bare "0"; treat as "never expires" (same as empty).
		{in: "30m", want: 30 * time.Minute},
		{in: "1h", want: time.Hour},
		{in: "2160h", want: 2160 * time.Hour}, // 90 days the long way
		{in: "1d", want: 24 * time.Hour},
		{in: "90d", want: 90 * 24 * time.Hour},
		{in: "1w", want: 7 * 24 * time.Hour},
		{in: "4w", want: 4 * 7 * 24 * time.Hour},
		{in: "  90d  ", want: 90 * 24 * time.Hour}, // trim
		{in: "-1h", err: true},                     // negative refused
		{in: "-7d", err: true},
		{in: "abc", err: true},
		{in: "10x", err: true},
	}
	for _, c := range cases {
		got, err := parseTokenTTL(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseTokenTTL(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTokenTTL(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseTokenTTL(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestFormatExpiry: 0 → "never"; future → date; past → date + "(expired)".
func TestFormatExpiry(t *testing.T) {
	if got := formatExpiry(0); got != "never" {
		t.Errorf("formatExpiry(0) = %q, want never", got)
	}
	if got := formatExpiry(time.Now().Add(time.Hour).Unix()); got == "never" || got == "" || strings.Contains(got, "expired") {
		t.Errorf("formatExpiry(future) = %q, want a future date without (expired)", got)
	}
	got := formatExpiry(time.Now().Add(-time.Hour).Unix())
	if !strings.Contains(got, "(expired)") {
		t.Errorf("formatExpiry(past) = %q, want suffix '(expired)'", got)
	}
}
