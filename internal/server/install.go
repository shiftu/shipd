package server

import (
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	stdtmpl "text/template"
	"time"

	"github.com/shiftu/shipd/internal/storage"
	qrcode "github.com/skip2/go-qrcode"
)

//go:embed templates/install.html templates/manifest.plist
var templatesFS embed.FS

var (
	installTmpl  = template.Must(template.ParseFS(templatesFS, "templates/install.html"))
	manifestTmpl = stdtmpl.Must(stdtmpl.ParseFS(templatesFS, "templates/manifest.plist"))
)

// installPageData is everything the install.html template needs.
type installPageData struct {
	Title        string
	Version      string
	Channel      string
	Platform     string
	SizeHuman    string
	SHA256Short  string
	PublishedAt  string
	Notes        string
	InstallURL   template.URL // itms-services:// for iOS, direct download for everything else. template.URL bypasses html/template's scheme allowlist (which would otherwise rewrite itms-services to #ZgotmplZ).
	InstallLabel string
	CanInstall   bool   // false when the artifact is missing required metadata (e.g. iOS without bundle_id)
	QRCode       string // base64-encoded PNG, empty if generation fails (page still renders)
	Yanked       bool
	YankedReason string
	Expired      bool             // true when the previous signed link expired and the page was re-entered via ?expired=1
	Alternates   []alternateLink // other-platform releases when the app has multiple platforms on the latest channel
}

// alternateLink is a non-primary platform's install option, rendered as a
// secondary button under the main one. Only populated on the unversioned
// /install/{app} path — the explicit /install/{app}/{version} path shows
// only that single release.
type alternateLink struct {
	Platform     string
	Version      string
	InstallURL   template.URL
	InstallLabel string
}

// plistData is everything the manifest.plist template needs.
type plistData struct {
	DownloadURL   string
	BundleID      string
	BundleVersion string
	Title         string
}

// --- handlers ---

// handleInstallPage renders an HTML page with a platform-appropriate install
// button and a QR code that points back at the same page. It is intentionally
// PUBLIC: a phone scanning the QR code has no API token.
//
// On the unversioned /install/{app} path it picks the primary release by
// matching User-Agent against the app's available platforms (iPhone/iPad UAs
// → ios, Android UAs → android). Other platforms appear as secondary
// "Also available" buttons so the same URL serves every visitor's device.
func (s *Server) handleInstallPage(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("name")
	version := r.PathValue("version") // empty when caller hits /install/{name}

	var primary *storage.Release
	var alternates []storage.Release
	if version == "" {
		latest, err := s.store.LatestReleases(r.Context(), app, "")
		if err != nil {
			writeStorageError(w, err)
			return
		}
		if len(latest) == 0 {
			writeError(w, http.StatusNotFound, fmt.Errorf("no releases for %q", app))
			return
		}
		primary, alternates = pickPrimary(latest, r.Header.Get("User-Agent"))
	} else {
		rel, err := s.store.GetRelease(r.Context(), app, version, "")
		if err != nil {
			writeStorageError(w, err)
			return
		}
		primary = rel
	}
	rel := primary

	publicBase := s.publicBase(r)
	// QR encodes the unversioned URL when we're on it, so a phone scanning
	// from a desktop view re-enters the page with its own UA and gets the
	// platform-appropriate primary — a desktop viewer might see Android
	// primary + iOS alternate, while the iPhone scanning the QR sees iOS
	// primary + Android alternate.
	var pageURL string
	if version == "" {
		pageURL = publicBase + "/install/" + url.PathEscape(rel.AppName)
	} else {
		pageURL = publicBase + "/install/" + url.PathEscape(rel.AppName) + "/" + url.PathEscape(rel.Version)
	}

	data := installPageData{
		Title:        installTitle(rel),
		Version:      rel.Version,
		Channel:      rel.Channel,
		Platform:     rel.Platform,
		SizeHuman:    humanSize(rel.Size),
		SHA256Short:  shortHash(rel.SHA256),
		PublishedAt:  time.Unix(rel.CreatedAt, 0).UTC().Format("2006-01-02 15:04 UTC"),
		Notes:        rel.Notes,
		Yanked:       rel.Yanked,
		YankedReason: rel.YankedReason,
		Expired:      r.URL.Query().Get("expired") == "1",
	}
	installURL, installLabel, canInstall := s.installLink(rel, publicBase)
	data.InstallURL = template.URL(installURL)
	data.InstallLabel = installLabel
	data.CanInstall = canInstall

	for i := range alternates {
		alt := &alternates[i]
		altURL, altLabel, altCan := s.installLink(alt, publicBase)
		if !altCan {
			continue
		}
		data.Alternates = append(data.Alternates, alternateLink{
			Platform:     alt.Platform,
			Version:      alt.Version,
			InstallURL:   template.URL(altURL),
			InstallLabel: altLabel,
		})
	}

	if png, err := qrcode.Encode(pageURL, qrcode.Medium, 320); err == nil {
		data.QRCode = base64.StdEncoding.EncodeToString(png)
	} else {
		s.log.Printf("qrcode encode failed: %v", err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page embeds freshly-minted signed URLs on every render; caching
	// would serve stale (expired) URLs to later visitors.
	w.Header().Set("Cache-Control", "no-store")
	s.metrics.installPageRenders.Add(1)
	if err := installTmpl.Execute(w, data); err != nil {
		s.log.Printf("install template: %v", err)
	}
}

// pickPrimary chooses which release should be the primary install button
// based on the request's User-Agent. iOS UAs (iPhone/iPad/iPod) → ios;
// Android UAs → android; everything else falls back to the first release
// alphabetically by platform. The remaining releases are returned as
// alternates in the same alphabetical order.
func pickPrimary(rels []storage.Release, ua string) (*storage.Release, []storage.Release) {
	if len(rels) == 0 {
		return nil, nil
	}
	preferred := platformFromUserAgent(ua)
	if preferred != "" {
		for i, r := range rels {
			if r.Platform == preferred {
				primary := r
				alts := append([]storage.Release{}, rels[:i]...)
				alts = append(alts, rels[i+1:]...)
				return &primary, alts
			}
		}
	}
	primary := rels[0]
	return &primary, append([]storage.Release{}, rels[1:]...)
}

// platformFromUserAgent maps a UA string to a shipd platform value. Returns
// "" when the UA matches neither iOS nor Android — desktop and other clients
// use the alphabetical fallback in pickPrimary.
func platformFromUserAgent(ua string) string {
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"), strings.Contains(ua, "iPod"):
		return "ios"
	case strings.Contains(ua, "Android"):
		return "android"
	}
	return ""
}

// handleManifestPlist returns the iOS install manifest. Public so the device
// can fetch it after the user taps the itms-services link.
func (s *Server) handleManifestPlist(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("name")
	version := r.PathValue("version")
	rel, err := s.store.GetRelease(r.Context(), app, version, "")
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if rel.BundleID == "" {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Errorf("release has no bundle_id; republish with --bundle-id"))
		return
	}
	data := plistData{
		DownloadURL:   s.signedDownloadURL(rel, s.publicBase(r)),
		BundleID:      rel.BundleID,
		BundleVersion: rel.Version,
		Title:         installTitle(rel),
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if err := manifestTmpl.Execute(w, data); err != nil {
		s.log.Printf("plist template: %v", err)
	}
}

// handleInstallDownload streams the artifact bytes. Same as handleDownload but
// reachable without a token, since iOS itms-services and direct browser
// installs come from devices that don't have the API token.
func (s *Server) handleInstallDownload(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("name")
	version := r.PathValue("version")
	rel, err := s.store.GetRelease(r.Context(), app, version, "")
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
	s.metrics.downloadInstall.Add(1)
	if _, err := io.Copy(w, body); err != nil && !errors.Is(err, io.EOF) {
		s.log.Printf("install download stream: %v", err)
	}
}

// --- helpers ---

// publicBase returns the scheme://host the user reached us by. Honors a
// configured override (PublicBaseURL) and the X-Forwarded-Proto header set by
// reverse proxies.
func (s *Server) publicBase(r *http.Request) string {
	if s.cfg.PublicBaseURL != "" {
		return s.cfg.PublicBaseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}

// installLink returns the URL the install button points at, the label to put
// on the button, and whether the install can actually proceed.
//
// It's a method so the signer can be reached: when signing is enabled, the
// download URL (and the plist URL embedded inside the itms-services link)
// carries an HMAC signature that limits its lifetime.
func (s *Server) installLink(rel *storage.Release, publicBase string) (string, string, bool) {
	switch rel.Platform {
	case "ios":
		if rel.BundleID == "" {
			return "#", "iOS install unavailable (no bundle_id)", false
		}
		plistPath := "/install/" + url.PathEscape(rel.AppName) + "/" + url.PathEscape(rel.Version) + "/manifest.plist"
		plistURL := publicBase + plistPath
		if s.signer != nil {
			plistURL += "?" + s.signer.SignedQuery(plistPath)
		}
		return "itms-services://?action=download-manifest&url=" + url.QueryEscape(plistURL), "Install on iOS", true
	case "android":
		return s.signedDownloadURL(rel, publicBase), "Install on Android", true
	default:
		return s.signedDownloadURL(rel, publicBase), "Download", true
	}
}

// signedDownloadURL builds the public install download URL for a release,
// appending an HMAC signature query when signing is enabled.
func (s *Server) signedDownloadURL(rel *storage.Release, publicBase string) string {
	path := "/install/" + url.PathEscape(rel.AppName) + "/" + url.PathEscape(rel.Version) + "/download"
	full := publicBase + path
	if s.signer != nil {
		full += "?" + s.signer.SignedQuery(path)
	}
	return full
}

func installTitle(rel *storage.Release) string {
	if rel.DisplayName != "" {
		return rel.DisplayName
	}
	return rel.AppName
}

func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// humanSize matches the formatter the CLI uses (kept here so the server has no
// reverse dep on internal/cli).
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	suffix := []string{"KiB", "MiB", "GiB", "TiB"}[exp]
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), suffix)
}
