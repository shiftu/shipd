package server

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shiftu/shipd/internal/storage"
)

// TestWriteProm checks the exposition format. We don't try to parse it as
// real Prometheus — just assert the lines an operator (or a Grafana
// dashboard) would key off survive a refactor.
func TestWriteProm(t *testing.T) {
	m := &metrics{}
	m.publishOK.Add(7)
	m.publishConflict.Add(2)
	m.publishError.Add(1)
	m.yankOK.Add(3)
	m.unyankOK.Add(1)
	m.promoteOK.Add(4)
	m.downloadAPI.Add(11)
	m.downloadInstall.Add(22)
	m.installPageRenders.Add(33)
	m.installSigFail.Add(5)
	m.gcRuns.Add(6)
	m.gcDeleteRuns.Add(2)
	m.gcRowsDeleted.Add(8)
	m.gcBlobsDeleted.Add(7)
	m.tokensCreated.Add(9)
	m.authInvalid.Add(40)
	m.authExpired.Add(2)
	m.authForbidden.Add(13)

	stats := storage.StorageStats{
		Apps:            5,
		ReleasesLive:    12,
		ReleasesYanked:  3,
		Tokens:          4,
		BlobBytesUnique: 1024 * 1024 * 50,
	}

	var buf bytes.Buffer
	m.writeProm(&buf, stats)
	out := buf.String()

	wantLines := []string{
		`shipd_publish_total{result="ok"} 7`,
		`shipd_publish_total{result="conflict"} 2`,
		`shipd_publish_total{result="error"} 1`,
		`shipd_download_total{source="api"} 11`,
		`shipd_download_total{source="install"} 22`,
		`shipd_gc_runs_total{mode="dry_run"} 6`,
		`shipd_gc_runs_total{mode="delete"} 2`,
		`shipd_auth_failure_total{reason="invalid"} 40`,
		`shipd_auth_failure_total{reason="expired"} 2`,
		`shipd_auth_failure_total{reason="forbidden"} 13`,
		`shipd_yank_total 3`,
		`shipd_unyank_total 1`,
		`shipd_promote_total 4`,
		`shipd_install_page_renders_total 33`,
		`shipd_install_sig_fail_total 5`,
		`shipd_gc_rows_deleted_total 8`,
		`shipd_gc_blobs_deleted_total 7`,
		`shipd_tokens_created_total 9`,
		`shipd_apps 5`,
		`shipd_releases{state="live"} 12`,
		`shipd_releases{state="yanked"} 3`,
		`shipd_tokens_active 4`,
		"shipd_blob_bytes 52428800",
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("missing line %q in output:\n%s", want, out)
		}
	}

	// Every counter family should declare its TYPE — Prometheus rejects
	// samples that lack one.
	for _, name := range []string{
		"shipd_publish_total", "shipd_download_total", "shipd_gc_runs_total",
		"shipd_auth_failure_total", "shipd_apps", "shipd_releases",
	} {
		if !strings.Contains(out, "# TYPE "+name) {
			t.Errorf("missing TYPE declaration for %s", name)
		}
	}
}

// TestMetricsRequiresAdminScope is the auth-gate contract. Hits /metrics
// from in-process httptest with three token shapes: admin (200), rw (403),
// and missing (401). Real Prometheus scrapers configure an admin bearer.
func TestMetricsRequiresAdminScope(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	ctx := t.Context()
	if err := store.CreateToken(ctx, "admin-tok", "shipd_test_admin", "admin", 0); err != nil {
		t.Fatalf("CreateToken admin: %v", err)
	}
	if err := store.CreateToken(ctx, "rw-tok", "shipd_test_rw", "rw", 0); err != nil {
		t.Fatalf("CreateToken rw: %v", err)
	}

	srv, err := New(Config{DataDir: dir}, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cases := []struct {
		name   string
		token  string
		status int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"rw rejected", "shipd_test_rw", http.StatusForbidden},
		{"admin ok", "shipd_test_admin", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
			if c.token != "" {
				req.Header.Set("X-Auth-Token", c.token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.status {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d (body: %s)", resp.StatusCode, c.status, body)
			}
			if c.status == http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				if !strings.Contains(string(body), "shipd_apps") {
					t.Errorf("expected metrics body to contain shipd_apps, got:\n%s", body)
				}
			}
		})
	}
}

// TestMetricsCountersIncrementOnHandlers does an integration-style check
// that a real publish bumps the counter we expose. Catches the easy-to-
// miss case where a new handler ships without instrumentation.
func TestMetricsCountersIncrementOnHandlers(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir, nil)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	ctx := t.Context()
	if err := store.CreateToken(ctx, "admin", "shipd_test_admin", "admin", 0); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	srv, err := New(Config{DataDir: dir}, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Publish twice to generate publish_total{result="ok"} = 2.
	for _, version := range []string{"1.0.0", "1.0.1"} {
		req, _ := http.NewRequest(http.MethodPost,
			ts.URL+"/api/v1/apps/demo/releases?version="+version+"&platform=ios",
			strings.NewReader("body-"+version))
		req.Header.Set("X-Auth-Token", "shipd_test_admin")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("publish status %d", resp.StatusCode)
		}
	}

	// One auth failure (no token).
	resp, _ := http.Get(ts.URL + "/api/v1/apps")
	resp.Body.Close()

	// Scrape /metrics and verify the counters reflect what just happened.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("X-Auth-Token", "shipd_test_admin")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("/metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), `shipd_publish_total{result="ok"} 2`) {
		t.Errorf("expected publish counter at 2, got:\n%s", body)
	}
	if !strings.Contains(string(body), `shipd_auth_failure_total{reason="invalid"} 1`) {
		t.Errorf("expected one auth invalid, got:\n%s", body)
	}
	if !strings.Contains(string(body), "shipd_apps 1") {
		t.Errorf("expected apps gauge at 1, got:\n%s", body)
	}
}
