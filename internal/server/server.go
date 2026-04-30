package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/shiftu/shipd/internal/storage"
)

// Config holds runtime configuration for the HTTP server.
type Config struct {
	Addr           string
	PublicReads    bool   // if true, list/info/download work without a token
	BootstrapToken string // optional plaintext token; created on startup if no tokens exist
	PublicBaseURL  string // override the auto-detected public URL used in install links and QR codes
}

// Server is a thin wrapper around http.Server holding shared state.
type Server struct {
	cfg   Config
	store *storage.Store
	mux   *http.ServeMux
	log   *log.Logger
}

func New(cfg Config, store *storage.Store, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{cfg: cfg, store: store, mux: http.NewServeMux(), log: logger}
	s.routes()
	return s
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
	if err := s.store.CreateToken(ctx, "bootstrap", s.cfg.BootstrapToken, "rw"); err != nil {
		return err
	}
	s.log.Printf("created bootstrap token 'bootstrap' (provided via SHIPD_BOOTSTRAP_TOKEN)")
	return nil
}

// --- routing ---

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/apps", s.requireRead(s.handleListApps))
	s.mux.HandleFunc("GET /api/v1/apps/{name}", s.requireRead(s.handleGetApp))
	s.mux.HandleFunc("GET /api/v1/apps/{name}/releases", s.requireRead(s.handleListReleases))
	s.mux.HandleFunc("GET /api/v1/apps/{name}/releases/{version}", s.requireRead(s.handleGetRelease))
	s.mux.HandleFunc("GET /api/v1/apps/{name}/releases/{version}/download", s.requireRead(s.handleDownload))
	s.mux.HandleFunc("GET /api/v1/apps/{name}/latest", s.requireRead(s.handleLatest))

	s.mux.HandleFunc("POST /api/v1/apps/{name}/releases", s.requireWrite(s.handlePublish))
	s.mux.HandleFunc("POST /api/v1/apps/{name}/releases/{version}/yank", s.requireWrite(s.handleYank))
	s.mux.HandleFunc("POST /api/v1/apps/{name}/releases/{version}/promote", s.requireWrite(s.handlePromote))

	// Install pages and the assets they depend on are intentionally PUBLIC: the
	// device opening the page (a phone scanning a QR code, an iOS device
	// fetching the plist) does not have an API token. If you need privacy,
	// front shipd with a reverse proxy that enforces it.
	s.mux.HandleFunc("GET /install/{name}", s.handleInstallPage)
	s.mux.HandleFunc("GET /install/{name}/{version}", s.handleInstallPage)
	s.mux.HandleFunc("GET /install/{name}/{version}/manifest.plist", s.handleManifestPlist)
	s.mux.HandleFunc("GET /install/{name}/{version}/download", s.handleInstallDownload)
}

// --- handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
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
	writeJSON(w, http.StatusCreated, rel)
}

// --- middleware ---

func (s *Server) requireWrite(h http.HandlerFunc) http.HandlerFunc {
	return s.requireToken(h, "rw")
}

func (s *Server) requireRead(h http.HandlerFunc) http.HandlerFunc {
	if s.cfg.PublicReads {
		return h
	}
	return s.requireToken(h, "")
}

// requireToken enforces auth. minScope of "rw" requires a writable token; empty means any valid token.
func (s *Server) requireToken(h http.HandlerFunc, minScope string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := extractToken(r)
		if tok == "" {
			writeError(w, http.StatusUnauthorized, fmt.Errorf("missing token"))
			return
		}
		t, err := s.store.LookupToken(r.Context(), tok)
		if err != nil {
			writeError(w, http.StatusUnauthorized, fmt.Errorf("invalid token"))
			return
		}
		if minScope == "rw" && t.Scope != "rw" {
			writeError(w, http.StatusForbidden, fmt.Errorf("token lacks write scope"))
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
