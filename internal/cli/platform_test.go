package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const acceptedPlatforms = "windows, macos, linux, android, ios, tvos, web, ps4, ps5, xbox, switch, other"

// --platform sends one of the endpoint's platforms: an alias or another case
// is folded first, and says so on stderr.
func TestUploadSendsTheFoldedPlatform(t *testing.T) {
	var mu sync.Mutex
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.FormValue("platform"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()
	path := writeSymFile(t, "game.sym", "MODULE Linux x86_64 A1B2C3 game\n")

	for i, tc := range []struct {
		flag, sent string
		note       bool
	}{
		{"linux", "linux", false},
		{"tvos", "tvos", false},
		{"other", "other", false},
		{"mac", "macos", true},
		{"Darwin", "macos", true},
		{"WindowsNoEditor", "windows", true},
		{"win64", "windows", true},
		{"iPhone", "ios", true},
		{"linuxarm64", "linux", true},
		{"PS5", "ps5", true},
	} {
		stdout, stderr, code := runCLI(t, []string{"symbols", "upload", "--platform", tc.flag, path}, envWith(srv.URL))
		if code != ExitOK {
			t.Fatalf("%s: exit = %d; stdout %q stderr %q", tc.flag, code, stdout, stderr)
		}
		mu.Lock()
		got := received[i]
		mu.Unlock()
		if got != tc.sent {
			t.Fatalf("%s: the endpoint received platform %q, want %q", tc.flag, got, tc.sent)
		}
		if !strings.Contains(stdout, "platform="+tc.sent) {
			t.Fatalf("%s: the result line must show the platform sent: %q", tc.flag, stdout)
		}
		if note := strings.Contains(stderr, `is sent as "`+tc.sent+`"`); note != tc.note {
			t.Fatalf("%s: fold note on stderr = %t, want %t: %q", tc.flag, note, tc.note, stderr)
		}
	}

	// Under --json the result object carries the platform sent.
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--json", "--platform", "OSX", path}, envWith(srv.URL))
	var result struct {
		Platform string `json:"platform"`
	}
	if code != ExitOK || json.Unmarshal([]byte(stdout), &result) != nil || result.Platform != "macos" {
		t.Fatalf("--json: exit %d, stdout %q", code, stdout)
	}
}

// A platform the endpoint would refuse is refused before any request, with
// the accepted values listed.
func TestUploadRefusesAnUnknownPlatform(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	path := writeSymFile(t, "game.sym", "MODULE Linux x86_64 A1B2C3 game\n")

	for _, platform := range []string{"steam", "win32", "playstation", "unknown", "  "} {
		_, stderr, code := runCLI(t, []string{"symbols", "upload", "--platform", platform, path}, envWith(srv.URL))
		if code != ExitUsage || !strings.Contains(stderr, "accepted: "+acceptedPlatforms) {
			t.Fatalf("%q: exit = %d, stderr %q; want %d listing the platforms", platform, code, stderr, ExitUsage)
		}
	}
	if _, stderr, code := runCLI(t, []string{"symbols", "upload", path}, envWith(srv.URL)); code != ExitUsage || !strings.Contains(stderr, "--platform is required; accepted: "+acceptedPlatforms) {
		t.Fatalf("missing --platform: exit = %d, stderr %q", code, stderr)
	}
	if hits.Load() != 0 {
		t.Fatalf("server hits = %d, want 0", hits.Load())
	}
}
