package server

import (
	"testing"

	"github.com/shiftu/shipd/internal/storage"
)

// User-Agent samples observed in the wild — kept here as constants so
// pickPrimary's behavior stays anchored to real-world strings, not
// hand-crafted near-misses.
const (
	uaIPhone      = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	uaIPad        = "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15"
	uaAndroid     = "Mozilla/5.0 (Linux; Android 13; SM-G990B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Mobile Safari/537.36"
	uaMacDesktop  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15"
	uaWeChatMacOS = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 MicroMessenger/7.0.20"
)

// TestPickPrimaryByUserAgent: the install page's whole purpose is "open the
// same /install/{app} URL on any device, get the right thing." This nails
// down the per-UA mapping so it doesn't drift.
func TestPickPrimaryByUserAgent(t *testing.T) {
	rels := []storage.Release{
		{Platform: "android", Version: "1.0.1"},
		{Platform: "ios", Version: "1.0.0"},
	}
	cases := []struct {
		name      string
		ua        string
		wantPlat  string
		wantAlts  int
	}{
		{"iPhone", uaIPhone, "ios", 1},
		{"iPad", uaIPad, "ios", 1},
		{"Android phone", uaAndroid, "android", 1},
		{"Mac desktop falls back to alphabetical first", uaMacDesktop, "android", 1},
		{"WeChat WebView on Mac falls back too", uaWeChatMacOS, "android", 1},
		{"empty UA falls back to alphabetical first", "", "android", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			primary, alts := pickPrimary(rels, c.ua)
			if primary == nil || primary.Platform != c.wantPlat {
				got := "<nil>"
				if primary != nil {
					got = primary.Platform
				}
				t.Errorf("primary platform = %q, want %q", got, c.wantPlat)
			}
			if len(alts) != c.wantAlts {
				t.Errorf("len(alts) = %d, want %d", len(alts), c.wantAlts)
			}
		})
	}
}

// TestPickPrimarySinglePlatformNoAlternates: when an app only has releases
// on one platform, the page renders exactly as before — no "Also available"
// section.
func TestPickPrimarySinglePlatformNoAlternates(t *testing.T) {
	rels := []storage.Release{{Platform: "ios", Version: "1.0.0"}}
	primary, alts := pickPrimary(rels, uaIPhone)
	if primary == nil || primary.Platform != "ios" {
		t.Fatalf("expected ios primary, got %+v", primary)
	}
	if len(alts) != 0 {
		t.Errorf("expected no alts, got %d", len(alts))
	}
}

// TestPickPrimaryEmpty: defensive — an empty input yields nil/nil rather
// than panicking on rels[0]. The install page checks len before calling.
func TestPickPrimaryEmpty(t *testing.T) {
	primary, alts := pickPrimary(nil, uaIPhone)
	if primary != nil {
		t.Errorf("expected nil primary, got %+v", primary)
	}
	if len(alts) != 0 {
		t.Errorf("expected no alts, got %d", len(alts))
	}
}

// TestPickPrimaryUAMissesPlatform: an iOS UA visits an app that only has
// Android releases — fallback to alphabetical first, treat it like a desktop.
func TestPickPrimaryUAMissesPlatform(t *testing.T) {
	rels := []storage.Release{{Platform: "android", Version: "1.0.0"}}
	primary, alts := pickPrimary(rels, uaIPhone)
	if primary == nil || primary.Platform != "android" {
		t.Fatalf("expected android (only option), got %+v", primary)
	}
	if len(alts) != 0 {
		t.Errorf("expected no alts, got %d", len(alts))
	}
}
