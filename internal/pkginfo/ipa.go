package pkginfo

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"howett.net/plist"
)

// IPAInfo is the subset of Info.plist fields shipd actually needs. The IPA's
// real CFBundleShortVersionString is what iOS's appstored compares the OTA
// manifest's bundle-version against; CFBundleVersion is the build number
// users see in TestFlight-style UIs. Both are surfaced on the install page.
type IPAInfo struct {
	BundleID     string `plist:"CFBundleIdentifier"`
	ShortVersion string `plist:"CFBundleShortVersionString"`
	BuildVersion string `plist:"CFBundleVersion"`
}

// ExtractIPAInfo opens the IPA as a zip, locates Payload/<App>.app/Info.plist
// (the first top-level .app under Payload/), and decodes the property list.
// Works for both binary and XML plist formats — howett.net/plist sniffs the
// magic bytes itself.
//
// Returns the partially-populated IPAInfo with whichever fields it could read;
// if Info.plist is unreadable or absent, err is non-nil and IPAInfo is zero.
// Callers should treat extraction failure as non-fatal — the release is still
// publishable; only OTA install ergonomics degrade.
func ExtractIPAInfo(r io.ReaderAt, size int64) (IPAInfo, error) {
	z, err := zip.NewReader(r, size)
	if err != nil {
		return IPAInfo{}, fmt.Errorf("open ipa as zip: %w", err)
	}
	plistFile := findAppInfoPlist(z)
	if plistFile == nil {
		return IPAInfo{}, errors.New("ipa: Payload/*.app/Info.plist not found")
	}
	rc, err := plistFile.Open()
	if err != nil {
		return IPAInfo{}, fmt.Errorf("open Info.plist: %w", err)
	}
	defer rc.Close()
	// Buffer the whole file — Info.plist is small (a few KB) and the plist
	// decoder needs an io.ReadSeeker, which zip's file reader is not.
	buf, err := io.ReadAll(rc)
	if err != nil {
		return IPAInfo{}, fmt.Errorf("read Info.plist: %w", err)
	}
	var info IPAInfo
	if _, err := plist.Unmarshal(buf, &info); err != nil {
		return IPAInfo{}, fmt.Errorf("decode Info.plist: %w", err)
	}
	return info, nil
}

// findAppInfoPlist walks the zip directory and returns the Info.plist that
// lives directly under Payload/<App>.app/. Nested resource bundles also have
// Info.plist files (e.g. frameworks, plugins) — those are skipped by the
// depth check, so we never accidentally read a framework's bundle ID.
func findAppInfoPlist(z *zip.Reader) *zip.File {
	for _, f := range z.File {
		name := path.Clean(f.Name)
		parts := strings.Split(name, "/")
		// Expect: ["Payload", "<App>.app", "Info.plist"] — exactly 3 segments.
		if len(parts) != 3 {
			continue
		}
		if parts[0] != "Payload" || !strings.HasSuffix(parts[1], ".app") || parts[2] != "Info.plist" {
			continue
		}
		return f
	}
	return nil
}
