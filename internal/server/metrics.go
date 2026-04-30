package server

import (
	"fmt"
	"io"
	"sync/atomic"

	"github.com/shiftu/shipd/internal/storage"
)

// Stats is the JSON-friendly snapshot served by GET /api/v1/stats. It
// flattens the gauge fields from storage.StorageStats together with the
// process-lifetime counters held in `metrics`, which is what the
// shipd_stats MCP tool and the `stats` chat verb both consume. Field names
// are JSON-natural ("publish_ok" rather than the Prometheus
// "shipd_publish_total{result=ok}") so a chat formatter or an agent can
// pick fields directly without parsing label syntax.
type Stats struct {
	// Catalog gauges (sampled at request time from SQLite).
	Apps           int64 `json:"apps"`
	ReleasesLive   int64 `json:"releases_live"`
	ReleasesYanked int64 `json:"releases_yanked"`
	TokensActive   int64 `json:"tokens_active"`
	BlobBytes      int64 `json:"blob_bytes"`

	// Counters since process start. Useful for "is anything weird?"
	// at-a-glance — operators on a chat bot can see e.g. an auth_invalid
	// climb without setting up a Prometheus scraper.
	PublishOK          int64 `json:"publish_ok"`
	PublishConflict    int64 `json:"publish_conflict"`
	PublishError       int64 `json:"publish_error"`
	Yank               int64 `json:"yank"`
	Unyank             int64 `json:"unyank"`
	Promote            int64 `json:"promote"`
	DownloadAPI        int64 `json:"download_api"`
	DownloadInstall    int64 `json:"download_install"`
	InstallPageRenders int64 `json:"install_page_renders"`
	InstallSigFail     int64 `json:"install_sig_fail"`
	GCDryRunRuns       int64 `json:"gc_dry_run_runs"`
	GCDeleteRuns       int64 `json:"gc_delete_runs"`
	GCRowsDeleted      int64 `json:"gc_rows_deleted"`
	GCBlobsDeleted     int64 `json:"gc_blobs_deleted"`
	TokensCreated      int64 `json:"tokens_created"`
	AuthInvalid        int64 `json:"auth_invalid"`
	AuthExpired        int64 `json:"auth_expired"`
	AuthForbidden      int64 `json:"auth_forbidden"`
}

// metrics is shipd's tiny, dependency-free Prometheus instrumentation. We
// hand-roll the exposition format rather than pulling client_golang because
// the metric set is small and stable, and one of shipd's selling points is
// "single static binary, no surprises".
//
// Counters are atomic.Int64 so handlers can bump them without locking.
// Gauges are computed from the storage layer at scrape time — they don't
// need to be in this struct.
type metrics struct {
	// publish outcomes
	publishOK       atomic.Int64
	publishConflict atomic.Int64 // (app, version, channel) already taken
	publishError    atomic.Int64 // I/O, internal

	yankOK    atomic.Int64
	unyankOK  atomic.Int64
	promoteOK atomic.Int64

	// Downloads split by which surface served them: api requires a token,
	// install runs through the public install page (and signature verifier
	// when --install-url-ttl > 0).
	downloadAPI     atomic.Int64
	downloadInstall atomic.Int64

	installPageRenders atomic.Int64
	installSigFail     atomic.Int64 // bad/expired/missing signature

	// gc
	gcRuns         atomic.Int64 // dry-run scrapes
	gcDeleteRuns   atomic.Int64 // runs with delete=true
	gcRowsDeleted  atomic.Int64
	gcBlobsDeleted atomic.Int64

	tokensCreated atomic.Int64

	// Auth failures by reason. Useful to spot misconfigured clients
	// (lots of "invalid") vs expired-token churn (lots of "expired") vs
	// scope-mismatch attempts (lots of "forbidden").
	authInvalid   atomic.Int64
	authExpired   atomic.Int64
	authForbidden atomic.Int64
}

// writeProm renders the Prometheus text exposition format (version 0.0.4).
// Caller is responsible for setting Content-Type. The output is small
// enough (≤ 60 lines) that the cost of fmt.Fprintf calls is negligible.
//
// Counters use the conventional "_total" suffix; counters that have a
// natural enum dimension (publish outcome, auth failure reason) are
// emitted under one family with a label rather than three families,
// since `sum by (result) (...)` is the idiomatic Prom query.
func (m *metrics) writeProm(w io.Writer, stats storage.StorageStats) {
	// Counters — labeled families first.
	fmt.Fprintln(w, "# HELP shipd_publish_total Publish requests by outcome.")
	fmt.Fprintln(w, "# TYPE shipd_publish_total counter")
	fmt.Fprintf(w, "shipd_publish_total{result=\"ok\"} %d\n", m.publishOK.Load())
	fmt.Fprintf(w, "shipd_publish_total{result=\"conflict\"} %d\n", m.publishConflict.Load())
	fmt.Fprintf(w, "shipd_publish_total{result=\"error\"} %d\n", m.publishError.Load())

	fmt.Fprintln(w, "# HELP shipd_download_total Successful download responses by surface.")
	fmt.Fprintln(w, "# TYPE shipd_download_total counter")
	fmt.Fprintf(w, "shipd_download_total{source=\"api\"} %d\n", m.downloadAPI.Load())
	fmt.Fprintf(w, "shipd_download_total{source=\"install\"} %d\n", m.downloadInstall.Load())

	fmt.Fprintln(w, "# HELP shipd_gc_runs_total GC invocations by mode.")
	fmt.Fprintln(w, "# TYPE shipd_gc_runs_total counter")
	fmt.Fprintf(w, "shipd_gc_runs_total{mode=\"dry_run\"} %d\n", m.gcRuns.Load())
	fmt.Fprintf(w, "shipd_gc_runs_total{mode=\"delete\"} %d\n", m.gcDeleteRuns.Load())

	fmt.Fprintln(w, "# HELP shipd_auth_failure_total Authentication / authorization failures by reason.")
	fmt.Fprintln(w, "# TYPE shipd_auth_failure_total counter")
	fmt.Fprintf(w, "shipd_auth_failure_total{reason=\"invalid\"} %d\n", m.authInvalid.Load())
	fmt.Fprintf(w, "shipd_auth_failure_total{reason=\"expired\"} %d\n", m.authExpired.Load())
	fmt.Fprintf(w, "shipd_auth_failure_total{reason=\"forbidden\"} %d\n", m.authForbidden.Load())

	// Counters — single-sample.
	writeCounter(w, "shipd_yank_total", "Yank requests that mutated a row.", m.yankOK.Load())
	writeCounter(w, "shipd_unyank_total", "Unyank requests.", m.unyankOK.Load())
	writeCounter(w, "shipd_promote_total", "Channel-promotion requests.", m.promoteOK.Load())
	writeCounter(w, "shipd_install_page_renders_total", "Install HTML page renders.", m.installPageRenders.Load())
	writeCounter(w, "shipd_install_sig_fail_total", "Install URL signature verification failures.", m.installSigFail.Load())
	writeCounter(w, "shipd_gc_rows_deleted_total", "Release rows removed by gc --delete.", m.gcRowsDeleted.Load())
	writeCounter(w, "shipd_gc_blobs_deleted_total", "Backing blobs removed by gc --delete (after dedup check).", m.gcBlobsDeleted.Load())
	writeCounter(w, "shipd_tokens_created_total", "Tokens minted via CLI or admin API.", m.tokensCreated.Load())

	// Gauges from storage.
	writeGauge(w, "shipd_apps", "Distinct apps tracked by shipd.", stats.Apps)
	fmt.Fprintln(w, "# HELP shipd_releases Release rows by yank state.")
	fmt.Fprintln(w, "# TYPE shipd_releases gauge")
	fmt.Fprintf(w, "shipd_releases{state=\"live\"} %d\n", stats.ReleasesLive)
	fmt.Fprintf(w, "shipd_releases{state=\"yanked\"} %d\n", stats.ReleasesYanked)
	writeGauge(w, "shipd_tokens_active", "Tokens in the auth store (including expired rows).", stats.Tokens)
	writeGauge(w, "shipd_blob_bytes", "Sum of blob sizes after content-addressed dedup (approximates disk usage).", stats.BlobBytesUnique)
}

// snapshot returns a Stats with the counter fields populated; the caller
// fills in the gauge fields from storage.StorageStats.
func (m *metrics) snapshot() Stats {
	return Stats{
		PublishOK:          m.publishOK.Load(),
		PublishConflict:    m.publishConflict.Load(),
		PublishError:       m.publishError.Load(),
		Yank:               m.yankOK.Load(),
		Unyank:             m.unyankOK.Load(),
		Promote:            m.promoteOK.Load(),
		DownloadAPI:        m.downloadAPI.Load(),
		DownloadInstall:    m.downloadInstall.Load(),
		InstallPageRenders: m.installPageRenders.Load(),
		InstallSigFail:     m.installSigFail.Load(),
		GCDryRunRuns:       m.gcRuns.Load(),
		GCDeleteRuns:       m.gcDeleteRuns.Load(),
		GCRowsDeleted:      m.gcRowsDeleted.Load(),
		GCBlobsDeleted:     m.gcBlobsDeleted.Load(),
		TokensCreated:      m.tokensCreated.Load(),
		AuthInvalid:        m.authInvalid.Load(),
		AuthExpired:        m.authExpired.Load(),
		AuthForbidden:      m.authForbidden.Load(),
	}
}

func writeCounter(w io.Writer, name, help string, v int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
}

func writeGauge(w io.Writer, name, help string, v int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
}
