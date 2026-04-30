package server

import (
	"fmt"
	"io"
	"sync/atomic"

	"github.com/shiftu/shipd/internal/storage"
)

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

func writeCounter(w io.Writer, name, help string, v int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
}

func writeGauge(w io.Writer, name, help string, v int64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
}
