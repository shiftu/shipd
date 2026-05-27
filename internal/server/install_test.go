package server

import (
	"bytes"
	"encoding/xml"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

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

// TestPlistBundleVersion: iOS's appstored rejects bundle-version values
// containing characters CFBundleShortVersionString disallows — `+` is the
// common one because Flutter publishes versions like "2.9.7+293" where the
// suffix is the build number. The manifest must surface only the marketing
// portion so iOS's version check against the IPA passes.
func TestPlistBundleVersion(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"2.9.7+293", "2.9.7"},     // Flutter / pubspec convention
		{"1.0.0", "1.0.0"},         // plain semver, untouched
		{"1.0.0-beta.1", "1.0.0-beta.1"}, // pre-release, untouched
		{"1.0+abc+def", "1.0"},     // first `+` wins
		{"", ""},                   // defensive
	}
	for _, c := range cases {
		if got := plistBundleVersion(c.in); got != c.want {
			t.Errorf("plistBundleVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestManifestPlistIsValidXML guards against a recurrence of the iOS
// "no install modal" bug: signed download URLs contain `&sig=...`, and
// embedding the raw `&` in the plist's <string> makes it invalid XML.
// iOS's plist parser is strict — it silently drops the manifest, so the
// user sees nothing happen when they tap Install. We assert the rendered
// plist round-trips through encoding/xml without error.
func TestManifestPlistIsValidXML(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	ctx := t.Context()
	if err := store.UpsertApp(ctx, "demo", "ios"); err != nil {
		t.Fatalf("UpsertApp: %v", err)
	}
	if _, err := store.PutRelease(ctx, storage.Release{
		AppName: "demo", Version: "2.9.7+293", Channel: "stable", Platform: "ios",
		BundleID: "com.example.demo", Filename: "demo.ipa",
		DisplayName: "Demo & Co's \"Test\" <build>", // exercise every XML metacharacter
	}, strings.NewReader("ipa-bytes")); err != nil {
		t.Fatalf("PutRelease: %v", err)
	}

	// Signing on (the default) is what triggers the bug — without it the
	// URL has no `&` and the manifest renders valid by accident.
	srv, err := New(Config{
		DataDir:       dir,
		PublicReads:   true,
		InstallURLTTL: 30 * time.Minute,
	}, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Pull the install page first to mint a fresh signed manifest URL the
	// same way iOS Safari does.
	page, err := http.Get(ts.URL + "/install/demo")
	if err != nil {
		t.Fatalf("install page: %v", err)
	}
	body, err := io.ReadAll(page.Body)
	page.Body.Close()
	if err != nil {
		t.Fatalf("read install page: %v", err)
	}
	m := regexp.MustCompile(`manifest\.plist%3F[^"&]+`).FindString(string(body))
	if m == "" {
		t.Fatalf("install page missing signed manifest URL:\n%s", body)
	}
	qs, _ := url.QueryUnescape(strings.TrimPrefix(m, "manifest.plist"))

	resp, err := http.Get(ts.URL + "/install/demo/2.9.7+293/manifest.plist" + qs)
	if err != nil {
		t.Fatalf("fetch manifest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200", resp.StatusCode)
	}
	plistBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	// encoding/xml.Decoder rejects malformed entities the same way iOS does.
	dec := xml.NewDecoder(bytes.NewReader(plistBytes))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("manifest is not valid XML — iOS will silently drop it: %v\nplist:\n%s",
				err, plistBytes)
		}
	}
}

// TestInstallDownloadRange exercises the iOS OTA hot path: the device's
// appstored issues a HEAD then Range requests; if either returns 200 with
// the full body instead of 206 Partial Content, iOS silently drops the
// install. Asserts Accept-Ranges advertisement, 206 status on Range, exact
// Content-Range, and that the returned bytes match the requested window.
func TestInstallDownloadRange(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	ctx := t.Context()
	if err := store.UpsertApp(ctx, "demo", "ios"); err != nil {
		t.Fatalf("UpsertApp: %v", err)
	}
	// Body just needs to be longer than the Range window we ask for. Use a
	// deterministic pattern so the slice comparison is exact.
	body := bytes.Repeat([]byte("0123456789"), 1024) // 10 KiB
	if _, err := store.PutRelease(ctx, storage.Release{
		AppName: "demo", Version: "1.0.0", Channel: "stable", Platform: "ios",
		BundleID: "com.example.demo", Filename: "demo.ipa",
	}, bytes.NewReader(body)); err != nil {
		t.Fatalf("PutRelease: %v", err)
	}

	srv, err := New(Config{DataDir: dir, PublicReads: true}, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	url := ts.URL + "/install/demo/1.0.0/download"

	// 1. HEAD — http.ServeContent should advertise Accept-Ranges: bytes.
	headResp, err := http.Head(url)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	headResp.Body.Close()
	if got := headResp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want %q (iOS install requires Range support)", got, "bytes")
	}

	// 2. Range request — must return 206 with the requested slice, not 200
	// with the full body.
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Range", "bytes=100-199")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d (server is ignoring Range header)", resp.StatusCode, http.StatusPartialContent)
	}
	if got := resp.Header.Get("Content-Range"); !strings.HasPrefix(got, "bytes 100-199/") {
		t.Errorf("Content-Range = %q, want prefix %q", got, "bytes 100-199/")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := body[100:200]
	if !bytes.Equal(got, want) {
		t.Errorf("body slice mismatch: got %q want %q", got, want)
	}
}
