package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The upload route never answers 404 itself, so a 404 means the host does not
// serve it. That gets its own exit code and says so, rather than reading as a
// rejected key (exit 2).
func TestUploadEndpointNotServedExit4(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"not_found"}`)
	}))
	defer srv.Close()

	path := writeSymFile(t, "app.sym", "MODULE a b c d\n")
	stdout, _, code := runCLI(t, []string{"symbols", "upload", "--platform", "linux", path}, envWith(srv.URL))
	if code != ExitNotServed {
		t.Fatalf("exit = %d, want %d", code, ExitNotServed)
	}
	if hits.Load() != 1 {
		t.Fatalf("requests = %d, want 1 (a 404 is not retried)", hits.Load())
	}
	if !strings.Contains(stdout, "does not serve POST /api/v1/symbols/upload (http 404); nothing is wrong with your API key or your symbols") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// Every request carries the API key, so plain http is refused unless the host
// is loopback, and the refusal comes before any request is made.
func TestPlainHTTPRefusedOutsideLoopback(t *testing.T) {
	instantRetries(t)
	path := writeSymFile(t, "app.sym", "MODULE a b c d\n")
	for _, base := range []string{"http://symbols.example.invalid", "http://192.0.2.1:8080", "http://10.0.0.5"} {
		for _, argv := range [][]string{
			{"symbols", "upload", "--timeout", "200ms", "--url", base, "--platform", "linux", path},
			{"symbols", "missing", "--timeout", "200ms", "--url", base},
		} {
			stdout, stderr, code := runCLI(t, argv, map[string]string{"SHARDPILOT_SYMBOLS_KEY": "sp_test_key"})
			if code != ExitUsage || !strings.Contains(stderr, "refusing plain http") {
				t.Errorf("%s %s: exit = %d stderr = %q, want %d refusing plain http", argv[1], base, code, stderr, ExitUsage)
			}
			assertNoKey(t, stdout, stderr)
		}
	}
}

// Plain http to localhost or a loopback address is how local testing works,
// and stays allowed.
func TestPlainHTTPAllowedToLoopback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/symbols/wanted" {
			io.WriteString(w, `{"wanted":[],"total":0}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	path := writeSymFile(t, "app.sym", "MODULE a b c d\n")
	for _, base := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)} {
		for _, argv := range [][]string{
			{"symbols", "upload", "--url", base, "--platform", "linux", path},
			{"symbols", "missing", "--url", base},
		} {
			if _, stderr, code := runCLI(t, argv, map[string]string{"SHARDPILOT_SYMBOLS_KEY": "sp_test_key"}); code != ExitOK {
				t.Errorf("%s %s: exit = %d, want 0; stderr: %s", argv[1], base, code, stderr)
			}
		}
	}
	if hits.Load() != 4 {
		t.Fatalf("requests = %d, want 4", hits.Load())
	}
}

// SHARDPILOT_SYMBOLS_URL names the endpoint for both commands.
// SHARDPILOT_INGEST_URL is no longer read: it is the analytics ingest origin
// in the SDKs, and the symbols routes are not served there.
func TestSymbolsURLEnvReplacesIngestURL(t *testing.T) {
	instantRetries(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/symbols/wanted" {
			io.WriteString(w, `{"wanted":[],"total":0}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, createdEnvelope)
	}))
	defer srv.Close()

	env := map[string]string{
		"SHARDPILOT_SYMBOLS_KEY": "sp_test_key",
		"SHARDPILOT_SYMBOLS_URL": srv.URL,
		"SHARDPILOT_INGEST_URL":  "http://127.0.0.1:1", // must not be read
	}
	path := writeSymFile(t, "app.sym", "MODULE a b c d\n")
	for _, argv := range [][]string{
		{"symbols", "upload", "--timeout", "2s", "--platform", "linux", path},
		{"symbols", "missing", "--timeout", "2s"},
	} {
		if _, stderr, code := runCLI(t, argv, env); code != ExitOK {
			t.Errorf("%s: exit = %d, want 0; stderr: %s", argv[1], code, stderr)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("requests to SHARDPILOT_SYMBOLS_URL = %d, want 2", hits.Load())
	}
}
