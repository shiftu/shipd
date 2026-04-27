package pkginfo

import (
	"path/filepath"
	"strings"
)

// InferAppName drops the extension and any trailing "-vX.Y.Z" / "-X.Y.Z"
// suffix so a filename like "myapp-v1.2.3.ipa" yields "myapp". Used by both
// the CLI publish command and the MCP publish tool so they agree on the
// inferred app name.
func InferAppName(filename string) string {
	name := strings.TrimSuffix(filename, filepath.Ext(filename))
	if i := strings.LastIndex(name, "-"); i > 0 {
		tail := name[i+1:]
		if looksLikeVersion(tail) {
			name = name[:i]
		}
	}
	return name
}

func looksLikeVersion(s string) bool {
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return false
	}
	dots := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.':
			dots++
		default:
			return false
		}
	}
	return dots >= 1
}

type Platform string

const (
	PlatformIOS     Platform = "ios"
	PlatformAndroid Platform = "android"
	PlatformMacOS   Platform = "macos"
	PlatformWindows Platform = "windows"
	PlatformLinux   Platform = "linux"
	PlatformGeneric Platform = "generic"
)

// Detect infers the target platform from a file path.
func Detect(path string) Platform {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".ipa":
		return PlatformIOS
	case ".apk", ".aab":
		return PlatformAndroid
	case ".dmg", ".pkg":
		return PlatformMacOS
	case ".exe", ".msi":
		return PlatformWindows
	case ".deb", ".rpm", ".appimage":
		return PlatformLinux
	}
	return PlatformGeneric
}
