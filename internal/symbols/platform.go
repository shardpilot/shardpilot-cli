package symbols

import "strings"

// Platforms are the values the symbol-upload endpoint accepts for its
// `platform` field. A symbol is matched to a crash by platform, so the endpoint
// refuses any other value rather than store a symbol under a platform no crash
// will ever carry.
var Platforms = []string{"windows", "macos", "linux", "android", "ios", "tvos", "web", "ps4", "ps5", "xbox", "switch", "other"}

// platformAliases are the other spellings engines and SDKs use for a platform.
// The endpoint folds them into the platform before it checks the value, and so
// does the CLI.
var platformAliases = map[string]string{
	"mac":        "macos",
	"osx":        "macos",
	"darwin":     "macos",
	"win":        "windows",
	"win64":      "windows",
	"linuxarm64": "linux",
	"iphone":     "ios",
	"ipad":       "ios",
	"ipados":     "ios",
}

// NormalizePlatform trims and lowercases raw and folds an alias into its
// platform; an engine target name that starts with "windows" (such as
// WindowsNoEditor) is windows. It reports whether the result is one of
// Platforms.
func NormalizePlatform(raw string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if folded, ok := platformAliases[value]; ok {
		value = folded
	} else if strings.HasPrefix(value, "windows") {
		value = "windows"
	}
	for _, platform := range Platforms {
		if value == platform {
			return value, true
		}
	}
	return value, false
}
