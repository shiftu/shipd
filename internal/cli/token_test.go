package cli

import (
	"strings"
	"testing"
	"time"
)

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
