package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A redirect is never followed, so the API key goes only to the URL that was
// set. Go's client keeps Authorization on a redirect to the same hostname,
// whatever the port or scheme, so following one could carry the key to a
// listener the user never named, or over plain http after an https start.
// A 3xx means this URL does not serve the endpoint: exit 4, like a 404.
func TestRedirectsAreNeverFollowed(t *testing.T) {
	instantRetries(t)
	var targetHits atomic.Int32
	var leakedAuth atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		leakedAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/symbols/wanted" {
			io.WriteString(w, `{"wanted":[],"total":0}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer target.Close()

	path := writeSymFile(t, "app.sym", "MODULE a b c d\n")
	for _, tc := range []struct {
		name   string
		status int
		argv   []string
		stream string
	}{
		{"upload, 302", http.StatusFound, []string{"symbols", "upload", "--platform", "linux", path}, "stdout"},
		{"upload, 303", http.StatusSeeOther, []string{"symbols", "upload", "--platform", "linux", path}, "stdout"},
		{"missing, 307", http.StatusTemporaryRedirect, []string{"symbols", "missing"}, "stderr"},
		{"missing, 301", http.StatusMovedPermanently, []string{"symbols", "missing"}, "stderr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targetHits.Store(0)
			leakedAuth.Store("")
			var hits atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				http.Redirect(w, r, target.URL+r.URL.RequestURI(), tc.status)
			}))
			defer origin.Close()

			stdout, stderr, code := runCLI(t, tc.argv, envWith(origin.URL))
			out := stdout
			if tc.stream == "stderr" {
				out = stderr
			}
			if targetHits.Load() != 0 {
				t.Fatalf("the redirect was followed: target got %d request(s), Authorization %q", targetHits.Load(), leakedAuth.Load())
			}
			if code != ExitNotServed || !strings.Contains(out, "redirect") || !strings.Contains(out, "does not follow redirects") {
				t.Fatalf("exit = %d %s = %q, want %d naming the redirect", code, tc.stream, out, ExitNotServed)
			}
			if hits.Load() != 1 {
				t.Fatalf("origin requests = %d, want 1 (a redirect is not retried)", hits.Load())
			}
		})
	}
}
