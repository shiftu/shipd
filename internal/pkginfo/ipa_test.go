package pkginfo

import (
	"archive/zip"
	"bytes"
	"testing"
)

// TestExtractIPAInfo builds a minimal IPA-shaped zip in memory:
//
//	Payload/Demo.app/Info.plist  (XML plist, the format Xcode-old + howett both read)
//	Payload/Demo.app/Frameworks/Helper.framework/Info.plist (a decoy)
//
// and asserts ExtractIPAInfo pulls fields out of the top-level Info.plist,
// NOT the framework's. The depth check in findAppInfoPlist is the only thing
// stopping us from returning the framework's bundle ID by accident.
func TestExtractIPAInfo(t *testing.T) {
	mainPlist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>com.example.demo</string>
  <key>CFBundleShortVersionString</key><string>2.9.7</string>
  <key>CFBundleVersion</key><string>293</string>
</dict>
</plist>`
	frameworkPlist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
  <key>CFBundleIdentifier</key><string>com.example.demo.helper</string>
  <key>CFBundleVersion</key><string>999</string>
</dict></plist>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range []struct{ name, body string }{
		{"Payload/Demo.app/Info.plist", mainPlist},
		{"Payload/Demo.app/Frameworks/Helper.framework/Info.plist", frameworkPlist},
	} {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatalf("zip create %q: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("zip write %q: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}

	info, err := ExtractIPAInfo(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("ExtractIPAInfo: %v", err)
	}
	if info.BundleID != "com.example.demo" {
		t.Errorf("BundleID = %q, want com.example.demo (framework's plist leaked through if this fails)", info.BundleID)
	}
	if info.ShortVersion != "2.9.7" {
		t.Errorf("ShortVersion = %q, want 2.9.7", info.ShortVersion)
	}
	if info.BuildVersion != "293" {
		t.Errorf("BuildVersion = %q, want 293", info.BuildVersion)
	}
}

// TestExtractIPAInfo_NoAppBundle: a zip that has Payload/ but no .app dir
// inside it returns a clear error rather than a zero-valued IPAInfo.
func TestExtractIPAInfo_NoAppBundle(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("Payload/README.txt")
	_, _ = w.Write([]byte("not an app"))
	zw.Close()

	_, err := ExtractIPAInfo(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err == nil {
		t.Fatal("expected error for IPA without .app, got nil")
	}
}
