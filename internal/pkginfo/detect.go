package pkginfo

import (
	"path/filepath"
	"strings"
)

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
