package symbols

import "testing"

func TestNormalizePlatform(t *testing.T) {
	t.Parallel()

	for _, platform := range Platforms {
		if got, ok := NormalizePlatform(platform); !ok || got != platform {
			t.Errorf("NormalizePlatform(%q) = %q, %t; want itself", platform, got, ok)
		}
	}
	if len(Platforms) != 12 {
		t.Fatalf("Platforms has %d values, want 12", len(Platforms))
	}
	for raw, want := range map[string]string{
		"mac":             "macos",
		"OSX":             "macos",
		"Darwin":          "macos",
		"win":             "windows",
		"Win64":           "windows",
		"Windows":         "windows",
		"WindowsNoEditor": "windows",
		"WindowsClient":   "windows",
		"WindowsServer":   "windows",
		"linuxarm64":      "linux",
		"iPhone":          "ios",
		"iPad":            "ios",
		"iPadOS":          "ios",
		"  Linux\t":       "linux",
		"TVOS":            "tvos",
		"PS5":             "ps5",
		"Switch":          "switch",
	} {
		if got, ok := NormalizePlatform(raw); !ok || got != want {
			t.Errorf("NormalizePlatform(%q) = %q, %t; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "  ", "unknown", "steam", "playstation", "win32", "linux-x64", "mac os"} {
		if got, ok := NormalizePlatform(raw); ok {
			t.Errorf("NormalizePlatform(%q) = %q, accepted; want it refused", raw, got)
		}
	}
}
