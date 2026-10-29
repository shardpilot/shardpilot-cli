package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	wantedPage1 = `{"wanted":[
		{"app_key":"","platform":"linux","format":"breakpad","symbol_id":"5A9C0D3E2F1B4A6C8D7E9F0A1B2C3D4E0","first_seen_at":"2026-09-20T10:00:00Z","last_seen_at":"2026-09-27T10:00:00Z","occurrence_count":3,"available_since":null},
		{"app_key":"","platform":"windows","format":"pdb","symbol_id":"AABBCCDDEEFF00112233445566778899A","first_seen_at":"2026-09-19T10:00:00Z","last_seen_at":"2026-09-26T10:00:00Z","occurrence_count":1,"available_since":"2026-09-26T12:00:00Z"}
	],"total":3,"next":"cursor-2"}`
	wantedPage2 = `{"wanted":[
		{"app_key":"com.example.game","platform":"android","format":"proguard","symbol_id":"build-42","first_seen_at":"2026-09-18T10:00:00Z","last_seen_at":"2026-09-25T10:00:00Z","occurrence_count":7,"available_since":null}
	],"total":3}`
)

// wantedServer serves the two pages, asserting the request shape on each.
func wantedServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/symbols/wanted" {
			t.Errorf("request = %s %s, want GET /api/v1/symbols/wanted", r.Method, r.URL.Path)
		}
		if auth, limit := r.Header.Get("Authorization"), r.URL.Query().Get("limit"); auth != "Bearer sp_test_key" || limit != "200" {
			t.Errorf("Authorization = %q, limit = %q; want the bearer key and 200", auth, limit)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = w.Write([]byte(wantedPage1))
		case "cursor-2":
			_, _ = w.Write([]byte(wantedPage2))
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	}))
}

func assertNoKey(t *testing.T, outputs ...string) {
	t.Helper()
	for _, o := range outputs {
		if strings.Contains(o, "sp_test_key") {
			t.Fatalf("output carries the API key: %q", o)
		}
	}
}

func TestMissingListsStillMissingAcrossPages(t *testing.T) {
	var hits atomic.Int32
	srv := wantedServer(t, &hits)
	defer srv.Close()
	stdout, stderr, code := runCLI(t, []string{"symbols", "missing"}, envWith(srv.URL))
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	if hits.Load() != 2 {
		t.Fatalf("requests = %d, want 2 (both pages)", hits.Load())
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "5A9C0D3E2F1B4A6C8D7E9F0A1B2C3D4E0") || !strings.Contains(lines[1], "build-42") {
		t.Fatalf("stdout = %q, want the two missing identities, one per line", stdout)
	}
	if strings.Contains(stdout, "AABBCCDDEEFF") {
		t.Fatalf("stdout lists an identity that is already available: %q", stdout)
	}
	if !strings.Contains(stderr, "2 missing") {
		t.Fatalf("stderr = %q, want the missing count", stderr)
	}
	assertNoKey(t, stdout, stderr)
}

func TestMissingAllIncludesAvailableRows(t *testing.T) {
	var hits atomic.Int32
	srv := wantedServer(t, &hits)
	defer srv.Close()
	stdout, stderr, code := runCLI(t, []string{"symbols", "missing", "--all"}, envWith(srv.URL))
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	if n := len(strings.Split(strings.TrimSpace(stdout), "\n")); n != 3 {
		t.Fatalf("stdout has %d lines, want 3: %q", n, stdout)
	}
	if !strings.Contains(stdout, "available since 2026-09-26T12:00:00Z") {
		t.Fatalf("stdout = %q, want the available row marked with its time", stdout)
	}
}

func TestMissingJSONOneObjectPerRow(t *testing.T) {
	var hits atomic.Int32
	srv := wantedServer(t, &hits)
	defer srv.Close()
	stdout, stderr, code := runCLI(t, []string{"symbols", "missing", "--json"}, envWith(srv.URL))
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}
		rows = append(rows, row)
	}
	if len(rows) != 2 || rows[1]["app_key"] != "com.example.game" || rows[1]["occurrence_count"] != float64(7) {
		t.Fatalf("rows = %v, want the two missing rows with their fields", rows)
	}
}

// The wanted route never answers 404 itself, so a 404 means the host does not
// serve the endpoint: a specific message and exit code, and no retries.
func TestMissingEndpointNotServedExit4(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	instantRetries(t)
	stdout, stderr, code := runCLI(t, []string{"symbols", "missing"}, envWith(srv.URL))
	if code != ExitNotServed {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitNotServed, stderr)
	}
	if hits.Load() != 1 {
		t.Fatalf("requests = %d, want 1 (a 404 is not retried)", hits.Load())
	}
	if !strings.Contains(stderr, "does not serve") || !strings.Contains(stderr, "GET /api/v1/symbols/wanted") {
		t.Fatalf("stderr = %q, want the specific not-served message naming the endpoint", stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
}

func TestMissingRejectedExit2(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"key lacks symbols:write"}}`))
	}))
	defer srv.Close()
	_, stderr, code := runCLI(t, []string{"symbols", "missing"}, envWith(srv.URL))
	if code != ExitRejected || !strings.Contains(stderr, "key lacks symbols:write") {
		t.Fatalf("exit = %d stderr = %q, want %d with the server's message", code, stderr, ExitRejected)
	}
}

func TestMissing5xxRetriesThenExit3(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	instantRetries(t)
	_, stderr, code := runCLI(t, []string{"symbols", "missing"}, envWith(srv.URL))
	if code != ExitUnavailable || hits.Load() != 5 {
		t.Fatalf("exit = %d requests = %d stderr = %q, want %d after 5 attempts", code, hits.Load(), stderr, ExitUnavailable)
	}
}

func TestMissingNetworkFailureExit3(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	instantRetries(t)
	_, stderr, code := runCLI(t, []string{"symbols", "missing"}, envWith(url))
	if code != ExitUnavailable {
		t.Fatalf("exit = %d stderr = %q, want %d", code, stderr, ExitUnavailable)
	}
	assertNoKey(t, stderr)
}

func TestMissingUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		env  map[string]string
		want string
	}{
		{"no API key", []string{"symbols", "missing"}, map[string]string{"SHARDPILOT_SYMBOLS_URL": "https://api.example.com"}, "SHARDPILOT_SYMBOLS_KEY is not set"},
		{"plain http to a remote host", []string{"symbols", "missing"}, envWith("http://symbols.example.invalid"), "refusing plain http to symbols.example.invalid"},
		{"stray argument", []string{"symbols", "missing", "extra"}, envWith("https://api.example.com"), "takes no arguments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := runCLI(t, tc.argv, tc.env)
			if code != ExitUsage || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit = %d stderr = %q, want %d with %q", code, stderr, ExitUsage, tc.want)
			}
		})
	}
}
