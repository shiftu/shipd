package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shiftu/shipd/internal/storage"
)

// Config holds runtime configuration for the HTTP server.
type Config struct {
	Addr           string
	PublicReads    bool   // if true, list/info/download work without a token
	BootstrapToken string // optional plaintext token; created on startup if no tokens exist
	PublicBaseURL  string // override the auto-detected public URL used in install links and QR codes
	DataDir        string // used to persist the install-URL signing secret when InstallURLSecret is empty

	// InstallURLTTL controls signed install URLs. When > 0, links to
	// /install/.../{download,manifest.plist} carry an HMAC signature that
	// expires after this duration; expired links 303-redirect back to the
	// install page (or 410 for plist) so the user can mint a fresh one with
	// one click. When == 0, install routes are fully public — backwards-
	// compat with shipd ≤ v0.9.
	InstallURLTTL time.Duration
	// InstallURLSecret is the HMAC key as hex (≥ 32 bytes). If empty, the
	// secret is auto-generated on first start and persisted under DataDir.
	InstallURLSecret string
}

// Server is a thin wrapper around http.Server holding shared state.
type Server struct {
	cfg     Config
	store   *storage.Store
	mux     *http.ServeMux
	log     *log.Logger
	signer  *installSigner // nil when InstallURLTTL == 0
	metrics *metrics
}

func New(cfg Config, store *storage.Store, logger *log.Logger) (*Server, error) {
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{cfg: cfg, store: store, mux: http.NewServeMux(), log: logger, metrics: &metrics{}}

	if cfg.InstallURLTTL > 0 {
		secret, err := loadOrGenInstallSecret(cfg.DataDir, cfg.InstallURLSecret)
		if err != nil {
			return nil, fmt.Errorf("install-url signer: %w", err)
		}
		s.signer = &installSigner{secret: secret, ttl: cfg.InstallURLTTL}
	}
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler { return logRequests(s.log, s.mux) }

func (s *Server) ListenAndServe(ctx context.Context) error {
	if s.cfg.BootstrapToken != "" {
		if err := s.bootstrapToken(ctx); err != nil {
			return fmt.Errorf("bootstrap token: %w", err)
		}
	}
	hs := &http.Server{Addr: s.cfg.Addr, Handler: s.Handler()}
	go func() {
		<-ctx.Done()
		_ = hs.Shutdown(context.Background())
	}()
	s.log.Printf("listening on %s (public_reads=%v)", s.cfg.Addr, s.cfg.PublicReads)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) bootstrapToken(ctx context.Context) error {
	tokens, err := s.store.ListTokens(ctx)
	if err != nil {
		return err
	}
	if len(tokens) > 0 {
		return nil
	}
	// Bootstrap token never expires AND has admin scope — losing it locks
	// the operator out of their own server, and they need admin to create
	// further tokens of any scope via the API.
	if err := s.store.CreateToken(ctx, "bootstrap", s.cfg.BootstrapToken, "admin", 0); err != nil {
		return err
	}
	s.log.Printf("created bootstrap token 'bootstrap' (provided via SHIPD_BOOTSTRAP_TOKEN)")
	return nil
}

// --- routing ---

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	// /metrics is admin-scoped: a Prometheus scraper configures its
	// authorization with an admin token via the standard Bearer flow.
	// Operational counts and storage stats leak more than reads do, so
	// this stays out of the public surface even when --public-reads is on.
	s.mux.HandleFunc("GET /metrics", s.requireAdmin(s.handleMetrics))
	s.mux.HandleFunc("GET /api/v1/apps", s.requireRead(s.handleListApps))
	s.mux.HandleFunc("GET /api/v1/apps/{name}", s.requireRead(s.handleGetApp))
	s.mux.HandleFunc("GET /api/v1/apps/{name}/releases", s.requireRead(s.handleListReleases))
	s.mux.HandleFunc("GET /api/v1/apps/{name}/releases/{version}", s.requireRead(s.handleGetRelease))
	s.mux.HandleFunc("GET /api/v1/apps/{name}/releases/{version}/download", s.requireRead(s.handleDownload))
	s.mux.HandleFunc("GET /api/v1/apps/{name}/latest", s.requireRead(s.handleLatest))

	s.mux.HandleFunc("POST /api/v1/apps/{name}/releases", s.requireWrite(s.handlePublish))
	s.mux.HandleFunc("POST /api/v1/apps/{name}/releases/{version}/yank", s.requireWrite(s.handleYank))
	s.mux.HandleFunc("POST /api/v1/apps/{name}/releases/{version}/unyank", s.requireWrite(s.handleUnyank))
	s.mux.HandleFunc("POST /api/v1/apps/{name}/releases/{version}/promote", s.requireWrite(s.handlePromote))

	// Admin endpoints: destructive (gc) or privilege-escalating (token
	// creation). admin scope required on every call; the bootstrap token
	// is the only one created with admin by default.
	s.mux.HandleFunc("POST /api/v1/admin/gc", s.requireAdmin(s.handleGC))
	s.mux.HandleFunc("POST /api/v1/admin/tokens", s.requireAdmin(s.handleCreateToken))

	// The HTML install page is intentionally public — a phone scanning a QR
	// code has no API token. The page mints fresh signed URLs at render time
	// for the routes below; with --install-url-ttl=0 those signatures are
	// disabled and the routes are fully public, matching pre-signing behavior.
	s.mux.HandleFunc("GET /install/{name}", s.handleInstallPage)
	s.mux.HandleFunc("GET /install/{name}/{version}", s.handleInstallPage)
	s.mux.HandleFunc("GET /install/{name}/{version}/manifest.plist", s.verifyInstallSig(s.handleManifestPlist, false))
	s.mux.HandleFunc("GET /install/{name}/{version}/download", s.verifyInstallSig(s.handleInstallDownload, true))
}

// verifyInstallSig wraps handlers under /install/.../{download,manifest.plist}.
// When signing is enabled, missing/invalid/expired signatures are rejected.
//
// For the download path, redirectOnFail=true sends a 303 back to the install
// page with ?expired=1 — browser users (Android, generic) get a one-click
// recovery. For the plist path, iOS expects a plist body and won't render
// HTML, so we return 410 Gone with a plain-text message; the user has to go
// back and refresh the install page manually.
func (s *Server) verifyInstallSig(h http.HandlerFunc, redirectOnFail bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.signer == nil {
			h(w, r)
			return
		}
		q := r.URL.Query()
		if err := s.signer.Verify(r.URL.Path, q.Get("exp"), q.Get("sig")); err != nil {
			s.metrics.installSigFail.Add(1)
			s.log.Printf("install-url verify failed: path=%s err=%v", r.URL.Path, err)
			if redirectOnFail {
				page := "/install/" + url.PathEscape(r.PathValue("name")) +
					"/" + url.PathEscape(r.PathValue("version")) + "?expired=1"
				http.Redirect(w, r, page, http.StatusSeeOther)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte("Install link expired. Please refresh the install page and try again.\n"))
			return
		}
		h(w, r)
	}
}

// --- handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.writeProm(w, stats)
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := s.store.GetApp(r.Context(), name)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rels, err := s.store.ListReleases(r.Context(), name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": rels})
}

func (s *Server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	version := r.PathValue("version")
	channel := r.URL.Query().Get("channel")
	rel, err := s.store.GetRelease(r.Context(), name, version, channel)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	channel := r.URL.Query().Get("channel")
	rel, err := s.store.LatestRelease(r.Context(), name, channel)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	version := r.PathValue("version")
	channel := r.URL.Query().Get("channel")
	rel, err := s.store.GetRelease(r.Context(), name, version, channel)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	body, err := s.store.OpenBlob(rel)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", rel.Size))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, rel.Filename))
	w.Header().Set("X-Content-SHA256", rel.SHA256)
	s.metrics.downloadAPI.Add(1)
	if _, err := copyTo(w, body); err != nil {
		s.log.Printf("download stream error: %v", err)
	}
}

// publishMeta carries metadata fields sent alongside the upload via query params or headers.
type publishMeta struct {
	Version     string
	Channel     string
	Platform    string
	Notes       string
	Filename    string
	BundleID    string
	DisplayName string
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	q := r.URL.Query()
	meta := publishMeta{
		Version:     q.Get("version"),
		Channel:     q.Get("channel"),
		Platform:    q.Get("platform"),
		Notes:       q.Get("notes"),
		Filename:    q.Get("filename"),
		BundleID:    q.Get("bundle_id"),
		DisplayName: q.Get("display_name"),
	}
	if meta.Version == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("version is required"))
		return
	}
	if meta.Channel == "" {
		meta.Channel = "stable"
	}
	if meta.Platform == "" {
		meta.Platform = "generic"
	}
	if meta.Filename == "" {
		meta.Filename = fmt.Sprintf("%s-%s", name, meta.Version)
	}

	if err := s.store.UpsertApp(r.Context(), name, meta.Platform); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	rel, err := s.store.PutRelease(r.Context(), storage.Release{
		AppName:     name,
		Version:     meta.Version,
		Channel:     meta.Channel,
		Platform:    meta.Platform,
		Filename:    meta.Filename,
		Notes:       meta.Notes,
		BundleID:    meta.BundleID,
		DisplayName: meta.DisplayName,
	}, r.Body)
	if err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			s.metrics.publishConflict.Add(1)
			writeError(w, http.StatusConflict, err)
			return
		}
		s.metrics.publishError.Add(1)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.metrics.publishOK.Add(1)
	writeJSON(w, http.StatusCreated, rel)
}

func (s *Server) handleYank(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	version := r.PathValue("version")
	channel := r.URL.Query().Get("channel")
	reason := r.URL.Query().Get("reason")
	if err := s.store.YankRelease(r.Context(), name, version, channel, reason); err != nil {
		writeStorageError(w, err)
		return
	}
	s.metrics.yankOK.Add(1)
	writeJSON(w, http.StatusOK, map[string]string{"status": "yanked"})
}

// handlePromote copies a release onto another channel without re-uploading
// the blob. The destination channel comes from ?to=...; the optional ?from=...
// disambiguates when the version exists on multiple source channels.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	version := r.PathValue("version")
	to := r.URL.Query().Get("to")
	from := r.URL.Query().Get("from")
	if to == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("query param 'to' is required"))
		return
	}
	rel, err := s.store.PromoteRelease(r.Context(), name, version, from, to)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrNotFound):
			writeError(w, http.StatusNotFound, err)
		case errors.Is(err, storage.ErrAlreadyExists):
			writeError(w, http.StatusConflict, err)
		default:
			// Ambiguous-source / yanked-source / same-channel are all caller errors.
			writeError(w, http.StatusBadRequest, err)
		}
		return
	}
	s.metrics.promoteOK.Add(1)
	writeJSON(w, http.StatusCreated, rel)
}

// handleUnyank reverses a yank — used to recover a release whose blob was
// kept by gc's --keep-last safety net but whose row is still flagged yanked.
// Idempotent: unyanking a non-yanked release is fine.
func (s *Server) handleUnyank(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	version := r.PathValue("version")
	channel := r.URL.Query().Get("channel")
	if err := s.store.UnyankRelease(r.Context(), name, version, channel); err != nil {
		writeStorageError(w, err)
		return
	}
	s.metrics.unyankOK.Add(1)
	writeJSON(w, http.StatusOK, map[string]string{"status": "unyanked"})
}

// handleGC runs the same logic as `shipd gc`, exposed over HTTP so an agent
// (via MCP or Slack) can run it. Scope is admin because it is destructive.
//
// Query params (all optional):
//
//	older_than=30d   minimum age since yank (default 30d, 0 disables)
//	keep_last=1      protected per (app, channel, platform), default 1
//	delete=true      actually delete (default: dry-run)
func (s *Server) handleGC(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	olderThan := 30 * 24 * time.Hour
	if v := q.Get("older_than"); v != "" {
		d, err := storage.ParseTTL(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("older_than: %w", err))
			return
		}
		olderThan = d
	}

	keepLast := 1
	if v := q.Get("keep_last"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("keep_last must be a non-negative integer"))
			return
		}
		keepLast = n
	}

	doDelete := q.Get("delete") == "true" || q.Get("delete") == "1"
	if doDelete {
		s.metrics.gcDeleteRuns.Add(1)
	} else {
		s.metrics.gcRuns.Add(1)
	}

	cands, err := s.store.GCCandidates(r.Context(), olderThan, keepLast)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	type cand struct {
		AppName  string `json:"app"`
		Version  string `json:"version"`
		Channel  string `json:"channel"`
		Platform string `json:"platform"`
		Size     int64  `json:"size"`
		SHA256   string `json:"sha256"`
		YankedAt int64  `json:"yanked_at"`
		Action   string `json:"action"`
	}
	type response struct {
		DryRun       bool   `json:"dry_run"`
		Candidates   []cand `json:"candidates"`
		RowsDeleted  int    `json:"rows_deleted"`
		BlobsDeleted int    `json:"blobs_deleted"`
		BlobsKept    int    `json:"blobs_kept_shared"`
		ScannedBytes int64  `json:"scanned_bytes"`
		Errors       int    `json:"errors"`
	}
	resp := response{DryRun: !doDelete}
	for _, c := range cands {
		resp.ScannedBytes += c.Size
		entry := cand{
			AppName: c.AppName, Version: c.Version, Channel: c.Channel, Platform: c.Platform,
			Size: c.Size, SHA256: c.SHA256, YankedAt: c.YankedAt, Action: "would-delete",
		}
		if doDelete {
			blobGone, err := s.store.DeleteReleaseAndBlob(r.Context(), c.AppName, c.Version, c.Channel)
			switch {
			case err != nil:
				entry.Action = "error: " + err.Error()
				resp.Errors++
			case blobGone:
				entry.Action = "deleted"
				resp.RowsDeleted++
				resp.BlobsDeleted++
				s.metrics.gcRowsDeleted.Add(1)
				s.metrics.gcBlobsDeleted.Add(1)
			default:
				entry.Action = "row-deleted-blob-shared"
				resp.RowsDeleted++
				resp.BlobsKept++
				s.metrics.gcRowsDeleted.Add(1)
			}
		}
		resp.Candidates = append(resp.Candidates, entry)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCreateToken mints a fresh token of the requested scope. Plaintext is
// returned in the response — shown once and never recoverable.
//
// Query params:
//
//	name=<unique>    required; conflict on duplicate
//	scope=r|rw|admin defaults to rw
//	ttl=30d|...      defaults to never-expires
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name is required"))
		return
	}
	scope := q.Get("scope")
	if scope == "" {
		scope = "rw"
	}
	if !storage.ValidScopes[scope] {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown scope %q (want r, rw, or admin)", scope))
		return
	}

	var expiresAt int64
	if ttl := q.Get("ttl"); ttl != "" {
		d, err := storage.ParseTTL(ttl)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("ttl: %w", err))
			return
		}
		if d > 0 {
			expiresAt = time.Now().Add(d).Unix()
		}
	}

	plaintext, err := storage.GenerateTokenPlaintext()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.CreateToken(r.Context(), name, plaintext, scope, expiresAt); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.metrics.tokensCreated.Add(1)
	s.log.Printf("admin: created token name=%q scope=%q expires_at=%d", name, scope, expiresAt)
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":       name,
		"scope":      scope,
		"plaintext":  plaintext,
		"expires_at": expiresAt,
	})
}

// --- middleware ---

func (s *Server) requireWrite(h http.HandlerFunc) http.HandlerFunc {
	return s.requireToken(h, "rw")
}

func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return s.requireToken(h, "admin")
}

func (s *Server) requireRead(h http.HandlerFunc) http.HandlerFunc {
	if s.cfg.PublicReads {
		return h
	}
	return s.requireToken(h, "")
}

// scopeRank orders the auth scopes so requireToken can do a single
// comparison. admin > rw > r; an admin token implicitly grants rw and r,
// an rw token implicitly grants r. Unknown scope strings rank below r so
// they can't accidentally satisfy any check.
var scopeRank = map[string]int{
	"r":     1,
	"rw":    2,
	"admin": 3,
}

// requireToken enforces auth. minScope == "" means any valid token; "rw"
// requires rw or admin; "admin" requires admin.
func (s *Server) requireToken(h http.HandlerFunc, minScope string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := extractToken(r)
		if tok == "" {
			s.metrics.authInvalid.Add(1)
			writeError(w, http.StatusUnauthorized, fmt.Errorf("missing token"))
			return
		}
		t, err := s.store.LookupToken(r.Context(), tok)
		if err != nil {
			// "expired" leaks one bit (token name was once valid) but
			// substantially helps a legitimate user understand why their
			// previously-working token suddenly stopped — a worthwhile trade.
			if errors.Is(err, storage.ErrExpired) {
				s.metrics.authExpired.Add(1)
				writeError(w, http.StatusUnauthorized, fmt.Errorf("token expired"))
				return
			}
			s.metrics.authInvalid.Add(1)
			writeError(w, http.StatusUnauthorized, fmt.Errorf("invalid token"))
			return
		}
		if minScope != "" && scopeRank[t.Scope] < scopeRank[minScope] {
			s.metrics.authForbidden.Add(1)
			writeError(w, http.StatusForbidden,
				fmt.Errorf("token scope %q insufficient (need %q)", t.Scope, minScope))
			return
		}
		h(w, r)
	}
}

func extractToken(r *http.Request) string {
	if t := r.Header.Get("X-Auth-Token"); t != "" {
		return t
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func logRequests(l *log.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(sw, r)
		l.Printf("%s %s -> %d", r.Method, r.URL.Path, sw.status)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
